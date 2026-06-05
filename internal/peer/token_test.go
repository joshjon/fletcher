package peer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateTokenIsRandomAndHashStable(t *testing.T) {
	a, err := generateToken()
	require.NoError(t, err)
	b, err := generateToken()
	require.NoError(t, err)
	require.NotEqual(t, a, b, "tokens must be random")
	require.NotEmpty(t, a)

	// Hashing is deterministic and does not leak the token.
	require.Equal(t, hashToken(a), hashToken(a))
	require.NotEqual(t, hashToken(a), hashToken(b))
	require.NotContains(t, hashToken(a), a)
}
