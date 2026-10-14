package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/egress"
	"github.com/joshjon/fletcher/internal/forkauth"
)

func TestHTTPPolicyRejectsUnknownNoneAndUnlistedForks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	client := policyHTTPClient(srv.Client(), egress.NewAllowlist([]string{"allowed.example"}))
	for _, policy := range []string{"none", "allowlist", "invalid", ""} {
		t.Run(policy, func(t *testing.T) {
			ctx := forkauth.WithCaller(t.Context(), forkauth.Caller{ID: "job_example", Policy: policy})
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
			require.NoError(t, err)
			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.ErrorContains(t, err, "egress denied")
		})
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorContains(t, err, "authenticated fork identity")
}

func TestHTTPPolicyChecksRedirectDestination(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("blocked redirect reached upstream")
		w.WriteHeader(204)
	}))
	defer blocked.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blocked.URL, http.StatusFound)
	}))
	defer allowed.Close()
	// Both test servers bind loopback. Distinguish their hostnames while using
	// the test transport to dial the allowed endpoint.
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copyReq := r.Clone(r.Context())
		copyURL := *r.URL
		copyURL.Host = allowed.Listener.Addr().String()
		copyReq.URL = &copyURL
		return http.DefaultTransport.RoundTrip(copyReq)
	})
	client := policyHTTPClient(&http.Client{Transport: transport}, egress.NewAllowlist([]string{"allowed.example"}))
	ctx := forkauth.WithCaller(t.Context(), forkauth.Caller{ID: "job_example", Policy: "allowlist"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://allowed.example", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorContains(t, err, "egress denied")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
