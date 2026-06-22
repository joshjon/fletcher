package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
)

// buildRecord is the status of one detached build (M19).
type buildRecord struct {
	state       string // "building" | "succeeded" | "failed"
	name        string
	exposedPort int
	errMsg      string
	updated     time.Time
}

// buildStateBuilding etc. are the buildRecord.state values reported to clients.
const (
	buildStateBuilding  = "building"
	buildStateSucceeded = "succeeded"
	buildStateFailed    = "failed"
)

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

	m.buildsMu.Lock()
	m.sweepBuildsLocked()
	m.builds[buildID] = &buildRecord{state: buildStateBuilding, updated: time.Now()}
	m.buildsMu.Unlock()

	go func() {
		// Detached: own context (not the request's), with a ceiling so a wedged
		// build cannot run forever.
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		name, port, berr := m.BuildImageFromSession(bctx, devRef, subdir, imageName, force)
		m.buildsMu.Lock()
		if berr != nil {
			m.builds[buildID] = &buildRecord{state: buildStateFailed, errMsg: berr.Error(), updated: time.Now()}
		} else {
			m.builds[buildID] = &buildRecord{state: buildStateSucceeded, name: name, exposedPort: port, updated: time.Now()}
		}
		m.buildsMu.Unlock()
	}()
	return buildID, nil
}

// BuildStatus reports a detached build's state. An unknown id (e.g. the daemon
// restarted, or it aged out) is reported as failed so the client stops polling.
func (m *Manager) BuildStatus(buildID string) (state, name string, exposedPort int, errMsg string) {
	m.buildsMu.Lock()
	defer m.buildsMu.Unlock()
	rec, ok := m.builds[buildID]
	if !ok {
		return buildStateFailed, "", 0, "build not found (it may have expired or the daemon restarted); try again"
	}
	return rec.state, rec.name, rec.exposedPort, rec.errMsg
}

// sweepBuildsLocked drops terminal build records older than an hour so the map
// does not grow unbounded. Caller holds buildsMu.
func (m *Manager) sweepBuildsLocked() {
	for id, rec := range m.builds {
		if rec.state != buildStateBuilding && time.Since(rec.updated) > time.Hour {
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
	rootfsGz, spec, exposedPort, err := m.buildInFork(ctx, contextGz)
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
func (m *Manager) buildInFork(ctx context.Context, contextGz []byte) ([]byte, appspec.Spec, int, error) {
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

	handle, err := m.runtime.StartSession(ctx, runtime.SessionSpec{
		SessionID:    buildID,
		RootfsPath:   fork.Path,
		Env:          m.sessionEnv("off", buildID, "build"),
		EgressPolicy: "open",
		Credentials:  []runtime.CredentialFile{{Path: buildContextPath, Mode: 0o644, Data: contextGz}},
	})
	if err != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("start build fork: %w", err)
	}
	defer func() { _ = handle.Stop(context.WithoutCancel(ctx)) }()

	// Build the Dockerfile and stage the outputs (the verified recipe: chroot
	// isolation - the microVM has no clean cgroups for crun).
	buildScript := strings.Join([]string{
		"set -e",
		"rm -rf /opt/ctx && mkdir -p /opt/ctx",
		"tar -xzf " + buildContextPath + " -C /opt/ctx",
		"buildah build --isolation chroot -t fletcherapp /opt/ctx",
		`ctr=$(buildah from fletcherapp)`,
		`mnt=$(buildah mount "$ctr")`,
		"tar -czf " + buildRootfsPath + ` -C "$mnt" .`,
		// Full inspect JSON; this buildah's template engine lacks the `json`
		// function, so the daemon parses OCIv1.config out of the raw dump.
		"buildah inspect fletcherapp > " + buildConfigPath,
	}, "\n")
	if res, eerr := m.execIn(ctx, handle, buildScript); eerr != nil {
		return nil, appspec.Spec{}, 0, fmt.Errorf("run build: %w", eerr)
	} else if res.ExitCode != 0 {
		return nil, appspec.Spec{}, 0, errs.Newf(errs.CategoryInvalidArgument, "docker build failed:\n%s", tailLines(res.Stdout+res.Stderr, 30))
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
