package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/appspec"
	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/image"
	"github.com/joshjon/fletcher/internal/imagebuild"
	"github.com/joshjon/fletcher/internal/runtime"
)

const (
	// builderImage is the template the ephemeral build fork boots from
	// (images/fletcher-builder: buildah + crun, vfs storage). It must be imported
	// (`fletcher image import fletcher-builder:dev --name fletcher-builder`).
	builderImage = "fletcher-builder"
	// buildContextPath is where the daemon injects the project's build context
	// (a .tar.gz) into the build fork; the build script extracts and builds it.
	buildContextPath = "/opt/build-context.tar.gz"
	// The build fork writes its outputs to these fixed paths, which the daemon
	// reads back over exec.
	buildRootfsPath = "/opt/rootfs.tar.gz"
	buildConfigPath = "/opt/app-config.json"
	// buildForkSizeBytes is how large the ephemeral build fork's root disk is
	// grown to before boot. The builder template is ~1 GiB (leaving only a few
	// hundred MiB free), and buildah's vfs storage duplicates every layer, so a
	// real build (e.g. a pnpm install) runs out of space fast. The grown image
	// is sparse, so this costs only the build's actual usage on the host.
	buildForkSizeBytes = 20 * (int64(1) << 30) // 20 GiB

	// The persistent buildah layer cache (M20): a daemon-owned ext4 disk
	// attached to each build fork so layers survive between builds. Sparse and
	// capped; reset (one cold build) if it grows past the reset threshold, so a
	// runaway cache can never fill the host. buildah's graphroot lives on it;
	// the runroot stays on ephemeral tmpfs so stale locks reset each boot.
	buildCacheSizeBytes  = 40 * (int64(1) << 30) // 40 GiB sparse cap
	buildCacheResetBytes = 30 * (int64(1) << 30) // recreate when real usage exceeds this
	buildCacheGraphroot  = "/volume/storage"     // /volume is the attached cache disk
	buildCacheRunroot    = "/run/buildah"
)

// buildStateBuilding etc. are the buildRecord.state values reported to clients.
const (
	buildStateBuilding  = "building"
	buildStateSucceeded = "succeeded"
	buildStateFailed    = "failed"
)

// buildLogCap bounds how much build output a status response carries (the tail
// is what matters for live progress and failure diagnosis).
const buildLogCap = 256 * 1024

// buildRecord is the status of one detached build (M19). It guards its own
// fields so the build goroutine can append to the log while a client polls.
type buildRecord struct {
	mu          sync.Mutex
	state       string // "building" | "succeeded" | "failed"
	name        string
	exposedPort int
	errMsg      string
	log         []byte
	updated     time.Time
}

// Write appends build output to the record's log (an io.Writer for the build's
// stdout/stderr), keeping only the last buildLogCap bytes.
func (r *buildRecord) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, p...)
	if len(r.log) > buildLogCap {
		r.log = r.log[len(r.log)-buildLogCap:]
	}
	r.updated = time.Now()
	return len(p), nil
}

func (r *buildRecord) finish(state, name string, port int, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state, r.name, r.exposedPort, r.errMsg, r.updated = state, name, port, errMsg, time.Now()
}

func (r *buildRecord) snapshot() (state, name string, port int, errMsg, log string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.name, r.exposedPort, r.errMsg, string(r.log)
}

