package doctor

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// fakeAdmin serves a canned Health response with a fixed public endpoint.
type fakeAdmin struct {
	fletcherv1connect.UnimplementedAdminServiceHandler
	endpoint string
}

func (f fakeAdmin) Health(context.Context, *connect.Request[fletcherv1.HealthRequest]) (*connect.Response[fletcherv1.HealthResponse], error) {
	return connect.NewResponse(&fletcherv1.HealthResponse{Status: "ok", PublicEndpoint: f.endpoint}), nil
}

// serveFakeAdmin starts an AdminService over a Unix socket and returns its
// path. /tmp keeps sun_path within the 104-byte limit on macOS and Linux.
func serveFakeAdmin(t *testing.T, endpoint string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fl-doc-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "f.sock")

	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	mux := http.NewServeMux()
	path, h := fletcherv1connect.NewAdminServiceHandler(fakeAdmin{endpoint: endpoint})
	mux.Handle(path, h)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestCheckPublicEndpointReportsConfiguredEndpoint(t *testing.T) {
	sock := serveFakeAdmin(t, "vpn.example.com:51820")
	res := CheckPublicEndpoint(sock).Check(context.Background())
	require.Equal(t, StatusOK, res.Status)
	require.Nil(t, res.Plan)
	require.Contains(t, res.Detail, "vpn.example.com:51820")
}

func TestCheckPublicEndpointEmitsRestartPlanWhenEmpty(t *testing.T) {
	sock := serveFakeAdmin(t, "")
	res := CheckPublicEndpoint(sock).Check(context.Background())
	require.Equal(t, StatusFail, res.Status)
	require.NotNil(t, res.Plan)
	// Shares the UPnP check's plan ID so the two collapse into one step.
	require.Equal(t, "configure-endpoint", res.Plan.ID)
	require.Equal(t, PriorityBlocker, res.Plan.Priority)
}

func TestCheckPublicEndpointSkipsWhenDaemonUnreachable(t *testing.T) {
	res := CheckPublicEndpoint("/tmp/fletcher-nonexistent-doctor.sock").Check(context.Background())
	require.Equal(t, StatusSkip, res.Status)
	require.Nil(t, res.Plan)
}
