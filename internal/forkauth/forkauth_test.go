package forkauth

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthorityRejectsForgedAndCrossProcessIdentity(t *testing.T) {
	a := New()
	caller := Caller{ID: "session_example", Policy: "none", Session: true}
	srv := httptest.NewServer(a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := FromContext(r.Context())
		require.True(t, ok)
		require.Equal(t, caller, got)
		w.WriteHeader(http.StatusNoContent)
	})))
	defer srv.Close()
	for token, want := range map[string]int{"": 401, "forged": 401, New().sign(caller): 401, a.sign(caller): 204} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		require.NoError(t, err)
		req.Header.Set(identityHeader, token)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		require.Equal(t, want, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
}

func TestHostForwardOverwritesGuestIdentity(t *testing.T) {
	ctx := t.Context()
	a := New()
	var got atomic.Pointer[Caller]
	backend := httptest.NewUnstartedServer(a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, _ := FromContext(r.Context())
		got.Store(&caller)
		_, _ = io.WriteString(w, "ok")
	})))
	require.NoError(t, backend.Listener.Close())
	var lc net.ListenConfig
	path := filepath.Join(t.TempDir(), "backend.sock")
	ln, err := lc.Listen(ctx, "unix", path)
	require.NoError(t, err)
	backend.Listener = ln
	backend.Start()
	defer backend.Close()
	forwardPath := filepath.Join(t.TempDir(), "forward.sock")
	caller := Caller{ID: "session_example", Policy: "none", Session: true}
	forward, err := a.Forward(ctx, forwardPath, path, caller)
	require.NoError(t, err)
	defer func() { _ = forward.Close() }()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", forwardPath)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/mcp", http.NoBody)
	require.NoError(t, err)
	req.Header.Set(identityHeader, a.sign(Caller{ID: "other", Policy: "open"}))
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, caller, *got.Load())
}