// StartBuildFromSession kicks off BuildImageFromSession DETACHED from the caller
// and returns a build id immediately, so a mobile client can poll GetBuildStatus
// and survive being backgrounded mid-build (builds take minutes). The build runs
// on its own context with a generous timeout, so a dropped client connection
// does not abort it. Obvious input errors are returned synchronously.
func (m *Manager) StartBuildFromSession(ctx context.Context, devRef, subdir, imageName string, force bool) (string, error) {
	if err := m.requireRuntime(); err != nil {
		return "", err
	}
	if !validImageName(strings.TrimSpace(imageName)) {
		return "", errs.Newf(errs.CategoryInvalidArgument,
			"invalid image name %q (lowercase letters, digits, '.', '_', '-')", imageName)
	}
	id, err := typeid.WithPrefix("build")
	if err != nil {
		return "", fmt.Errorf("generate build id: %w", err)
	}
	buildID := id.String()

	rec := &buildRecord{state: buildStateBuilding, updated: time.Now()}
	m.buildsMu.Lock()
	m.sweepBuildsLocked()
	m.builds[buildID] = rec
	m.buildsMu.Unlock()

	go func() {
		// Detached: own context (not the request's), with a ceiling so a wedged
		// build cannot run forever.
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		name, port, berr := m.buildImageFromSession(bctx, devRef, subdir, imageName, force, rec)
		if berr != nil {
			rec.finish(buildStateFailed, "", 0, berr.Error())
		} else {
			rec.finish(buildStateSucceeded, name, port, "")
		}
	}()
	return buildID, nil
}

// BuildStatus reports a detached build's state and log tail. An unknown id (e.g.
// the daemon restarted, or it aged out) is reported as failed so the client
// stops polling.
func (m *Manager) BuildStatus(buildID string) (state, name string, exposedPort int, errMsg, log string) {
	m.buildsMu.Lock()
	rec, ok := m.builds[buildID]
	m.buildsMu.Unlock()
	if !ok {
		return buildStateFailed, "", 0, "build not found (it may have expired or the daemon restarted); try again", ""
	}
	return rec.snapshot()
}

// sweepBuildsLocked drops terminal build records older than an hour so the map
// does not grow unbounded. Caller holds buildsMu.
func (m *Manager) sweepBuildsLocked() {
	for id, rec := range m.builds {
		state, _, _, _, _ := rec.snapshot()
		rec.mu.Lock()
		old := time.Since(rec.updated) > time.Hour
		rec.mu.Unlock()
		if state != buildStateBuilding && old {
			delete(m.builds, id)
		}
	}
}

// BuildImageFromSession builds a project's Dockerfile into a deployable template,
// entirely inside Fletcher (M19): it tars the project subdir out of the dev
// session, builds it with buildah in an ephemeral, sandboxed build fork (no host
// Docker), and flattens the result into an ext4 template named imageName. The
// build fork has open egress so it can pull base images through the daemon proxy,
// and is discarded when the build finishes. Returns the template name and the
// image's lowest EXPOSE.
func (m *Manager) BuildImageFromSession(ctx context.Context, devRef, subdir, imageName string, force bool) (string, int, error) {
	return m.buildImageFromSession(ctx, devRef, subdir, imageName, force, nil)
}

