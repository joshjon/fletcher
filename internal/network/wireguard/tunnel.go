package wireguard

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// PeerConfig is the per-peer entry the Tunnel needs to install.
type PeerConfig struct {
	// PublicKey is the peer's base64-encoded Curve25519 public key.
	PublicKey Key
	// AllowedIPs is the set of CIDRs routed to this peer (typically a
	// single /32 inside the tunnel subnet).
	AllowedIPs []string
}

// TunnelConfig configures a freshly-started Tunnel.
type TunnelConfig struct {
	// InterfaceName is the network interface created (e.g. "fletcher0").
	// Linux ignores the name if the OS picks one; macOS picks utunN.
	InterfaceName string
	// Address is the server-side tunnel address with CIDR, e.g.
	// "10.99.0.1/24".
	Address string
	// ListenPort is the UDP port the WireGuard device listens on.
	ListenPort int
	// PrivateKey is the server's WireGuard private key.
	PrivateKey Key
	// Peers is the initial peer set; can be updated later via SetPeers.
	Peers []PeerConfig
	// MTU caps the in-tunnel MTU. Zero defaults to 1420 (WireGuard's
	// recommended value: 1500 ethernet - 80 WG overhead).
	MTU int
}

// Tunnel owns a running WireGuard interface. The Linux build wires
// wireguard-go's device library + netlink to actually create the
// kernel-visible interface; the non-Linux build returns a stub that
// errors on Start so the daemon's coordination code can still compile
// and run on macOS during development (with --enable-tunnel=false).
type Tunnel interface {
	// Start brings up the WireGuard interface with cfg. Returns an
	// error if the platform does not support kernel-level networking
	// (Mac) or if the operator lacks CAP_NET_ADMIN.
	Start(ctx context.Context, cfg TunnelConfig) error
	// SetPeers replaces the full peer list. Cheap enough to call on
	// every PairPeer / DeletePeer event; no need for incremental ops.
	SetPeers(ctx context.Context, peers []PeerConfig) error
	// Stop tears down the interface. Safe to call before Start.
	Stop() error
}

// uapiConfig renders a wireguard-go UAPI configuration string for the
// given private key, listen port, and peers. wireguard-go expects keys
// in hex, not base64 - we transcode here.
//
// UAPI reference: https://www.wireguard.com/xplatform/#configuration-protocol
func uapiConfig(privateKey Key, listenPort int, peers []PeerConfig) (string, error) {
	privHex, err := base64KeyToHex(privateKey)
	if err != nil {
		return "", fmt.Errorf("server private key: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", privHex)
	fmt.Fprintf(&b, "listen_port=%d\n", listenPort)
	fmt.Fprintln(&b, "replace_peers=true")
	for _, p := range peers {
		pubHex, err := base64KeyToHex(p.PublicKey)
		if err != nil {
			return "", fmt.Errorf("peer public key: %w", err)
		}
		fmt.Fprintf(&b, "public_key=%s\n", pubHex)
		fmt.Fprintln(&b, "replace_allowed_ips=true")
		for _, cidr := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", cidr)
		}
	}
	return b.String(), nil
}

// base64KeyToHex converts a base64-encoded WireGuard key (the on-wire
// form used in wg-quick configs) to the hex form wireguard-go's UAPI
// expects. Returns an error if the input isn't a valid 32-byte key.
func base64KeyToHex(k Key) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(string(k))
	if err != nil {
		return "", fmt.Errorf("decode base64 key: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("wireguard key must be 32 bytes, got %d", len(raw))
	}
	return hex.EncodeToString(raw), nil
}
