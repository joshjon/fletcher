package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"time"

	"github.com/joshjon/fletcher/internal/network/portmap"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	"github.com/joshjon/fletcher/internal/peer"
)

// defaultWireGuardListenPort is the UDP port wireguard-go binds on by
// default and the port UPnP asks the router to forward. Picked to match
// the WireGuard project's documented standard.
const defaultWireGuardListenPort = 51820

// defaultPairingPort is the public TCP port the pairing listener binds by
// default (and UPnP forwards). Sits next to the WireGuard port so an
// operator forwarding ports manually has one obvious neighbour to open.
const defaultPairingPort = 51821

// pairingPort resolves the configured pairing port, falling back to the
// default when unset.
func pairingPort(cfg Config) int {
	if cfg.PairingPort != 0 {
		return cfg.PairingPort
	}
	return defaultPairingPort
}

// networkSetup is the result of bringing up the daemon's WireGuard
// tunnel plus the (optional) UPnP port-forward at boot. The fields are
// returned so the run group can tear them down on shutdown and so the
// peer-pair handler knows what the discovered endpoint is.
type networkSetup struct {
	// Tunnel is the running WireGuard tunnel, or nil on non-Linux /
	// when no effective public endpoint could be resolved. nil tunnel
	// means peer-pair will fail with a clear failed_precondition.
	Tunnel wireguard.Tunnel
	// EffectivePublicEndpoint is what peer pairing should advertise to
	// clients. Empty when neither the operator nor UPnP supplied one.
	EffectivePublicEndpoint string
	// UPnPResult is the outcome of the auto-port-forward attempt; nil
	// if the attempt was skipped or failed (the failure is logged
	// separately, not propagated as a fatal error).
	UPnPResult *portmap.Result
}

// bringUpNetwork runs the boot-time networking dance: try UPnP to open
// the WireGuard port on the router, decide the effective public
// endpoint (operator-supplied wins, falls back to UPnP discovery), and
// start the WireGuard tunnel if we have an endpoint. Each step logs
// what happened; nothing is fatal at this layer - the daemon still
// runs without a tunnel, just with peer-pair refusing to mint configs.
func bringUpNetwork(
	ctx context.Context,
	cfg Config,
	logger *slog.Logger,
	peers *peer.Service,
	serverKey api_ServerKeyLoader,
	mapper *portmap.Mapper,
) (*networkSetup, error) {
	listenPort := cfg.WireGuardListenPort
	if listenPort == 0 {
		listenPort = defaultWireGuardListenPort
	}

	setup := &networkSetup{EffectivePublicEndpoint: cfg.PublicEndpoint}

	if !cfg.DisableUPnP {
		upCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		res, err := mapper.Ensure(upCtx, portmap.Request{
			Protocol:     portmap.ProtocolUDP,
			InternalPort: uint16(listenPort),
			Description:  "Fletcher (WireGuard tunnel)",
		})
		cancel()
		if err == nil {
			setup.UPnPResult = &res
			if cfg.PublicEndpoint == "" {
				if derived := publicEndpointFromUPnP(&res); derived != "" {
					setup.EffectivePublicEndpoint = derived
					logger.Info("public endpoint derived from router port-mapping", slog.String("endpoint", derived))
				}
			}
		}
	}

	// Tell the peer service what the effective endpoint is so PairPeer
	// renders the right value into client configs.
	peers.SetPublicEndpoint(setup.EffectivePublicEndpoint)

	if setup.EffectivePublicEndpoint == "" {
		logger.Warn("no public endpoint configured and upnp did not provide one; peer pair will fail until --public-endpoint is set or the router accepts upnp")
		return setup, nil
	}

	if runtime.GOOS != "linux" {
		logger.Info("wireguard tunnel skipped: requires linux (development host detected)", slog.String("goos", runtime.GOOS))
		return setup, nil
	}

	priv, err := serverKey.ServerPrivateKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("load server wireguard key: %w", err)
	}

	tunnelAddr, err := serverTunnelAddress(peers.TunnelCIDR())
	if err != nil {
		return nil, fmt.Errorf("compute server tunnel address: %w", err)
	}

	currentPeers, err := loadPeerConfigs(ctx, peers)
	if err != nil {
		return nil, fmt.Errorf("load existing peers: %w", err)
	}

	tunnel := wireguard.NewLinuxTunnel(logger)
	startCfg := wireguard.TunnelConfig{
		InterfaceName: "fletcher0",
		Address:       tunnelAddr,
		ListenPort:    listenPort,
		PrivateKey:    priv,
		Peers:         currentPeers,
	}
	if err := tunnel.Start(ctx, startCfg); err != nil {
		// Don't make the daemon fail; log and continue without a
		// tunnel. CAP_NET_ADMIN missing is the most common cause and
		// the operator deserves a clear path forward.
		logger.Error("wireguard tunnel start failed; continuing without tunnel",
			slog.String("err", err.Error()),
			slog.String("hint", "ensure the daemon runs as root or with CAP_NET_ADMIN; see docs/site/guide/troubleshooting.md"),
		)
		return setup, nil
	}
	setup.Tunnel = tunnel
	return setup, nil
}

