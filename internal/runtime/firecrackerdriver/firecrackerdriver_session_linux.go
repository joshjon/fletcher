//go:build linux

package firecrackerdriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

	fcruntime "github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

// sessionStartGrace bounds how long StartSession waits for the guest to finish
// booting and start listening before giving up.
const sessionStartGrace = 30 * time.Second

// StartSession brings a persistent session VM up and returns a handle whose
// Exec runs commands in it. If a valid hibernation snapshot exists it is
// restored (instant wake, live process tree intact); otherwise the VM cold
// boots against its fork. Unlike Run, the VM stays up until the handle's Stop.
// Satisfies runtime.SessionRuntime.
func (d *Driver) StartSession(ctx context.Context, spec fcruntime.SessionSpec) (fcruntime.SessionHandle, error) {
	if spec.RootfsPath == "" {
		return nil, errors.New("firecracker: session RootfsPath is required")
	}
	vmDir := filepath.Join(d.runDir, "session-"+sanitiseID(spec.SessionID))

	if d.hasValidSnapshot(vmDir) {
		handle, err := d.restoreSession(ctx, spec, vmDir)
		if err == nil {
			return handle, nil
		}
		// A snapshot that won't restore is not load-bearing: disk is the source
		// of truth (DESIGN.md §5/§11), so fall back to a cold boot.
		d.logger.Warn("restore from hibernation snapshot failed; cold-booting from disk",
			slog.String("session_id", spec.SessionID), slog.String("err", err.Error()))
	}
	return d.coldBootSession(ctx, spec, vmDir)
}

// coldBootSession boots a fresh session VM against its fork from a clean vmDir.
func (d *Driver) coldBootSession(ctx context.Context, spec fcruntime.SessionSpec, vmDir string) (fcruntime.SessionHandle, error) {
	// The vmDir is reused across starts (keyed by session id). A previous run
	// that died with the daemon - or a snapshot we just failed to restore -
	// leaves stale files behind; start from a clean slate.
	if err := os.RemoveAll(vmDir); err != nil {
		return nil, fmt.Errorf("firecracker: clear stale session vm dir: %w", err)
	}
	if err := os.MkdirAll(vmDir, 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: create session vm dir: %w", err)
	}
	apiSock := filepath.Join(vmDir, "fc.sock")
	vsockUDS := filepath.Join(vmDir, "v.sock")

	// The session VM outlives the request that started it: give it its own
	// context, cancelled only when the session is stopped.
	vmCtx, vmCancel := context.WithCancel(context.WithoutCancel(ctx))
	cleanup := func() {
		vmCancel()
		_ = os.RemoveAll(vmDir)
	}

	console := &capWriter{max: 32 << 10}
	cfg := d.machineConfig(apiSock, vsockUDS, spec.RootfsPath, true)
	fcCmd := firecracker.VMCommandBuilder{}.
		WithBin(d.firecrackerBinary).
		WithSocketPath(apiSock).
		WithStdout(console).
		WithStderr(console).
		Build(vmCtx)

	machine, err := firecracker.NewMachine(vmCtx, cfg,
		firecracker.WithProcessRunner(fcCmd),
		firecracker.WithLogger(d.log),
	)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker: new session machine: %w", err)
	}
	if err := machine.Start(vmCtx); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker: start session microVM: %w%s", err, console.suffix())
	}

	// Wait until the guest's control server is listening, so the session is
	// exec-ready when StartSession returns.
	probeCtx, cancel := context.WithTimeout(vmCtx, sessionStartGrace)
	defer cancel()
	probe, derr := dialGuest(probeCtx, vsockUDS, guestproto.ControlPort)
	if derr != nil {
		_ = machine.StopVMM()
		cleanup()
		return nil, fmt.Errorf("firecracker: session guest not ready: %w%s", derr, console.suffix())
	}
	_ = probe.Close()

	return &fcSession{
		machine:  machine,
		vsockUDS: vsockUDS,
		vmDir:    vmDir,
		vmCancel: vmCancel,
		env:      spec.Env,
		snapID:   d.snapshotIdentity(),
	}, nil
}

// DiscardSession removes a session's on-disk VM state (any hibernation snapshot
// and sockets). Called when a session is deleted; the fork itself is owned by
// the snapshot driver.
func (d *Driver) DiscardSession(_ context.Context, sessionID string) error {
	vmDir := filepath.Join(d.runDir, "session-"+sanitiseID(sessionID))
	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("firecracker: discard session vm dir: %w", err)
	}
	return nil
}

// fcSession is a running session VM. Satisfies runtime.SessionHandle.
type fcSession struct {
	machine  *firecracker.Machine
	vsockUDS string
	vmDir    string
	vmCancel context.CancelFunc
	env      []string
	// snapID fingerprints the VMM+kernel a hibernation snapshot is tied to, so a
	// later start can tell a usable snapshot from one a Fletcher upgrade staled.
	snapID string
}

