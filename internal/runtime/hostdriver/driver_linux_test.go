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

type terminalOutput struct {
	mu      sync.Mutex
	text    strings.Builder
	changed chan struct{}
}

func (w *terminalOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.text.Write(p)
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (w *terminalOutput) wait(t *testing.T, text string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		w.mu.Lock()
		matched := strings.Contains(w.text.String(), text)
		w.mu.Unlock()
		if matched {
			return
		}
		select {
		case <-w.changed:
		case <-timer.C:
			t.Fatalf("terminal did not produce expected output %q", text)
		}
	}
}

func TestDriverTerminalRoundTrip(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is required for the optional host terminal test")
	}
	s, spec := testExecutor(t)
	s.tmux = tmux
	path := filepath.Join(s.dir, "executor.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	serverDone := make(chan error, 1)
	background.GoNamed(ctx, "test.host.executor", func(ctx context.Context) { serverDone <- s.serveConnections(ctx, listener, uint32(os.Geteuid())) })
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer done()
		require.NoError(t, s.stop(cleanup, spec.SessionID))
		cancel()
		select {
		case err := <-serverDone:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("executor did not shut down")
		}
	})
	d := &Driver{account: spec.Account, socket: path}
	account, err := d.HostAccount(ctx)
	require.NoError(t, err)
	require.Equal(t, spec.Account, account)
	wrong := *d
	wrong.account.UID++
	_, err = wrong.HostAccount(ctx)
	require.ErrorContains(t, err, "account changed")
	h, err := d.StartHostSession(ctx, spec)
	require.NoError(t, err)
	listing, err := h.ListDir(ctx, "")
	require.NoError(t, err)
	require.Equal(t, spec.WorkDir, listing.Path)
	endCtx, endCancel := context.WithTimeout(ctx, 5*time.Second)
	code, err := h.Shell(endCtx, runtime.ShellSpec{TerminalName: "eof", ControlMode: true}, strings.NewReader(""), io.Discard, nil)
	endCancel()
	require.NoError(t, err)
	require.Zero(t, code)

	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	output := &terminalOutput{changed: make(chan struct{}, 1)}
	shellDone := make(chan error, 1)
	shellCtx, detach := context.WithCancel(ctx)
	defer detach()
	background.GoNamed(shellCtx, "test.host.shell", func(ctx context.Context) {
		_, err := h.Shell(ctx, runtime.ShellSpec{TerminalName: "roundtrip", ControlMode: true, Cols: 100, Rows: 30}, input, output, nil)
		shellDone <- err
	})
	output.wait(t, "%session-changed")
	// The attached pane supplies its cwd without injecting a pwd command into
	// the user's shell. New tabs and splits use the same control command.
	_, err = io.WriteString(writer, "display-message -p 'FLETCHER_CWD=#{pane_current_path}'\n")
	require.NoError(t, err)
	output.wait(t, "FLETCHER_CWD="+spec.WorkDir)
	_, err = io.WriteString(writer, "display-message -p 'FLETCHER_UTF8=\u2603'\n")
	require.NoError(t, err)
	output.wait(t, "FLETCHER_UTF8=\u2603")
	_, err = io.WriteString(writer, "send-keys 'printf \"%s%s\\n\" FLETCHER_ HOST_ROUNDTRIP' Enter\n")
	require.NoError(t, err)
	output.wait(t, "FLETCHER_HOST_ROUNDTRIP")
	detach()
	require.NoError(t, writer.Close())
	select {
	case err := <-shellDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not cancel")
	}
	require.Contains(t, tmuxOutput(t, s, spec.SessionID, "list-sessions", "-F", "#{session_name}"), "roundtrip")
	require.NoError(t, h.Stop(ctx))
	_, err = h.ListDir(ctx, "")
	require.ErrorContains(t, err, "stopped")
}