// mapTCPPort installs a TCP port-forward through the shared port Mapper
// (which keeps it refreshed and releases it on shutdown). Best-effort:
// failures are logged by the Mapper, not fatal - the operator may have
// forwarded the port manually, or the box may not be the network edge.
func mapTCPPort(ctx context.Context, mapper *portmap.Mapper, port int, desc string) {
	if mapper == nil {
		return
	}
	upCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = mapper.Ensure(upCtx, portmap.Request{
		Protocol:     portmap.ProtocolTCP,
		InternalPort: uint16(port), //nolint:gosec // port is a fixed/bounded value
		Description:  desc,
	})
}

// publicEndpointFromUPnP returns host:port form for the UPnP result, or
// "" if the router reported a private IP (which means it doesn't
// actually have a public address - common with double-NAT, CGNAT, or
// some ISP routers that confuse "external" with "WAN-facing LAN").
func publicEndpointFromUPnP(res *portmap.Result) string {
	if res == nil || res.ExternalIP == "" {
		return ""
	}
	addr, err := netip.ParseAddr(res.ExternalIP)
	if err != nil {
		return ""
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
		return ""
	}
	return net.JoinHostPort(res.ExternalIP, fmt.Sprintf("%d", res.ExternalPort))
}

// serverTunnelAddress derives the server-side tunnel address (.1 of the
// configured /24) from peer.Service's tunnel CIDR.
func serverTunnelAddress(tunnelCIDR string) (string, error) {
	prefix, err := netip.ParsePrefix(tunnelCIDR)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", tunnelCIDR, err)
	}
	server := prefix.Addr().Next()
	if !prefix.Contains(server) {
		return "", errors.New("tunnel cidr too narrow for a server address")
	}
	return fmt.Sprintf("%s/%d", server, prefix.Bits()), nil
}

// loadPeerConfigs returns the full peer registry mapped to the wire
// shape wireguard.TunnelConfig.Peers expects.
func loadPeerConfigs(ctx context.Context, peers *peer.Service) ([]wireguard.PeerConfig, error) {
	all, err := peers.List(ctx, 1<<30, 0)
	if err != nil {
		return nil, err
	}
	out := make([]wireguard.PeerConfig, len(all))
	for i, p := range all {
		out[i] = wireguard.PeerConfig{
			PublicKey:  p.PublicKey,
			AllowedIPs: append([]string(nil), p.AllowedIPs...),
		}
	}
	return out, nil
}

// api_ServerKeyLoader matches the subset of api.ServerKeyProvider the
// networking layer needs. We avoid importing the api package directly
// (circular dep risk) by restating the one method.
type api_ServerKeyLoader interface { //nolint:revive // underscore name avoids confusion with api.ServerKeyProvider
	ServerPrivateKey(ctx context.Context) (wireguard.Key, error)
}
