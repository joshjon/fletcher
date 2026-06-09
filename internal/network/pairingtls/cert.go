// Package pairingtls provides the self-signed TLS material for the
// daemon's public pairing listener. Pairing is a bootstrap that happens
// before a client is a WireGuard peer, so CompletePair must be reachable
// over the public internet rather than the tunnel. That public channel
// carries the one-time pairing code, so it has to be confidential and
// MITM-proof - but the daemon has no CA-issued certificate and often no
// DNS name (peers dial a bare IP discovered via UPnP).
//
// The answer is a self-signed certificate whose SHA-256 fingerprint is
// published in the pairing QR blob. The QR is the out-of-band trust
// anchor: the client pins exactly that fingerprint, so a man-in-the-middle
// cannot present a substitute certificate. Nothing here is hosted; the
// cert lives on the box the operator owns, next to the database.
//
// Pinning is not the whole story on iOS: URLSession independently enforces
// Apple's TLS server-certificate requirements (support.apple.com/en-us/103769)
// and they are not overridable from the client even with a pin. So the
// certificate is bound by SubjectAltName to the public endpoint host the
// client dials, kept under Apple's 398-day validity cap, and signed with
// ECDSA P-256 / SHA-256 and a serverAuth EKU. Because the SAN tracks the
// public endpoint, the cert is regenerated whenever that host changes (or
// it nears expiry), and the served cert and advertised fingerprint always
// move together.
package pairingtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	certFile = "pairing-cert.pem"
	keyFile  = "pairing-key.pem"

	// certValidity is the cert's lifetime. Apple caps TLS server certs at
	// 398 days and rejects longer ones regardless of pinning, so this
	// stays well under that ceiling.
	certValidity = 365 * 24 * time.Hour

	// defaultRenewWindow regenerates a cert once it is this close to
	// expiry, so a long-running daemon never serves an about-to-expire (or
	// expired) cert that iOS would reject.
	defaultRenewWindow = 30 * 24 * time.Hour
)

// Manager owns the daemon's pairing TLS certificate. It loads or generates
// a cert bound to the public endpoint host, serves it via GetCertificate
// (so a rotation takes effect without rebuilding the listener), and
// reports the fingerprint clients pin. All methods are safe for concurrent
// use.
type Manager struct {
	dir         string
	logger      *slog.Logger
	renewWindow time.Duration

	mu   sync.RWMutex
	host string
	cur  *issued
}

// issued is one loaded/generated certificate plus its parsed leaf and the
// fingerprint clients pin (lowercase hex SHA-256 of the leaf DER).
type issued struct {
	cert        *tls.Certificate
	leaf        *x509.Certificate
	fingerprint string
}

// NewManager builds a Manager that persists its cert under dir.
func NewManager(dir string, logger *slog.Logger) *Manager {
	return &Manager{dir: dir, logger: logger, renewWindow: defaultRenewWindow}
}

// EnsureHost loads the persisted cert when it already covers host (SAN
// match and not near expiry), otherwise generates and persists a fresh one
// bound to host. Call once at startup before serving.
func (m *Manager) EnsureHost(host string) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("create pairing dir: %w", err)
	}
	if certPEM, keyPEM, ok := readPair(m.certPath(), m.keyPath()); ok {
		if iss, err := parseIssued(certPEM, keyPEM); err == nil && m.covers(iss.leaf, host) {
			m.store(host, iss)
			return nil
		}
		// Corrupt, bound to a different host, or near expiry: regenerate
		// rather than fail - a stale cert must not wedge pairing forever.
	}
	iss, err := m.generateAndPersist(host)
	if err != nil {
		return err
	}
	m.store(host, iss)
	return nil
}

