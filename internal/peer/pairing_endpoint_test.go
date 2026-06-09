package peer_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/peer"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

func newServiceWithOptions(t *testing.T, opts peer.Options) *peer.Service {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	return peer.NewService(sqliteq.New(db), opts)
}

func TestPairingEndpointDerivesFromPublicEndpoint(t *testing.T) {
	s := newServiceWithOptions(t, peer.Options{
		PublicEndpoint:        "home.example.com:51820",
		PairingPort:           51821,
		PairingTLSFingerprint: "deadbeef",
	})
	// The pairing endpoint takes the public endpoint's host and the
	// pairing port - a different port on the same reachable host.
	require.Equal(t, "home.example.com:51821", s.PairingEndpoint())
	require.Equal(t, "deadbeef", s.PairingTLSFingerprint())
}

func TestPairingEndpointEmptyWithoutPortOrEndpoint(t *testing.T) {
	// No pairing port: nothing to advertise even with a public endpoint.
	noPort := newServiceWithOptions(t, peer.Options{
		PublicEndpoint:        "home.example.com:51820",
		PairingTLSFingerprint: "deadbeef",
	})
	require.Empty(t, noPort.PairingEndpoint())
	require.Empty(t, noPort.PairingTLSFingerprint())

	// No public endpoint: there is no reachable host to dial.
	noEndpoint := newServiceWithOptions(t, peer.Options{
		PairingPort:           51821,
		PairingTLSFingerprint: "deadbeef",
	})
	require.Empty(t, noEndpoint.PairingEndpoint())
	require.Empty(t, noEndpoint.PairingTLSFingerprint())
}

func TestPairingEndpointTracksSetters(t *testing.T) {
	s := newServiceWithOptions(t, peer.Options{})
	require.Empty(t, s.PairingEndpoint())

	// SetPairing alone is not enough; the host comes from the public
	// endpoint, which UPnP may supply after boot.
	s.SetPairing(51821, "abc123")
	require.Empty(t, s.PairingEndpoint())

	s.SetPublicEndpoint("203.0.113.7:51820")
	require.Equal(t, "203.0.113.7:51821", s.PairingEndpoint())
	require.Equal(t, "abc123", s.PairingTLSFingerprint())
}
