package session

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type publicTestBackend struct {
	hosts map[string]string
}

func (b publicTestBackend) LookupPublicPort(_ context.Context, host string) (PublishedPort, error) {
	addr, ok := b.hosts[host]
	if !ok {
		return PublishedPort{}, ErrNotFound
	}
	return PublishedPort{SessionID: addr, GuestPort: 8080, Public: true, Host: host}, nil
}

func (b publicTestBackend) DialPort(ctx context.Context, ref string, _ uint16) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", ref)
}

func publicTestConfig(t *testing.T, handler http.Handler) PublicConfig {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	return PublicConfig{
		Backend: publicTestBackend{hosts: map[string]string{"app.example.com": upstream.Listener.Addr().String()}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		CertDir: t.TempDir(),
	}
}

func TestPublicProxyMetadata(t *testing.T) {
	cfg := publicTestConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Seen-Host", r.Host)
		_ = json.NewEncoder(w).Encode(r.Header)
	}))
	for _, tc := range []struct {
		name, peer, scheme, client string
		trusted                    bool
		wantScheme, wantClient     string
	}{
		{"untrusted", "127.0.0.1:1234", "https", "192.0.2.10", false, "http", "127.0.0.1"},
		{"trusted", "127.0.0.1:1234", "https", "192.0.2.10", true, "https", "192.0.2.10"},
		{"wrong source", "192.0.2.11:1234", "https", "192.0.2.10", true, "http", "192.0.2.11"},
		{"chain rejected", "127.0.0.1:1234", "https, http", "192.0.2.10, 192.0.2.11", true, "http", "127.0.0.1"},
		{"invalid rejected", "127.0.0.1:1234", "javascript", "garbage", true, "http", "127.0.0.1"},
		{"mapped peer", "[::ffff:127.0.0.1]:1234", "https", "2001:db8::1", true, "https", "2001:db8::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trusted []netip.Prefix
			if tc.trusted {
				trusted = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
			}
			s := NewBehindProxyServer(cfg, trusted)
			defer s.Close()
			r := httptest.NewRequest(http.MethodGet, "http://app.example.com/path", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-Proto", tc.scheme)
			r.Header.Set("X-Forwarded-For", tc.client)
			r.Header.Set("X-Forwarded-Host", "admin.example.com")
			r.Header.Set("Forwarded", "host=admin.example.com;proto=https")
			r.Header.Set("X-Forwarded-Port", "443")
			r.Header.Set("X-Real-IP", "192.0.2.99")
			w := httptest.NewRecorder()
			s.AppHandler().ServeHTTP(w, r)
			require.Equal(t, http.StatusOK, w.Code)
			var headers http.Header
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &headers))
			require.Equal(t, "app.example.com", headers.Get("Seen-Host"))
			require.Equal(t, "app.example.com", headers.Get("X-Forwarded-Host"))
			require.Equal(t, tc.wantScheme, headers.Get("X-Forwarded-Proto"))
			require.Equal(t, tc.wantClient, headers.Get("X-Forwarded-For"))
			for _, name := range []string{"Forwarded", "X-Forwarded-Port", "X-Real-IP"} {
				require.Empty(t, headers.Get(name))
			}
		})
	}
}

func TestPublicRouterIsolation(t *testing.T) {
	for _, edge := range []bool{false, true} {
		t.Run(fmt.Sprintf("edge=%t", edge), func(t *testing.T) {
			cfg := publicTestConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, "guest:"+r.URL.Path)
			}))
			var s *PublicServer
			if edge {
				s = NewPublicServer(cfg)
			} else {
				s = NewBehindProxyServer(cfg, nil)
			}
			defer s.Close()
			for _, host := range []string{"unknown.example.com", "127.0.0.1", "app.example.com"} {
				for _, path := range []string{"/", "/healthz", "/readyz", "/fletcher.v1.SessionsService/ListSessions", "/v1/messages", "/mcp"} {
					r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
					r.Header.Set("X-Forwarded-Host", "app.example.com")
					w := httptest.NewRecorder()
					s.AppHandler().ServeHTTP(w, r)
					if host == "app.example.com" {
						require.Equal(t, "guest:"+path, w.Body.String())
					} else {
						require.Equal(t, http.StatusNotFound, w.Code)
					}
				}
			}
			if edge {
				require.NotNil(t, s.TLSConfig().GetCertificate)
				require.Error(t, s.magic.OnDemand.DecisionFunc(t.Context(), "unknown.example.com"))
				require.NoError(t, s.magic.OnDemand.DecisionFunc(t.Context(), "app.example.com"))
				w := httptest.NewRecorder()
				s.HTTPHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://app.example.com/a?q=b", nil))
				require.Equal(t, http.StatusMovedPermanently, w.Code)
				require.Equal(t, "https://app.example.com/a?q=b", w.Header().Get("Location"))
			} else {
				require.Nil(t, s.magic)
				require.Nil(t, s.acme)
				status, expiry := s.CertStatus(t.Context(), "app.example.com")
				require.Equal(t, CertExternal, status)
				require.Zero(t, expiry)
			}
		})
	}
}

func TestPublicRouterTwoApps(t *testing.T) {
	backend := publicTestBackend{hosts: map[string]string{}}
	for _, host := range []string{"one.example.com", "two.example.com"} {
		app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, host) }))
		defer app.Close()
		backend.hosts[host] = app.Listener.Addr().String()
	}
	s := NewBehindProxyServer(PublicConfig{Backend: backend, Logger: slog.Default()}, nil)
	defer s.Close()
	for host := range backend.hosts {
		w := httptest.NewRecorder()
		s.AppHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://"+host+":443/", nil))
		require.Equal(t, host, w.Body.String())
	}
	// Republish the first hostname to a different guest. Pooled connections
	// must follow the new guest, not keep sending requests to the old one.
	backend.hosts["one.example.com"] = backend.hosts["two.example.com"]
	w := httptest.NewRecorder()
	s.AppHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://one.example.com:443/", nil))
	require.Equal(t, "two.example.com", w.Body.String())
}

func TestPublicEdgeIgnoresForwardedScheme(t *testing.T) {
	cfg := publicTestConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, r.Header.Get("X-Forwarded-Proto")) }))
	s := NewPublicServer(cfg)
	defer s.Close()
	r := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	s.HTTPSHandler().ServeHTTP(w, r)
	require.Equal(t, "https", w.Body.String())
}

func TestPublicProxyWebSocketUpgrade(t *testing.T) {
	cfg := publicTestConfig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		// Relay opaque frames after the HTTP upgrade, as a guest WebSocket would.
		_, _ = io.CopyN(conn, rw, 4)
	}))
	s := NewBehindProxyServer(cfg, nil)
	defer s.Close()
	server := httptest.NewServer(s.AppHandler())
	defer server.Close()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app.example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	_, err = io.WriteString(conn, "ping")
	require.NoError(t, err)
	var payload strings.Builder
	_, err = io.CopyN(&payload, reader, 4)
	require.NoError(t, err)
	require.Equal(t, "ping", payload.String())
}