// SetHost rotates the certificate when host no longer matches what is being
// served (or the current cert is near expiry). Best-effort: on a generation
// error it logs and keeps serving the current cert. Safe to call at runtime
// (e.g. when UPnP rediscovers the public endpoint).
func (m *Manager) SetHost(host string) {
	if host == "" {
		return
	}
	m.mu.RLock()
	cur := m.cur
	m.mu.RUnlock()
	if cur != nil && m.covers(cur.leaf, host) {
		return
	}
	iss, err := m.generateAndPersist(host)
	if err != nil {
		m.logger.Error("pairing cert rotation failed; keeping current cert",
			slog.String("host", host), slog.String("err", err.Error()))
		return
	}
	m.store(host, iss)
	m.logger.Info("pairing cert rotated to match public endpoint",
		slog.String("host", host), slog.String("tls_fingerprint", iss.fingerprint))
}

// Fingerprint returns the lowercase hex SHA-256 of the leaf currently
// served, or "" before EnsureHost runs. Read live so a rotation is
// reflected everywhere the fingerprint is surfaced.
func (m *Manager) Fingerprint() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cur == nil {
		return ""
	}
	return m.cur.fingerprint
}

// TLSConfig returns a server config that serves the current cert via
// GetCertificate, so a rotation takes effect on the next handshake without
// rebuilding the listener.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: m.getCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}

func (m *Manager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cur == nil {
		return nil, errors.New("no pairing certificate loaded")
	}
	return m.cur.cert, nil
}

func (m *Manager) store(host string, iss *issued) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.host = host
	m.cur = iss
}

// covers reports whether leaf is usable for host: its SAN includes host (as
// an IP or DNS name, whichever host is) and it is not within the renew
// window of expiry.
func (m *Manager) covers(leaf *x509.Certificate, host string) bool {
	if leaf == nil {
		return false
	}
	if time.Until(leaf.NotAfter) <= m.renewWindow {
		return false
	}
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, cip := range leaf.IPAddresses {
			if cip.Equal(ip) {
				return true
			}
		}
		return false
	}
	for _, dns := range leaf.DNSNames {
		if dns == host {
			return true
		}
	}
	return false
}

func (m *Manager) certPath() string { return filepath.Join(m.dir, certFile) }
func (m *Manager) keyPath() string  { return filepath.Join(m.dir, keyFile) }

func (m *Manager) generateAndPersist(host string) (*issued, error) {
	certPEM, keyPEM, err := issueFor(host)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(m.keyPath(), keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write pairing key: %w", err)
	}
	if err := os.WriteFile(m.certPath(), certPEM, 0o644); err != nil { //nolint:gosec // a leaf cert is public material
		return nil, fmt.Errorf("write pairing cert: %w", err)
	}
	return parseIssued(certPEM, keyPEM)
}

// readPair reads both PEM files, reporting ok only when both are present.
func readPair(certPath, keyPath string) (certPEM, keyPEM []byte, ok bool) {
	c, err := os.ReadFile(certPath) //nolint:gosec // path is the daemon's own state dir, not user input
	if err != nil {
		return nil, nil, false
	}
	k, err := os.ReadFile(keyPath) //nolint:gosec // same: daemon-controlled path
	if err != nil {
		return nil, nil, false
	}
	return c, k, true
}

func parseIssued(certPEM, keyPEM []byte) (*issued, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load pairing key pair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("pairing certificate is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse pairing leaf: %w", err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return &issued{cert: &cert, leaf: leaf, fingerprint: hex.EncodeToString(sum[:])}, nil
}

// issueFor mints a self-signed cert for host. An IP host becomes an
// IPAddress SAN; any other non-empty host becomes a DNSName SAN (a single
// endpoint is one or the other). iOS reads SAN, not CN, so the SAN is what
// makes its hostname check pass; ECDSA P-256 + SHA-256, a serverAuth EKU,
// and the sub-398-day validity satisfy Apple's remaining requirements.
func issueFor(host string) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate pairing key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	// A small backdate absorbs client/daemon clock skew; the total span
	// (NotAfter - NotBefore) stays at certValidity, under Apple's cap.
	notBefore := time.Now().Add(-1 * time.Hour)
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "fletcher-pairing"},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else if host != "" {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create pairing cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal pairing key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
