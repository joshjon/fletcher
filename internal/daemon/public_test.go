package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/settings"
)

func TestPublicWebConfig(t *testing.T) {
	cfg := Config{}
	require.NoError(t, validateConfig(&cfg, discardLogger()))
	require.Equal(t, "edge", cfg.PublicWebMode)
	require.Equal(t, settings.DefaultPublicWebListen, cfg.PublicWebListen)
	for _, tc := range []struct{ mode, listen, trust string }{
		{mode: "invalid"},
		{listen: "0.0.0.0:11800"},
		{trust: "not-a-cidr"},
	} {
		bad := Config{PublicWebMode: tc.mode, PublicWebListen: tc.listen, PublicWebTrustedProxies: tc.trust}
		require.Error(t, validateConfig(&bad, discardLogger()))
	}
	for key, value := range map[string]string{
		settings.KeyPublicWebMode:           "behind_proxy",
		settings.KeyPublicWebListen:         "[::1]:11801",
		settings.KeyPublicWebTrustedProxies: "::1/128",
	} {
		require.True(t, applyPublicWebSetting(&cfg, key, value))
		require.Equal(t, value, settingsDefaults(cfg)[key])
	}
	require.NoError(t, validateConfig(&cfg, discardLogger()))
}

type noPublicApps struct{}

func (noPublicApps) LookupPublicPort(context.Context, string) (session.PublishedPort, error) {
	return session.PublishedPort{}, session.ErrNotFound
}

func (noPublicApps) DialPort(context.Context, string, uint16) (net.Conn, error) {
	panic("unknown host must not reach a guest")
}

func TestBehindProxyListener(t *testing.T) {
	// Port zero is used only by this listener-level test, bypassing boot validation.
	cfg := Config{PublicWeb: true, PublicWebMode: "behind_proxy", PublicWebListen: "127.0.0.1:0"}
	servers := buildBehindProxyWeb(t.Context(), cfg, noPublicApps{}, discardLogger())
	require.NotNil(t, servers)
	defer servers.pub.Close()
	require.Nil(t, servers.httpsSrv)
	require.Nil(t, servers.httpsLn)
	status, expiry := servers.pub.CertStatus(t.Context(), "app.example.com")
	require.Equal(t, session.CertExternal, status)
	require.Zero(t, expiry)

	execute, interrupt := httpServeActor(discardLogger(), "test-public", servers.httpSrv, servers.httpLn, "test")
	done := make(chan error, 1)
	go func() { done <- execute() }()
	defer func() { interrupt(nil); require.NoError(t, <-done) }()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+servers.httpLn.Addr().String()+"/healthz", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, string(body), "no published service")
}

func TestPublicWebGateAndUnsafeBind(t *testing.T) {
	require.Nil(t, buildPublicWeb(t.Context(), Config{PublicWebMode: "behind_proxy"}, nil, nil, discardLogger()))
	require.Nil(t, buildPublicWeb(t.Context(), Config{PublicWeb: true, PublicWebMode: "behind_proxy", PublicWebListen: "0.0.0.0:11800"}, nil, nil, discardLogger()))
}
