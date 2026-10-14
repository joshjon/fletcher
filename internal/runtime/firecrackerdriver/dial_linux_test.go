//go:build linux

package firecrackerdriver

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVsockHandshakeCancelsWithoutGuestReply(t *testing.T) {
	var lc net.ListenConfig
	path := filepath.Join(t.TempDir(), "vsock.sock")
	ln, err := lc.Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := dialGuest(ctx, path, 1); result <- err }()
	conn, err := ln.Accept()
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = readLine(conn)
	require.NoError(t, err)
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestVsockControlStreamClosesOnCancellation(t *testing.T) {
	var lc net.ListenConfig
	path := filepath.Join(t.TempDir(), "vsock.sock")
	ln, err := lc.Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, err := dialGuest(ctx, path, 1)
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_, err = io.Copy(io.Discard, conn)
		result <- err
	}()
	conn, err := ln.Accept()
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = readLine(conn)
	require.NoError(t, err)
	_, err = fmt.Fprintln(conn, "OK 1")
	require.NoError(t, err)
	cancel()
	require.Error(t, <-result)
}
