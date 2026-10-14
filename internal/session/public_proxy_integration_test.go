//go:build integration

package session

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPublicAppsBehindNGINX exercises a real operator proxy at the HTTP boundary;
// fixture apps stand in for guest sockets, not Firecracker or durable VM state.
func TestPublicAppsBehindNGINX(t *testing.T) {
	nginx := os.Getenv("FLETCHER_TEST_NGINX")
	if nginx == "" {
		t.Skip("set FLETCHER_TEST_NGINX to the nginx executable, or run scripts/test-public-proxy.sh")
	}
	backend := publicTestBackend{hosts: map[string]string{}}
	for _, host := range []string{"one.example.com", "two.example.com"} {
		app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ws" {
				proxyTestWebSocket(w, r)
				return
			}
			r.Header.Set("App", host)
			r.Header.Set("Seen-Host", r.Host)
			_ = json.NewEncoder(w).Encode(r.Header)
		}))
		t.Cleanup(app.Close)
		backend.hosts[host] = app.Listener.Addr().String()
	}
	origin, stop := startProxyTestOrigin(t, backend, "127.0.0.1:0")
	addr, roots := startTestNGINX(t, nginx, origin)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", addr)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	for _, host := range []string{"one.example.com", "two.example.com"} {
		t.Run(host, func(t *testing.T) {
			// Even management-looking paths must reach only the selected app.
			for _, path := range []string{"/", "/healthz", "/readyz", "/fletcher.v1.SessionService/ListSessions", "/v1/messages", "/mcp"} {
				response := proxyTestRequest(t, client, host, path)
				var headers http.Header
				require.NoError(t, json.NewDecoder(response.Body).Decode(&headers))
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.Equal(t, host, headers.Get("App"))
				require.Equal(t, host, headers.Get("Seen-Host"))
				require.Equal(t, host, headers.Get("X-Forwarded-Host"))
				require.Equal(t, "https", headers.Get("X-Forwarded-Proto"))
				require.Equal(t, "127.0.0.1", headers.Get("X-Forwarded-For"))
				require.Empty(t, headers.Get("Forwarded"))
				require.Empty(t, headers.Get("X-Real-IP"))
			}
		})
	}
	response := proxyTestRequest(t, client, "unknown.example.com", "/healthz")
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNotFound, response.StatusCode)

	t.Run("websocket over TLS", func(t *testing.T) { checkProxyTestWebSocket(t, addr, roots) })
	t.Run("origin listener restart", func(t *testing.T) {
		stop()
		startProxyTestOrigin(t, backend, origin)
		response := proxyTestRequest(t, client, "two.example.com", "/")
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var headers http.Header
		require.NoError(t, json.NewDecoder(response.Body).Decode(&headers))
		require.Equal(t, "two.example.com", headers.Get("App"))
	})
}

func proxyTestRequest(t *testing.T, client *http.Client, host, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+path, nil)
	require.NoError(t, err)
	req.Header.Set("Forwarded", "host=one.example.com;proto=http;for=192.0.2.9")
	req.Header.Set("X-Forwarded-Host", "one.example.com")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-For", "192.0.2.9")
	req.Header.Set("X-Real-IP", "192.0.2.9")
	response, err := client.Do(req)
	require.NoError(t, err)
	return response
}

func startProxyTestOrigin(t *testing.T, backend PublicBackend, addr string) (string, func()) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", addr)
	require.NoError(t, err)
	pub := NewBehindProxyServer(PublicConfig{Backend: backend, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	server := &http.Server{Handler: pub.AppHandler(), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	stop := sync.OnceFunc(func() {
		require.NoError(t, server.Close())
		require.ErrorIs(t, <-done, http.ErrServerClosed)
		pub.Close()
	})
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

func startTestNGINX(t *testing.T, binary, origin string) (string, *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	roots := proxyTestCertificate(t, dir)
	var lc net.ListenConfig
	reservation, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := reservation.Addr().String()
	require.NoError(t, reservation.Close())
	config := fmt.Sprintf(`daemon off;
master_process off;
pid %s/nginx.pid;
error_log stderr info;
events {}
http {
    access_log off;
    proxy_temp_path %s/proxy;
    client_body_temp_path %s/body;
    fastcgi_temp_path %s/fastcgi;
    uwsgi_temp_path %s/uwsgi;
    scgi_temp_path %s/scgi;
    map $http_upgrade $connection_upgrade { default upgrade; '' close; }
    server {
        listen %s ssl;
        server_name one.example.com two.example.com;
        ssl_certificate %s/cert.pem;
        ssl_certificate_key %s/key.pem;
        location / {
            proxy_pass http://%s;
            proxy_http_version 1.1;
            proxy_set_header Host $http_host;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_set_header X-Forwarded-For $remote_addr;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
        }
    }
}
`, dir, dir, dir, dir, dir, dir, addr, dir, dir, origin)
	configPath := filepath.Join(dir, "nginx.conf")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	logFile, err := os.Create(filepath.Join(dir, "nginx.log"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, binary, "-p", dir, "-c", configPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		_ = logFile.Close()
		if t.Failed() {
			logs, _ := os.ReadFile(logFile.Name())
			t.Log(string(logs))
		}
	})
	require.Eventually(t, func() bool {
		var d net.Dialer
		conn, err := d.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 25*time.Millisecond)
	return addr, roots
}

func proxyTestCertificate(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fletcher proxy test"},
		DNSNames: []string{"*.example.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(certPEM))
	return roots
}

// proxyTestWebSocket accepts the RFC 6455 sample handshake and echoes one short text frame.
func proxyTestWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-WebSocket-Key") != "dGhlIHNhbXBsZSBub25jZQ==" {
		http.Error(w, "bad test handshake", http.StatusBadRequest)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	frame := make([]byte, 10)
	if _, err := io.ReadFull(rw, frame); err != nil || frame[0] != 0x81 || frame[1] != 0x84 {
		return
	}
	for i := range 4 {
		frame[6+i] ^= frame[2+i]
	}
	_, _ = conn.Write(append([]byte{0x81, 4}, frame[6:]...))
}

func checkProxyTestWebSocket(t *testing.T, addr string, roots *x509.CertPool) {
	t.Helper()
	d := tls.Dialer{Config: &tls.Config{RootCAs: roots, ServerName: "one.example.com", MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: one.example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	require.Equal(t, "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=", response.Header.Get("Sec-WebSocket-Accept"))
	_, err = conn.Write([]byte{0x81, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'i' ^ 2, 'n' ^ 3, 'g' ^ 4})
	require.NoError(t, err)
	frame := make([]byte, 6)
	_, err = io.ReadFull(reader, frame)
	require.NoError(t, err)
	require.Equal(t, []byte{0x81, 4, 'p', 'i', 'n', 'g'}, frame)
}
