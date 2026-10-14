//go:build linux

package firecrackerdriver

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

func TestGuestStatsCancelWhenGuestNeverReplies(t *testing.T) {
	var lc net.ListenConfig
	path := filepath.Join(t.TempDir(), "vsock.sock")
	ln, err := lc.Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	defer ln.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &fcSession{vsockUDS: path}
	done := make(chan error, 1)
	go func() { _, err := s.stat(ctx); done <- err }()
	conn, err := ln.Accept()
	require.NoError(t, err)
	defer conn.Close()
	_, err = readLine(conn)
	require.NoError(t, err)
	_, err = fmt.Fprintln(conn, "OK 1")
	require.NoError(t, err)
	request, err := guestproto.ReadRequest(conn)
	require.NoError(t, err)
	require.Equal(t, guestproto.RequestStat, request.Kind)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("guest stat remained blocked after cancellation")
	}
}