// Exec runs a command in the running session VM, streaming output back.
func (s *fcSession) Exec(ctx context.Context, spec fcruntime.Spec, stdout, stderr io.Writer) (fcruntime.Result, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return fcruntime.Result{}, fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()

	env := spec.Env
	if len(env) == 0 {
		env = s.env
	}
	req := guestproto.Request{
		Kind: guestproto.RequestExec,
		Spec: guestproto.Spec{Command: spec.Command, Env: env, WorkDir: guestWorkDir},
	}
	if err := guestproto.WriteRequest(conn, req); err != nil {
		return fcruntime.Result{}, fmt.Errorf("firecracker: send exec: %w", err)
	}
	code, err := demuxFrames(ctx, conn, stdout, stderr)
	if err != nil {
		return fcruntime.Result{}, err
	}
	return fcruntime.Result{ExitCode: code}, nil
}

// Shell opens an interactive PTY in the running session VM. It sends the host's
// keystrokes (stdin) and window resizes to the guest as frames, and writes the
// guest's terminal output to stdout, returning the shell's exit code.
func (s *fcSession) Shell(ctx context.Context, spec fcruntime.ShellSpec, stdin io.Reader, stdout io.Writer, resize <-chan fcruntime.WinSize) (int32, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return 0, fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()

	env := spec.Env
	if len(env) == 0 {
		env = s.env
	}
	req := guestproto.Request{
		Kind:  guestproto.RequestShell,
		Shell: guestproto.ShellSpec{Term: spec.Term, Cols: spec.Cols, Rows: spec.Rows, Env: env},
	}
	if err := guestproto.WriteRequest(conn, req); err != nil {
		return 0, fmt.Errorf("firecracker: send shell: %w", err)
	}

	// stdin and resize both write frames; serialise them.
	var wmu sync.Mutex
	writeFrame := func(kind byte, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return guestproto.WriteFrame(conn, kind, payload)
	}

	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pumpStdin(pumpCtx, stdin, writeFrame)
	go pumpResize(pumpCtx, resize, writeFrame)

	// Guest -> host: terminal output, then a final exit frame.
	for {
		kind, payload, rerr := guestproto.ReadFrame(conn)
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return 0, nil
			}
			return 0, fmt.Errorf("firecracker: shell stream: %w", rerr)
		}
		switch kind {
		case guestproto.KindStdout, guestproto.KindStderr:
			if _, werr := stdout.Write(payload); werr != nil {
				return 0, werr
			}
		case guestproto.KindExit:
			code, _ := guestproto.DecodeExit(payload)
			return code, nil
		}
	}
}

// pumpStdin forwards the host's keystrokes to the guest as KindStdin frames
// until stdin ends or the shell closes.
func pumpStdin(ctx context.Context, stdin io.Reader, writeFrame func(byte, []byte) error) {
	buf := make([]byte, 32<<10)
	for {
		n, rerr := stdin.Read(buf)
		if n > 0 {
			if werr := writeFrame(guestproto.KindStdin, buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// pumpResize forwards window-size changes to the guest as KindResize frames.
func pumpResize(ctx context.Context, resize <-chan fcruntime.WinSize, writeFrame func(byte, []byte) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case w, ok := <-resize:
			if !ok {
				return
			}
			if err := writeFrame(guestproto.KindResize, guestproto.EncodeResize(w.Cols, w.Rows)); err != nil {
				return
			}
		}
	}
}

// DialSSH opens a raw vsock connection to the guest's SSH relay (which splices
// to its loopback sshd). The caller proxies an SSH session over it.
func (s *fcSession) DialSSH(ctx context.Context) (net.Conn, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.SSHPort)
	if err != nil {
		return nil, fmt.Errorf("firecracker: connect session ssh: %w", err)
	}
	return conn, nil
}

// Load returns the guest's 1-minute load average via a stat control request.
func (s *fcSession) Load(ctx context.Context) (float64, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return 0, fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := guestproto.WriteRequest(conn, guestproto.Request{Kind: guestproto.RequestStat}); err != nil {
		return 0, fmt.Errorf("firecracker: send stat: %w", err)
	}
	stat, err := guestproto.ReadStat(conn)
	if err != nil {
		return 0, fmt.Errorf("firecracker: read stat: %w", err)
	}
	return stat.Load1, nil
}

// Stop hibernates the session: it snapshots the VM's memory to disk and exits
// the VMM (freeing host RAM), so a later Start wakes it instantly with its
// process tree intact. If hibernation fails it falls back to a clean shutdown.
// Either way the fork on disk survives.
func (s *fcSession) Stop(ctx context.Context) error {
	if err := s.hibernate(ctx); err != nil {
		s.cleanShutdown(ctx)
	}
	return nil
}

// cleanShutdown asks the guest to sync and reset, waits for the VMM to exit,
// and removes the vmDir (no snapshot is kept).
func (s *fcSession) cleanShutdown(ctx context.Context) {
	if conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort); err == nil {
		_ = guestproto.WriteRequest(conn, guestproto.Request{Kind: guestproto.RequestShutdown})
		_ = conn.Close()
	}
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	_ = s.machine.Wait(waitCtx)
	_ = s.machine.StopVMM()
	s.vmCancel()
	_ = os.RemoveAll(s.vmDir)
}
