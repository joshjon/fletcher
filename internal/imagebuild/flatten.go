// Package imagebuild flattens an extracted container rootfs tree into an ext4
// template the Firecracker runtime boots. It is the shared tail of two paths:
// the CLI `image import` (after `docker export`) and the daemon's session-native
// build (M19, after a `buildah` build inside a fork). Both end with the same
// steps - inject the guest init, write the app run-config, mkfs.ext4 -d - so the
// logic lives here once.
package imagebuild

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joshjon/fletcher/internal/appspec"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestagent"
)

// FlattenRootfs packs the extracted rootfs tree at stagingDir into an ext4 image
// at target: it injects the guest agent as the microVM init
// (init=/sbin/fletcher-init), writes the app run-config so an `--app` session
// runs the image's own entrypoint, and builds the ext4 with `mkfs.ext4 -d` (no
// mount or loop device). The caller stages the rootfs and owns target's parent
// directory; on a build failure target is left for the caller to clean up.
func FlattenRootfs(ctx context.Context, stagingDir, target string, spec appspec.Spec) error {
	if err := requireTools("truncate", "mkfs.ext4", "du"); err != nil {
		return err
	}
	initDest := filepath.Join(stagingDir, guestagent.InitPath)
	if err := os.MkdirAll(filepath.Dir(initDest), 0o755); err != nil { //nolint:gosec // standard /sbin perms inside the rootfs
		return fmt.Errorf("create init dir in rootfs: %w", err)
	}
	if err := guestagent.WriteTo(initDest); err != nil {
		return fmt.Errorf("inject guest agent: %w", err)
	}
	if err := appspec.Write(spec, filepath.Join(stagingDir, appspec.Path)); err != nil {
		return fmt.Errorf("write app spec: %w", err)
	}
	return buildExt4(ctx, stagingDir, target)
}

// buildExt4 sizes and writes the ext4 image at target, populated from stagingDir
// via mkfs.ext4 -d. Size = populated + 50% + 512 MiB working space, floored at
// 1 GiB (dynamic growth at first boot is a later enhancement).
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
	size = (size + mib - 1) / mib * mib // round up to a whole MiB

	truncate := exec.CommandContext(ctx, "truncate", "-s", strconv.FormatInt(size, 10), target) //nolint:gosec // fixed args + the operator's path
	truncate.Stderr = os.Stderr
	if err := truncate.Run(); err != nil {
		return fmt.Errorf("allocate ext4 image: %w", err)
	}
	mkfs := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", "-d", stagingDir, target) //nolint:gosec // fixed args + operator paths
	mkfs.Stdout = os.Stderr
	mkfs.Stderr = os.Stderr
	if err := mkfs.Run(); err != nil {
		return fmt.Errorf("build ext4 rootfs: %w", err)
	}
	return nil
}

func dirSizeBytes(ctx context.Context, dir string) (int64, error) {
	out, err := exec.CommandContext(ctx, "du", "-sb", dir).Output() //nolint:gosec // dir is our own staging path
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

func requireTools(names ...string) error {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return fmt.Errorf("%q not found in PATH (required to build an ext4 template): %w", n, err)
		}
	}
	return nil
}
