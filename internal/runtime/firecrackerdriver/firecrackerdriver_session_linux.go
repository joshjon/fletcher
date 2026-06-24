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
	"strings"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

	"github.com/joshjon/fletcher/internal/errs"
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

	if d.hasValidSnapshot(vmDir, spec.RootfsPath) {
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

	// Bring the fork's guest init up to this daemon's version before booting it:
	// the init pairs with the host wire protocol, so an image built by an older
	// release must not boot its stale init. Best-effort - a refresh failure logs
	// and boots whatever the fork carries rather than blocking the session.
	if err := d.refreshGuestInit(ctx, spec.RootfsPath); err != nil {
		d.logger.Warn("could not refresh guest init before cold boot; booting the rootfs's existing init",
			slog.String("session_id", spec.SessionID), slog.String("err", err.Error()))
	}

	// The session VM outlives the request that started it: give it its own
	// context, cancelled only when the session is stopped.
	vmCtx, vmCancel := context.WithCancel(context.WithoutCancel(ctx))
	cleanup := func() {
		vmCancel()
		_ = os.RemoveAll(vmDir)
	}

	console := &capWriter{max: 32 << 10}
	cfg := d.machineConfig(apiSock, vsockUDS, spec.RootfsPath, true, spec.RunApp, spec.VolumePath)
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

	// Bring up the gateway/MCP forwards (and the egress proxy forward as gated by
	// the session's egress policy) so the agent env (ANTHROPIC_BASE_URL etc.)
	// resolves to live listeners. Non-fatal: a session without them is still
	// usable for a shell, just not for model/MCP calls. envForPolicy strips the
	// proxy vars for a "none" session so they match its (absent) egress forward.
	effEnv := envForPolicy(spec.EgressPolicy, spec.Env)
	forwardLns, ferr := d.startSessionForwards(vmCtx, vsockUDS, d.forwardsForPolicy(spec.EgressPolicy), effEnv, spec.AppEnv, guestCredentials(spec.Credentials))
	if ferr != nil {
		d.logger.Warn("session service forwards not fully established; model gateway/MCP may be unreachable in this session",
			slog.String("session_id", spec.SessionID), slog.String("err", ferr.Error()))
	}

	return &fcSession{
		machine:    machine,
		vsockUDS:   vsockUDS,
		vmDir:      vmDir,
		vmCancel:   vmCancel,
		env:        effEnv,
		forwardLns: forwardLns,
		snapID:     d.snapshotIdentity(spec.RootfsPath),
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

// ReclaimOrphans removes session vmDirs whose id is not in keep - leaked VM
// state (hibernation snapshots are ~GiB each) from sessions deleted while the
// daemon was down, an ephemeral build fork that did not clean up, or an older
// release. keep is the live session ids (all of them, running and hibernated -
// a hibernated session's snapshot must be retained). Returns how many it
// reclaimed. Called on boot, when no build forks are in flight.
func (d *Driver) ReclaimOrphans(_ context.Context, keep []string) (int, error) {
	keepDirs := make(map[string]bool, len(keep))
	for _, id := range keep {
		keepDirs["session-"+sanitiseID(id)] = true
	}
	entries, err := os.ReadDir(d.runDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("firecracker: list vm dirs: %w", err)
	}
	reclaimed := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, "session-") || keepDirs[name] {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(d.runDir, name)); rerr == nil {
			reclaimed++
		}
	}
	return reclaimed, nil
}

// fcSession is a running session VM. Satisfies runtime.SessionHandle.
type fcSession struct {
	machine  *firecracker.Machine
	vsockUDS string
	vmDir    string
	vmCancel context.CancelFunc
	env      []string
	// forwardLns are the host-side proxy listeners for the session's service
	// forwards (gateway, MCP). They live for the VM's lifetime and are closed
	// when it stops, so a later start (or restore) can recreate them.
	forwardLns []net.Listener
	// snapID fingerprints the VMM+kernel a hibernation snapshot is tied to, so a
	// later start can tell a usable snapshot from one a Fletcher upgrade staled.
	snapID string
}

// startSessionForwards stands up the host side of the session's service forwards
// (one unix-socket proxy per Forward, keyed by vsock port) and tells the guest
// to bring up the matching loopback listeners. Without this a session's agent
// env points at gateway/MCP ports with nothing listening - the ephemeral path
// does the equivalent inline in Run. Best-effort: it returns any listeners it
// did open alongside an error so the caller can both close them and log, leaving
// the session usable (just without model/MCP access) rather than failing to boot.
func (d *Driver) startSessionForwards(ctx context.Context, vsockUDS string, forwards []Forward, env, appEnv []string, creds []guestproto.CredentialFile) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, len(forwards))
	gforwards := make([]guestproto.Forward, 0, len(forwards))
	for i, f := range forwards {
		port := uint32(guestproto.ForwardPortBase + i)
		ln, err := startForwardProxy(ctx, fmt.Sprintf("%s_%d", vsockUDS, port), f.HostSocket)
		if err != nil {
			return lns, fmt.Errorf("forward proxy %s: %w", f.ListenAddr, err)
		}
		lns = append(lns, ln)
		gforwards = append(gforwards, guestproto.Forward{ListenAddr: f.ListenAddr, VsockPort: port})
	}

	// Nothing to deliver (no forwards, no env, no app env, no seeded
	// credentials): skip the setup round-trip, preserving the prior behaviour for
	// a bare session. A run_app session launches its app from setup, so its env -
	// always non-empty - keeps setup flowing.
	if len(gforwards) == 0 && len(env) == 0 && len(appEnv) == 0 && len(creds) == 0 {
		return lns, nil
	}
	setupCtx, cancel := context.WithTimeout(ctx, sessionStartGrace)
	defer cancel()
	if err := sendSessionSetup(setupCtx, vsockUDS, gforwards, env, appEnv, creds); err != nil {
		return lns, fmt.Errorf("send setup: %w", err)
	}
	return lns, nil
}

