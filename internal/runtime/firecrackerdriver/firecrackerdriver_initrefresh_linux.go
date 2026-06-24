//go:build linux

package firecrackerdriver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestagent"
)

// repairRootfs makes a cold-booted fork mountable. A fork can be left
// crash-inconsistent - e.g. a hibernated session whose ext4 metadata was not
// fully flushed - and the kernel refuses to mount a filesystem with bad group
// descriptor checksums, so the VM panics at boot unless the fork is repaired
// offline first. e2fsck -p (preen) is a fast no-op on a clean fork and replays
// the journal / fixes minor issues on a dirty one; when preen gives up (exit
// >= 4, "RUN fsck MANUALLY") it escalates to a full -fy repair. Best-effort: a
// failure is logged and boot proceeds (it may still panic, but nothing more can
// be tried). NEVER call on the hibernation-restore path - the restored guest's
// page cache assumes the current on-disk state, and an offline edit would
// diverge from it.
func (d *Driver) repairRootfs(ctx context.Context, rootfs string) {
	if out, err := exec.CommandContext(ctx, "e2fsck", "-p", rootfs).CombinedOutput(); err != nil { //nolint:gosec // rootfs is a daemon-owned fork path
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() < 4 {
			return // 1/2: preen fixed minor issues. Fine.
		}
		// Preen could not handle it (>= 4) or e2fsck could not run: full repair.
		d.logger.Warn("fork filesystem inconsistent; running full repair before boot",
			slog.String("rootfs", rootfs), slog.String("preen", strings.TrimSpace(string(out))))
		out2, err2 := exec.CommandContext(ctx, "e2fsck", "-fy", rootfs).CombinedOutput() //nolint:gosec // rootfs is a daemon-owned fork path
		if err2 != nil {
			var ee2 *exec.ExitError
			if !errors.As(err2, &ee2) || ee2.ExitCode() >= 4 {
				d.logger.Error("fork filesystem repair failed; boot may panic",
					slog.String("rootfs", rootfs), slog.String("err", err2.Error()),
					slog.String("fsck", strings.TrimSpace(string(out2))))
				return
			}
		}
		d.logger.Warn("fork filesystem repaired before boot", slog.String("rootfs", rootfs))
	}
}

// initFingerprintPath records, inside the rootfs, the hex SHA-256 of the guest
// init currently written there. refreshGuestInit reads it to skip the ext4 edit
// when the rootfs already carries this daemon's init.
const initFingerprintPath = "/etc/fletcher/init.sha256"

// refreshGuestInit ensures rootfs boots this daemon's guest init at
// guestagent.InitPath, replacing whatever was baked in when the image was built.
//
// The init pairs with the host's guestproto wire protocol, so it must track the
// daemon, not the image: an image built by an older release carries an older
// init, and a fork is a plain clone of that image, so without this a daemon
// upgrade silently keeps booting the stale init. A fingerprint marker lets the
// common case (already current) skip the edit, so only a daemon that actually
// changed the guest pays the cost - and only on a cold boot, never on the
// instant-wake hibernation restore, which resumes the already-running init.
func (d *Driver) refreshGuestInit(ctx context.Context, rootfs string) error {
	want, err := guestagent.Fingerprint()
	if err != nil {
		return err // no bundled guest in this build: nothing to refresh to
	}
	if onDiskInitFingerprint(ctx, rootfs) == want {
		return nil // the rootfs already carries this exact init
	}
	data, err := guestagent.Bytes()
	if err != nil {
		return err
	}
	// The caller runs repairRootfs before this on the cold-boot path, so the fork
	// is journal-replayed and consistent here - the debugfs write (which bypasses
	// the journal) lands safely without a separate fsck.
	if err := writeRootfsFile(ctx, rootfs, guestagent.InitPath, data, "0100755"); err != nil {
		return err
	}
	// Verify the init landed byte-for-byte (debugfs exits 0 even on a failed
	// write), then record the marker so the next boot can skip all of this.
	sum := sha256.Sum256(data)
	if got := readRootfsFile(ctx, rootfs, guestagent.InitPath); hex.EncodeToString(sum[:]) != fingerprintBytes(got) {
		return fmt.Errorf("refresh guest init: write to %s could not be verified (is debugfs from e2fsprogs installed?)", guestagent.InitPath)
	}
	if err := writeRootfsFile(ctx, rootfs, initFingerprintPath, []byte(want), "0100644"); err != nil {
		return err
	}
	return nil
}

