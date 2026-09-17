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
	dataDir := t.TempDir()
	cfg := daemon.Config{
		SocketPath:        shortSocketPath(t),
		DatabasePath:      filepath.Join(dataDir, "fletcher.db"),
		LogLevel:          "warn",
		GatewayListenAddr: "127.0.0.1:0", // random free port for the test
		MCPListenAddr:     "127.0.0.1:0",
		AgeIdentityPath:   filepath.Join(dataDir, "age.key"),
		// No router discovery in a health/shutdown test: skip UPnP/NAT-PMP so
		// boot does not spend the endpoint-derivation retry window probing a
		// gateway that is not there.
		DisableUPnP: true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	waitForSocket(t, cfg.SocketPath, done)

	client := newAdminClient(cfg.SocketPath)
	resp, err := client.Health(ctx, connect.NewRequest(&fletcherv1.HealthRequest{}))
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Msg.GetStatus())
	require.NotZero(t, resp.Msg.GetStartedAt())

	settingsClient := fletcherv1connect.NewSettingsServiceClient(testHTTPClient(cfg.SocketPath), "http://unix")
	reloaded, err := settingsClient.ReloadSettings(ctx, connect.NewRequest(&fletcherv1.ReloadSettingsRequest{}))
	require.NoError(t, err)
	require.Empty(t, reloaded.Msg.GetPendingRestart(), "runtime discovery must not create restart drift")
	_, err = settingsClient.SetSetting(ctx, connect.NewRequest(&fletcherv1.SetSettingRequest{Key: "default_image", Value: "custom"}))
	require.NoError(t, err)
	_, err = settingsClient.DeleteSetting(ctx, connect.NewRequest(&fletcherv1.DeleteSettingRequest{Key: "default_image"}))
	require.NoError(t, err)
	listed, err := settingsClient.ListSettings(ctx, connect.NewRequest(&fletcherv1.ListSettingsRequest{}))
	require.NoError(t, err)
	for _, setting := range listed.Msg.GetSettings() {
		if setting.GetKey() == "default_image" {
			require.Equal(t, "fletcher-base", setting.GetValue())
		}
	}

	hostClient := fletcherv1connect.NewHostServiceClient(testHTTPClient(cfg.SocketPath), "http://unix")
	hostInfo, err := hostClient.GetHost(ctx, connect.NewRequest(&fletcherv1.GetHostRequest{}))
	require.NoError(t, err)
	require.Equal(t, resp.Msg.GetStartedAt(), hostInfo.Msg.GetStartedAt())
	storage, err := hostClient.GetStorage(ctx, connect.NewRequest(&fletcherv1.GetStorageRequest{}))
	require.NoError(t, err)
	require.NotZero(t, storage.Msg.GetTotalBytes())
	require.NotEmpty(t, storage.Msg.GetCategories())

	// The socket must be group-accessible (0660), not owner-only: under
	// systemd the daemon runs as fletcher:fletcher and the operator reaches
	// it via fletcher-group membership. A 0600 socket would deny every group
	// member regardless of membership.
	info, err := os.Stat(cfg.SocketPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o660), info.Mode().Perm())

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

func testHTTPClient(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

func newAdminClient(socket string) fletcherv1connect.AdminServiceClient {
	return fletcherv1connect.NewAdminServiceClient(testHTTPClient(socket), "http://unix")
}