// sendSessionSetup tells the guest to bring up the given forwards, export the
// session env to login shells, layer the user env vars (appEnv) onto a run_app
// app, and seed any agent-login credentials, then waits for its ack frame so all
// are in place before the session is reported ready.
func sendSessionSetup(ctx context.Context, vsockUDS string, forwards []guestproto.Forward, env, appEnv []string, creds []guestproto.CredentialFile) error {
	conn, err := dialGuest(ctx, vsockUDS, guestproto.ControlPort)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	req := guestproto.Request{Kind: guestproto.RequestSetup, Spec: guestproto.Spec{Forwards: forwards, Env: env, AppEnv: appEnv, Credentials: creds}}
	if err := guestproto.WriteRequest(conn, req); err != nil {
		return err
	}
	if _, _, err := guestproto.ReadFrame(conn); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// guestCredentials converts the runtime's seeded credential files into the
// guest wire form. Returns nil for none, so a start (which seeds nothing) sends
// an empty setup.
func guestCredentials(creds []fcruntime.CredentialFile) []guestproto.CredentialFile {
	if len(creds) == 0 {
		return nil
	}
	out := make([]guestproto.CredentialFile, len(creds))
	for i, c := range creds {
		out[i] = guestproto.CredentialFile{Path: c.Path, Mode: c.Mode, Data: c.Data}
	}
	return out
}

// closeForwards shuts the host-side forward proxies (called when the VM stops or
// hibernates); a later start recreates them.
func (s *fcSession) closeForwards() {
	for _, ln := range s.forwardLns {
		_ = ln.Close()
	}
	s.forwardLns = nil
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

// WriteFile uploads content into the guest fork: it sends the request, waits for
// the guest's readiness ack, streams spec.Size bytes, then reads the final
// result (bytes written + content hash). Two-phase so a bad destination fails
// before the upload streams.
func (s *fcSession) WriteFile(ctx context.Context, spec fcruntime.FileWriteSpec, content io.Reader) (fcruntime.FileWriteResult, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return fcruntime.FileWriteResult{}, fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Close the conn when ctx is cancelled so a stalled transfer unblocks.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req := guestproto.Request{
		Kind: guestproto.RequestWriteFile,
		File: guestproto.FileSpec{Path: spec.Path, Mode: spec.Mode, Size: spec.Size},
	}
	if err := guestproto.WriteRequest(conn, req); err != nil {
		return fcruntime.FileWriteResult{}, fmt.Errorf("firecracker: send upload: %w", err)
	}
	ack, err := guestproto.ReadFileResult(conn)
	if err != nil {
		return fcruntime.FileWriteResult{}, fmt.Errorf("firecracker: upload ack: %w", err)
	}
	if ack.Error != "" {
		return fcruntime.FileWriteResult{}, errs.New(errs.CategoryFailedPrecondition, ack.Error)
	}
	if _, err := io.CopyN(conn, content, spec.Size); err != nil {
		return fcruntime.FileWriteResult{}, fmt.Errorf("firecracker: stream upload: %w", err)
	}
	res, err := guestproto.ReadFileResult(conn)
	if err != nil {
		return fcruntime.FileWriteResult{}, fmt.Errorf("firecracker: upload result: %w", err)
	}
	if res.Error != "" {
		return fcruntime.FileWriteResult{}, fmt.Errorf("firecracker: guest write failed: %s", res.Error)
	}
	return fcruntime.FileWriteResult{BytesWritten: res.BytesWritten, Sha256: res.Sha256}, nil
}

// ReadFile downloads a guest file: it sends the request, reads the size/mode
// header (delivered to onInfo), then streams exactly that many bytes to w.
func (s *fcSession) ReadFile(ctx context.Context, path string, onInfo func(fcruntime.FileReadResult) error, w io.Writer) error {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req := guestproto.Request{Kind: guestproto.RequestReadFile, File: guestproto.FileSpec{Path: path}}
	if err := guestproto.WriteRequest(conn, req); err != nil {
		return fmt.Errorf("firecracker: send download: %w", err)
	}
	hdr, err := guestproto.ReadFileResult(conn)
	if err != nil {
		return fmt.Errorf("firecracker: download header: %w", err)
	}
	if hdr.Error != "" {
		return errs.New(errs.CategoryNotFound, hdr.Error)
	}
	if onInfo != nil {
		if err := onInfo(fcruntime.FileReadResult{Size: hdr.Size, Mode: hdr.Mode}); err != nil {
			return err
		}
	}
	if _, err := io.CopyN(w, conn, hdr.Size); err != nil {
		return fmt.Errorf("firecracker: stream download: %w", err)
	}
	return nil
}

// ListDir lists a directory in the guest fork. The guest serves it in pure Go,
// so it works on an image with no shell.
func (s *fcSession) ListDir(ctx context.Context, path string) (fcruntime.DirListing, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return fcruntime.DirListing{}, fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req := guestproto.Request{Kind: guestproto.RequestListDir, File: guestproto.FileSpec{Path: path}}
	if err := guestproto.WriteRequest(conn, req); err != nil {
		return fcruntime.DirListing{}, fmt.Errorf("firecracker: send list: %w", err)
	}
	listing, err := guestproto.ReadDirListing(conn)
	if err != nil {
		return fcruntime.DirListing{}, fmt.Errorf("firecracker: read listing: %w", err)
	}
	if listing.Error != "" {
		return fcruntime.DirListing{}, errs.New(errs.CategoryNotFound, listing.Error)
	}
	out := fcruntime.DirListing{Path: listing.Path, Truncated: listing.Truncated}
	out.Entries = make([]fcruntime.DirEntry, len(listing.Entries))
	for i, e := range listing.Entries {
		out.Entries[i] = fcruntime.DirEntry{
			Name:          e.Name,
			Size:          e.Size,
			Mode:          e.Mode,
			IsDir:         e.IsDir,
			ModTime:       e.ModTime,
			IsSymlink:     e.IsSymlink,
			SymlinkTarget: e.SymlinkTarget,
		}
	}
	return out, nil
}

// FileOp performs a delete, move, or copy in the guest fork (served by the guest
// in pure Go).
func (s *fcSession) FileOp(ctx context.Context, spec fcruntime.FileOpSpec) error {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req := guestproto.Request{
		Kind: guestproto.RequestFileOp,
		FileOp: guestproto.FileOpSpec{
			Op:        guestproto.FileOpKind(spec.Op),
			Path:      spec.Path,
			Dest:      spec.Dest,
			Recursive: spec.Recursive,
		},
	}
	if err := guestproto.WriteRequest(conn, req); err != nil {
		return fmt.Errorf("firecracker: send file op: %w", err)
	}
	res, err := guestproto.ReadFileResult(conn)
	if err != nil {
		return fmt.Errorf("firecracker: file op result: %w", err)
	}
	if res.Error != "" {
		return errs.New(errs.CategoryFailedPrecondition, res.Error)
	}
	return nil
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
		Shell: guestproto.ShellSpec{Term: spec.Term, Cols: spec.Cols, Rows: spec.Rows, Env: env, ControlMode: spec.ControlMode},
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

// DialPort opens a raw vsock connection to the guest's generic port-forward
// relay and tells it which loopback port to splice to. The caller proxies a
// connection (e.g. a published preview port) over it; the VM needs no NIC.
func (s *fcSession) DialPort(ctx context.Context, port uint16) (net.Conn, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.PortForwardPort)
	if err != nil {
		return nil, fmt.Errorf("firecracker: connect session port forward: %w", err)
	}
	if err := guestproto.WriteDialPort(conn, port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("firecracker: send port-forward header: %w", err)
	}
	return conn, nil
}

// Load returns the guest's 1-minute load average via a stat control request.
func (s *fcSession) Load(ctx context.Context) (float64, error) {
	stat, err := s.stat(ctx)
	return stat.Load1, err
}

// AppRestarts returns how many times the guest's app supervisor has restarted a
// run_app session's app since the VM booted.
func (s *fcSession) AppRestarts(ctx context.Context) (int64, error) {
	stat, err := s.stat(ctx)
	return stat.AppRestarts, err
}

// stat fetches the guest's liveness sample (load + app restart count) over the
// control vsock.
func (s *fcSession) stat(ctx context.Context) (guestproto.Stat, error) {
	conn, err := dialGuest(ctx, s.vsockUDS, guestproto.ControlPort)
	if err != nil {
		return guestproto.Stat{}, fmt.Errorf("firecracker: connect session: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := guestproto.WriteRequest(conn, guestproto.Request{Kind: guestproto.RequestStat}); err != nil {
		return guestproto.Stat{}, fmt.Errorf("firecracker: send stat: %w", err)
	}
	stat, err := guestproto.ReadStat(conn)
	if err != nil {
		return guestproto.Stat{}, fmt.Errorf("firecracker: read stat: %w", err)
	}
	return stat, nil
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
	s.closeForwards()
	s.vmCancel()
	_ = os.RemoveAll(s.vmDir)
}
