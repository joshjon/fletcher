package peer_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/network/pairingtls"
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

// fakeCert is a minimal PairingCertProvider that records the host it was
// last asked to bind and returns a fixed fingerprint.
type fakeCert struct {
	mu          sync.Mutex
	fingerprint string
	host        string
}

func (f *fakeCert) Fingerprint() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fingerprint
}

func (f *fakeCert) SetHost(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.host = host
}

func (f *fakeCert) lastHost() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.host
}

func TestPairingEndpointDerivesFromPublicEndpoint(t *testing.T) {
	s := newServiceWithOptions(t, peer.Options{PublicEndpoint: "home.example.com:51820"})
	s.SetPairingCert(51821, &fakeCert{fingerprint: "deadbeef"})

	// The pairing endpoint takes the public endpoint's host and the
	// pairing port - a different port on the same reachable host.
	require.Equal(t, "home.example.com:51821", s.PairingEndpoint())
	require.Equal(t, "deadbeef", s.PairingTLSFingerprint())
}

func TestPairingEndpointEmptyWithoutPortOrCert(t *testing.T) {
	// No pairing cert wired: nothing to advertise even with a public endpoint.
	noCert := newServiceWithOptions(t, peer.Options{PublicEndpoint: "home.example.com:51820"})
	require.Empty(t, noCert.PairingEndpoint())
	require.Empty(t, noCert.PairingTLSFingerprint())

	// No public endpoint: there is no reachable host to dial.
	noEndpoint := newServiceWithOptions(t, peer.Options{})
	noEndpoint.SetPairingCert(51821, &fakeCert{fingerprint: "deadbeef"})
	require.Empty(t, noEndpoint.PairingEndpoint())
	require.Empty(t, noEndpoint.PairingTLSFingerprint())
}

func TestPairingEndpointTracksSetters(t *testing.T) {
	s := newServiceWithOptions(t, peer.Options{})
	require.Empty(t, s.PairingEndpoint())

	// SetPairingCert alone is not enough; the host comes from the public
	// endpoint, which UPnP may supply after boot.
	s.SetPairingCert(51821, &fakeCert{fingerprint: "abc123"})
	require.Empty(t, s.PairingEndpoint())

	s.SetPublicEndpoint("203.0.113.7:51820")
	require.Equal(t, "203.0.113.7:51821", s.PairingEndpoint())
	require.Equal(t, "abc123", s.PairingTLSFingerprint())
}

func TestSetPublicEndpointRotatesPairingCertHost(t *testing.T) {
	s := newServiceWithOptions(t, peer.Options{})
	fake := &fakeCert{fingerprint: "fp"}
	s.SetPairingCert(51821, fake)

	// Changing the public endpoint must re-bind the cert to the new host,
	// passing only the host (not host:port) to the provider.
	s.SetPublicEndpoint("203.0.113.7:51820")
	require.Equal(t, "203.0.113.7", fake.lastHost())

	s.SetPublicEndpoint("home.example.com:51820")
	require.Equal(t, "home.example.com", fake.lastHost())
}

// TestPairingFingerprintRotatesWithEndpoint wires the real cert manager
// through the peer service and confirms the live fingerprint changes when
// the public endpoint (and thus the cert SAN) does - the regeneration path
// the iOS-compliance fix depends on.
func TestPairingFingerprintRotatesWithEndpoint(t *testing.T) {
	mgr := pairingtls.NewManager(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, mgr.EnsureHost("a.example.com"))

	s := newServiceWithOptions(t, peer.Options{})
	s.SetPublicEndpoint("a.example.com:51820")
	s.SetPairingCert(51821, mgr)

	before := s.PairingTLSFingerprint()
	require.NotEmpty(t, before)

	s.SetPublicEndpoint("b.example.com:51820")
	require.Equal(t, "b.example.com:51821", s.PairingEndpoint())
	require.NotEqual(t, before, s.PairingTLSFingerprint())
}
