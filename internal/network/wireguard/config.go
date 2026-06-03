package wireguard

import (
	"fmt"
	"strings"
)

// ServerConfig is the inputs for emitting a server-side wg-quick conf.
type ServerConfig struct {
	PrivateKey Key
	// Address is the server's WireGuard interface address with CIDR, e.g.
	// "10.99.0.1/24".
	Address string
	// ListenPort is the UDP port the server listens on.
	ListenPort int
	// Peers is the list of allowed peers to include as [Peer] sections.
	Peers []PeerEntry
	// PostUp / PostDown are optional commands run by wg-quick.
	PostUp   []string
	PostDown []string
}

// ClientConfig is the inputs for emitting a client-side wg-quick conf.
type ClientConfig struct {
	PrivateKey Key
	// Address is the client's interface address with CIDR, e.g.
	// "10.99.0.2/32".
	Address string
	// DNS is the optional list of DNS servers to push to the client.
	DNS []string
	// ServerPublicKey identifies the server peer.
	ServerPublicKey Key
	// Endpoint is host:port the client dials.
	Endpoint string
	// AllowedIPs tells the client which traffic to send over the tunnel
	// (e.g., "10.99.0.0/24" for split-tunnel; "0.0.0.0/0, ::/0" for full).
	AllowedIPs []string
	// PersistentKeepalive in seconds; 0 disables.
	PersistentKeepalive int
}

// PeerEntry is one allowed peer in a server config.
type PeerEntry struct {
	Name       string
	PublicKey  Key
	AllowedIPs []string
}

// RenderServer emits the wg-quick config for cfg.
func RenderServer(cfg ServerConfig) string {
	var b strings.Builder
	fmt.Fprintln(&b, "[Interface]")
	fmt.Fprintf(&b, "PrivateKey = %s\n", cfg.PrivateKey)
	if cfg.Address != "" {
		fmt.Fprintf(&b, "Address = %s\n", cfg.Address)
	}
	if cfg.ListenPort > 0 {
		fmt.Fprintf(&b, "ListenPort = %d\n", cfg.ListenPort)
	}
	for _, line := range cfg.PostUp {
		fmt.Fprintf(&b, "PostUp = %s\n", line)
	}
	for _, line := range cfg.PostDown {
		fmt.Fprintf(&b, "PostDown = %s\n", line)
	}
	for _, p := range cfg.Peers {
		fmt.Fprintln(&b)
		if p.Name != "" {
			fmt.Fprintf(&b, "# %s\n", p.Name)
		}
		fmt.Fprintln(&b, "[Peer]")
		fmt.Fprintf(&b, "PublicKey = %s\n", p.PublicKey)
		if len(p.AllowedIPs) > 0 {
			fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(p.AllowedIPs, ", "))
		}
	}
	return b.String()
}

// RenderClient emits the wg-quick config for cfg.
func RenderClient(cfg ClientConfig) string {
	var b strings.Builder
	fmt.Fprintln(&b, "[Interface]")
	fmt.Fprintf(&b, "PrivateKey = %s\n", cfg.PrivateKey)
	if cfg.Address != "" {
		fmt.Fprintf(&b, "Address = %s\n", cfg.Address)
	}
	if len(cfg.DNS) > 0 {
		fmt.Fprintf(&b, "DNS = %s\n", strings.Join(cfg.DNS, ", "))
	}

	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Peer]")
	fmt.Fprintf(&b, "PublicKey = %s\n", cfg.ServerPublicKey)
	if cfg.Endpoint != "" {
		fmt.Fprintf(&b, "Endpoint = %s\n", cfg.Endpoint)
	}
	if len(cfg.AllowedIPs) > 0 {
		fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(cfg.AllowedIPs, ", "))
	}
	if cfg.PersistentKeepalive > 0 {
		fmt.Fprintf(&b, "PersistentKeepalive = %d\n", cfg.PersistentKeepalive)
	}
	return b.String()
}
