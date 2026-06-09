package doctor

import (
	"context"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// CheckPublicEndpoint asks the daemon what public endpoint it will advertise
// to paired devices. This closes the gap the UPnP probe alone leaves: the
// daemon captures its endpoint only at startup, so if UPnP was enabled (or
// FLETCHER_PUBLIC_ENDPOINT set) after the daemon last started, the router
// probe reports healthy while the daemon still holds an empty endpoint and
// `fletcher peer pair` fails. Reading the daemon's actual state catches that.
//
// When the endpoint is missing it contributes to the same "configure-endpoint"
// plan the UPnP check uses, so the two collapse into one coherent step: if
// UPnP is healthy this stands alone telling the operator to restart; if UPnP
// is also failing the UPnP step leads and this adds the restart option.
func CheckPublicEndpoint(socketPath string) Checker {
	return CheckerFunc(func(ctx context.Context) Result {
		client := &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		}
		admin := fletcherv1connect.NewAdminServiceClient(client, "http://unix")
		resp, err := admin.Health(ctx, connect.NewRequest(&fletcherv1.HealthRequest{}))
		if err != nil {
			// The daemon is down or unreachable; CheckDaemon already
			// reports that with the right remediation. Don't double up.
			return Result{
				Category: CategoryReachability,
				Name:     "Daemon public endpoint",
				Status:   StatusSkip,
				Detail:   "skipped: daemon not reachable (see DAEMON above)",
			}
		}

		if endpoint := resp.Msg.GetPublicEndpoint(); endpoint != "" {
			return Result{
				Category: CategoryReachability,
				Name:     "Daemon public endpoint",
				Status:   StatusOK,
				Detail:   "daemon advertises " + endpoint + " to paired devices",
			}
		}

		return Result{
			Category: CategoryReachability,
			Name:     "Daemon public endpoint",
			Status:   StatusFail,
			Detail:   "daemon holds no public endpoint; peer pairing will fail",
			Plan: &PlanStep{
				ID:       "configure-endpoint",
				Priority: PriorityBlocker,
				Title:    "Restart the daemon to apply its public endpoint",
				Why:      "The daemon resolves its public endpoint only at startup. If you enabled UPnP or set FLETCHER_PUBLIC_ENDPOINT after it last started, it has not picked the change up yet, so peer pairing fails even though the router probe looks fine.",
				Options: []PlanOption{{
					Label: "Restart so the daemon re-resolves its endpoint",
					Steps: []string{
						"sudo systemctl restart fletcher",
						"fletcher doctor   # the endpoint should now be set",
						"# Still empty afterwards? The router is not offering a",
						"# forward - see the ROUTER step to enable UPnP or set an",
						"# endpoint manually.",
					},
				}},
			},
		}
	})
}

// CheckPairingEndpoint asks the daemon whether its public pairing listener is
// up - the TLS endpoint the native (iOS) app dials to complete pairing before
// the WireGuard tunnel exists. A non-empty pairing endpoint means the listener
// bound and is being advertised; empty means the iOS app cannot pair (laptop
// and CLI pairing are unaffected, which is why this warns rather than fails).
//
// The empty case overlaps with a missing public endpoint, which
// CheckPublicEndpoint already reports as a blocker with a restart plan, so this
// stays a warning and does not duplicate that remediation.
func CheckPairingEndpoint(socketPath string) Checker {
	return CheckerFunc(func(ctx context.Context) Result {
		client := &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		}
		admin := fletcherv1connect.NewAdminServiceClient(client, "http://unix")
		resp, err := admin.Health(ctx, connect.NewRequest(&fletcherv1.HealthRequest{}))
		if err != nil {
			return Result{
				Category: CategoryReachability,
				Name:     "iOS pairing endpoint",
				Status:   StatusSkip,
				Detail:   "skipped: daemon not reachable (see DAEMON above)",
			}
		}

		if endpoint := resp.Msg.GetPairingEndpoint(); endpoint != "" {
			return Result{
				Category: CategoryReachability,
				Name:     "iOS pairing endpoint",
				Status:   StatusOK,
				Detail:   "daemon advertises " + endpoint + " for the iOS app to complete pairing",
			}
		}

		return Result{
			Category: CategoryReachability,
			Name:     "iOS pairing endpoint",
			Status:   StatusWarn,
			Detail:   "no pairing listener; the iOS app cannot pair (laptop/CLI pairing is unaffected). Needs a public endpoint and a daemon restart.",
		}
	})
}
