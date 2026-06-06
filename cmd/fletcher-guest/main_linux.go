//go:build linux

// Command fletcher-guest is the init process inside a Firecracker microVM. It
// is injected into the rootfs as /sbin/fletcher-init and booted via the kernel
// init= argument. It mounts the basic pseudo-filesystems, dials the daemon over
// vsock, receives the job spec, runs the command while streaming its output
// back, reports the exit code, and powers the VM off.
//
// It is deliberately tiny and dependency-light: it is PID 1, so anything it
// pulls in ships in every rootfs and runs as init.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

// guestWorkspace is the conventional working directory inside a session VM.
const guestWorkspace = "/workspace"

func main() {
	mountBasics()
	setupLoopback()
	if sessionMode() {
		// A durable session: stay up and serve host-initiated exec/shutdown
		// requests until a shutdown request resets the VM.
		serve()
		shutdown()
		os.Exit(0)
	}
	code := run()
	// PID 1 must not simply return (the kernel panics). Trigger a reboot so the
	// VMM exits and the host's Wait returns. Firecracker has no ACPI, so a
	// power-off would hang; with reboot=k the kernel does a keyboard-controller
	// reset, which Firecracker intercepts and exits on.
	shutdown()
	os.Exit(code) // unreachable once shutdown succeeds
}

// sessionMode reports whether the kernel was booted for a durable session
// (the daemon adds fletcher.session=1 to the cmdline for session VMs).
func sessionMode() bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "fletcher.session=1")
}

// serve is the session control loop: listen on vsock and serve each
// host-initiated connection. It returns only if the listener fails.
func serve() {
	ln, err := vsock.Listen(guestproto.ControlPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: listen control: %v\n", err)
		return
	}
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveControl(conn)
	}
}

// serveControl handles one host control connection: run a command, or shut down.
func serveControl(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	req, err := guestproto.ReadRequest(conn)
	if err != nil {
		// A clean disconnect (e.g. the host's readiness probe) is not an error.
		if !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "fletcher-guest: read request: %v\n", err)
		}
		return
	}
	switch req.Kind {
	case guestproto.RequestExec:
		runExec(conn, req.Spec)
	case guestproto.RequestShell:
		runShell(conn, req.Shell)
	case guestproto.RequestShutdown:
		shutdown() // resets the VM; does not return
	default:
		fmt.Fprintf(os.Stderr, "fletcher-guest: unknown request kind %q\n", req.Kind)
	}
}

// runShell opens a PTY running a login shell and bridges it to the host over
// conn: the guest streams terminal output back as KindStdout frames while
// reading KindStdin/KindResize frames from the host. It returns when the shell
// exits (user typed exit) or the host closes the connection (client detached).
func runShell(conn net.Conn, spec guestproto.ShellSpec) {
	shell := loginShell()
	cmd := exec.CommandContext(context.Background(), shell, "-l") //nolint:gosec // launching a shell is the entire purpose
	cmd.Dir = shellDir()
	cmd.Env = shellEnv(spec)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		fc := &frameConn{w: conn}
		_ = fc.write(guestproto.KindStderr, []byte(fmt.Sprintf("fletcher-guest: start shell: %v\n", err)))
		_ = fc.write(guestproto.KindExit, guestproto.EncodeExit(1))
		return
	}
	defer func() { _ = ptmx.Close() }()
	if spec.Cols > 0 && spec.Rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: spec.Cols, Rows: spec.Rows})
	}

	fc := &frameConn{w: conn}
	// Pump PTY output to the host until the shell closes the terminal.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := fc.write(guestproto.KindStdout, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	// Pump host input (keystrokes, resizes) to the PTY. When the host closes the
	// connection, ReadFrame errors; closing the PTY hangs up the shell's
	// controlling terminal so it exits and cmd.Wait below returns.
	go func() {
		for {
			kind, payload, rerr := guestproto.ReadFrame(conn)
			if rerr != nil {
				_ = ptmx.Close()
				return
			}
			switch kind {
			case guestproto.KindStdin:
				_, _ = ptmx.Write(payload)
			case guestproto.KindResize:
				if cols, rows, derr := guestproto.DecodeResize(payload); derr == nil {
					_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
				}
			}
		}
	}()

	code := waitCode(cmd)
	<-outDone                                                             // drain any output the shell wrote on its way out
	_ = fc.write(guestproto.KindExit, guestproto.EncodeExit(int32(code))) //nolint:gosec // exit codes are 0-255
}

// loginShell prefers bash, falling back to sh on a minimal rootfs.
func loginShell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// shellDir is the shell's working directory: /workspace if present, else /.
func shellDir() string {
	if fi, err := os.Stat(guestWorkspace); err == nil && fi.IsDir() {
		return guestWorkspace
	}
	return "/"
}

// shellEnv builds the login shell's environment: the spec's TERM plus the
// usual PATH/HOME defaults and any extra entries the host passed.
func shellEnv(spec guestproto.ShellSpec) []string {
	env := withDefaults(spec.Env)
	term := spec.Term
	if term == "" {
		term = "xterm-256color"
	}
	if !hasKey(env, "TERM") {
		env = append(env, "TERM="+term)
	}
	return env
}

// waitCode waits for cmd and maps its result to an exit code.
func waitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return 1 // killed by signal (e.g. the host hung up the terminal)
	}
	return 1
}

