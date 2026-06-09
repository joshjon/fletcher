package peer_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	"github.com/joshjon/fletcher/internal/peer"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

func newServiceWithEndpoint(t *testing.T) *peer.Service {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	return peer.NewService(sqliteq.New(db), peer.Options{
		PublicEndpoint: "home.example.com:51820",
		APIEndpoint:    "10.99.0.1:11700",
	})
}

func TestBeginPairReservesAddressAndCodeWithoutPersisting(t *testing.T) {
	s := newServiceWithEndpoint(t)
	ctx := context.Background()

	res, err := s.BeginPair(ctx, "phone")
	require.NoError(t, err)
	require.NotEmpty(t, res.Code)
	require.Equal(t, "10.99.0.2/32", res.Address)
	require.True(t, res.ExpiresAt.After(res.ExpiresAt.Add(-peer.PendingPairTTL)))

	// No peer row yet.
	peers, err := s.List(ctx, 10, 0)
	require.NoError(t, err)
	require.Empty(t, peers)

	// A second BeginPair skips the address held by the first slot.
	res2, err := s.BeginPair(ctx, "tablet")
	require.NoError(t, err)
	require.Equal(t, "10.99.0.3/32", res2.Address)
}

func TestBeginPairRejectsWhenNameAlreadyPersisted(t *testing.T) {
	s := newServiceWithEndpoint(t)
	ctx := context.Background()

	_, err := s.Create(ctx, peer.CreateParams{Name: "phone", AllowedIPs: []string{"10.99.0.2/32"}})
	require.NoError(t, err)

	_, err = s.BeginPair(ctx, "phone")
	require.ErrorIs(t, err, peer.ErrNameTaken)
}

func TestBeginPairFailsWithoutPublicEndpoint(t *testing.T) {
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	s := peer.NewService(sqliteq.New(db), peer.Options{})

	_, err = s.BeginPair(context.Background(), "phone")
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}

func TestCompletePairPersistsPeerWithSuppliedPublicKey(t *testing.T) {
	s := newServiceWithEndpoint(t)
	ctx := context.Background()

	begin, err := s.BeginPair(ctx, "phone")
	require.NoError(t, err)

	clientKP, err := wireguard.GenerateKeypair()
	require.NoError(t, err)

	done, err := s.CompletePair(ctx, begin.Code, "phone", clientKP.Public)
	require.NoError(t, err)
	require.Equal(t, "phone", done.Peer.Name)
	require.Equal(t, clientKP.Public, done.Peer.PublicKey)
	require.Contains(t, done.Peer.AllowedIPs, begin.Address)
	require.NotEmpty(t, done.APIToken)

	// Token resolves back to the peer.
	got, err := s.AuthenticateToken(ctx, done.APIToken)
	require.NoError(t, err)
	require.Equal(t, done.Peer.ID, got.ID)
}

func TestCompletePairIsOneTimeUse(t *testing.T) {
	s := newServiceWithEndpoint(t)
	ctx := context.Background()

	begin, err := s.BeginPair(ctx, "phone")
	require.NoError(t, err)
	clientKP, err := wireguard.GenerateKeypair()
	require.NoError(t, err)

	_, err = s.CompletePair(ctx, begin.Code, "phone", clientKP.Public)
	require.NoError(t, err)

	_, err = s.CompletePair(ctx, begin.Code, "phone", clientKP.Public)
	require.Error(t, err)
	require.Equal(t, errs.CategoryUnauthenticated, errs.CategoryOf(err))
}

func TestCompletePairRejectsNameMismatch(t *testing.T) {
	s := newServiceWithEndpoint(t)
	ctx := context.Background()

	begin, err := s.BeginPair(ctx, "phone")
	require.NoError(t, err)
	clientKP, err := wireguard.GenerateKeypair()
	require.NoError(t, err)

	_, err = s.CompletePair(ctx, begin.Code, "tablet", clientKP.Public)
	require.Error(t, err)
	require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
}

func TestCompletePairRejectsMalformedPublicKey(t *testing.T) {
	s := newServiceWithEndpoint(t)
	ctx := context.Background()

	begin, err := s.BeginPair(ctx, "phone")
	require.NoError(t, err)

	_, err = s.CompletePair(ctx, begin.Code, "phone", wireguard.Key("not-base64-!@#"))
	require.Error(t, err)
	require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
}

func TestNextAvailableAddressSkipsPendingReservations(t *testing.T) {
	s := newServiceWithEndpoint(t)
	ctx := context.Background()

	begin, err := s.BeginPair(ctx, "phone")
	require.NoError(t, err)
	require.Equal(t, "10.99.0.2/32", begin.Address)

	// A power-user CreatePeer that auto-allocates should skip the
	// pending reservation.
	got, err := s.NextAvailableAddress(ctx)
	require.NoError(t, err)
	require.Equal(t, "10.99.0.3/32", got)
}
