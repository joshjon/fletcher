package doctor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// CheckDaemon verifies the daemon is reachable over the local Unix
// socket and reports its version. The doctor remains useful when the
// daemon is down (most other checks don't require it), but this is the
// first thing to surface so the operator knows whether to look at
// service state or at networking.
func CheckDaemon(socketPath string) Checker {
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
				Category: CategoryDaemon,
				Name:     "Running",
				Status:   StatusFail,
				Detail:   fmt.Sprintf("could not reach the daemon over %s: %v", socketPath, err),
				Plan: &PlanStep{
					ID:       "start-daemon",
					Priority: PriorityBlocker,
					Title:    "Start the Fletcher daemon",
					Why:      "Most operations require the daemon to be running. The rest of these checks may not be meaningful while it is down.",
					Options: []PlanOption{{
						Label: "Start the systemd service",
						Steps: []string{
							"sudo systemctl enable --now fletcher",
							"sudo systemctl status fletcher --no-pager",
							"# If start fails, see: sudo journalctl -u fletcher -n 50 --no-pager",
						},
					}},
				},
			}
		}
		return Result{
			Category: CategoryDaemon,
			Name:     "Running",
			Status:   StatusOK,
			Detail:   fmt.Sprintf("version %s", resp.Msg.GetVersion()),
		}
	})
}
