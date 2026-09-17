package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/host"
	"github.com/joshjon/fletcher/internal/peer"
)

type managementAuth struct{ revoked bool }

func (a *managementAuth) AuthenticateToken(_ context.Context, token string) (peer.Peer, error) {
	if token != "test-token" || a.revoked {
		return peer.Peer{}, errors.New("invalid token")
	}
	return peer.Peer{ID: "peer_test"}, nil
}

func TestManagementRequiresTokenAndBlocksServerKeyExport(t *testing.T) {
	auth := &managementAuth{}
	calls := 0
	handler := authMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/fletcher.v1.HostService/GetHost", nil)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	require.Equal(t, http.StatusUnauthorized, out.Code)
	req.Header.Set("Authorization", "Bearer test-token")
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code)
	req.URL.Path = fletcherv1connect.PeerServiceServerConfigProcedure
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	require.Equal(t, http.StatusForbidden, out.Code)
	auth.revoked = true
	req.URL.Path = "/fletcher.v1.HostService/GetHost"
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	require.Equal(t, http.StatusUnauthorized, out.Code)
	require.Equal(t, 1, calls)
}

type peerListBackend struct{ api.PeersBackend }

func (peerListBackend) List(context.Context, int32, int32) ([]peer.Peer, error) {
	return []peer.Peer{{ID: "peer_test", Name: "phone"}}, nil
}

func TestPeerListIdentifiesAuthenticatedCaller(t *testing.T) {
	_, handler := fletcherv1connect.NewPeerServiceHandler(api.NewPeersService(peerListBackend{}, nil, nil))
	server := httptest.NewServer(authMiddleware(&managementAuth{}, handler))
	defer server.Close()
	client := fletcherv1connect.NewPeerServiceClient(server.Client(), server.URL)
	req := connect.NewRequest(&fletcherv1.ListPeersRequest{})
	req.Header().Set("Authorization", "Bearer test-token")
	got, err := client.ListPeers(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "peer_test", got.Msg.GetCurrentPeerId())
}

func TestHostRPCRejectsInvalidLogLimitsAndUnsupervisedRestart(t *testing.T) {
	manager := host.NewManager(t.Context(), host.Options{StartedAt: 42, Logs: &host.Logs{}})
	path, handler := fletcherv1connect.NewHostServiceHandler(api.NewHostService(manager), connect.WithInterceptors(api.ErrorInterceptor(slog.New(slog.NewTextHandler(io.Discard, nil)))))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(authMiddleware(&managementAuth{}, mux))
	defer server.Close()
	client := fletcherv1connect.NewHostServiceClient(server.Client(), server.URL)
	req := connect.NewRequest(&fletcherv1.GetHostRequest{})
	req.Header().Set("Authorization", "Bearer test-token")
	got, err := client.GetHost(t.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, 42, got.Msg.GetStartedAt())
	require.False(t, got.Msg.GetCanRestart())
	bad := connect.NewRequest(&fletcherv1.GetDaemonLogsRequest{Lines: 1001})
	bad.Header().Set("Authorization", "Bearer test-token")
	_, err = client.GetDaemonLogs(t.Context(), bad)
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	restart := connect.NewRequest(&fletcherv1.RestartDaemonRequest{RequestId: "request_1234567890", ExpectedStartedAt: 42})
	restart.Header().Set("Authorization", "Bearer test-token")
	_, err = client.RestartDaemon(t.Context(), restart)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
}
