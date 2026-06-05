//go:build linux

// Package ext4driver is the snapshot driver for the Firecracker runtime. Where
// the btrfs driver hands runc a subvolume directory, this driver hands
// Firecracker a block-device rootfs: each snapshot is a clone of an ext4 image
// file that the microVM boots as /dev/vda.
//
// Templates live at <imagesDir>/<image>.ext4 (built by `fletcher image import
// --format ext4`). Each Create clones one to <rootDir>/<id>.ext4. On a
// reflink-capable filesystem (btrfs, xfs) the clone is an instant, space-shared
// CoW copy via the FICLONE ioctl; on any other filesystem it degrades to a full
// copy - correct, just not space-shared. Per DESIGN.md §10 all snapshot work
// lives behind the snapshot.Driver interface; this is its ext4 implementation.
package ext4driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/joshjon/fletcher/internal/snapshot"
)

// templateExt distinguishes ext4 rootfs templates from btrfs subvolume
// templates of the same name living under the same images dir.
const templateExt = ".ext4"

// Driver is a snapshot.Driver backed by cloned ext4 image files.
type Driver struct {
	rootDir   string
	imagesDir string
	counter   atomic.Uint64
}

// Options configures a Driver.
type Options struct {
	// RootDir is the directory the per-job rootfs clones are written to. For
	// the clone to be a cheap CoW reflink it should live on btrfs or xfs;
	// elsewhere clones still work as full copies.
	RootDir string
	// ImagesDir holds the <name>.ext4 templates. Defaults to <RootDir>/images.
	ImagesDir string
}

// New constructs a Driver. The caller ensures RootDir exists and is writable
// by the daemon user.
func New(opts Options) (*Driver, error) {
	if opts.RootDir == "" {
		return nil, errors.New("ext4: RootDir is required")
	}
	// Ensure the root exists and is owned by the daemon user (we run as it), so
	// a fresh install can write per-job clones here without any manual chown.
	// Unlike btrfs, this works on any filesystem, so no provisioning is needed.
	if err := os.MkdirAll(opts.RootDir, 0o750); err != nil {
		return nil, fmt.Errorf("ext4: create root dir %s: %w", opts.RootDir, err)
	}
	imagesDir := opts.ImagesDir
	if imagesDir == "" {
		imagesDir = filepath.Join(opts.RootDir, "images")
	}
	return &Driver{rootDir: opts.RootDir, imagesDir: imagesDir}, nil
}

// Create clones the named ext4 template into a fresh per-job rootfs image and
// returns its path. Unlike the btrfs driver, an image is required: a microVM
// cannot boot without a root block device.
func (d *Driver) Create(ctx context.Context, image string) (snapshot.Snapshot, error) {
	if image == "" {
		return snapshot.Snapshot{}, errors.New("ext4: image is required (Firecracker needs a rootfs to boot)")
	}
	// Guard against path traversal: image names the template file, so it must
	// be a bare name, not a path that could escape imagesDir.
	if image != filepath.Base(image) || image == "." || image == ".." {
		return snapshot.Snapshot{}, fmt.Errorf("ext4: invalid image name %q", image)
	}
	src := filepath.Join(d.imagesDir, image+templateExt)
	if _, err := os.Stat(src); err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("ext4: rootfs template %s: %w", src, err)
	}
	id := fmt.Sprintf("snap-%d-%d", time.Now().UnixNano(), d.counter.Add(1))
	dst := filepath.Join(d.rootDir, id+templateExt)
	if err := cloneFile(ctx, src, dst); err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("ext4: clone rootfs: %w", err)
	}
	return snapshot.Snapshot{ID: id, Path: dst}, nil
}

// Delete removes the per-job rootfs clone. A missing file is not an error.
func (d *Driver) Delete(_ context.Context, id string) error {
	path := filepath.Join(d.rootDir, id+templateExt)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ext4: delete snapshot %s: %w", id, err)
	}
	return nil
}

// cloneFile copies src to dst, preferring a reflink (instant CoW) and falling
// back to a byte copy. The write is to dst directly with cleanup on failure so
// a cancelled or failed clone never leaves a truncated rootfs a later boot
// would fail on opaquely.
func cloneFile(ctx context.Context, src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // src is a validated bare image name under imagesDir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // dst is a daemon-generated id under rootDir
	if err != nil {
		return err
	}
	defer func() {
		cerr := out.Close()
		if err != nil {
			_ = os.Remove(dst) // don't leave a half-written rootfs behind
			return
		}
		if cerr != nil {
			err = cerr
		}
	}()

	// Fast path: reflink. Instant and space-shared on btrfs/xfs.
	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) == nil {
		return nil
	}
	// Fallback: full copy. Correct on any filesystem, just not space-shared.
	_, err = copyWithCtx(ctx, out, in)
	return err
}

// copyWithCtx is io.Copy that honours ctx cancellation between chunks, so a
// large fallback copy of a cancelled job stops promptly.
func copyWithCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
		}
		if errors.Is(rerr, io.EOF) {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
