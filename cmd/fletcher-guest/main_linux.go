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
	"math"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// Bring up sshd (brokered SSH / IDE attach) and its vsock relay before
	// accepting control connections; both are best-effort so a session is still
	// usable via exec/shell if sshd cannot start.
	startSSHD()
	go serveSSHRelay()
	go servePortRelay()

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

// startSSHD generates host keys on first boot (the image ships none) and runs
// sshd in the foreground, relaunching it if it exits. sshd listens on loopback;
// serveSSHRelay bridges the daemon's vsock connections to it.
func startSSHD() {
	if _, err := os.Stat("/usr/sbin/sshd"); err != nil {
		return // image without an SSH server; brokered SSH is simply unavailable
	}
	// ssh-keygen -A creates any missing host keys under /etc/ssh.
	if out, err := exec.CommandContext(context.Background(), "/usr/bin/ssh-keygen", "-A").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: ssh-keygen: %v: %s\n", err, out)
	}
	_ = os.MkdirAll("/run/sshd", 0o755) //nolint:gosec // sshd privilege-separation dir, standard perms
	go func() {
		for {
			cmd := exec.CommandContext(context.Background(), "/usr/sbin/sshd", "-D", "-e")
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "fletcher-guest: sshd exited: %v\n", err)
			}
			time.Sleep(time.Second) // avoid a hot loop if sshd cannot start
		}
	}()
}

// serveSSHRelay accepts the daemon's vsock connections on SSHPort and splices
// each to the loopback sshd, so SSH reaches the VM without it having a NIC.
func serveSSHRelay() {
	ln, err := vsock.Listen(guestproto.SSHPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: listen ssh relay: %v\n", err)
		return
	}
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go relayToSSHD(conn)
	}
}

