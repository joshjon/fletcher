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

type fakeRuntimeAdmin struct {
	fletcherv1connect.UnimplementedAdminServiceHandler
	resp *fletcherv1.HealthResponse
}

func (f fakeRuntimeAdmin) Health(context.Context, *connect.Request[fletcherv1.HealthRequest]) (*connect.Response[fletcherv1.HealthResponse], error) {
	return connect.NewResponse(f.resp), nil
}

func serveRuntimeAdmin(t *testing.T, resp *fletcherv1.HealthResponse) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fl-doc-rt-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "f.sock")

	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	mux := http.NewServeMux()
	path, h := fletcherv1connect.NewAdminServiceHandler(fakeRuntimeAdmin{resp: resp})
	mux.Handle(path, h)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestCheckRuntimeReadyOK(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "firecracker", Snapshot: "ext4", BaseImageAvailable: true,
	})
	res := CheckRuntimeReady(sock).Check(context.Background())
	require.Equal(t, StatusOK, res.Status)
	require.Nil(t, res.Plan)
	require.Contains(t, res.Detail, "firecracker")
}

func TestCheckRuntimeReadyMockWarns(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "mock", Snapshot: "mock", BaseImageAvailable: true,
	})
	res := CheckRuntimeReady(sock).Check(context.Background())
	require.Equal(t, StatusWarn, res.Status)
	require.NotNil(t, res.Plan)
	require.Equal(t, "real-runtime", res.Plan.ID)
}

func TestCheckRuntimeReadyNoBaseImageWarns(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "firecracker", Snapshot: "ext4", BaseImageAvailable: false,
	})
	res := CheckRuntimeReady(sock).Check(context.Background())
	require.Equal(t, StatusWarn, res.Status)
	require.NotNil(t, res.Plan)
	require.Equal(t, "import-base-image", res.Plan.ID)
	require.Equal(t, PriorityBlocker, res.Plan.Priority)
}

func TestCheckRuntimeReadySkipsWhenDaemonDown(t *testing.T) {
	res := CheckRuntimeReady("/tmp/fletcher-nonexistent-rt.sock").Check(context.Background())
	require.Equal(t, StatusSkip, res.Status)
	require.Nil(t, res.Plan)
}
