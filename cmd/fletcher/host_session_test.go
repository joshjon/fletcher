package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

type cliHostSessions struct {
	fletcherv1connect.UnimplementedSessionServiceHandler
	created    chan *fletcherv1.CreateSessionRequest
	discovered atomic.Int64
}

func (f *cliHostSessions) GetHostSessionAccount(context.Context, *connect.Request[fletcherv1.GetHostSessionAccountRequest]) (*connect.Response[fletcherv1.GetHostSessionAccountResponse], error) {
	f.discovered.Add(1)
	return connect.NewResponse(&fletcherv1.GetHostSessionAccountResponse{Machine: "linux-host", Account: &fletcherv1.HostSessionAccount{User: "operator", Uid: 1000, Home: "/home/operator"}}), nil
}

func (f *cliHostSessions) CreateSession(_ context.Context, req *connect.Request[fletcherv1.CreateSessionRequest]) (*connect.Response[fletcherv1.CreateSessionResponse], error) {
	f.created <- req.Msg
	return connect.NewResponse(&fletcherv1.CreateSessionResponse{Session: &fletcherv1.Session{Id: "session_test", ExecutionKind: req.Msg.ExecutionKind}}), nil
}

func TestHostSessionCLIExplicitSelection(t *testing.T) {
	backend := &cliHostSessions{created: make(chan *fletcherv1.CreateSessionRequest, 2)}
	_, handler := fletcherv1connect.NewSessionServiceHandler(backend)
	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	defer server.Close()
	run := func(args ...string) {
		captureStdout(t, func() {
			command := append([]string{"fletcher", "--remote", strings.TrimPrefix(server.URL, "http://"), "--token", "test", "session", "create", "--name", "work", "-o", "json"}, args...)
			require.NoError(t, newApp().Run(t.Context(), command))
		})
	}
	run()
	vm := <-backend.created
	require.Empty(t, vm.ExecutionKind)
	require.Empty(t, vm.HostUser)
	require.Zero(t, backend.discovered.Load())
	run("--host", "--workdir", "/home/operator/repo")
	host := <-backend.created
	require.Equal(t, "host", host.ExecutionKind)
	require.Equal(t, "operator", host.HostUser)
	require.EqualValues(t, 1000, host.HostUid)
	require.Equal(t, "/home/operator/repo", host.WorkingDir)
	require.Empty(t, host.Image)
	require.Empty(t, host.EgressPolicy)
	require.Empty(t, host.Gateway)
	require.EqualValues(t, 1, backend.discovered.Load())
}
