package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRestrictToPairingAllowsOnlyCompletePair is the guard rail test for
// the public pairing listener: CompletePair reaches the handler, every
// other PeerService method (and any other path) 404s, so the token-free
// public surface stays exactly one RPC wide.
func TestRestrictToPairingAllowsOnlyCompletePair(t *testing.T) {
	var reached bool
	guarded := restrictToPairing(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(guarded)
	t.Cleanup(srv.Close)

	cases := []struct {
		name      string
		path      string
		wantReach bool
	}{
		{"CompletePair allowed", pairingProcedure, true},
		{"BeginPair blocked", "/fletcher.v1.PeerService/BeginPair", false},
		{"ListPeers blocked", "/fletcher.v1.PeerService/ListPeers", false},
		{"DeletePeer blocked", "/fletcher.v1.PeerService/DeletePeer", false},
		{"other service blocked", "/fletcher.v1.JobService/ListJobs", false},
		{"root blocked", "/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			resp, err := http.Post(srv.URL+tc.path, "application/json", http.NoBody)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			if tc.wantReach {
				require.True(t, reached, "expected handler to be reached")
				require.Equal(t, http.StatusOK, resp.StatusCode)
			} else {
				require.False(t, reached, "handler must not be reached")
				require.Equal(t, http.StatusNotFound, resp.StatusCode)
			}
		})
	}
}