// buildImageFromSession is the core build; logSink (when non-nil) receives the
// live build output so the detached path can stream it to a polling client.
func (m *Manager) buildImageFromSession(ctx context.Context, devRef, subdir, imageName string, force bool, logSink io.Writer) (string, int, error) {
	if err := m.requireRuntime(); err != nil {
		return "", 0, err
	}
	imageName = strings.TrimSpace(imageName)
	if !validImageName(imageName) {
		return "", 0, errs.Newf(errs.CategoryInvalidArgument,
			"invalid image name %q (lowercase letters, digits, '.', '_', '-')", imageName)
	}
	if m.opt().ImagesDir == "" {
		return "", 0, errs.New(errs.CategoryFailedPrecondition, "the daemon has no images directory configured")
	}
	subdir = strings.TrimSpace(subdir)
	if subdir == "" {
		subdir = "."
	}

	target := filepath.Join(m.opt().ImagesDir, imageName+".ext4")
	if _, err := os.Stat(target); err == nil && !force {
		return "", 0, errs.Newf(errs.CategoryConflict, "template %q already exists (use --force to replace)", imageName)
	}

	// 1. Tar the project context out of the dev session (it runs as the login
	// user, which owns the workspace). base64 so the archive survives exec's text
	// capture; -C cds into the subdir so the archive is rooted at the project.
	tarCmd := fmt.Sprintf("tar -czf - -C %s . | base64 -w0", shellSingleQuote(subdir))
	ctxRes, err := m.Exec(ctx, devRef, tarCmd)
	if err != nil {
		return "", 0, fmt.Errorf("read build context from session %q: %w", devRef, err)
	}
	if ctxRes.ExitCode != 0 {
		return "", 0, errs.Newf(errs.CategoryInvalidArgument,
			"could not read %s in session %q (does the directory exist?): %s", subdir, devRef, strings.TrimSpace(ctxRes.Stderr))
	}
	contextGz, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ctxRes.Stdout))
	if err != nil {
		return "", 0, fmt.Errorf("decode build context: %w", err)
	}

	// 2-4. Build the Dockerfile in a sandboxed, ephemeral fork and read back the
	// flattened rootfs + run config.
	rootfsGz, spec, exposedPort, err := m.buildInFork(ctx, contextGz, logSink)
	if err != nil {
		return "", 0, err
	}

	// 5. Flatten the rootfs into the template (extract daemon-side - daemon-owned
	// files are fine for an app image that runs as root, same as a registry import).
	staging, err := os.MkdirTemp("", "fletcher-build-*")
	if err != nil {
		return "", 0, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := extractTarGz(ctx, rootfsGz, staging); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(m.opt().ImagesDir, 0o750); err != nil {
		return "", 0, fmt.Errorf("create images dir: %w", err)
	}
	if _, serr := os.Stat(target); serr == nil {
		if rerr := os.Remove(target); rerr != nil {
			return "", 0, fmt.Errorf("replace existing template: %w", rerr)
		}
	}
	if err := imagebuild.FlattenRootfs(ctx, staging, target, spec); err != nil {
		_ = os.Remove(target)
		return "", 0, err
	}

	// 6. Record metadata (local build - no registry digest; entrypoint + port for
	// app mode / deploy).
	meta := image.TemplateMeta{
		Format:      "ext4",
		ImportedAt:  time.Now().Unix(),
		Entrypoint:  spec.Argv(),
		ExposedPort: exposedPort,
	}
	if werr := image.WriteMeta(m.opt().ImagesDir, imageName, meta); werr != nil {
		m.logger.Warn("could not record built image metadata", "err", werr.Error())
	}
	return imageName, exposedPort, nil
}

