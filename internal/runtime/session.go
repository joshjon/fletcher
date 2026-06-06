package runtime

import (
	"context"
	"io"
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
}

// SessionHandle is a running session VM. It stays up until Stop; Exec runs
// commands inside it without tearing it down.
type SessionHandle interface {
	// Exec runs spec.Command in the running VM, streaming output to
	// stdout/stderr, and returns the exit code.
	Exec(ctx context.Context, spec Spec, stdout, stderr io.Writer) (Result, error)
	// Stop shuts the VM down cleanly. The fork on disk is untouched.
	Stop(ctx context.Context) error
}

// SessionRuntime is the optional capability a Driver advertises when it can host
// durable sessions (persistent, exec-able VMs). Firecracker implements it; the
// mock and runc drivers do not, so the daemon reports sessions as unavailable
// on those runtimes.
type SessionRuntime interface {
	StartSession(ctx context.Context, spec SessionSpec) (SessionHandle, error)
}
