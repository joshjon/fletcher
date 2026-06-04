package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/joshjon/fletcher/internal/network/portmap"
)

// CheckUPnP probes the LAN for a UPnP IGD by attempting a UDP port
// mapping. This is the same call the daemon runs at startup; running
// it again from the doctor is a re-request, which UPnP routers handle
// idempotently. The mapping is installed (or refreshed) as a side
// effect, which is fine: the operator wanted a port forward anyway,
// and the lease is short.
//
// listenPort defaults to 51820 (WireGuard's standard) - pass the
// daemon's configured WireGuardListenPort here if it has been
// overridden.
func CheckUPnP(listenPort int) Checker {
	return CheckerFunc(func(ctx context.Context) Result {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		res, err := portmap.Map(probeCtx, portmap.Request{
			InternalPort:  uint16(listenPort), //nolint:gosec // listenPort is bounded to a UDP port
			ExternalPort:  uint16(listenPort), //nolint:gosec // same
			Protocol:      portmap.ProtocolUDP,
			LeaseDuration: 1 * time.Hour,
			Description:   "fletcher doctor probe",
		})
		if err != nil {
			return Result{
				Category: CategoryRouter,
				Name:     "UPnP IGD",
				Status:   StatusFail,
				Detail:   "not responding: " + err.Error(),
				Plan: &PlanStep{
					ID:       "configure-endpoint",
					Priority: PriorityBlocker,
					Title:    "Get a public endpoint working",
					Why:      "Without UPnP, the daemon cannot auto-set up the port forward, so peers cannot reach it from outside the LAN.",
					Options: []PlanOption{{
						Label: "Enable UPnP on your router",
						Steps: []string{
							"# Open your router's admin UI in a browser on your LAN.",
							"# Find the gateway address:",
							"ip route | awk '/default/{print $3}'",
							"# In the router UI, look for a UPnP setting. The location",
							"# varies by brand and model; common sections include",
							"# Advanced, NAT Forwarding, Application & Gaming, or Network.",
							"# Enable it and save.",
							"sudo systemctl restart fletcher",
							"fletcher doctor   # re-run to confirm",
						},
					}, {
						Label: "Forward the port manually and set the endpoint",
						Steps: []string{
							"# In the router UI, forward UDP " + fmt.Sprintf("%d", listenPort) + " to this server's LAN IP.",
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
							"# See docs/setup.md \"Mode B: bring your own VPN\".",
						},
					}},
				},
			}
		}
		return Result{
			Category: CategoryRouter,
			Name:     "UPnP IGD",
			Status:   StatusOK,
			Detail:   fmt.Sprintf("port forward installed via %s; external port %d on %s", res.Method, res.ExternalPort, res.ExternalIP),
		}
	})
}
