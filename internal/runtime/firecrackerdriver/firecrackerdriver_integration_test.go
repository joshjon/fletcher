//go:build linux && integration

package firecrackerdriver_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/vmm"
)

// These tests boot real microVMs. They need /dev/kvm, the bundled VMM, and an
// ext4 rootfs carrying the guest agent (build one with `fletcher image import
// --format ext4` and point FLETCHER_TEST_ROOTFS at it).

// newDriver extracts the VMM, copies the rootfs template to a throwaway image
// (the VM boots it rw), and builds a driver with the given forwards.
func newDriver(t *testing.T, forwards []firecrackerdriver.Forward) (*firecrackerdriver.Driver, string) {
	t.Helper()
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
	rootfs := filepath.Join(dir, "rootfs.ext4")
	copyFile(t, template, rootfs)

	d, err := firecrackerdriver.New(firecrackerdriver.Options{
		FirecrackerBinary: bundle.FirecrackerPath,
		KernelPath:        bundle.KernelPath,
		RunDir:            filepath.Join(dir, "run"),
		Forwards:          forwards,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, rootfs
}

// TestFirecrackerRun boots a microVM and runs a command, checking the exit code
// and that stdout/stderr come back demultiplexed.
func TestFirecrackerRun(t *testing.T) {
	d, rootfs := newDriver(t, nil)
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := d.Run(ctx, runtime.Spec{
		JobID:   "fc-run",
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
		t.Errorf("stdout = %q, want hello-from-vm", stdout.String())
	}
	if !strings.Contains(stderr.String(), "oops") {
		t.Errorf("stderr = %q, want oops", stderr.String())
	}
}

// TestFirecrackerNoEgress asserts the VM has no route to the internet: the
// trust boundary (§5/§6) holds structurally because the VM has no NIC.
func TestFirecrackerNoEgress(t *testing.T) {
	d, rootfs := newDriver(t, nil)
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, err := d.Run(ctx, runtime.Spec{
		JobID:   "fc-no-egress",
		Command: "ping -c1 -W2 1.1.1.1 >/dev/null 2>&1 && echo HAS_EGRESS || echo NO_EGRESS",
		WorkDir: rootfs,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(stdout.String(), "HAS_EGRESS") || !strings.Contains(stdout.String(), "NO_EGRESS") {
		t.Errorf("expected no egress, stdout = %q", stdout.String())
	}
}

// TestFirecrackerGatewayForward asserts the agent inside the VM can reach a host
// service over the vsock forward (the path the model gateway / MCP use), while
// still having no internet egress.
func TestFirecrackerGatewayForward(t *testing.T) {
	// A host unix socket that answers any request with a tiny HTTP "PONG".
	sock := filepath.Join(t.TempDir(), "svc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go serveHTTPPong(ln)

	const listenAddr = "127.0.0.1:12000"
	d, rootfs := newDriver(t, []firecrackerdriver.Forward{{ListenAddr: listenAddr, HostSocket: sock}})

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := d.Run(ctx, runtime.Spec{
		JobID:   "fc-forward",
		Command: "wget -q -T 5 -O- http://" + listenAddr + "/",
		WorkDir: rootfs,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, stderr = %q", res.ExitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "PONG") {
		t.Errorf("stdout = %q, want PONG (forward to host service failed)", stdout.String())
	}
}

// TestFirecrackerSession boots a persistent session VM and runs two commands in
// it: the second reads a file the first wrote, proving the VM stays up between
// execs (the durable-session core).
func TestFirecrackerSession(t *testing.T) {
	d, rootfs := newDriver(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	handle, err := d.StartSession(ctx, runtime.SessionSpec{
		SessionID:  "fc-session-test",
		RootfsPath: rootfs,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	// Exec 1: record the guest kernel and write a file.
	var out bytes.Buffer
	res, err := handle.Exec(ctx, runtime.Spec{
		Command: "echo KERNEL=$(uname -r); echo persisted-value > /workspace/marker",
	}, &out, &out)
	if err != nil {
		t.Fatalf("Exec 1: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exec 1 exit = %d, want 0 (%q)", res.ExitCode, out.String())
	}
	if !strings.Contains(out.String(), "KERNEL=5.10") {
		t.Errorf("exec 1 stdout = %q, want guest kernel", out.String())
	}

	// Exec 2: a separate exec into the SAME running VM reads the file back.
	out.Reset()
	res2, err := handle.Exec(ctx, runtime.Spec{Command: "cat /workspace/marker"}, &out, &out)
	if err != nil {
		t.Fatalf("Exec 2: %v", err)
	}
	if res2.ExitCode != 0 {
		t.Errorf("exec 2 exit = %d, want 0 (%q)", res2.ExitCode, out.String())
	}
	if !strings.Contains(out.String(), "persisted-value") {
		t.Errorf("exec 2 stdout = %q, want persisted-value (VM did not stay up between execs)", out.String())
	}
	t.Logf("session ok: exec2 read %q from a file exec1 wrote", strings.TrimSpace(out.String()))
}

// serveHTTPPong answers each connection with a minimal HTTP 200 carrying "PONG".
func serveHTTPPong(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			buf := make([]byte, 1024)
			_, _ = c.Read(buf) // drain the request line/headers
			_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Length: 4\r\nConnection: close\r\n\r\nPONG")
		}(conn)
	}
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
