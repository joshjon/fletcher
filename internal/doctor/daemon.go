package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
		return socketPermissionResult(socketPath)
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

// socketPermissionResult builds the remediation for a permission-denied
// socket. The fix differs by membership state, so the plan is tailored:
//
//   - Not yet in the group: add the user, then just re-run - the CLI
//     re-execs itself under the group automatically on the next invocation,
//     with newgrp/login as a fallback.
//   - Already in the group on disk but the daemon still refused us: the
//     auto-activation could not run (typically sg(1) is missing), so the
//     operator has to activate the group themselves this once.
func socketPermissionResult(socketPath string) Result {
	group, gid, known := socketGroup(socketPath)
	if !known {
		group = "fletcher"
	}

	var why string
	var steps []string
	if known && userInGroup(gid) {
		why = fmt.Sprintf("Your account already belongs to the %q group, but this login session predates the change, so the group is not active yet. The CLI normally re-activates it for you automatically; since that did not happen, the sg(1) helper is probably missing.", group)
		steps = []string{
			"# Activate the group in this shell:",
			"newgrp " + group,
			"# (or log out and back in - it sticks for every new shell)",
			"# Then re-run:",
			"fletcher doctor",
		}
	} else {
		why = fmt.Sprintf("The daemon's socket is restricted to the %q group. Your account is not a member yet, so the CLI cannot connect.", group)
		steps = []string{
			"sudo usermod -aG " + group + " $USER",
			"# Then just re-run fletcher - it picks up the new group",
			"# automatically. If it does not, log out and back in, or run:",
			"newgrp " + group,
			"# Then:",
			"fletcher doctor",
		}
	}

	return Result{
		Category: CategoryDaemon,
		Name:     "Running",
		Status:   StatusFail,
		Detail:   "the socket exists but your user does not have permission to talk to it",
		Plan: &PlanStep{
			ID:       "socket-permission",
			Priority: PriorityBlocker,
			Title:    "Grant your user access to the daemon socket",
			Why:      why,
			Options:  []PlanOption{{Label: "Join the daemon's group", Steps: steps}},
		},
	}
}

// socketGroup returns the name and GID of the group owning the daemon socket.
// It falls back to the socket's parent directory when the socket inode itself
// is not statable (the 0750 runtime directory denies stat to non-members);
// both carry the same ownership under systemd.
func socketGroup(socketPath string) (name string, gid int, ok bool) {
	for _, p := range []string{socketPath, filepath.Dir(socketPath)} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		st, isStat := fi.Sys().(*syscall.Stat_t)
		if !isStat {
			continue
		}
		g := int(st.Gid)
		if grp, err := user.LookupGroupId(strconv.Itoa(g)); err == nil {
			return grp.Name, g, true
		}
		return "", g, true
	}
	return "", 0, false
}

// userInGroup reports whether the current user is a member of gid according
// to /etc/group, independent of this process's active group set.
func userInGroup(gid int) bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	ids, err := u.GroupIds()
	if err != nil {
		return false
	}
	target := strconv.Itoa(gid)
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
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
