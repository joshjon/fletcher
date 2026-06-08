package runtime

import (
	"context"
	"io"
	"net"
)

// SessionSpec describes a persistent session VM to start.
type SessionSpec struct {
	// SessionID identifies the session; drivers use it for per-VM paths.
	SessionID string
	// RootfsPath is the host path to the session's persistent fork (its disk).
	// Unlike an ephemeral job, this fork is created once and reused across
	// stop/start, so the workspace survives.
	RootfsPath string
	// Env is the default environment for commands run in the session.
	Env []string
	// EgressPolicy gates the fork's outbound network: "none" | "allowlist" |
	// "open" (empty is treated as "allowlist"). The driver uses it to pick the
	// egress proxy socket the fork's HTTP_PROXY reaches, or to deny egress.
	EgressPolicy string
	// RunApp makes the guest run the image's own app (its captured entrypoint) on
	// boot, instead of waiting for exec/shell. Set for a session created --app
	// (Milestone 9). The driver signals it via the guest kernel cmdline.
	RunApp bool
}

// ShellSpec parameterises an interactive PTY in a session VM.
type ShellSpec struct {
	// Term is the TERM value set inside the PTY (e.g. xterm-256color).
	Term string
	// Cols and Rows are the initial window size.
	Cols uint16
	Rows uint16
	// Env is extra environment for the login shell.
	Env []string
}

// WinSize is a terminal window size pushed mid-session on a resize.
type WinSize struct {
	Cols uint16
	Rows uint16
}

// SessionHandle is a running session VM. It stays up until Stop; Exec runs
// commands inside it without tearing it down.
type SessionHandle interface {
	// Exec runs spec.Command in the running VM, streaming output to
	// stdout/stderr, and returns the exit code.
	Exec(ctx context.Context, spec Spec, stdout, stderr io.Writer) (Result, error)
	// Shell opens an interactive login shell on a PTY. It writes terminal
	// output to stdout, reads keystrokes from stdin, applies window sizes from
	// resize, and returns the shell's exit code when it ends or stdin closes.
	Shell(ctx context.Context, spec ShellSpec, stdin io.Reader, stdout io.Writer, resize <-chan WinSize) (int32, error)
	// DialSSH opens a raw byte stream to the VM's SSH server (relayed over
	// vsock). The caller proxies an SSH connection through it; the VM needs no
	// network route. The caller closes the returned conn.
	DialSSH(ctx context.Context) (net.Conn, error)
	// DialPort opens a raw byte stream to a loopback TCP port inside the VM
	// (relayed over vsock, like DialSSH but for an arbitrary port). The caller
	// proxies a connection through it to reach a service the session is serving
	// - a published/preview port - while the VM stays unroutable. The caller
	// closes the returned conn.
	DialPort(ctx context.Context, port uint16) (net.Conn, error)
	// Load returns the guest's 1-minute load average, a proxy for in-guest work
	// in flight. Used to avoid auto-stopping a session whose task is running.
	Load(ctx context.Context) (float64, error)
	// Stop shuts the VM down cleanly. The fork on disk is untouched.
	Stop(ctx context.Context) error
}

// SessionRuntime is the optional capability a Driver advertises when it can host
// durable sessions (persistent, exec-able VMs). Firecracker implements it; the
// mock and runc drivers do not, so the daemon reports sessions as unavailable
// on those runtimes.
type SessionRuntime interface {
	StartSession(ctx context.Context, spec SessionSpec) (SessionHandle, error)
	// DiscardSession removes a session's on-disk VM state (any hibernation
	// snapshot and runtime sockets) when the session is deleted. The fork is
	// owned by the snapshot driver and removed separately.
	DiscardSession(ctx context.Context, sessionID string) error
}