func relayToSSHD(conn net.Conn) {
	// sshd may still be coming up just after a cold boot; retry briefly so the
	// first connection after wake does not fail the race.
	var upstream net.Conn
	for range 50 {
		c, err := net.Dial("tcp", "127.0.0.1:22") //nolint:noctx // lifetime is the spliced connection
		if err == nil {
			upstream = c
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if upstream == nil {
		_ = conn.Close()
		return
	}
	splice(conn, upstream)
}

// servePortRelay accepts the daemon's vsock connections on PortForwardPort,
// reads the 2-byte target loopback port, and splices each to that port inside
// the VM. It is the generic form of serveSSHRelay (which is fixed to sshd):
// the daemon uses it to broker a published session port (a preview port)
// without the VM having any network route.
func servePortRelay() {
	ln, err := vsock.Listen(guestproto.PortForwardPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: listen port relay: %v\n", err)
		return
	}
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go relayToPort(conn)
	}
}

// relayToPort reads the target port header from conn, dials that loopback port
// inside the VM, and splices the two. The published service may still be coming
// up just after a wake, so it retries the dial briefly (as relayToSSHD does).
func relayToPort(conn net.Conn) {
	port, err := guestproto.ReadDialPort(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var upstream net.Conn
	for range 50 {
		c, derr := net.Dial("tcp", addr) //nolint:noctx // lifetime is the spliced connection
		if derr == nil {
			upstream = c
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if upstream == nil {
		_ = conn.Close()
		return
	}
	splice(conn, upstream)
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
	case guestproto.RequestSetup:
		applySetup(conn, req.Spec)
	case guestproto.RequestExec:
		runExec(conn, req.Spec)
	case guestproto.RequestShell:
		runShell(conn, req.Shell)
	case guestproto.RequestStat:
		if err := guestproto.WriteStat(conn, guestproto.Stat{Load1: loadAvg1()}); err != nil {
			fmt.Fprintf(os.Stderr, "fletcher-guest: write stat: %v\n", err)
		}
	case guestproto.RequestShutdown:
		shutdown() // resets the VM; does not return
	default:
		fmt.Fprintf(os.Stderr, "fletcher-guest: unknown request kind %q\n", req.Kind)
	}
}

// loadAvg1 reads the 1-minute load average from /proc/loadavg; 0 if unreadable.
func loadAvg1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// runShell opens a PTY running a login shell and bridges it to the host over
// conn: the guest streams terminal output back as KindStdout frames while
// reading KindStdin/KindResize frames from the host. It returns when the shell
// exits (user typed exit) or the host closes the connection (client detached).
func runShell(conn net.Conn, spec guestproto.ShellSpec) {
	lu := lookupLoginUser()
	shell := loginShell()
	cmd := exec.CommandContext(context.Background(), shell, "-l") //nolint:gosec // launching a shell is the entire purpose
	cmd.Dir = shellDir()
	cmd.Env = shellEnv(spec, lu)
	applyLoginUser(cmd, lu) // before pty.Start, which augments SysProcAttr

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

// loginUserName is the unprivileged account agent commands run as, so session
// shell/exec (and jobs) match brokered SSH - all run as the image's login user,
// sharing one home and agent config - instead of root.
const loginUserName = "fletcher"

// loginUser is the resolved login account; ok is false on an image without it
// (then commands stay as root, the historical behaviour).
type loginUser struct {
	uid, gid uint32
	home     string
	ok       bool
}

// lookupLoginUser resolves loginUserName from the rootfs (/etc/passwd, parsed by
// pure-Go os/user under CGO_ENABLED=0).
func lookupLoginUser() loginUser {
	u, err := user.Lookup(loginUserName)
	if err != nil {
		return loginUser{}
	}
	uid, uerr := strconv.Atoi(u.Uid)
	gid, gerr := strconv.Atoi(u.Gid)
	if uerr != nil || gerr != nil || uid < 0 || gid < 0 || uid > math.MaxUint32 || gid > math.MaxUint32 {
		return loginUser{}
	}
	return loginUser{uid: uint32(uid), gid: uint32(gid), home: u.HomeDir, ok: true}
}

// applyLoginUser makes cmd run as the login user when resolved. For a PTY shell,
// call it before pty.Start so creack/pty adds Setctty/Setsid to the same
// SysProcAttr rather than replacing the credential.
func applyLoginUser(cmd *exec.Cmd, lu loginUser) {
	if !lu.ok {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: lu.uid, Gid: lu.gid}
}

// shellEnv builds the login shell's environment: the spec's TERM plus the
// usual PATH/HOME defaults (for the login user) and any extra entries the host
// passed.
func shellEnv(spec guestproto.ShellSpec, lu loginUser) []string {
	env := withDefaults(spec.Env, lu)
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
	lu := lookupLoginUser()
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", spec.Command) //nolint:gosec // running the job is the entire purpose
	cmd.Dir = workDir
	cmd.Env = withDefaults(spec.Env, lu)
	applyLoginUser(cmd, lu)
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

// withDefaults ensures the command has a usable PATH, HOME, and USER even if the
// spec did not set them. HOME/USER track the login user (root when unresolved)
// so an agent finds its config in the same home it uses over SSH.
func withDefaults(env []string, lu loginUser) []string {
	home, name := "/root", "root"
	if lu.ok {
		home, name = lu.home, loginUserName
	}
	out := append([]string(nil), env...)
	if !hasKey(out, "PATH") {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !hasKey(out, "HOME") {
		out = append(out, "HOME="+home)
	}
	if !hasKey(out, "USER") {
		out = append(out, "USER="+name)
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

// setupOnce guards the session setup (service forwards + the global env file) so
// it runs exactly once per guest lifetime. A hibernation restore resumes the
// same guest process (forwards already listening, env file already written), so
// a resent RequestSetup is a no-op rather than a double-bind.
var setupOnce sync.Once

// sessionEnvFile is sourced by every login shell (sshd, console), so an agent
// started over brokered SSH or an IDE terminal - not just the guestproto exec
// and shell paths - inherits the gateway/MCP env.
const sessionEnvFile = "/etc/profile.d/fletcher.sh"

// applySetup handles a RequestSetup: once per guest lifetime it brings up the
// session's service forwards and writes the gateway/MCP env where login shells
// pick it up, then acks with a single Exit frame so the host knows the loopback
// listeners are up before it reports the session ready.
func applySetup(conn net.Conn, spec guestproto.Spec) {
	setupOnce.Do(func() {
		startForwards(spec.Forwards)
		writeSessionEnv(spec.Env)
	})
	if err := guestproto.WriteFrame(conn, guestproto.KindExit, guestproto.EncodeExit(0)); err != nil {
		fmt.Fprintf(os.Stderr, "fletcher-guest: ack setup: %v\n", err)
	}
}

// writeSessionEnv writes the session env as a profile.d snippet so login shells
// (brokered SSH, IDE terminals) export the gateway/MCP variables. The guestproto
// exec/shell paths inject the same env directly; this covers sshd, which spawns
// its login shells with only the init environment.
func writeSessionEnv(env []string) {
	if len(env) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("# Fletcher session environment (model gateway + MCP); written by the guest init.\n")
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "export %s=%s\n", k, shellSingleQuote(v))
	}
	if err := os.WriteFile(sessionEnvFile, []byte(b.String()), 0o644); err != nil { //nolint:gosec // sourced by login shells; non-secret gateway URLs + placeholder keys
		fmt.Fprintf(os.Stderr, "fletcher-guest: write session env: %v\n", err)
	}
}

// shellSingleQuote wraps a value in single quotes for safe sourcing, escaping any
// embedded single quotes the POSIX way ('\”).
func shellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
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
