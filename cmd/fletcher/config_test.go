package main

import (
	"encoding/base64"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	require.Equal(t, clientConfig{}, loadClientConfig(), "absent config reads as zero")

	require.NoError(t, saveClientConfig(clientConfig{Remote: "10.99.0.1:11700", Token: "tok-123"}))
	got := loadClientConfig()
	require.Equal(t, "10.99.0.1:11700", got.Remote)
	require.Equal(t, "tok-123", got.Token)

	// The file holds a bearer token, so it must be 0600.
	path, err := clientConfigPath()
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	removed, err := clearClientConfig()
	require.NoError(t, err)
	require.True(t, removed)
	require.Equal(t, clientConfig{}, loadClientConfig())

	// Clearing again is a no-op, not an error.
	removed, err = clearClientConfig()
	require.NoError(t, err)
	require.False(t, removed)
}

func TestLoginBlobRoundTrip(t *testing.T) {
	blob := encodeLoginBlob("10.99.0.1:11700", "tok-123")
	got, err := decodeLoginBlob(blob)
	require.NoError(t, err)
	require.Equal(t, "10.99.0.1:11700", got.Remote)
	require.Equal(t, "tok-123", got.Token)

	// Tolerates surrounding whitespace from a copy-paste.
	got, err = decodeLoginBlob("  " + blob + "\n")
	require.NoError(t, err)
	require.Equal(t, "tok-123", got.Token)
}

func TestDecodeLoginBlobRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"not-base64-!!!",
		base64.RawURLEncoding.EncodeToString([]byte("not json")),
		base64.RawURLEncoding.EncodeToString([]byte(`{"r":"","t":""}`)), // missing fields
		base64.RawURLEncoding.EncodeToString([]byte(`{"r":"host:1"}`)),  // missing token
	}
	for _, s := range cases {
		_, err := decodeLoginBlob(s)
		require.Errorf(t, err, "expected error for %q", s)
	}
}

func TestPairBlobRoundTrip(t *testing.T) {
	in := pairBlob{
		PairingCode:         "code-123",
		ExpiresAt:           1_700_000_000,
		ServerPublicKey:     "SERVERPUB",
		Endpoint:            "home.example.com:51820",
		Address:             "10.99.0.2/32",
		AllowedIPs:          []string{"10.99.0.0/24"},
		APIEndpoint:         "10.99.0.1:11700",
		PersistentKeepalive: 25,
		Name:                "phone",
		PairingEndpoint:     "home.example.com:51821",
		PairingFingerprint:  "abc123def456",
	}
	encoded := encodePairBlob(in)
	got, err := decodePairBlob(encoded)
	require.NoError(t, err)
	require.Equal(t, pairBlobVersion, got.Version)
	require.Equal(t, in.PairingCode, got.PairingCode)
	require.Equal(t, in.ServerPublicKey, got.ServerPublicKey)
	require.Equal(t, in.Endpoint, got.Endpoint)
	require.Equal(t, in.Address, got.Address)
	require.Equal(t, in.AllowedIPs, got.AllowedIPs)
	require.Equal(t, in.APIEndpoint, got.APIEndpoint)
	require.Equal(t, in.PersistentKeepalive, got.PersistentKeepalive)
	require.Equal(t, in.Name, got.Name)
	require.Equal(t, in.PairingEndpoint, got.PairingEndpoint)
	require.Equal(t, in.PairingFingerprint, got.PairingFingerprint)

	// Tolerates surrounding whitespace from a copy-paste.
	got, err = decodePairBlob("  " + encoded + "\n")
	require.NoError(t, err)
	require.Equal(t, in.PairingCode, got.PairingCode)
}

func TestDecodePairBlobRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"not-base64-!!!",
		base64.RawURLEncoding.EncodeToString([]byte("not json")),
		// Wrong version.
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":99,"code":"x","spk":"y","ep":"z","addr":"a"}`)),
		// Missing required field (code).
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"spk":"y","ep":"z","addr":"a"}`)),
	}
	for _, s := range cases {
		_, err := decodePairBlob(s)
		require.Errorf(t, err, "expected error for %q", s)
	}
}
