package pairingtls

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureCertGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()

	mat, err := EnsureCert(dir)
	require.NoError(t, err)
	require.NotEmpty(t, mat.Fingerprint)
	require.NotEmpty(t, mat.Certificate.Certificate)

	// The advertised fingerprint must be exactly SHA-256 of the leaf DER,
	// so a client recomputing it over the offered cert matches.
	sum := sha256.Sum256(mat.Certificate.Certificate[0])
	require.Equal(t, hex.EncodeToString(sum[:]), mat.Fingerprint)

	// Both files are written, key locked down to the owner.
	keyInfo, err := os.Stat(filepath.Join(dir, keyFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), keyInfo.Mode().Perm())
	_, err = os.Stat(filepath.Join(dir, certFile))
	require.NoError(t, err)
}

func TestEnsureCertStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	first, err := EnsureCert(dir)
	require.NoError(t, err)

	// A second call reloads the persisted pair rather than minting a new
	// one, so the fingerprint a paired-but-incomplete client holds stays
	// valid across daemon restarts.
	second, err := EnsureCert(dir)
	require.NoError(t, err)
	require.Equal(t, first.Fingerprint, second.Fingerprint)
}

func TestEnsureCertRegeneratesCorruptPair(t *testing.T) {
	dir := t.TempDir()

	first, err := EnsureCert(dir)
	require.NoError(t, err)

	// A truncated cert file must not wedge pairing forever: EnsureCert
	// regenerates (new fingerprint) rather than returning an error.
	require.NoError(t, os.WriteFile(filepath.Join(dir, certFile), []byte("garbage"), 0o644))
	regenerated, err := EnsureCert(dir)
	require.NoError(t, err)
	require.NotEmpty(t, regenerated.Fingerprint)
	require.NotEqual(t, first.Fingerprint, regenerated.Fingerprint)
}
