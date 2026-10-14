package api_test

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/runtime"
)

type shellBackend struct {
	api.SessionsBackend
	spec chan runtime.ShellSpec
}

func (s shellBackend) Shell(_ context.Context, _ string, spec runtime.ShellSpec, _ io.Reader, stdout io.Writer, _ <-chan runtime.WinSize) (int32, error) {
	s.spec <- spec
	_, err := io.WriteString(stdout, "terminal")
	return 0, err
}

func TestShellRPCForwardsAndValidatesTarget(t *testing.T) {
	f := shellBackend{spec: make(chan runtime.ShellSpec, 1)}
	_, handler := fletcherv1connect.NewSessionServiceHandler(api.NewSessionsService(f, api.SessionsDeps{}))
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	client := fletcherv1connect.NewSessionServiceClient(srv.Client(), srv.URL, connect.WithGRPC())
	stream := client.ShellSession(t.Context())
	require.NoError(t, stream.Send(&fletcherv1.ShellSessionRequest{Msg: &fletcherv1.ShellSessionRequest_Start{Start: &fletcherv1.ShellStart{
		Ref: "dev", Term: "xterm", Cols: 80, Rows: 24, WorkDir: "/workspace/app", TerminalName: "project_123-build", ControlMode: true,
	}}}))
	require.NoError(t, stream.CloseRequest())
	response, err := stream.Receive()
	require.NoError(t, err)
	require.Equal(t, "terminal", string(response.GetData()))
	_, err = stream.Receive()
	require.NoError(t, err)
	require.NoError(t, stream.CloseResponse())
	spec := <-f.spec
	require.Equal(t, "/workspace/app", spec.WorkDir)
	require.Equal(t, "project_123-build", spec.TerminalName)
	require.True(t, spec.ControlMode)
	invalid := client.ShellSession(t.Context())
	require.NoError(t, invalid.Send(&fletcherv1.ShellSessionRequest{Msg: &fletcherv1.ShellSessionRequest_Start{Start: &fletcherv1.ShellStart{
		Ref: "dev", WorkDir: "relative", TerminalName: "main",
	}}}))
	require.NoError(t, invalid.CloseRequest())
	_, err = invalid.Receive()
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	require.NoError(t, invalid.CloseResponse())
	require.Empty(t, f.spec)
}
