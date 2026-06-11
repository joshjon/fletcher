package api_test

import (
	"context"
	"io"
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
	sess       session.Session
	ports      []session.PublishedPort
	restarts   int64
	restartsOK bool
	logs       string
}

func (f fakeSessionsBackend) Get(context.Context, string) (session.Session, error) {
	return f.sess, nil
}

func (f fakeSessionsBackend) ListPorts(context.Context, string) ([]session.PublishedPort, error) {
	return f.ports, nil
}

func (f fakeSessionsBackend) Redeploy(context.Context, string) (session.Session, error) {
	return f.sess, nil
}

func (f fakeSessionsBackend) AppRestartCount(context.Context, string) (int64, bool) {
	return f.restarts, f.restartsOK
}

func (f fakeSessionsBackend) StreamLogs(_ context.Context, _ string, _ int, _ bool, w io.Writer) error {
	_, err := io.WriteString(w, f.logs)
	return err
}

type fakeRefresher struct {
	refreshed bool
	calls     *int
}

func (f fakeRefresher) RefreshImage(context.Context, string) bool {
	if f.calls != nil {
		*f.calls++
	}
	return f.refreshed
}

type fakeCerts struct {
	status  string
	expires int64
}

func (f fakeCerts) CertStatus(context.Context, string) (string, int64) {
	return f.status, f.expires
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

	d := get(api.NewSessionsService(
		fakeSessionsBackend{sess: runApp, restarts: 3, restartsOK: true},
		api.SessionsDeps{Deploy: resolver},
	))
	require.NotNil(t, d)
	require.Equal(t, []string{"/app", "serve"}, d.GetEntrypoint())
	require.EqualValues(t, 8080, d.GetExposedPort())
	require.EqualValues(t, 3, d.GetRestartCount())

	// A bare (non-run_app) session carries no deploy info even with a resolver.
	bare := session.Session{ID: "s2", Name: "app", Image: "myapp"}
	require.Nil(t, get(api.NewSessionsService(fakeSessionsBackend{sess: bare}, api.SessionsDeps{Deploy: resolver})))

	// No resolver: no deploy info.
	require.Nil(t, get(api.NewSessionsService(fakeSessionsBackend{sess: runApp}, api.SessionsDeps{})))

	// Resolver without metadata (ok=false): no deploy info.
	noMeta := fakeDeployResolver{ok: false}
	require.Nil(t, get(api.NewSessionsService(fakeSessionsBackend{sess: runApp}, api.SessionsDeps{Deploy: noMeta})))
}

// ListPorts attaches TLS status only to public ports that have a hostname, and
// only when a cert resolver (public web) is present.
func TestListPortsAttachesTLSStatus(t *testing.T) {
	ports := []session.PublishedPort{
		{ID: "p1", Public: true, Host: "app.example.com", GuestPort: 8080},
		{ID: "p2", Public: false, GuestPort: 5432},          // tunnel-only
		{ID: "p3", Public: true, Host: "", GuestPort: 9090}, // public, no host
	}
	backend := fakeSessionsBackend{ports: ports}
	certs := fakeCerts{status: "valid", expires: 1234567890}

	resp, err := api.NewSessionsService(backend, api.SessionsDeps{Certs: certs}).
		ListPorts(context.Background(), connect.NewRequest(&fletcherv1.ListPortsRequest{Ref: "s"}))
	require.NoError(t, err)
	got := resp.Msg.GetPorts()
	require.Len(t, got, 3)
	require.Equal(t, "valid", got[0].GetTlsStatus())
	require.EqualValues(t, 1234567890, got[0].GetTlsExpiresAt())
	require.Empty(t, got[1].GetTlsStatus(), "tunnel-only port has no TLS status")
	require.Empty(t, got[2].GetTlsStatus(), "public port without host has no TLS status")

	// No cert resolver (public web off): no status even for a public port.
	resp2, err := api.NewSessionsService(backend, api.SessionsDeps{}).
		ListPorts(context.Background(), connect.NewRequest(&fletcherv1.ListPortsRequest{Ref: "s"}))
	require.NoError(t, err)
	require.Empty(t, resp2.Msg.GetPorts()[0].GetTlsStatus())
}

// RedeploySession re-pulls (best-effort) then redeploys, reporting whether the
// image was refreshed; with no refresher it still redeploys (refreshed=false).
func TestRedeploySession(t *testing.T) {
	sess := session.Session{ID: "s1", Name: "app", Image: "ghcr.io/x/app:v1", RunApp: true}

	calls := 0
	resp, err := api.NewSessionsService(fakeSessionsBackend{sess: sess}, api.SessionsDeps{
		Refresher: fakeRefresher{refreshed: true, calls: &calls},
	}).RedeploySession(context.Background(), connect.NewRequest(&fletcherv1.RedeploySessionRequest{Ref: "app"}))
	require.NoError(t, err)
	require.True(t, resp.Msg.GetImageRefreshed())
	require.Equal(t, 1, calls, "refresher is consulted once")
	require.Equal(t, "app", resp.Msg.GetSession().GetName())

	resp2, err := api.NewSessionsService(fakeSessionsBackend{sess: sess}, api.SessionsDeps{}).
		RedeploySession(context.Background(), connect.NewRequest(&fletcherv1.RedeploySessionRequest{Ref: "app"}))
	require.NoError(t, err)
	require.False(t, resp2.Msg.GetImageRefreshed())
}
