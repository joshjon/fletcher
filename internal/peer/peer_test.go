package peer_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/peer"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

func newService(t *testing.T) *peer.Service {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	return peer.NewService(sqliteq.New(db), peer.Options{})
}

func TestNextAvailableAddressStartsAtTwoAndSkipsTaken(t *testing.T) {
	t.Parallel()
	s := newService(t)
	ctx := t.Context()

	got, err := s.NextAvailableAddress(ctx)
	require.NoError(t, err)
	require.Equal(t, "10.99.0.2/32", got, "fresh subnet allocates .2 first (.1 is reserved for the server)")

	// Claim .2 and .3; next should be .4.
	_, err = s.Create(ctx, peer.CreateParams{Name: "a", AllowedIPs: []string{"10.99.0.2/32"}})
	require.NoError(t, err)
	_, err = s.Create(ctx, peer.CreateParams{Name: "b", AllowedIPs: []string{"10.99.0.3/32"}})
	require.NoError(t, err)

	got, err = s.NextAvailableAddress(ctx)
	require.NoError(t, err)
	require.Equal(t, "10.99.0.4/32", got)
}

func TestPublicEndpointPassThrough(t *testing.T) {
	t.Parallel()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))

	s := peer.NewService(sqliteq.New(db), peer.Options{PublicEndpoint: "home.example.com:51820"})
	require.Equal(t, "home.example.com:51820", s.PublicEndpoint())
	require.Equal(t, peer.DefaultTunnelCIDR, s.TunnelCIDR())
}

func TestCreatePeerReturnsKeypairOnce(t *testing.T) {
	t.Parallel()
	s := newService(t)
	ctx := t.Context()

	got, err := s.Create(ctx, peer.CreateParams{
		Name:       "laptop",
		AllowedIPs: []string{"10.99.0.2/32"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, got.PrivateKey, "private key must be returned at create time")
	require.NotEmpty(t, got.Peer.PublicKey, "public key must be stored")
	require.NotEqual(t, got.PrivateKey, got.Peer.PublicKey)

	// Subsequent Get does not return the private key (it isn't stored).
	fetched, err := s.Get(ctx, got.Peer.ID)
	require.NoError(t, err)
	require.Equal(t, got.Peer.PublicKey, fetched.PublicKey)
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	t.Parallel()
	s := newService(t)
	ctx := t.Context()

	_, err := s.Create(ctx, peer.CreateParams{Name: "x", AllowedIPs: []string{"10.99.0.2/32"}})
	require.NoError(t, err)

	_, err = s.Create(ctx, peer.CreateParams{Name: "x", AllowedIPs: []string{"10.99.0.3/32"}})
	require.ErrorIs(t, err, peer.ErrNameTaken)
}

func TestCreateValidatesInputs(t *testing.T) {
	t.Parallel()
	s := newService(t)
	cases := []peer.CreateParams{
		{AllowedIPs: []string{"10.99.0.2/32"}}, // missing name
		{Name: "x"},                            // missing allowed IPs
	}
	for _, p := range cases {
		_, err := s.Create(t.Context(), p)
		require.Error(t, err)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := newService(t)
	_, err := s.Get(t.Context(), "peer_missing")
	require.ErrorIs(t, err, peer.ErrNotFound)
}

func TestListAndDelete(t *testing.T) {
	t.Parallel()
	s := newService(t)
	ctx := t.Context()
	c1, err := s.Create(ctx, peer.CreateParams{Name: "a", AllowedIPs: []string{"10.99.0.2/32"}})
	require.NoError(t, err)
	_, err = s.Create(ctx, peer.CreateParams{Name: "b", AllowedIPs: []string{"10.99.0.3/32"}})
	require.NoError(t, err)

	all, err := s.List(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)

	ok, err := s.Delete(ctx, c1.Peer.ID)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = s.Delete(ctx, c1.Peer.ID)
	require.NoError(t, err)
	require.False(t, ok)
}
