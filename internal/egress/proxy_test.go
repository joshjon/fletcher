package egress_test

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/egress"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAllowlistMatching(t *testing.T) {
	a := egress.NewAllowlist([]string{"api.anthropic.com", "*.githubusercontent.com", ".pypi.org"})

	require.True(t, a.Allow("api.anthropic.com"))
	require.True(t, a.Allow("API.Anthropic.com")) // case-insensitive
	require.True(t, a.Allow("raw.githubusercontent.com"))
	require.True(t, a.Allow("githubusercontent.com")) // wildcard matches base
	require.True(t, a.Allow("pypi.org"))
	require.True(t, a.Allow("files.pypi.org"))

	require.False(t, a.Allow("anthropic.com"))
	require.False(t, a.Allow("evil.com"))
	require.False(t, a.Allow("notgithubusercontent.com"))

	require.True(t, egress.Open{}.Allow("anything.example"))
	require.False(t, egress.Deny{}.Allow("anything.example"))
}

// hostOf returns the hostname of a test server URL.
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Hostname()
}

// plainDialer bypasses the netguard guard so tests can reach loopback fixtures.
func plainDialer() egress.Option { return egress.WithDialer(&net.Dialer{}) }

func TestProxyForwardsAllowedHTTP(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	defer target.Close()

	p := egress.New(egress.NewAllowlist([]string{hostOf(t, target.URL)}), testLogger(), plainDialer())
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(target.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "upstream-ok", string(body))
}

func TestProxyDeniesHTTPByPolicy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "should not reach")
	}))
	defer target.Close()

	p := egress.New(egress.Deny{}, testLogger(), plainDialer())
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(target.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestProxyTunnelsConnectHTTPS(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secure-ok")
	}))
	defer target.Close()

	p := egress.New(egress.NewAllowlist([]string{hostOf(t, target.URL)}), testLogger(), plainDialer())
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := target.Client() // trusts the test server's cert
	client.Transport.(*http.Transport).Proxy = http.ProxyURL(proxyURL)

	resp, err := client.Get(target.URL) // https -> CONNECT through the proxy
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "secure-ok", string(body))
}

// TestProxyLANGuardOverridesOpen proves the netguard guard blocks a loopback
// target even under the Open policy, when the production (guarded) dialer is
// used - a fork cannot pivot into the operator's network regardless of policy.
func TestProxyLANGuardOverridesOpen(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "lan")
	}))
	defer target.Close()

	p := egress.New(egress.Open{}, testLogger()) // default guarded dialer
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	resp, err := client.Get(target.URL) // target is 127.0.0.1 -> guard blocks the dial
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}
