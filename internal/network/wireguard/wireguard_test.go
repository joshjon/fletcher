package wireguard_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/network/wireguard"
)

func TestGenerateKeypairProducesBase64Curve25519Keys(t *testing.T) {
	kp, err := wireguard.GenerateKeypair()
	require.NoError(t, err)

	priv, err := base64.StdEncoding.DecodeString(string(kp.Private))
	require.NoError(t, err)
	pub, err := base64.StdEncoding.DecodeString(string(kp.Public))
	require.NoError(t, err)
	require.Len(t, priv, 32)
	require.Len(t, pub, 32)
}

func TestPublicFromPrivateRoundTripsAgainstGenerate(t *testing.T) {
	kp, err := wireguard.GenerateKeypair()
	require.NoError(t, err)
	derived, err := wireguard.PublicFromPrivate(kp.Private)
	require.NoError(t, err)
	require.Equal(t, kp.Public, derived)
}

func TestValidatePublicKeyAcceptsGeneratedAndRejectsMalformed(t *testing.T) {
	kp, err := wireguard.GenerateKeypair()
	require.NoError(t, err)
	require.NoError(t, wireguard.ValidatePublicKey(kp.Public))

	require.Error(t, wireguard.ValidatePublicKey(""))
	require.Error(t, wireguard.ValidatePublicKey("not-base64-!@#"))
	// Right encoding, wrong length.
	short := wireguard.Key(base64.StdEncoding.EncodeToString([]byte("twenty-byte string..")))
	require.Error(t, wireguard.ValidatePublicKey(short))
}

func TestRenderServerIncludesPeerBlocks(t *testing.T) {
	out := wireguard.RenderServer(wireguard.ServerConfig{
		PrivateKey: "AAAA",
		Address:    "10.99.0.1/24",
		ListenPort: 51820,
		Peers: []wireguard.PeerEntry{
			{Name: "laptop", PublicKey: "BBBB", AllowedIPs: []string{"10.99.0.2/32"}},
			{Name: "phone", PublicKey: "CCCC", AllowedIPs: []string{"10.99.0.3/32"}},
		},
	})
	require.Contains(t, out, "[Interface]")
	require.Contains(t, out, "PrivateKey = AAAA")
	require.Contains(t, out, "ListenPort = 51820")
	require.Contains(t, out, "# laptop")
	require.Contains(t, out, "PublicKey = BBBB")
	require.Contains(t, out, "AllowedIPs = 10.99.0.2/32")
	require.Equal(t, 2, strings.Count(out, "[Peer]"))
}

func TestRenderClientFullConfig(t *testing.T) {
	out := wireguard.RenderClient(wireguard.ClientConfig{
		PrivateKey:          "CLIENT",
		Address:             "10.99.0.2/32",
		DNS:                 []string{"10.99.0.1"},
		ServerPublicKey:     "SERVER",
		Endpoint:            "example.com:51820",
		AllowedIPs:          []string{"10.99.0.0/24"},
		PersistentKeepalive: 25,
	})
	require.Contains(t, out, "PrivateKey = CLIENT")
	require.Contains(t, out, "Endpoint = example.com:51820")
	require.Contains(t, out, "AllowedIPs = 10.99.0.0/24")
	require.Contains(t, out, "PersistentKeepalive = 25")
	require.Contains(t, out, "DNS = 10.99.0.1")
}
