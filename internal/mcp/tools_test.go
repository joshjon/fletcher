package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateEgressURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"http ok", "http://example.com/x", false},
		{"https ok", "https://example.com", false},
		{"ip literal ok at this layer", "http://127.0.0.1:8080", false}, // blocked later, at dial
		{"ftp scheme", "ftp://example.com", true},
		{"file scheme", "file:///etc/passwd", true},
		{"no scheme", "example.com/x", true},
		{"no host", "http://", true},
		{"garbage", "://nope", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEgressURL(tc.url)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestEgressHTTPClientBlocksLoopback proves the SSRF guard refuses to dial a
// loopback target even when handed its URL directly (the httptest server binds
// 127.0.0.1), so an agent cannot reach the daemon's own surface.
func TestEgressHTTPClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should not be reachable"))
	}))
	defer srv.Close()

	client := NewEgressHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
}
