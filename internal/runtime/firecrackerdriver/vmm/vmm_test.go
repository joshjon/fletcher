package vmm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/vmm"
)

func TestExtract(t *testing.T) {
	if !vmm.Available() {
		t.Skip("VMM assets not bundled in this build (run `make fetch-vmm`); skipping")
	}

	dir := t.TempDir()
	b, err := vmm.Extract(dir)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	fc, statErr := os.Stat(b.FirecrackerPath)
	if statErr != nil {
		t.Fatalf("firecracker not extracted: %v", statErr)
	}
	if fc.Mode().Perm() != 0o755 {
		t.Errorf("firecracker mode = %v, want 0755", fc.Mode().Perm())
	}
	if _, err := os.Stat(b.KernelPath); err != nil {
		t.Fatalf("kernel not extracted: %v", err)
	}
	if filepath.Dir(b.FirecrackerPath) != dir {
		t.Errorf("firecracker extracted outside target dir: %s", b.FirecrackerPath)
	}

	// Idempotent: a second extract into the same dir must not error or rewrite.
	before := fc.ModTime()
	if _, err := vmm.Extract(dir); err != nil {
		t.Fatalf("second Extract: %v", err)
	}
	after, _ := os.Stat(b.FirecrackerPath)
	if !after.ModTime().Equal(before) {
		t.Errorf("idempotent Extract rewrote firecracker (modtime changed)")
	}

	// On the native arch the extracted binary should actually run.
	if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		out, err := exec.Command(b.FirecrackerPath, "--version").CombinedOutput()
		if err != nil {
			t.Fatalf("extracted firecracker --version failed: %v (%s)", err, out)
		}
		if !strings.Contains(string(out), "Firecracker") {
			t.Errorf("unexpected --version output: %q", out)
		}
	}
}
