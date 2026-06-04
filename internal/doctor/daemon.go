package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// CheckDaemon verifies the daemon is reachable over the local Unix
// socket and reports its version. The doctor remains useful when the
// daemon is down (most other checks don't require it), but this is the
// first thing to surface so the operator knows whether to look at
// service state, socket-group membership, or networking.
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
			return diagnoseDaemonError(socketPath, err)
		}
		return Result{
			Category: CategoryDaemon,
			Name:     "Running",
			Status:   StatusOK,
			Detail:   fmt.Sprintf("version %s", resp.Msg.GetVersion()),
		}
	})
}

// diagnoseDaemonError maps the raw connect error into the right plan.
// Permission-denied on the socket and "socket missing entirely" produce
// different plans because the fixes are different: group membership in
// the first case, starting the service in the second.
func diagnoseDaemonError(socketPath string, err error) Result {
	detail := fmt.Sprintf("could not reach the daemon over %s: %v", socketPath, err)

	switch {
	case isPermissionDenied(err):
		return Result{
			Category: CategoryDaemon,
			Name:     "Running",
			Status:   StatusFail,
			Detail:   "the socket exists but your user does not have permission to talk to it",
			Plan: &PlanStep{
				ID:       "socket-permission",
				Priority: PriorityBlocker,
				Title:    "Grant your user access to the daemon socket",
				Why:      "The daemon's socket lives under a directory the systemd service restricts to its own group. The CLI cannot connect until your account is a member of that group.",
				Options: []PlanOption{{
					Label: "Add your user to the fletcher group",
					Steps: []string{
						"sudo usermod -aG fletcher $USER",
						"# Log out and back in so the new group takes effect,",
						"# or apply it to the current shell with:",
						"newgrp fletcher",
						"# Then re-run:",
						"fletcher doctor",
					},
				}},
			},
		}
	default:
		return Result{
			Category: CategoryDaemon,
			Name:     "Running",
			Status:   StatusFail,
			Detail:   detail,
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
}

// isPermissionDenied returns true when err (typically wrapped by
// connect-go and net.OpError) ultimately is a syscall EACCES or any
// "permission denied" failure mode. We check both fs.ErrPermission via
// errors.Is and the literal error-string suffix because the unix-
// socket dial path doesn't always wrap predictably.
func isPermissionDenied(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "permission denied")
}
