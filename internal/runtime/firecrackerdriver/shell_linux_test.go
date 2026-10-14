//go:build linux

package firecrackerdriver

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	fcruntime "github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

func TestNamedShellAcknowledgementCanBeCancelled(t *testing.T) {
	var lc net.ListenConfig
	path := filepath.Join(t.TempDir(), "vsock.sock")
	ln, err := lc.Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	defer ln.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &fcSession{vsockUDS: path}
	done := make(chan error, 1)
	go func() {
		_, err := s.Shell(ctx, fcruntime.ShellSpec{TerminalName: "project_123-main"}, strings.NewReader(""), &bytes.Buffer{}, nil)
		done <- err
	}()
	conn, err := ln.Accept()
	require.NoError(t, err)
	defer conn.Close()
	_, err = readLine(conn)
	require.NoError(t, err)
	_, err = fmt.Fprintln(conn, "OK 1")
	require.NoError(t, err)
	_, err = guestproto.ReadRequest(conn)
	require.NoError(t, err)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("named terminal acknowledgement remained blocked after cancellation")
	}
}

func TestShellRequestSelectsCompatibleGuestProtocol(t *testing.T) {
	for _, tt := range []struct {
		name string
		spec fcruntime.ShellSpec
		kind guestproto.RequestKind
		code int32
	}{
		{name: "legacy", spec: fcruntime.ShellSpec{Term: "xterm"}, kind: guestproto.RequestShell},
		{name: "project", spec: fcruntime.ShellSpec{Term: "xterm", WorkDir: "/workspace/app", TerminalName: "project_123-main", ControlMode: true}, kind: guestproto.RequestShellTarget},
		{name: "older guest rejects target", spec: fcruntime.ShellSpec{TerminalName: "project_123-main"}, kind: guestproto.RequestShellTarget, code: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var lc net.ListenConfig
			path := filepath.Join(t.TempDir(), "vsock.sock")
			ln, err := lc.Listen(t.Context(), "unix", path)
			require.NoError(t, err)
			defer ln.Close()
			s := &fcSession{vsockUDS: path, env: []string{"BASE=1"}}
			var output bytes.Buffer
			type result struct {
				code int32
				err  error
			}
			done := make(chan result, 1)
			go func() {
				code, err := s.Shell(t.Context(), tt.spec, strings.NewReader(""), &output, nil)
				done <- result{code, err}
			}()
			conn, err := ln.Accept()
			require.NoError(t, err)
			defer conn.Close()
			_, err = readLine(conn)
			require.NoError(t, err)
			_, err = fmt.Fprintln(conn, "OK 1")
			require.NoError(t, err)
			req, err := guestproto.ReadRequest(conn)
			require.NoError(t, err)
			require.Equal(t, tt.kind, req.Kind)
			require.Equal(t, tt.spec.WorkDir, req.Shell.WorkDir)
			require.Equal(t, tt.spec.TerminalName, req.Shell.TerminalName)
			require.Equal(t, tt.spec.ControlMode, req.Shell.ControlMode)
			require.Equal(t, []string{"BASE=1"}, req.Shell.Env)
			if tt.code != 0 {
				// This is the actual unknown-request reply from older guests,
				// not a shell frame and not permission to fall back to main.
				require.NoError(t, guestproto.WriteError(conn, "guest does not support request shell_target"))
				res := <-done
				require.Error(t, res.err)
				require.Contains(t, res.err.Error(), "cold-restart")
				require.Empty(t, output.String())
				return
			}
			if tt.kind == guestproto.RequestShellTarget {
				require.NoError(t, guestproto.WriteShellReady(conn))
			}
			require.NoError(t, guestproto.WriteFrame(conn, guestproto.KindExit, guestproto.EncodeExit(tt.code)))
			res := <-done
			require.NoError(t, res.err)
			require.Equal(t, tt.code, res.code)
		})
	}
}
