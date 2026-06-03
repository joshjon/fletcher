package daemon_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/daemon"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// TestDaemonServesHealthAndShutsDownCleanly is the Phase 1 end-to-end check:
// start the daemon in-process, call Health over the Unix socket, then cancel
// and verify the socket is cleaned up.
func TestDaemonServesHealthAndShutsDownCleanly(t *testing.T) {
	cfg := daemon.Config{
		SocketPath:   shortSocketPath(t),
		DatabasePath: filepath.Join(t.TempDir(), "fletcher.db"),
		LogLevel:     "warn",
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	waitForSocket(t, cfg.SocketPath, done)

	client := newAdminClient(cfg.SocketPath)
	resp, err := client.Health(ctx, connect.NewRequest(&fletcherv1.HealthRequest{}))
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Msg.GetStatus())
	require.NotZero(t, resp.Msg.GetStartedAt())

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down within 5s")
	}

	_, err = os.Stat(cfg.SocketPath)
	require.True(t, os.IsNotExist(err), "socket file should be removed on shutdown, got: %v", err)
}

func waitForSocket(t *testing.T, path string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("daemon exited before listening: %v", err)
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %s never appeared", path)
}

// shortSocketPath returns a short-enough path for a Unix-domain socket. macOS
// caps sun_path at 104 bytes, and t.TempDir() under /var/folders is usually
// longer. /tmp keeps us inside the limit on both macOS and Linux.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fl-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "f.sock")
}

func newAdminClient(socket string) fletcherv1connect.AdminServiceClient {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
	return fletcherv1connect.NewAdminServiceClient(httpClient, "http://unix")
}
