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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/runtime"
)

const testSessionID = "session_00000000000000000000000001"

func testExecutor(t *testing.T) (*executor, runtime.HostSessionSpec) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fh-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux.conf"), []byte("set-option -g history-limit 50000\n"), 0o600))
	home := t.TempDir()
	account := runtime.HostAccount{User: "operator", UID: 1000, Home: home}
	s := &executor{account: account, shell: "/bin/sh", dir: dir, sessions: make(map[string]*hostSession)}
	spec := runtime.HostSessionSpec{SessionID: testSessionID, Account: account, WorkDir: home}
	return s, spec
}

func TestHostAuthorization(t *testing.T) {
	for _, name := range []string{"root", "../operator", "operator/other", "--user", ""} {
		_, err := New(name)
		require.Error(t, err)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "peer.sock"))
	require.NoError(t, err)
	defer listener.Close()
	client, err := net.Dial("unix", listener.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	server, err := listener.Accept()
	require.NoError(t, err)
	defer server.Close()
	require.True(t, peerAllowed(server, uint32(os.Geteuid())))
	require.False(t, peerAllowed(server, uint32(os.Geteuid())+1))
}

func TestHostSessionValidationAndEnvironment(t *testing.T) {
	s, spec := testExecutor(t)
	t.Setenv("FLETCHER_TOKEN", "test-must-not-leak")
	t.Setenv("ANTHROPIC_API_KEY", "test-must-not-leak")
	require.NotContains(t, strings.Join(s.env("xterm"), "\n"), "test-must-not-leak")
	require.NoError(t, s.start(t.Context(), spec))
	require.NoError(t, s.start(t.Context(), spec))
	changed := spec
	changed.WorkDir = t.TempDir()
	require.ErrorContains(t, s.start(t.Context(), changed), "existing terminals")
	changed.SessionID = "session_00000000000000000000000002"
	changed.WorkDir = "relative"
	require.Error(t, s.start(t.Context(), changed))
	changed.WorkDir = filepath.Join(t.TempDir(), "missing")
	require.Error(t, s.start(t.Context(), changed))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	left, right := net.Pipe()
	defer left.Close()
	background.GoNamed(ctx, "test.host.invalid-account", func(ctx context.Context) { defer right.Close(); s.serve(ctx, right) })
	spec.Account.UID++
	require.NoError(t, writePacket(left, request{Version: protocolVersion, Op: "start", Session: spec}))
	var reply packet
	require.NoError(t, readPacket(left, &reply))
	require.Contains(t, reply.Error, "account or identity")
}

func TestExecutorRejectsUnknownProtocolAndSessionIdentity(t *testing.T) {
	s, spec := testExecutor(t)
	for _, test := range []struct {
		name    string
		req     request
		message string
	}{
		{name: "old protocol", req: request{Op: "start", Session: spec}, message: "protocol mismatch"},
		{name: "future protocol", req: request{Version: protocolVersion + 1, Op: "info"}, message: "protocol mismatch"},
		{name: "path traversal", req: request{Version: protocolVersion, Op: "start", Session: runtime.HostSessionSpec{SessionID: "../escape", Account: spec.Account}}, message: "identity is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			background.GoNamed(t.Context(), "test.host.reject", func(ctx context.Context) {
				defer right.Close()
				s.serve(ctx, right)
			})
			require.NoError(t, left.SetDeadline(time.Now().Add(time.Second)))
			require.NoError(t, writePacket(left, test.req))
			var reply packet
			require.NoError(t, readPacket(left, &reply))
			require.Contains(t, reply.Error, test.message)
		})
	}
	require.Empty(t, s.sessions)
}

