package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
)

type hostSessionsBackend struct {
	fakeSessionsBackend
	created session.CreateParams
}

func (b *hostSessionsBackend) HostAccount(context.Context) (runtime.HostAccount, error) {
	return b.sess.HostAccount, nil
}

func (b *hostSessionsBackend) Create(_ context.Context, p session.CreateParams) (session.Session, error) {
	b.created = p
	return b.sess, nil
}

func TestHostSessionAPIContract(t *testing.T) {
	account := runtime.HostAccount{User: "operator", UID: 1000, Home: "/home/operator"}
	backend := &hostSessionsBackend{fakeSessionsBackend: fakeSessionsBackend{sess: session.Session{
		ID: "session_test", ExecutionKind: session.ExecutionHost, HostAccount: account, WorkingDir: account.Home,
	}}}
	svc := api.NewSessionsService(backend, api.SessionsDeps{})
	info, err := svc.GetHostSessionAccount(t.Context(), connect.NewRequest(&fletcherv1.GetHostSessionAccountRequest{}))
	require.NoError(t, err)
	require.Equal(t, account.User, info.Msg.GetAccount().GetUser())
	require.Equal(t, account.UID, info.Msg.GetAccount().GetUid())
	require.NotEmpty(t, info.Msg.GetMachine())
	response, err := svc.CreateSession(t.Context(), connect.NewRequest(&fletcherv1.CreateSessionRequest{
		Name: "host", ExecutionKind: "host", HostUser: account.User, HostUid: account.UID, WorkingDir: account.Home,
	}))
	require.NoError(t, err)
	require.Equal(t, session.CreateParams{Name: "host", ExecutionKind: "host", HostUser: account.User, HostUID: account.UID, WorkingDir: account.Home}, backend.created)
	require.Equal(t, "host", response.Msg.Session.ExecutionKind)
	require.Equal(t, account.User, response.Msg.Session.HostAccount.User)
	require.Equal(t, account.Home, response.Msg.Session.WorkingDir)
	require.Empty(t, response.Msg.Session.EgressPolicy)
	_, err = api.NewSessionsService(fakeSessionsBackend{}, api.SessionsDeps{}).GetHostSessionAccount(t.Context(), connect.NewRequest(&fletcherv1.GetHostSessionAccountRequest{}))
	require.Error(t, err)
}

func TestHostRedeployRefusesBeforeImageImport(t *testing.T) {
	for _, target := range []string{"", "base", "registry.example.com/app:latest"} {
		var imports, redeploys []string
		calls := 0
		svc := api.NewSessionsService(fakeSessionsBackend{
			sess: session.Session{ExecutionKind: session.ExecutionHost}, redeploys: &redeploys,
		}, api.SessionsDeps{Refresher: fakeRefresher{calls: &calls, imports: &imports, templates: []string{"base"}}})
		_, err := svc.RedeploySession(t.Context(), connect.NewRequest(&fletcherv1.RedeploySessionRequest{Ref: "host", Image: target}))
		require.Error(t, err)
		require.Empty(t, imports)
		require.Empty(t, redeploys)
		require.Zero(t, calls)
	}
}
