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

func TestCheckJobRuntimeOK(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "firecracker", Snapshot: "ext4", BaseImageAvailable: true,
	})
	res := CheckJobRuntime(sock).Check(context.Background())
	require.Equal(t, StatusOK, res.Status)
	require.Nil(t, res.Plan)
	require.Contains(t, res.Detail, "firecracker")
}

func TestCheckJobRuntimeMockWarns(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "mock", Snapshot: "mock", BaseImageAvailable: true,
	})
	res := CheckJobRuntime(sock).Check(context.Background())
	require.Equal(t, StatusWarn, res.Status)
	require.NotNil(t, res.Plan)
	require.Equal(t, "real-runtime", res.Plan.ID)
}

// CheckJobRuntime no longer cares about the base image: a healthy runtime with
// no image imported is still a healthy runtime (CheckBaseImage owns that).
func TestCheckJobRuntimeOKWithoutBaseImage(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "firecracker", Snapshot: "ext4", BaseImageAvailable: false,
	})
	res := CheckJobRuntime(sock).Check(context.Background())
	require.Equal(t, StatusOK, res.Status)
	require.Nil(t, res.Plan)
}

func TestCheckJobRuntimeSkipsWhenDaemonDown(t *testing.T) {
	res := CheckJobRuntime("/tmp/fletcher-nonexistent-rt.sock").Check(context.Background())
	require.Equal(t, StatusSkip, res.Status)
	require.Nil(t, res.Plan)
}

func TestCheckBaseImageOK(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "firecracker", Snapshot: "ext4", BaseImageAvailable: true,
	})
	res := CheckBaseImage(sock).Check(context.Background())
	require.Equal(t, StatusOK, res.Status)
	require.Nil(t, res.Plan)
}

func TestCheckBaseImageNoImageFails(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "firecracker", Snapshot: "ext4", BaseImageAvailable: false,
	})
	res := CheckBaseImage(sock).Check(context.Background())
	// A missing base image blocks all job/session creation: Fail status and a
	// blocker plan, kept consistent so the summary and the plan agree.
	require.Equal(t, StatusFail, res.Status)
	require.NotNil(t, res.Plan)
	require.Equal(t, "import-base-image", res.Plan.ID)
	require.Equal(t, PriorityBlocker, res.Plan.Priority)
}

func TestCheckBaseImageUpdateWarns(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "firecracker", Snapshot: "ext4",
		BaseImageAvailable: true, BaseImageUpdateAvailable: true,
	})
	res := CheckBaseImage(sock).Check(context.Background())
	require.Equal(t, StatusWarn, res.Status)
	require.NotNil(t, res.Plan)
	require.Equal(t, "update-base-image", res.Plan.ID)
	require.Equal(t, PriorityFollowup, res.Plan.Priority)
}

// The mock snapshot driver clones no real template, so a base image is not
// required and the check skips rather than flagging a phantom blocker.
func TestCheckBaseImageSkipsForMockSnapshot(t *testing.T) {
	sock := serveRuntimeAdmin(t, &fletcherv1.HealthResponse{
		Status: "ok", Runtime: "mock", Snapshot: "mock", BaseImageAvailable: false,
	})
	res := CheckBaseImage(sock).Check(context.Background())
	require.Equal(t, StatusSkip, res.Status)
	require.Nil(t, res.Plan)
}

func TestCheckBaseImageSkipsWhenDaemonDown(t *testing.T) {
	res := CheckBaseImage("/tmp/fletcher-nonexistent-rt.sock").Check(context.Background())
	require.Equal(t, StatusSkip, res.Status)
	require.Nil(t, res.Plan)
}