func TestDirectoryBrowsing(t *testing.T) {
	s, _ := testExecutor(t)
	require.NoError(t, os.Mkdir(filepath.Join(s.account.Home, "folder"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(s.account.Home, "file"), []byte("retained"), 0o600))
	require.NoError(t, os.Symlink("folder", filepath.Join(s.account.Home, "link")))
	listing, err := s.list("")
	require.NoError(t, err)
	require.Len(t, listing.Entries, 3)
	require.Equal(t, "folder", listing.Entries[0].Name)
	require.Equal(t, "link", listing.Entries[1].Name)
	require.True(t, listing.Entries[1].IsSymlink)
	require.True(t, listing.Entries[1].IsDir)
	resolved, err := s.list(filepath.Join(s.account.Home, "link"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(s.account.Home, "folder"), resolved.Path)
	_, err = s.list("relative")
	require.Error(t, err)
	_, err = s.list(filepath.Join(s.account.Home, "missing"))
	require.Error(t, err)
}

func TestHostTerminalsDetachAndStopOnlyTheirOwnServer(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is required for the optional host terminal test")
	}
	s, spec := testExecutor(t)
	s.tmux = tmux
	other := spec
	other.SessionID = "session_00000000000000000000000002"
	require.NoError(t, s.start(t.Context(), spec))
	require.NoError(t, s.start(t.Context(), other))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, s.stop(ctx, spec.SessionID))
		require.NoError(t, s.stop(ctx, other.SessionID))
	})
	shell := runtime.ShellSpec{TerminalName: "main", Cols: 80, Rows: 24, ControlMode: true}
	require.NoError(t, s.ensureTerminal(t.Context(), s.sessions[spec.SessionID], "main", spec.WorkDir, shell))
	require.NoError(t, s.ensureTerminal(t.Context(), s.sessions[other.SessionID], "main", other.WorkDir, shell))
	// Closing the attachment must not end the server or change its pane.
	pane := tmuxOutput(t, s, spec.SessionID, "display-message", "-p", "-t", "=main:", "#{pane_id}")
	left, right := net.Pipe()
	done := make(chan struct{})
	background.GoNamed(t.Context(), "test.host.terminal", func(ctx context.Context) {
		defer close(done)
		defer right.Close()
		s.shellSession(ctx, right, s.sessions[spec.SessionID], shell)
	})
	require.NoError(t, left.SetDeadline(time.Now().Add(5*time.Second)))
	var ready packet
	require.NoError(t, readPacket(left, &ready))
	require.Equal(t, "ready", ready.Kind)
	require.NoError(t, left.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal attachment did not cancel")
	}
	require.Equal(t, pane, tmuxOutput(t, s, spec.SessionID, "display-message", "-p", "-t", "=main:", "#{pane_id}"))
	file := filepath.Join(spec.WorkDir, "keep")
	require.NoError(t, os.WriteFile(file, []byte("keep"), 0o600))
	stopped := s.sessions[spec.SessionID]
	require.NoError(t, s.stop(t.Context(), spec.SessionID))
	require.NoError(t, s.stop(t.Context(), spec.SessionID))
	require.NotContains(t, s.sessions, spec.SessionID)
	require.NotEmpty(t, tmuxOutput(t, s, other.SessionID, "display-message", "-p", "-t", "=main:", "#{pane_id}"))
	_, err = os.Stat(file)
	require.NoError(t, err)
	require.Error(t, s.ensureTerminal(t.Context(), stopped, "later", spec.WorkDir, shell))
}

func TestHostStopConfirmsListenerShutdown(t *testing.T) {
	for _, command := range []string{"true", "false"} {
		t.Run(command, func(t *testing.T) {
			s, spec := testExecutor(t)
			var err error
			s.tmux, err = exec.LookPath(command)
			require.NoError(t, err)
			require.NoError(t, s.start(t.Context(), spec))
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.socket(spec.SessionID), Net: "unix"})
			require.NoError(t, err)
			defer listener.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			background.GoNamed(ctx, "test.host.stop-listener", func(ctx context.Context) {
				done <- s.stop(ctx, spec.SessionID)
			})
			require.NoError(t, listener.SetDeadline(time.Now().Add(time.Second)))
			probe, err := listener.Accept()
			require.NoError(t, err, "Stop must confirm socket shutdown even when kill-server succeeds")
			require.NoError(t, probe.Close())
			select {
			case err := <-done:
				t.Fatalf("Stop returned while the terminal socket was still live: %v", err)
			default:
			}
			require.NoError(t, listener.Close())
			select {
			case err := <-done:
				require.NoError(t, err, "an exiting server is stopped even if a repeated kill-server returned an error")
			case <-ctx.Done():
				t.Fatal("Stop did not observe terminal socket shutdown")
			}
			require.NotContains(t, s.sessions, spec.SessionID)
		})
	}
}

func TestHostStopRetainsNamespaceWhenListenerSurvives(t *testing.T) {
	s, spec := testExecutor(t)
	var err error
	s.tmux, err = exec.LookPath("false")
	require.NoError(t, err)
	require.NoError(t, s.start(t.Context(), spec))
	listener, err := net.Listen("unix", s.socket(spec.SessionID))
	require.NoError(t, err)
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = s.stop(ctx, spec.SessionID)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, s.sessions, spec.SessionID, "unconfirmed cleanup must retain its namespace")
}

func tmuxOutput(t *testing.T, s *executor, id string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.tmux, append([]string{"-S", s.socket(id)}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func TestHostHandshakeCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer listener.Close()
	done := make(chan struct{})
	background.GoNamed(t.Context(), "test.host.blocked", func(context.Context) {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	d := &Driver{socket: path}
	_, err = d.HostAccount(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("host handshake did not close on cancellation")
	}
}
