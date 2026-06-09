// Package pairingtls provides the self-signed TLS material for the
// daemon's public pairing listener. Pairing is a bootstrap that happens
// before a client is a WireGuard peer, so CompletePair must be reachable
// over the public internet rather than the tunnel. That public channel
// carries the one-time pairing code, so it has to be confidential and
// MITM-proof - but the daemon has no CA-issued certificate and often no
// DNS name (peers dial a bare IP discovered via UPnP).
//
// The answer is a long-lived self-signed certificate whose SHA-256
// fingerprint is published in the pairing QR blob. The QR is the
// out-of-band trust anchor: the client pins exactly that fingerprint and
// skips CA/hostname validation, so a man-in-the-middle cannot present a
// substitute certificate. Nothing here is hosted; the cert lives on the
// box the operator owns, next to the database.
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
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	certFile = "pairing-cert.pem"
	keyFile  = "pairing-key.pem"
	// certValidity is deliberately long: the certificate is pinned by
	// fingerprint, not trusted via a CA, so rotation only matters if the
	// key is compromised. A short expiry would just break pairing for
	// boxes that run untouched for years.
	certValidity = 100 * 365 * 24 * time.Hour
)

// Material is the loaded pairing certificate plus the fingerprint clients
// pin against.
type Material struct {
	// Certificate is the parsed key pair the pairing listener serves.
	Certificate tls.Certificate
	// Fingerprint is the lowercase hex SHA-256 of the leaf certificate's
	// DER bytes - the value published in the pairing blob and pinned by
	// the client.
	Fingerprint string
}

// TLSConfig returns a server TLS config that presents the pairing
// certificate. TLS 1.2 is the floor; clients pin the leaf, so no client
// auth or CA chain is involved.
func (m Material) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{m.Certificate},
		MinVersion:   tls.VersionTLS12,
	}
}

// EnsureCert loads the pairing certificate from dir, generating and
// persisting a fresh self-signed one (0600 key, 0644 cert) the first time.
// The returned fingerprint is stable across restarts because the cert is
// persisted - a paired-but-incomplete client's blob stays valid after a
// daemon bounce.
func EnsureCert(dir string) (Material, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Material{}, fmt.Errorf("create pairing dir: %w", err)
	}
	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)

	if certPEM, keyPEM, ok := readPair(certPath, keyPath); ok {
		mat, err := materialFromPEM(certPEM, keyPEM)
		if err == nil {
			return mat, nil
		}
		// A corrupt or unreadable pair is regenerated rather than fatal:
		// the fingerprint changes, but a stale half-written cert should
		// not wedge the daemon out of ever pairing again.
	}

	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		return Material{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return Material{}, fmt.Errorf("write pairing key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil { //nolint:gosec // a leaf cert is public material
		return Material{}, fmt.Errorf("write pairing cert: %w", err)
	}
	return materialFromPEM(certPEM, keyPEM)
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

func materialFromPEM(certPEM, keyPEM []byte) (Material, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Material{}, fmt.Errorf("load pairing key pair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return Material{}, errors.New("pairing certificate is empty")
	}
	return Material{Certificate: cert, Fingerprint: fingerprint(cert.Certificate[0])}, nil
}

// fingerprint is the lowercase hex SHA-256 of a DER-encoded certificate.
// The client recomputes this over the leaf it is offered and compares.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func generateSelfSigned() (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate pairing key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	// NotBefore is backdated a day so a client whose clock lags the
	// daemon's still accepts the cert. Hostname/SAN is irrelevant: the
	// client pins the fingerprint and does not validate the name.
	notBefore := time.Now().Add(-24 * time.Hour)
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "fletcher-pairing"},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
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
