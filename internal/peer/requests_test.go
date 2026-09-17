package peer_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/peer"
)

func TestRevokeCancelsExistingRequestsAndRejectsNewOnes(t *testing.T) {
	s := newService(t)
	p, err := s.Create(t.Context(), peer.CreateParams{Name: "phone", AllowedIPs: []string{"10.99.0.2/32"}})
	require.NoError(t, err)
	authenticated, ctx, release, err := s.AuthenticateRequest(t.Context(), p.APIToken)
	require.NoError(t, err)
	defer release()
	require.Equal(t, p.Peer.ID, authenticated.ID)
	require.NoError(t, ctx.Err())
	deleted, err := s.Delete(t.Context(), p.Peer.ID)
	require.NoError(t, err)
	require.True(t, deleted)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	_, _, done, err := s.AuthenticateRequest(t.Context(), p.APIToken)
	defer done()
	require.ErrorIs(t, err, peer.ErrUnauthenticated)
}
