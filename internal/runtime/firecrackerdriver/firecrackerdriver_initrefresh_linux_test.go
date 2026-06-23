//go:build linux

package firecrackerdriver

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeExt4 builds a small empty ext4 image for the debugfs round-trip tests,
// skipping when the e2fsprogs tooling the helpers rely on is not installed.
func makeExt4(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"mkfs.ext4", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(16 << 20); err != nil {
		t.Fatalf("size image: %v", err)
	}
	_ = f.Close()
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	return img
}

func TestWriteAndReadRootfsFile(t *testing.T) {
	img := makeExt4(t)
	ctx := context.Background()

	// A nested path exercises the parent-directory creation, and a non-trivial
	// payload (with NUL and high bytes) exercises the binary-safe write.
	payload := bytes.Repeat([]byte{0x00, 0x7f, 0xff, 'a'}, 4096) // 16 KiB
	if err := writeRootfsFile(ctx, img, "/etc/fletcher/sub/fletcher-init", payload, "0100755"); err != nil {
		t.Fatalf("writeRootfsFile: %v", err)
	}
	got := readRootfsFile(ctx, img, "/etc/fletcher/sub/fletcher-init")
	if !bytes.Equal(got, payload) {
		t.Fatalf("readback mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// The init must keep its exec bit, or the kernel cannot run it as init.
	out, err := exec.Command("debugfs", "-R", "stat "+"/etc/fletcher/sub/fletcher-init", img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs stat: %v: %s", err, out)
	}
	// debugfs prints the permission bits without the file-type bits (0755, not
	// 0100755). The exec bit must be set or the kernel cannot run it as init.
	if !bytes.Contains(out, []byte("Mode:  0755")) {
		t.Fatalf("init mode missing exec bit:\n%s", out)
	}
}

func TestWriteRootfsFileReplaces(t *testing.T) {
	img := makeExt4(t)
	ctx := context.Background()

	if err := writeRootfsFile(ctx, img, initFingerprintPath, []byte("oldhash"), "0100644"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeRootfsFile(ctx, img, initFingerprintPath, []byte("newhash"), "0100644"); err != nil {
		t.Fatalf("replace write: %v", err)
	}
	if got := onDiskInitFingerprint(ctx, img); got != "newhash" {
		t.Fatalf("fingerprint after replace = %q, want %q", got, "newhash")
	}
}

func TestOnDiskInitFingerprintAbsent(t *testing.T) {
	img := makeExt4(t)
	if got := onDiskInitFingerprint(context.Background(), img); got != "" {
		t.Fatalf("absent marker = %q, want empty", got)
	}
}
