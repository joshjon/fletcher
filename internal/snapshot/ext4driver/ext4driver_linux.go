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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
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

// CommitTemplate clones the snapshot's rootfs back into <imagesDir>/<name>.ext4
// so future jobs/sessions can boot from it (snapshot.TemplateCommitter). The
// clone goes via a temp file + rename, so a failed or cancelled commit never
// clobbers an existing template; on a reflink-capable filesystem committing a
// large fork is instant. extraFiles are injected into the cloned image offline
// (journal replay + debugfs), so no privileges and no running guest are needed.
func (d *Driver) CommitTemplate(ctx context.Context, id, name string, force bool, extraFiles map[string][]byte) error {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("ext4: invalid template name %q", name)
	}
	if id != filepath.Base(id) || id == "." || id == ".." {
		return fmt.Errorf("ext4: invalid snapshot id %q", id)
	}
	src := filepath.Join(d.rootDir, id+templateExt)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("ext4: snapshot rootfs %s: %w", src, err)
	}
	if err := os.MkdirAll(d.imagesDir, 0o750); err != nil {
		return fmt.Errorf("ext4: create images dir: %w", err)
	}
	dst := filepath.Join(d.imagesDir, name+templateExt)
	if _, err := os.Stat(dst); err == nil && !force {
		return fmt.Errorf("ext4: template %q already exists", name)
	}
	tmp := dst + ".partial"
	if err := cloneFile(ctx, src, tmp); err != nil {
		return fmt.Errorf("ext4: commit rootfs to template: %w", err)
	}
	if len(extraFiles) > 0 {
		if err := injectFiles(ctx, tmp, extraFiles); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ext4: finalise template: %w", err)
	}
	return nil
}

// injectFiles writes files into an unmounted ext4 image. The journal is
// replayed first (e2fsck): debugfs writes bypass it, so a pending transaction
// from a just-synced live disk could otherwise clobber the edit on first mount.
func injectFiles(ctx context.Context, img string, files map[string][]byte) error {
	if out, err := exec.CommandContext(ctx, "e2fsck", "-fp", img).CombinedOutput(); err != nil { //nolint:gosec // img is a daemon-owned template path
		// Exit 1/2 mean preen fixed issues - fine for a crash-consistent clone.
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() >= 4 {
			return fmt.Errorf("ext4: fsck template before file injection: %w: %s", err, out)
		}
	}
	for guestPath, data := range files {
		if err := injectFile(ctx, img, guestPath, data); err != nil {
			return err
		}
	}
	return nil
}

// injectFile writes one file into the image via debugfs, creating parent
// directories and replacing any existing file. debugfs exits 0 even when a
// command fails, so the write is verified by reading the content back.
func injectFile(ctx context.Context, img, guestPath string, data []byte) error {
	if !path.IsAbs(guestPath) || strings.ContainsAny(guestPath, " \t\n\"") {
		return fmt.Errorf("ext4: invalid template file path %q", guestPath)
	}
	tmpf, err := os.CreateTemp("", "fletcher-inject-*")
	if err != nil {
		return fmt.Errorf("ext4: stage template file: %w", err)
	}
	defer func() {
		_ = tmpf.Close()
		_ = os.Remove(tmpf.Name())
	}()
	if _, err := tmpf.Write(data); err != nil {
		return fmt.Errorf("ext4: stage template file: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		return fmt.Errorf("ext4: stage template file: %w", err)
	}

	// One stdin-driven debugfs run: `write` only creates a file in the current
	// directory, so cd to the parent first. mkdir/rm fail harmlessly when the
	// directory exists / the file is absent.
	var script strings.Builder
	for _, dir := range ancestorDirs(guestPath) {
		fmt.Fprintf(&script, "mkdir %s\n", dir)
	}
	fmt.Fprintf(&script, "cd %s\n", path.Dir(guestPath))
	fmt.Fprintf(&script, "rm %s\n", path.Base(guestPath))
	fmt.Fprintf(&script, "write %s %s\n", tmpf.Name(), path.Base(guestPath))
	cmd := exec.CommandContext(ctx, "debugfs", "-w", img) //nolint:gosec // img is a daemon-owned template path
	cmd.Stdin = strings.NewReader(script.String())
	_ = cmd.Run()

	got, err := exec.CommandContext(ctx, "debugfs", "-R", "cat "+guestPath, img).Output() //nolint:gosec // validated path
	if err != nil || !bytes.Equal(got, data) {
		return fmt.Errorf("ext4: inject %s into template: write could not be verified (is debugfs from e2fsprogs installed?)", guestPath)
	}
	return nil
}

// ancestorDirs lists the parent directories of an absolute path, shallowest
// first (e.g. /etc/fletcher/app.json -> /etc, /etc/fletcher).
func ancestorDirs(p string) []string {
	var dirs []string
	for dir := path.Dir(p); dir != "/" && dir != "."; dir = path.Dir(dir) {
		dirs = append([]string{dir}, dirs...)
	}
	return dirs
}

// CreateVolume provisions a blank ext4 volume (snapshot.VolumeProvisioner) at
// <rootDir>/volumes/<id>.ext4. The backing file is sparse, so a generously
// sized volume costs only the blocks data actually lands on.
func (d *Driver) CreateVolume(ctx context.Context, id string, sizeBytes int64) (string, error) {
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return "", fmt.Errorf("ext4: invalid volume id %q", id)
	}
	if sizeBytes < 64<<20 {
		return "", fmt.Errorf("ext4: volume size %d is too small (minimum 64 MiB)", sizeBytes)
	}
	dir := filepath.Join(d.rootDir, "volumes")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("ext4: create volumes dir: %w", err)
	}
	path := filepath.Join(dir, id+templateExt)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("ext4: volume %q already exists", id)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is a validated daemon-generated id under rootDir
	if err != nil {
		return "", fmt.Errorf("ext4: create volume file: %w", err)
	}
	truncErr := f.Truncate(sizeBytes) // sparse allocation
	if cerr := f.Close(); truncErr == nil {
		truncErr = cerr
	}
	if truncErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("ext4: allocate volume: %w", truncErr)
	}
	mkfs := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", path) //nolint:gosec // fixed args + daemon path
	if out, err := mkfs.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("ext4: format volume: %w: %s", err, out)
	}
	return path, nil
}

// DeleteVolume removes a volume's backing file. Missing is a no-op.
func (d *Driver) DeleteVolume(_ context.Context, id string) error {
	if id != filepath.Base(id) || id == "." || id == ".." {
		return fmt.Errorf("ext4: invalid volume id %q", id)
	}
	path := filepath.Join(d.rootDir, "volumes", id+templateExt)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ext4: delete volume %s: %w", id, err)
	}
	return nil
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
