package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

// A Mode B pairing renders a {remote, token} login blob carrying the daemon's
// VPN address, decodable by the same path `fletcher login` uses.
func TestRenderByoVPNPairResultEmitsLoginBlob(t *testing.T) {
	resp := &fletcherv1.PairPeerResponse{
		Peer:              &fletcherv1.Peer{Id: "peer-1", Name: "phone"},
		ApiToken:          "tok-abc",
		ApiEndpoint:       "10.99.0.1:11700",
		RemoteApiEndpoint: "100.80.0.5:11700",
	}

	var buf bytes.Buffer
	require.NoError(t, renderByoVPNPairResult(&buf, resp, false))
	out := buf.String()

	require.Contains(t, out, "100.80.0.5:11700")
	require.NotContains(t, out, "10.99.0.1:11700", "Mode B output must advertise the VPN address, not the tunnel address")

	// Pull the blob line (the only base64url token line) and decode it.
	var blob string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, " ") {
			continue
		}
		blob = line
	}
	require.NotEmpty(t, blob, "expected a login blob line in the output")

	decoded, err := decodeLoginBlob(blob)
	require.NoError(t, err)
	require.Equal(t, "100.80.0.5:11700", decoded.Remote)
	require.Equal(t, "tok-abc", decoded.Token)
}

// Without a configured VPN address there is nothing to advertise, so the
// command fails with a clear pointer at --remote-api-listen rather than
// emitting a useless blob.
func TestRenderByoVPNPairResultErrorsWithoutRemote(t *testing.T) {
	resp := &fletcherv1.PairPeerResponse{
		Peer:        &fletcherv1.Peer{Id: "peer-1", Name: "phone"},
		ApiToken:    "tok-abc",
		ApiEndpoint: "10.99.0.1:11700",
		// RemoteApiEndpoint deliberately empty: daemon not configured for Mode B.
	}

	var buf bytes.Buffer
	err := renderByoVPNPairResult(&buf, resp, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "remote_api_listen")
	require.Empty(t, buf.String())
}
