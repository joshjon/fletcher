package doctor

import (
	"context"
	"net/http"
	"time"
)

// providerProbes are the upstream endpoints the doctor pings to
// confirm outbound reachability. Order matters - the first failure
// short-circuits the rest because they all imply the same upstream-
// reachability issue.
//
// We intentionally probe the OpenAI and Anthropic root URLs even if
// the daemon currently only routes to Anthropic: a future provider
// addition shouldn't silently regress reachability without the doctor
// noticing.
var providerProbes = []struct {
	label string
	url   string
}{
	{"api.anthropic.com", "https://api.anthropic.com"},
	{"api.openai.com", "https://api.openai.com"},
}

// CheckProviderReachability does a HEAD request against each upstream.
// We don't authenticate - a 401 / 404 is fine; we're testing whether
// the network can talk to the host, not whether keys work.
func CheckProviderReachability() Checker {
	return CheckerFunc(func(ctx context.Context) Result {
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		client := &http.Client{Timeout: 5 * time.Second}

		for _, p := range providerProbes {
			req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, p.url, nil)
			if err != nil {
				return Result{
					Category: CategoryProviders,
					Name:     "Outbound reachability",
					Status:   StatusWarn,
					Detail:   "could not build probe request: " + err.Error(),
				}
			}
			resp, err := client.Do(req)
			if err != nil {
				return Result{
					Category: CategoryProviders,
					Name:     "Outbound reachability",
					Status:   StatusFail,
					Detail:   p.label + " unreachable: " + err.Error(),
					Plan: &PlanStep{
						ID:       "fix-outbound",
						Priority: PriorityBlocker,
						Title:    "Restore outbound internet reachability",
						Why:      "The daemon cannot proxy model calls if it can't reach the upstream providers. This usually means DNS, the default route, or a host firewall is blocking outbound HTTPS.",
						Options: []PlanOption{{
							Label: "Verify the basics",
							Steps: []string{
								"# DNS:",
								"getent hosts api.anthropic.com",
								"# Direct reachability:",
								"curl -I https://api.anthropic.com",
								"# Outbound firewall (ufw shown; firewalld / nftables look different):",
								"sudo ufw status verbose",
							},
						}},
					},
				}
			}
			_ = resp.Body.Close()
		}
		return Result{
			Category: CategoryProviders,
			Name:     "Outbound reachability",
			Status:   StatusOK,
			Detail:   "all configured provider hosts responded",
		}
	})
}