// buildInFork boots an ephemeral, sandboxed build fork from the builder image
// (context injected, open egress to pull base images through the daemon proxy),
// runs the buildah build, and reads back the flattened rootfs (.tar.gz bytes)
// and the image's run config. The fork is created and discarded entirely within
// this call.
func (m *Manager) buildInFork(ctx context.Context, contextGz []byte, logSink io.Writer) ([]byte, appspec.Spec, int, error) {
	// Serialise builds: one at a time writes the shared persistent layer cache
	// (M20, the self-hosted-runner model). A queued build's caller just waits.
	m.buildCacheMu.Lock()
	defer m.buildCacheMu.Unlock()

	cachePath, err := m.ensureBuildCache(ctx)
	if err != nil {
		return nil, appspec.Spec{}, 0, err
	}

	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("generate build id: %w", err)
	}
	buildID := id.String()
	fork, err := m.snapshot.Create(ctx, builderImage)
	if err != nil {
		return nil, appspec.Spec{}, 0, errs.Newf(errs.CategoryFailedPrecondition,
			"create build fork (is the %q image imported?): %v", builderImage, err)
	}
	defer func() { _ = m.snapshot.Delete(context.WithoutCancel(ctx), fork.ID) }()

	// Grow the fork's disk so a real build has room (the template is small and
	// vfs storage is space-hungry); the grown image is sparse.
	if err := growExt4(ctx, fork.Path, buildForkSizeBytes); err != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("size build fork: %w", err)
	}

	handle, err := m.runtime.StartSession(ctx, runtime.SessionSpec{
		SessionID:    buildID,
		RootfsPath:   fork.Path,
		Env:          m.sessionEnv("off", buildID, "build"),
		EgressPolicy: "open",
		// Attach the persistent layer cache as the fork's /volume disk (M20).
		VolumePath:  cachePath,
		Credentials: []runtime.CredentialFile{{Path: buildContextPath, Mode: 0o644, Data: contextGz}},
	})
	if err != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("start build fork: %w", err)
	}
	defer func() { _ = handle.Stop(context.WithoutCancel(ctx)) }()

	// Build the Dockerfile and stage the outputs (the verified recipe: chroot
	// isolation - the microVM has no clean cgroups for crun). --layers + a
	// graphroot on the persistent cache disk (/volume) reuse unchanged layers
	// across builds; the runroot stays on ephemeral tmpfs so locks reset.
	bh := "buildah --root " + buildCacheGraphroot + " --runroot " + buildCacheRunroot
	buildScript := strings.Join([]string{
		"set -e",
		"mkdir -p " + buildCacheGraphroot + " " + buildCacheRunroot,
		"rm -rf /opt/ctx && mkdir -p /opt/ctx",
		"tar -xzf " + buildContextPath + " -C /opt/ctx",
		bh + " build --isolation chroot --layers -t fletcherapp /opt/ctx",
		`ctr=$(` + bh + ` from fletcherapp)`,
		`mnt=$(` + bh + ` mount "$ctr")`,
		"tar -czf " + buildRootfsPath + ` -C "$mnt" .`,
		// Full inspect JSON; this buildah's template engine lacks the `json`
		// function, so the daemon parses OCIv1.config out of the raw dump.
		bh + " inspect fletcherapp > " + buildConfigPath,
	}, "\n")
	// Run the build with output going both to a buffer (for the error tail) and,
	// when a sink is set, live to the build record so a client can stream it.
	// stdout+stderr share one writer so the log reads in order.
	var buildOut strings.Builder
	var out io.Writer = &buildOut
	if logSink != nil {
		out = io.MultiWriter(&buildOut, logSink)
	}
	if res, eerr := handle.Exec(ctx, runtime.Spec{Command: buildScript}, out, out); eerr != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("run build: %w", eerr)
	} else if res.ExitCode != 0 {
		return nil, appspec.Spec{}, 0, errs.Newf(errs.CategoryInvalidArgument, "docker build failed:\n%s", tailLines(buildOut.String(), 40))
	}

	rootfsRes, err := m.execIn(ctx, handle, "base64 -w0 "+buildRootfsPath)
	if err != nil || rootfsRes.ExitCode != 0 {
		return nil, appspec.Spec{}, 0, fmt.Errorf("read built rootfs: %w%s", err, rootfsRes.Stderr)
	}
	rootfsGz, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rootfsRes.Stdout))
	if err != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("decode built rootfs: %w", err)
	}
	cfgRes, err := m.execIn(ctx, handle, "cat "+buildConfigPath)
	if err != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("read image config: %w", err)
	}
	spec, exposedPort := parseOCIConfig(cfgRes.Stdout)
	return rootfsGz, spec, exposedPort, nil
}