// onDiskInitFingerprint reads the init fingerprint marker from rootfs, returning
// "" when it is absent (an image built before init refresh, or a rootfs whose
// init was never refreshed) so the caller treats it as stale.
func onDiskInitFingerprint(ctx context.Context, rootfs string) string {
	return strings.TrimSpace(string(readRootfsFile(ctx, rootfs, initFingerprintPath)))
}

// fingerprintBytes is the hex SHA-256 of b (matching guestagent.Fingerprint).
func fingerprintBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readRootfsFile reads guestPath out of an unmounted ext4 image via debugfs,
// returning nil on any error (caller treats absence as a mismatch). Read-only:
// it does not touch the journal.
func readRootfsFile(ctx context.Context, rootfs, guestPath string) []byte {
	out, err := exec.CommandContext(ctx, "debugfs", "-R", "cat "+guestPath, rootfs).Output() //nolint:gosec // rootfs is a daemon-owned fork path; guestPath is a fixed constant
	if err != nil {
		return nil
	}
	return out
}

// writeRootfsFile writes one file into an unmounted ext4 image via debugfs,
// creating parent directories, replacing any existing file, and setting the
// inode mode (e.g. "0100755" for the executable init). The image's journal must
// already be clean (the caller runs e2fsck first).
func writeRootfsFile(ctx context.Context, rootfs, guestPath string, data []byte, mode string) error {
	if !path.IsAbs(guestPath) || strings.ContainsAny(guestPath, " \t\n\"") {
		return fmt.Errorf("refresh guest init: invalid rootfs path %q", guestPath)
	}
	tmpf, err := os.CreateTemp("", "fletcher-init-*")
	if err != nil {
		return fmt.Errorf("refresh guest init: stage file: %w", err)
	}
	defer func() {
		_ = tmpf.Close()
		_ = os.Remove(tmpf.Name())
	}()
	if _, err := tmpf.Write(data); err != nil {
		return fmt.Errorf("refresh guest init: stage file: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		return fmt.Errorf("refresh guest init: stage file: %w", err)
	}

	// One stdin-driven debugfs run: `write` only creates a file in the current
	// directory, so cd to the parent first. mkdir fails harmlessly when the
	// directory exists; rm fails harmlessly when the file is absent. sif sets the
	// inode mode so the init keeps its exec bit (debugfs write defaults to 0644).
	var script strings.Builder
	for _, dir := range rootfsAncestorDirs(guestPath) {
		fmt.Fprintf(&script, "mkdir %s\n", dir)
	}
	base := path.Base(guestPath)
	fmt.Fprintf(&script, "cd %s\n", path.Dir(guestPath))
	fmt.Fprintf(&script, "rm %s\n", base)
	fmt.Fprintf(&script, "write %s %s\n", tmpf.Name(), base)
	fmt.Fprintf(&script, "sif %s mode %s\n", base, mode)
	cmd := exec.CommandContext(ctx, "debugfs", "-w", rootfs) //nolint:gosec // rootfs is a daemon-owned fork path
	cmd.Stdin = strings.NewReader(script.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("refresh guest init: debugfs write %s: %w: %s", guestPath, err, bytes.TrimSpace(out))
	}
	return nil
}

// rootfsAncestorDirs lists the parent directories of an absolute path, shallowest
// first (e.g. /etc/fletcher/init.sha256 -> /etc, /etc/fletcher).
func rootfsAncestorDirs(p string) []string {
	var dirs []string
	for dir := path.Dir(p); dir != "/" && dir != "."; dir = path.Dir(dir) {
		dirs = append([]string{dir}, dirs...)
	}
	return dirs
}
