//go:build linux

package hostdriver

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/runtime"
)

func serveTestExecutor(t *testing.T, s *executor, path string) func() {
	t.Helper()
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	background.GoNamed(ctx, "test.host.recovery.executor", func(ctx context.Context) {
		done <- s.serveConnections(ctx, listener, uint32(os.Geteuid()))
	})
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("executor connections did not drain")
		}
	})
	t.Cleanup(stop)
	return stop
}

func attachTestTerminal(t *testing.T, h runtime.SessionHandle) (<-chan error, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	input, writer := io.Pipe()
	output := &terminalOutput{changed: make(chan struct{}, 1)}
	done := make(chan error, 1)
	background.GoNamed(ctx, "test.host.recovery.shell", func(ctx context.Context) {
		defer close(done)
		_, err := h.Shell(ctx, runtime.ShellSpec{TerminalName: "recovery", ControlMode: true, Cols: 100, Rows: 30}, input, output, nil)
		done <- err
	})
	stop := sync.OnceFunc(func() {
		cancel()
		_ = writer.Close()
		_ = input.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("terminal attachment did not drain")
		}
	})
	t.Cleanup(stop)
	output.wait(t, "%session-changed")
	return done, stop
}

func TestDriverExecutorReplacementRequiresExplicitRecovery(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is required for the optional host terminal test")
	}
	s, spec := testExecutor(t)
	s.tmux = tmux
	path := filepath.Join(s.dir, "executor.sock")
	stopServer := serveTestExecutor(t, s, path)
	d := &Driver{account: spec.Account, socket: path}
	h, err := d.StartHostSession(t.Context(), spec)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, s.stop(ctx, spec.SessionID))
	})
	_, detach := attachTestTerminal(t, h)
	pane := tmuxOutput(t, s, spec.SessionID, "display-message", "-p", "-t", "=recovery:", "#{pane_id}|#{pane_pid}")
	detach()
	interrupted, detachAgain := attachTestTerminal(t, h)
	require.Equal(t, pane, tmuxOutput(t, s, spec.SessionID, "display-message", "-p", "-t", "=recovery:", "#{pane_id}|#{pane_pid}"), "transport reconnection must reuse the existing shell")
	keep := filepath.Join(spec.WorkDir, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("retained"), 0o600))

	stopServer()
	select {
	case err := <-interrupted:
		require.Error(t, err, "executor loss must not report a successful terminal exit")
	case <-time.After(5 * time.Second):
		t.Fatal("active terminal did not report executor loss")
	}
	detachAgain()
	_, err = d.HostAccount(t.Context())
	require.ErrorContains(t, err, "unavailable")
	_, err = h.ListDir(t.Context(), spec.WorkDir)
	require.ErrorContains(t, err, "unavailable")
	require.Error(t, h.Stop(t.Context()), "socket loss must not report confirmed cleanup")

	// systemd owns process cleanup in production. This private test server has
	// no cgroup supervisor, so explicitly stop only its dedicated tmux server.
	require.NoError(t, s.stop(t.Context(), spec.SessionID))
	fresh := &executor{account: s.account, shell: s.shell, tmux: s.tmux, dir: s.dir, sessions: make(map[string]*hostSession)}
	serveTestExecutor(t, fresh, path)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, fresh.stop(ctx, spec.SessionID))
	})
	_, err = d.HostAccount(t.Context())
	require.NoError(t, err)
	_, err = h.ListDir(t.Context(), spec.WorkDir)
	require.ErrorContains(t, err, "restart the session")
	_, err = h.Shell(t.Context(), runtime.ShellSpec{TerminalName: "recovery"}, strings.NewReader(""), io.Discard, nil)
	require.ErrorContains(t, err, "restart the session")
	require.Empty(t, fresh.sessions, "stale handles must not silently recreate namespaces")

	recovered, err := d.StartHostSession(t.Context(), spec)
	require.NoError(t, err)
	_, detachRecovered := attachTestTerminal(t, recovered)
	detachRecovered()
	require.NoError(t, recovered.Stop(t.Context()))
	data, err := os.ReadFile(keep)
	require.NoError(t, err)
	require.Equal(t, "retained", string(data))
}

func TestDriverInterruptedTerminalFrameIsNotSuccessfulExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executor.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer listener.Close()
	done := make(chan struct{})
	background.GoNamed(t.Context(), "test.host.truncated-frame", func(context.Context) {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var req request
		if readPacket(conn, &req) != nil || writePacket(conn, packet{Kind: "ready"}) != nil {
			return
		}
		// A killed executor or interrupted socket can leave a partial frame.
		_, _ = conn.Write([]byte{0, 0, 0, 32, '{'})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	h := &handle{driver: &Driver{socket: path}}
	_, err = h.Shell(ctx, runtime.ShellSpec{}, input, io.Discard, nil)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("interrupted executor connection did not close")
	}
}
