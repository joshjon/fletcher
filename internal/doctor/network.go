package doctor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// defaultPublicIPProbeURL is the third-party endpoint the doctor hits
// to discover the external IP. icanhazip.com is a long-running,
// privacy-respectful service that returns just the caller's IP as
// plain text regardless of User-Agent (unlike ifconfig.me, which
// serves HTML to non-curl clients by default).
const defaultPublicIPProbeURL = "https://icanhazip.com"

// cgnatPrefix is the IETF-assigned range (RFC 6598) that ISPs use
// for Carrier-Grade NAT. An address in this range means the
// "public" IP reported by ifconfig.me actually sits behind the
// ISP's own NAT, and the operator cannot host an externally-
// reachable service from their router.
const cgnatPrefix = "100.64.0.0/10"

// CheckPublicIP fetches the host's externally-visible IP and flags
// CGNAT. The result also carries the IP itself in Detail so the
// operator can copy-paste it into FLETCHER_PUBLIC_ENDPOINT when
// taking the manual path.
func CheckPublicIP() Checker {
	return CheckerFunc(func(ctx context.Context) Result {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, defaultPublicIPProbeURL, nil)
		if err != nil {
			return failPublicIP("could not build probe request: " + err.Error())
		}
		req.Header.Set("user-agent", "fletcher-doctor")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return Result{
				Category: CategoryReachability,
				Name:     "Public IP",
				Status:   StatusWarn,
				Detail:   "could not determine public IP (no internet, or " + defaultPublicIPProbeURL + " is unreachable): " + err.Error(),
			}
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		if err != nil {
			return failPublicIP("could not read probe response: " + err.Error())
		}
		ipStr := strings.TrimSpace(string(body))
		addr, err := netip.ParseAddr(ipStr)
		if err != nil {
			// Don't echo the body - if the probe service returns HTML
			// or an error page, dumping it here makes the doctor noisy.
			return failPublicIP(fmt.Sprintf("probe at %s returned non-IP response (HTTP %d)", defaultPublicIPProbeURL, resp.StatusCode))
		}

		_, cgnatNet, _ := net.ParseCIDR(cgnatPrefix)
		if cgnatNet.Contains(net.ParseIP(addr.String())) {
			return Result{
				Category: CategoryReachability,
				Name:     "Public IP",
				Status:   StatusFail,
				Detail:   "in CGNAT range; you cannot host an externally-reachable service from this network",
				Plan: &PlanStep{
					ID:       "configure-endpoint",
					Priority: PriorityBlocker,
					Title:    "Make the daemon reachable from the public internet",
					Why:      "Your ISP places this network behind Carrier-Grade NAT, so port forwarding cannot help. The daemon needs an externally-reachable address by some other route.",
					Options: []PlanOption{{
						Label: "Use a VPN you already run (recommended)",
						Steps: []string{
							"# Bring your own VPN (Tailscale, Headscale, ZeroTier, plain WireGuard).",
							"# See docs/site/advanced/networking.md \"Mode B: bring your own VPN\" for the bind config.",
						},
					}, {
						Label: "Use a small public-IP relay (advanced)",
						Steps: []string{
							"# A $5/mo VPS with a public IPv4 + WireGuard can relay traffic to the daemon.",
							"# Out of scope for this doc; treat as the escape hatch when no VPN is acceptable.",
						},
					}},
				},
			}
		}
		return Result{
			Category: CategoryReachability,
			Name:     "Public IP",
			Status:   StatusOK,
			Detail:   "not in CGNAT range; the network can receive incoming connections",
		}
	})
}

func failPublicIP(detail string) Result {
	return Result{
		Category: CategoryReachability,
		Name:     "Public IP",
		Status:   StatusWarn,
		Detail:   detail,
	}
}

// CheckDefaultRoutes warns when the host has multiple default routes
// on the same subnet, which can cause asymmetric paths that break
// WireGuard handshakes intermittently. Returns Status OK when there
// is exactly one default route, Warn otherwise.
func CheckDefaultRoutes() Checker {
	return CheckerFunc(func(_ context.Context) Result {
		routes, err := defaultRoutes()
		if err != nil {
			return Result{
				Category: CategoryNetwork,
				Name:     "Default routes",
				Status:   StatusWarn,
				Detail:   "could not enumerate routes: " + err.Error(),
			}
		}
		if len(routes) == 0 {
			return Result{
				Category: CategoryNetwork,
				Name:     "Default routes",
				Status:   StatusFail,
				Detail:   "no default route; the host cannot reach the public internet",
				Plan: &PlanStep{
					ID:       "fix-default-route",
					Priority: PriorityBlocker,
					Title:    "Restore a default route on this host",
					Why:      "Without a default route, the daemon cannot reach upstream providers and peers cannot reach the daemon.",
					Options: []PlanOption{{
						Label: "Restart the network stack",
						Steps: []string{
							"# With NetworkManager:",
							"sudo systemctl restart NetworkManager",
							"# With systemd-networkd:",
							"sudo systemctl restart systemd-networkd",
							"# Verify a default route exists:",
							"ip route",
						},
					}},
				},
			}
		}
		if len(routes) == 1 {
			return Result{
				Category: CategoryNetwork,
				Name:     "Default routes",
				Status:   StatusOK,
				Detail:   fmt.Sprintf("single default route via %s", routes[0]),
			}
		}
		return Result{
			Category: CategoryNetwork,
			Name:     "Default routes",
			Status:   StatusWarn,
			Detail:   fmt.Sprintf("%d default routes detected (interfaces: %s)", len(routes), strings.Join(routes, ", ")),
			Plan: &PlanStep{
				ID:       "single-default-route",
				Priority: PriorityFollowup,
				Title:    "Reduce to a single default route",
				Why:      "Multiple default routes on the same subnet can cause asymmetric paths that break WireGuard handshakes intermittently. Pick one interface for outbound traffic.",
				Options: []PlanOption{{
					Label: "Disable the secondary interface (most common with both wired + wifi connected)",
					Steps: []string{
						"# Identify interfaces with default routes:",
						"ip route | awk '/default/{print $5}'",
						"# Disable the secondary one (example uses NetworkManager):",
						"nmcli device disconnect <interface-name>",
						"# Prevent auto-reconnect:",
						"nmcli connection modify <connection-name> connection.autoconnect no",
					},
				}, {
					Label: "Keep both interfaces but use different metrics",
					Steps: []string{
						"# Set the wired connection's metric lower (preferred):",
						"# Exact command depends on your network manager - see its docs.",
					},
				}},
			},
		}
	})
}
