package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestTrackedRequestsReleaseUnreadStreamingResponses(t *testing.T) {
	for _, protocol := range []string{"http1", "h2c"} {
		t.Run(protocol, func(t *testing.T) {
			connections := newRequestConnections()
			entered, exited := make(chan struct{}), make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				defer close(exited)
				w.WriteHeader(http.StatusOK)
				if err := http.NewResponseController(w).Flush(); err != nil {
					return
				}
				close(entered)
				payload := make([]byte, 64<<10)
				for {
					if _, err := w.Write(payload); err != nil {
						return
					}
				}
			})
			srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
			if protocol == "h2c" {
				srv.Protocols = h2cProtocols()
			}
			var lc net.ListenConfig
			ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { connections.closeAll(); _ = srv.Close() })
			done := make(chan error, 1)
			go func() { done <- srv.Serve(connections.listener(ln)) }()
			var transport http.RoundTripper = &http.Transport{}
			if protocol == "h2c" {
				transport = &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, network, addr)
				}}
			}
			defer transport.(interface{ CloseIdleConnections() }).CloseIdleConnections()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
			require.NoError(t, err)
			response, err := transport.RoundTrip(req)
			require.NoError(t, err)
			defer response.Body.Close()
			<-entered
			// Deliberately never read the body. Cancellation alone must not be
			// relied on to release an HTTP/1 or HTTP/2 network writer.
			connections.closeAll()
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				t.Fatal("stream writer remained blocked after connection shutdown")
			}
			require.NoError(t, srv.Close())
			require.ErrorIs(t, <-done, http.ErrServerClosed)
			connections.mu.Lock()
			remaining := len(connections.live)
			connections.mu.Unlock()
			require.Zero(t, remaining)
		})
	}
}

func TestHTTPServerCloseDoesNotLeaveHijackedConnectionsAlive(t *testing.T) {
	connections := newRequestConnections()
	entered, done := make(chan struct{}), make(chan error, 1)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			done <- err
			close(entered)
			return
		}
		defer conn.Close()
		close(entered)
		_, err = io.Copy(io.Discard, conn)
		done <- err
	})}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { connections.closeAll(); _ = srv.Close() })
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(connections.listener(ln)) }()
	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	_, err = fmt.Fprint(client, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	require.NoError(t, err)
	<-entered
	require.NoError(t, srv.Close())
	select {
	case <-done:
		t.Fatal("expected the hijacked stream to remain open before tracked cleanup")
	default:
	}
	connections.closeAll()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hijacked stream was not closed")
	}
	require.ErrorIs(t, <-serveDone, http.ErrServerClosed)
}

func TestTrackedHijacksCloseAndRejectLateAcceptance(t *testing.T) {
	connections := newRequestConnections()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	wrapped := connections.listener(ln)
	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	server, err := wrapped.Accept()
	require.NoError(t, err)
	blocked := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, server); blocked <- err }()
	connections.closeAll()
	require.Error(t, <-blocked)
	late, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	defer late.Close()
	_, err = wrapped.Accept()
	require.True(t, errors.Is(err, net.ErrClosed))
	connections.closeAll()
}
