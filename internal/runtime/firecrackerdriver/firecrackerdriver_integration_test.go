//go:build linux && integration

package firecrackerdriver_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/vmm"
)

// TestFirecrackerRun boots a real microVM and runs a command in it. It needs
// /dev/kvm, the bundled VMM, and an ext4 rootfs that carries the guest agent
// (build one with `fletcher image import --format ext4` and point
// FLETCHER_TEST_ROOTFS at it).
func TestFirecrackerRun(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm on this host")
	}
	if !vmm.Available() {
		t.Skip("VMM assets not bundled (run make fetch-vmm)")
	}
	template := os.Getenv("FLETCHER_TEST_ROOTFS")
	if template == "" {
		t.Skip("set FLETCHER_TEST_ROOTFS to an ext4 rootfs containing the guest agent")
	}

	dir := t.TempDir()
	bundle, err := vmm.Extract(filepath.Join(dir, "vmm"))
	if err != nil {
		t.Fatalf("extract vmm: %v", err)
	}

	// The VM boots the rootfs read-write, so give it a throwaway copy.
	rootfs := filepath.Join(dir, "rootfs.ext4")
	copyFile(t, template, rootfs)

	d, err := firecrackerdriver.New(firecrackerdriver.Options{
		FirecrackerBinary: bundle.FirecrackerPath,
		KernelPath:        bundle.KernelPath,
		RunDir:            filepath.Join(dir, "run"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := d.Run(ctx, runtime.Spec{
		JobID:   "fc-test-job",
		Command: "echo hello-from-vm; echo oops 1>&2; exit 7",
		WorkDir: rootfs,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "hello-from-vm") {
		t.Errorf("stdout = %q, want it to contain hello-from-vm", stdout.String())
	}
	if !strings.Contains(stderr.String(), "oops") {
		t.Errorf("stderr = %q, want it to contain oops", stderr.String())
	}
	t.Logf("VM ran: exit=%d stdout=%q stderr=%q", res.ExitCode, stdout.String(), stderr.String())
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close dst: %v", err)
	}
}
