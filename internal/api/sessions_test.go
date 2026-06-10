package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/session"
)

// fakeSessionsBackend stubs SessionsBackend by embedding the interface (nil) and
// overriding only Get - the single method GetSession needs. Any other call
// would nil-panic, which is fine: this test exercises GetSession alone.
type fakeSessionsBackend struct {
	api.SessionsBackend
	sess session.Session
}

func (f fakeSessionsBackend) Get(context.Context, string) (session.Session, error) {
	return f.sess, nil
}

type fakeDeployResolver struct {
	entrypoint []string
	port       int
	ok         bool
}

func (f fakeDeployResolver) DeployInfo(string) ([]string, int, bool) {
	return f.entrypoint, f.port, f.ok
}

// GetSession attaches deploy detail only for a run_app session with a resolver
// that has metadata; never for a bare session or with no resolver.
func TestGetSessionPopulatesDeployInfo(t *testing.T) {
	resolver := fakeDeployResolver{entrypoint: []string{"/app", "serve"}, port: 8080, ok: true}
	runApp := session.Session{ID: "s1", Name: "app", Image: "myapp", RunApp: true}
	get := func(svc *api.SessionsService) *fletcherv1.DeployInfo {
		resp, err := svc.GetSession(context.Background(), connect.NewRequest(&fletcherv1.GetSessionRequest{Ref: "app"}))
		require.NoError(t, err)
		return resp.Msg.GetSession().GetDeploy()
	}

	d := get(api.NewSessionsService(fakeSessionsBackend{sess: runApp}, "", resolver))
	require.NotNil(t, d)
	require.Equal(t, []string{"/app", "serve"}, d.GetEntrypoint())
	require.EqualValues(t, 8080, d.GetExposedPort())

	// A bare (non-run_app) session carries no deploy info even with a resolver.
	bare := session.Session{ID: "s2", Name: "app", Image: "myapp"}
	require.Nil(t, get(api.NewSessionsService(fakeSessionsBackend{sess: bare}, "", resolver)))

	// No resolver: no deploy info.
	require.Nil(t, get(api.NewSessionsService(fakeSessionsBackend{sess: runApp}, "", nil)))

	// Resolver without metadata (ok=false): no deploy info.
	noMeta := fakeDeployResolver{ok: false}
	require.Nil(t, get(api.NewSessionsService(fakeSessionsBackend{sess: runApp}, "", noMeta)))
}
