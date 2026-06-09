// Package wireguard owns the daemon's WireGuard support layer:
// Curve25519 key generation, peer configuration storage, wg-quick-style
// config emission (for the power-user `peer add` flow that hands the
// operator a text config), and the Tunnel abstraction that drives the
// in-daemon data plane via embedded wireguard-go + netlink.
//
// The Tunnel implementation is Linux-only (tunnel_linux.go); other
// platforms get a stub that errors on Start so the daemon's coordination
// code still compiles and runs on macOS for development. Bringing the
// interface up requires CAP_NET_ADMIN; the systemd unit grants it via
// AmbientCapabilities.
package wireguard

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Key is a base64-encoded 32-byte Curve25519 key - the on-wire form
// WireGuard config files use.
type Key string

// Keypair pairs a private and public key.
type Keypair struct {
	Private Key
	Public  Key
}

// GenerateKeypair returns a fresh Curve25519 keypair, encoded base64
// for use in wg-quick configs. Uses stdlib crypto/ecdh (Go ≥ 1.20)
// so there's no external dependency.
func GenerateKeypair() (Keypair, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return Keypair{}, fmt.Errorf("generate x25519 key: %w", err)
	}
	return Keypair{
		Private: Key(base64.StdEncoding.EncodeToString(priv.Bytes())),
		Public:  Key(base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())),
	}, nil
}

// PublicFromPrivate derives the public key for a base64-encoded private.
func PublicFromPrivate(priv Key) (Key, error) {
	raw, err := base64.StdEncoding.DecodeString(string(priv))
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	curve := ecdh.X25519()
	k, err := curve.NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	return Key(base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())), nil
}

// ValidatePublicKey returns nil if k decodes as a 32-byte X25519 public
// key in wg-quick base64 form. Used to reject malformed client-supplied
// keys before they enter the peer registry.
func ValidatePublicKey(k Key) error {
	if k == "" {
		return fmt.Errorf("public key is required")
	}
	raw, err := base64.StdEncoding.DecodeString(string(k))
	if err != nil {
		return fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != 32 {
		return fmt.Errorf("public key is %d bytes; expected 32", len(raw))
	}
	return nil
}
