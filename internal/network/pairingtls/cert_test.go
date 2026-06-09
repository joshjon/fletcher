package pairingtls

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// leaf returns the parsed leaf currently served, for asserting SAN/validity.
func (m *Manager) testLeaf(t *testing.T) (notBefore, notAfter time.Time, ips []net.IP, dns []string) {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	require.NotNil(t, m.cur)
	l := m.cur.leaf
	return l.NotBefore, l.NotAfter, l.IPAddresses, l.DNSNames
}

func TestEnsureHostIPSAN(t *testing.T) {
	m := testManager(t)
	require.NoError(t, m.EnsureHost("198.51.100.42"))

	_, _, ips, dns := m.testLeaf(t)
	require.Empty(t, dns)
	require.Len(t, ips, 1)
	require.True(t, ips[0].Equal(net.ParseIP("198.51.100.42")))
}

func TestEnsureHostDNSSAN(t *testing.T) {
	m := testManager(t)
	require.NoError(t, m.EnsureHost("example.com"))

	_, _, ips, dns := m.testLeaf(t)
	require.Empty(t, ips)
	require.Equal(t, []string{"example.com"}, dns)
}

func TestCertWithinAppleValidityCap(t *testing.T) {
	m := testManager(t)
	require.NoError(t, m.EnsureHost("example.com"))

	notBefore, notAfter, _, _ := m.testLeaf(t)
	// Apple rejects TLS server certs whose validity period exceeds 398 days.
	require.LessOrEqual(t, notAfter.Sub(notBefore), 398*24*time.Hour)
}

func TestFingerprintMatchesLeafDER(t *testing.T) {
	m := testManager(t)
	require.NoError(t, m.EnsureHost("example.com"))

	m.mu.RLock()
	der := m.cur.cert.Certificate[0]
	m.mu.RUnlock()
	sum := sha256.Sum256(der)
	require.Equal(t, hex.EncodeToString(sum[:]), m.Fingerprint())
}

func TestEnsureHostStableWhenHostMatches(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	first := NewManager(dir, logger)
	require.NoError(t, first.EnsureHost("example.com"))

	// A fresh manager over the same dir reloads the persisted cert (same
	// fingerprint) rather than minting a new one, since the SAN still
	// covers the host.
	second := NewManager(dir, logger)
	require.NoError(t, second.EnsureHost("example.com"))
	require.Equal(t, first.Fingerprint(), second.Fingerprint())

	keyInfo, err := os.Stat(filepath.Join(dir, keyFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), keyInfo.Mode().Perm())
}

func TestEnsureHostRegeneratesOnHostChange(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	first := NewManager(dir, logger)
	require.NoError(t, first.EnsureHost("a.example.com"))

	// A new manager started against a different public endpoint must not
	// reuse the persisted cert: its SAN no longer matches what the client
	// will dial, so iOS would reject it.
	second := NewManager(dir, logger)
	require.NoError(t, second.EnsureHost("b.example.com"))
	require.NotEqual(t, first.Fingerprint(), second.Fingerprint())

	_, _, _, dns := second.testLeaf(t)
	require.Equal(t, []string{"b.example.com"}, dns)
}

func TestSetHostRotatesCert(t *testing.T) {
	m := testManager(t)
	require.NoError(t, m.EnsureHost("a.example.com"))
	before := m.Fingerprint()

	m.SetHost("b.example.com")
	require.NotEqual(t, before, m.Fingerprint())

	_, _, _, dns := m.testLeaf(t)
	require.Equal(t, []string{"b.example.com"}, dns)
}

func TestSetHostNoopWhenHostUnchanged(t *testing.T) {
	m := testManager(t)
	require.NoError(t, m.EnsureHost("example.com"))
	before := m.Fingerprint()

	m.SetHost("example.com")
	require.Equal(t, before, m.Fingerprint())
}

func TestSetHostRotatesWithinRenewWindow(t *testing.T) {
	m := testManager(t)
	// Force every cert to look "near expiry" so a same-host call still
	// rotates - exercising the renewal path without waiting a year.
	m.renewWindow = 2 * certValidity
	require.NoError(t, m.EnsureHost("example.com"))
	before := m.Fingerprint()

	m.SetHost("example.com")
	require.NotEqual(t, before, m.Fingerprint())
}

func TestEnsureHostRegeneratesCorruptPair(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	first := NewManager(dir, logger)
	require.NoError(t, first.EnsureHost("example.com"))

	// A truncated cert file must not wedge pairing forever.
	require.NoError(t, os.WriteFile(filepath.Join(dir, certFile), []byte("garbage"), 0o644))
	second := NewManager(dir, logger)
	require.NoError(t, second.EnsureHost("example.com"))
	require.NotEmpty(t, second.Fingerprint())
	require.NotEqual(t, first.Fingerprint(), second.Fingerprint())
}