// run dials the host, executes the job, and returns the exit code to report.
func run() int {
	conn, err := dialHost()
	if err != nil {
		// Nothing to report to; surface on the console for debugging.
		fmt.Fprintf(os.Stderr, "fletcher-guest: dial host: %v\n", err)
		return 1
	}
	defer func() { _ = conn.Close() }()

	spec, err := guestproto.ReadSpec(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: read spec: %v\n", err)
		return 1
	}

	// Bring up the loopback service relays (gateway, MCP) before running, so the
	// agent's calls to them succeed. The VM has no NIC, so this vsock relay is
	// its only path off-box - and it reaches only the daemon, never the internet.
	startForwards(spec.Forwards)

	return runExec(conn, spec)
}

// runExec runs spec and frames its stdout/stderr and exit code back over conn.
// Shared by the ephemeral path and the session control loop.
func runExec(conn net.Conn, spec guestproto.Spec) int {
	fc := &frameConn{w: conn}
	code := runCommand(spec, fc)
	if err := fc.write(guestproto.KindExit, guestproto.EncodeExit(int32(code))); err != nil { //nolint:gosec // exit codes are 0-255
		fmt.Fprintf(os.Stderr, "fletcher-guest: send exit: %v\n", err)
	}
	return code
}

// runCommand runs the job command with its stdout/stderr framed back over fc.
func runCommand(spec guestproto.Spec, fc *frameConn) int {
	workDir := spec.WorkDir
	if workDir == "" {
		workDir = "/"
	}
	// Ensure the working directory exists (e.g. /workspace on a minimal rootfs);
	// fall back to / if it cannot be created.
	if err := os.MkdirAll(workDir, 0o755); err != nil { //nolint:gosec // standard rootfs dir perms inside the VM
		workDir = "/"
	}
	// The host tears down the whole VM to cancel, so an in-guest context adds
	// nothing; Background satisfies the lint that wants a context-aware call.
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", spec.Command) //nolint:gosec // running the job is the entire purpose
	cmd.Dir = workDir
	cmd.Env = withDefaults(spec.Env)
	cmd.Stdout = streamWriter{fc: fc, kind: guestproto.KindStdout}
	cmd.Stderr = streamWriter{fc: fc, kind: guestproto.KindStderr}

	err := cmd.Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return 1 // killed by signal
	}
	// Couldn't start the command at all (e.g. no /bin/sh).
	_ = fc.write(guestproto.KindStderr, []byte(fmt.Sprintf("fletcher-guest: %v\n", err)))
	return 127
}

// dialHost connects to the daemon over vsock, retrying while the VM finishes
// bringing up its vsock device.
func dialHost() (net.Conn, error) {
	var lastErr error
	for range 50 {
		conn, err := vsock.Dial(guestproto.HostCID, guestproto.Port, nil)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, lastErr
}

// frameConn serialises frame writes from the concurrent stdout/stderr streams.
type frameConn struct {
	mu sync.Mutex
	w  net.Conn
}

func (c *frameConn) write(kind byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return guestproto.WriteFrame(c.w, kind, payload)
}

// streamWriter frames each Write as one output frame of its kind.
type streamWriter struct {
	fc   *frameConn
	kind byte
}

func (s streamWriter) Write(p []byte) (int, error) {
	if err := s.fc.write(s.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// withDefaults ensures the command has a usable PATH and HOME even if the spec
// did not set them.
func withDefaults(env []string) []string {
	out := append([]string(nil), env...)
	if !hasKey(out, "PATH") {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !hasKey(out, "HOME") {
		out = append(out, "HOME=/root")
	}
	return out
}

func hasKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// startForwards launches a TCP->vsock relay for each forward. The agent inside
// the VM connects to f.ListenAddr (loopback); each connection is relayed to the
// host on f.VsockPort, where the daemon proxies it to the matching unix socket.
func startForwards(forwards []guestproto.Forward) {
	for _, f := range forwards {
		if err := startForward(f); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: forward %s: %v\n", f.ListenAddr, err)
		}
	}
}

func startForward(f guestproto.Forward) error {
	ln, err := net.Listen("tcp", f.ListenAddr) //nolint:noctx // lifetime is the VM; closed at poweroff
	if err != nil {
		return err
	}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go relayToVsock(client, f.VsockPort)
		}
	}()
	return nil
}

func relayToVsock(client net.Conn, vsockPort uint32) {
	upstream, err := vsock.Dial(guestproto.HostCID, vsockPort, nil)
	if err != nil {
		_ = client.Close()
		return
	}
	splice(client, upstream)
}

// splice copies bidirectionally between two connections, closing both when
// either direction ends so half-closed HTTP/SSE streams terminate.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// setupLoopback brings the loopback interface up so the agent (and the forward
// relays) can use 127.0.0.1. Firecracker gives the guest a lo device but does
// not bring it up.
func setupLoopback() {
	if err := ifup("lo"); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: bring up lo: %v\n", err)
	}
}

func ifup(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// mountBasics mounts the pseudo-filesystems most programs expect. Best-effort:
// a missing mount point or an already-mounted fs should not stop the job.
func mountBasics() {
	type m struct{ source, target, fstype string }
	for _, mt := range []m{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"devpts", "/dev/pts", "devpts"},
	} {
		_ = os.MkdirAll(mt.target, 0o755) //nolint:gosec // standard mountpoint perms inside the VM
		if err := unix.Mount(mt.source, mt.target, mt.fstype, 0, ""); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: mount %s: %v\n", mt.target, err)
		}
	}
}

// shutdown flushes and resets the VM so Firecracker exits. As PID 1 we hold
// CAP_SYS_BOOT. RESTART (with the kernel's reboot=k) does a keyboard-controller
// reset that Firecracker intercepts; POWER_OFF would need ACPI, which
// Firecracker does not provide, and would hang.
func shutdown() {
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: reboot: %v\n", err)
		for {
			_ = unix.Reboot(unix.LINUX_REBOOT_CMD_HALT)
			time.Sleep(time.Second)
		}
	}
}
