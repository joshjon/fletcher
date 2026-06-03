// Package wireguard owns the daemon's WireGuard support layer: Curve25519
// key generation, peer configuration storage, and wg-quick-style config
// emission for both server and client devices.
//
// This package does NOT itself run the WireGuard data plane (the TUN
// interface, packet forwarding, key handshake). The DESIGN.md §9 stack
// names wireguard-go for that - but its integration needs OS-level
// privilege handling that's its own phase. For now the daemon manages
// peers and emits configs that the operator applies via wg-quick.
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
