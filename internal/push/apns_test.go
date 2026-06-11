package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// signJWT must produce a well-formed ES256 token whose signature verifies under
// the key's public half, with the header/claims APNs expects.
func TestSignJWT(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	now := time.Unix(1700000000, 0)
	tok, err := signJWT(key, "KEYID123", "TEAMID456", now)
	require.NoError(t, err)

	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)

	header := decodeSegment(t, parts[0])
	require.Equal(t, "ES256", header["alg"])
	require.Equal(t, "KEYID123", header["kid"])

	claims := decodeSegment(t, parts[1])
	require.Equal(t, "TEAMID456", claims["iss"])
	require.EqualValues(t, now.Unix(), claims["iat"])

	// The signature is R||S over SHA-256 of "header.claims"; verify it.
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	require.Len(t, sig, 64)
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.True(t, ecdsa.Verify(&key.PublicKey, digest[:], r, s), "signature must verify")
}

func decodeSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// buildPayload emits a content-light alert plus the custom data the app reads.
func TestBuildPayload(t *testing.T) {
	body, err := buildPayload(Notification{
		Title: "Fletcher",
		Body:  "An action needs your approval.",
		Data:  map[string]string{"approval_id": "approval_123"},
	})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "approval_123", got["approval_id"])
	aps, ok := got["aps"].(map[string]any)
	require.True(t, ok)
	alert, ok := aps["alert"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Fletcher", alert["title"])
	require.Equal(t, "An action needs your approval.", alert["body"])
}

func TestNewSenderRequiresConfig(t *testing.T) {
	_, err := NewSender(Config{KeyID: "k", TeamID: "t", Topic: "x"}) // no KeyPath
	require.Error(t, err)
}