// parseOCIConfig pulls the app launch spec and the image's lowest EXPOSE out of
// a raw `buildah inspect` dump (its OCIv1.config object).
func parseOCIConfig(jsonStr string) (appspec.Spec, int) {
	var inspect struct {
		OCIv1 struct {
			Config struct {
				Entrypoint   []string            `json:"Entrypoint"`
				Cmd          []string            `json:"Cmd"`
				Env          []string            `json:"Env"`
				WorkingDir   string              `json:"WorkingDir"`
				User         string              `json:"User"`
				ExposedPorts map[string]struct{} `json:"ExposedPorts"`
			} `json:"config"`
		} `json:"OCIv1"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(jsonStr)), &inspect)
	cfg := inspect.OCIv1.Config
	spec := appspec.Spec{
		Entrypoint: cfg.Entrypoint,
		Cmd:        cfg.Cmd,
		Env:        cfg.Env,
		WorkingDir: cfg.WorkingDir,
		User:       cfg.User,
	}
	best := 0
	for p := range cfg.ExposedPorts { // e.g. "80/tcp"
		numStr, _, _ := strings.Cut(p, "/")
		n, perr := strconv.Atoi(numStr)
		if perr != nil || n < 1 || n > 65535 {
			continue
		}
		if best == 0 || n < best {
			best = n
		}
	}
	return spec, best
}

// buildCachePath is the persistent build cache disk, alongside the images dir.
func (m *Manager) buildCachePath() string {
	if m.opt().ImagesDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(m.opt().ImagesDir), "buildcache.ext4")
}

// ensureBuildCache returns the path to the persistent layer-cache ext4 disk,
// creating it if missing and resetting it (one cold build) if it has grown past
// the reset threshold. Caller holds buildCacheMu, so there is no concurrent use.
func (m *Manager) ensureBuildCache(ctx context.Context) (string, error) {
	path := m.buildCachePath()
	if path == "" {
		return "", errs.New(errs.CategoryFailedPrecondition, "the daemon has no images directory configured")
	}
	if info, err := os.Stat(path); err == nil {
		if allocatedBytes(info) <= buildCacheResetBytes {
			return path, nil
		}
		m.logger.Info("resetting oversized build cache", "path", path)
		if rerr := os.Remove(path); rerr != nil {
			return "", fmt.Errorf("reset build cache: %w", rerr)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := exec.CommandContext(ctx, "truncate", "-s", strconv.FormatInt(buildCacheSizeBytes, 10), path).Run(); err != nil { //nolint:gosec // daemon-owned path
		return "", fmt.Errorf("allocate build cache: %w", err)
	}
	// -F: a plain file; -q: quiet. A journaled fs, so an unclean fork shutdown
	// replays on next mount - no fsck needed.
	mkfs := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", path) //nolint:gosec // daemon-owned path
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("format build cache: %w", err)
	}
	return path, nil
}

// allocatedBytes is the real on-disk size of a (sparse) file - its allocated
// blocks, not its apparent length.
func allocatedBytes(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

// growExt4 grows the ext4 image at path to sizeBytes (the file is sparse, so
// this is cheap) and resizes the filesystem to fill it. The image must be
// unmounted - it is, here, before the fork boots.
func growExt4(ctx context.Context, path string, sizeBytes int64) error {
	if err := exec.CommandContext(ctx, "truncate", "-s", strconv.FormatInt(sizeBytes, 10), path).Run(); err != nil { //nolint:gosec // path is the daemon-owned fork file
		return fmt.Errorf("grow fork file: %w", err)
	}
	// resize2fs requires a clean fs; force-check the freshly-cloned image first.
	// e2fsck exits non-zero when it *corrects* issues, which is not a failure here.
	_ = exec.CommandContext(ctx, "e2fsck", "-fy", path).Run()                                 //nolint:gosec // path is the daemon-owned fork file
	if out, err := exec.CommandContext(ctx, "resize2fs", path).CombinedOutput(); err != nil { //nolint:gosec // path is the daemon-owned fork file
		return fmt.Errorf("resize fork fs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// extractTarGz extracts a gzip tar archive's bytes into dir via `tar -xzf -`.
func extractTarGz(ctx context.Context, gz []byte, dir string) error {
	cmd := exec.CommandContext(ctx, "tar", "-xzf", "-", "-C", dir) //nolint:gosec // dir is our own staging path
	cmd.Stdin = bytes.NewReader(gz)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract built rootfs: %w", err)
	}
	return nil
}

// shellSingleQuote wraps s in single quotes, safe for /bin/sh.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tailLines returns the last n lines of s (build output for an error message).
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
