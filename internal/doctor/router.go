package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/joshjon/fletcher/internal/network/portmap"
)

// CheckPortMapping probes whether the router will forward the ports the
// daemon needs: the WireGuard UDP port (tunnel) and the pairing TCP port
// (iOS bootstrap). It uses the same NAT-PMP/UPnP path the daemon runs, so
// running it re-installs (or refreshes) the mappings as a harmless,
// short-lived side effect.
//
// Both protocols are probed deliberately: some routers honor UDP mappings
// but silently drop TCP ones (UPnP especially), so a green UDP check alone
// used to hide a pairing port that never forwarded. NAT-PMP is now tried
// first, which fixes many of those routers automatically.
func CheckPortMapping(wireguardPort, pairingPort int) Checker {
	return CheckerFunc(func(ctx context.Context) Result {
		udp, udpErr := probeMapping(ctx, portmap.ProtocolUDP, wireguardPort)
		if udpErr != nil {
			return routerUnreachable(udpErr, wireguardPort)
		}

		_, tcpErr := probeMapping(ctx, portmap.ProtocolTCP, pairingPort)
		if tcpErr != nil {
			return Result{
				Category: CategoryRouter,
				Name:     "Router port-mapping",
				Status:   StatusWarn,
				Detail: fmt.Sprintf(
					"UDP %d forwards via %s, but TCP %d does not (%s); the iOS pairing port will not open automatically - enable NAT-PMP on the router, or forward TCP %d manually",
					wireguardPort, udp.Method, pairingPort, tcpErr, pairingPort),
			}
		}

		return Result{
			Category: CategoryRouter,
			Name:     "Router port-mapping",
			Status:   StatusOK,
			Detail: fmt.Sprintf("forwards installed via %s: UDP %d (tunnel) + TCP %d (pairing), external IP %s",
				udp.Method, wireguardPort, pairingPort, udp.ExternalIP),
		}
	})
}

func probeMapping(ctx context.Context, proto portmap.Protocol, port int) (portmap.Result, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return portmap.Map(probeCtx, portmap.Request{
		InternalPort:  uint16(port), //nolint:gosec // port is a bounded TCP/UDP port
		Protocol:      proto,
		LeaseDuration: 1 * time.Hour,
		Description:   "fletcher doctor probe",
	})
}

func routerUnreachable(err error, listenPort int) Result {
	return Result{
		Category: CategoryRouter,
		Name:     "Router port-mapping",
		Status:   StatusFail,
		Detail:   "no UPnP or NAT-PMP responder on the LAN: " + err.Error(),
		Plan: &PlanStep{
			ID:       "configure-endpoint",
			Priority: PriorityBlocker,
			Title:    "Get a public endpoint working",
			Why:      "Without UPnP or NAT-PMP, the daemon cannot auto-open the port forward, so peers cannot reach it from outside the LAN.",
			Options: []PlanOption{{
				Label: "Enable UPnP or NAT-PMP on your router",
				Steps: []string{
					"# Open your router's admin UI in a browser on your LAN.",
					"# Find the gateway address:",
					"ip route | awk '/default/{print $3}'",
					"# Look for a UPnP and/or NAT-PMP setting. The location varies",
					"# by brand and model; common sections include Advanced, NAT",
					"# Forwarding, Application & Gaming, or Network. Enable it, save.",
					"sudo systemctl restart fletcher",
					"fletcher doctor   # re-run to confirm",
				},
			}, {
				Label: "Forward the ports manually and set the endpoint",
				Steps: []string{
					"# In the router UI, forward UDP " + fmt.Sprintf("%d", listenPort) + " and TCP 51821 to this server's LAN IP.",
					"# Find the LAN IP:",
					"ip -4 addr | grep inet",
					"# Reserve that IP in the router's DHCP settings so it does not drift.",
					"# Find your public IP:",
					"curl -s ifconfig.me",
					"# Apply the endpoint:",
					"sudo systemctl edit fletcher",
					"# Paste:",
					"#   [Service]",
					"#   Environment=FLETCHER_PUBLIC_ENDPOINT=<your-public-ip>:" + fmt.Sprintf("%d", listenPort),
					"#   Environment=FLETCHER_NO_UPNP=true",
					"sudo systemctl restart fletcher",
					"fletcher doctor",
				},
			}, {
				Label: "Skip Fletcher's WireGuard, use a VPN you already run",
				Steps: []string{
					"# Bring your own VPN (Tailscale, Headscale, ZeroTier, plain WireGuard).",
					"# See docs/site/advanced/networking.md \"Mode B: bring your own VPN\".",
				},
			}},
		},
	}
}
