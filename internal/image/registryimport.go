package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/joshjon/fletcher/internal/appspec"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestagent"
)

// ImportOptions configures a server-side registry import.
type ImportOptions struct {
	// Ref is the Docker image reference to pull (e.g. ghcr.io/you/app:v1).
	Ref string
	// Name is the template name jobs/sessions reference via --image.
	Name string
	// ImagesDir is the daemon's <snapshot-root>/images directory.
	ImagesDir string
	// Username and Password authenticate to a private registry; empty falls back
	// to the host's default docker keychain.
	Username string
	Password string
	// Force replaces an existing template of the same name.
	Force bool
}

// ImportResult reports what a registry import produced.
type ImportResult struct {
	Name        string
	Digest      string
	ExposedPort int
}

// ImportRegistry pulls a Docker image from a registry and flattens it into an
// ext4 rootfs template, entirely in-process (no docker) - so the daemon can do
// it server-side on behalf of a remote client. It captures the image's run
// config for app mode (M9) and reports the image's lowest EXPOSE.
//
// It extracts as the daemon user (no root), so the rootfs files are owned by the
// daemon rather than the image's original uids - fine for app images that run as
// root, but a full base image needing setuid binaries or non-root file ownership
// should use the root-privileged CLI `image import` instead.
func ImportRegistry(ctx context.Context, opts ImportOptions) (ImportResult, error) {
	ref, err := name.ParseReference(opts.Ref)
	if err != nil {
		return ImportResult{}, fmt.Errorf("parse image ref %q: %w", opts.Ref, err)
	}

	remoteOpts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	}
	if opts.Username != "" || opts.Password != "" {
		remoteOpts = append(remoteOpts, remote.WithAuth(&authn.Basic{Username: opts.Username, Password: opts.Password}))
	} else {
		remoteOpts = append(remoteOpts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}

	img, err := remote.Image(ref, remoteOpts...)
	if err != nil {
		return ImportResult{}, fmt.Errorf("pull %q (check the ref and --registry-auth for a private image): %w", opts.Ref, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return ImportResult{}, fmt.Errorf("read image config: %w", err)
	}
	digest, err := img.Digest()
	if err != nil {
		return ImportResult{}, fmt.Errorf("read image digest: %w", err)
	}

	target := filepath.Join(opts.ImagesDir, opts.Name+".ext4")
	if _, err := os.Stat(target); err == nil {
		if !opts.Force {
			return ImportResult{}, fmt.Errorf("template %q already exists (use --force to replace)", opts.Name)
		}
		if err := os.Remove(target); err != nil {
			return ImportResult{}, fmt.Errorf("remove existing template: %w", err)
		}
	}
	if err := os.MkdirAll(opts.ImagesDir, 0o750); err != nil {
		return ImportResult{}, fmt.Errorf("create images dir: %w", err)
	}

	staging, err := os.MkdirTemp("", "fletcher-rootfs-*")
	if err != nil {
		return ImportResult{}, fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := extractRootfs(ctx, img, staging); err != nil {
		return ImportResult{}, err
	}

	// Inject the guest init and the app launch spec, so the template boots and
	// (in app mode) runs the image's own entrypoint.
	initDest := filepath.Join(staging, guestagent.InitPath)
	if err := os.MkdirAll(filepath.Dir(initDest), 0o755); err != nil { //nolint:gosec // standard /sbin perms in the rootfs
		return ImportResult{}, fmt.Errorf("create init dir: %w", err)
	}
	if err := guestagent.WriteTo(initDest); err != nil {
		return ImportResult{}, fmt.Errorf("inject guest agent: %w", err)
	}
	spec := appspec.Spec{
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		Env:        cfg.Config.Env,
		WorkingDir: cfg.Config.WorkingDir,
		User:       cfg.Config.User,
	}
	if err := appspec.Write(spec, filepath.Join(staging, appspec.Path)); err != nil {
		return ImportResult{}, fmt.Errorf("write app spec: %w", err)
	}

	if err := buildExt4(ctx, staging, target); err != nil {
		_ = os.Remove(target)
		return ImportResult{}, err
	}

	exposedPort := lowestExposedPort(cfg.Config.ExposedPorts)
	meta := TemplateMeta{
		Source:      opts.Ref,
		Digest:      digest.String(),
		Format:      "ext4",
		ImportedAt:  time.Now().Unix(),
		Entrypoint:  spec.Argv(),
		ExposedPort: exposedPort,
	}
	if err := WriteMeta(opts.ImagesDir, opts.Name, meta); err != nil {
		// Non-fatal: the template is usable; only the update-check metadata is lost.
		fmt.Fprintf(os.Stderr, "warning: could not record image metadata: %v\n", err)
	}

	return ImportResult{Name: opts.Name, Digest: digest.String(), ExposedPort: exposedPort}, nil
}

// extractRootfs flattens the image's layers and writes the rootfs into dir via
// `tar -x` (run as the daemon user, so files are daemon-owned; device nodes the
// extractor cannot create are skipped).
func extractRootfs(ctx context.Context, img v1.Image, dir string) error {
	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()
	tarCmd := exec.CommandContext(ctx, "tar", "-x", "-C", dir) //nolint:gosec // dir is our own temp staging path
	tarCmd.Stdin = rc
	tarCmd.Stderr = os.Stderr
	if err := tarCmd.Run(); err != nil {
		return fmt.Errorf("extract image rootfs: %w", err)
	}
	return nil
}

// buildExt4 sizes and formats an ext4 image populated from stagingDir. Mirrors
// the CLI import's sizing (populated + 50% + 512 MiB, floored at 1 GiB).
func buildExt4(ctx context.Context, stagingDir, target string) error {
	used, err := dirSizeBytes(ctx, stagingDir)
	if err != nil {
		return err
	}
	const (
		mib   = int64(1) << 20
		floor = int64(1) << 30
	)
	size := used + used/2 + 512*mib
	if size < floor {
		size = floor
	}
	size = (size + mib - 1) / mib * mib

	truncate := exec.CommandContext(ctx, "truncate", "-s", strconv.FormatInt(size, 10), target) //nolint:gosec // fixed args + daemon path
	truncate.Stderr = os.Stderr
	if err := truncate.Run(); err != nil {
		return fmt.Errorf("allocate ext4 image: %w", err)
	}
	mkfs := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", "-d", stagingDir, target) //nolint:gosec // fixed args + daemon paths
	mkfs.Stdout = os.Stderr
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return fmt.Errorf("build ext4 rootfs: %w", err)
	}
	return nil
}

func dirSizeBytes(ctx context.Context, dir string) (int64, error) {
	out, err := exec.CommandContext(ctx, "du", "-sb", dir).Output() //nolint:gosec // dir is our own temp staging path
	if err != nil {
		return 0, fmt.Errorf("measure rootfs size: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("measure rootfs size: unexpected du output %q", out)
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse rootfs size %q: %w", fields[0], err)
	}
	return n, nil
}

// DefaultName derives a template name from an image ref - its repository's base
// name, e.g. "ghcr.io/you/app:v1" -> "app", "nginx:alpine" -> "nginx".
func DefaultName(ref string) string {
	r, err := name.ParseReference(ref)
	if err != nil {
		return ref
	}
	return path.Base(r.Context().RepositoryStr())
}

// lowestExposedPort returns the smallest TCP port the image EXPOSEs, or 0.
func lowestExposedPort(exposed map[string]struct{}) int {
	best := 0
	for p := range exposed { // e.g. "80/tcp"
		numStr, _, _ := strings.Cut(p, "/")
		n, err := strconv.Atoi(numStr)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		if best == 0 || n < best {
			best = n
		}
	}
	return best
}
