package doctor

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

// CheckTUN verifies /dev/net/tun is present and accessible. The
// daemon-managed WireGuard tunnel needs this; without it the tunnel
// can't come up regardless of capabilities. Linux-only - other
// platforms return StatusSkip because the doctor still wants to report
// "this check doesn't apply here" rather than silently omit.
func CheckTUN() Checker {
	return CheckerFunc(func(_ context.Context) Result {
		if runtime.GOOS != "linux" {
			return Result{
				Category: CategoryHost,
				Name:     "/dev/net/tun",
				Status:   StatusSkip,
				Detail:   fmt.Sprintf("not applicable on %s; WireGuard tunnel is Linux-only", runtime.GOOS),
			}
		}
		info, err := os.Stat("/dev/net/tun")
		if err != nil {
			return Result{
				Category: CategoryHost,
				Name:     "/dev/net/tun",
				Status:   StatusFail,
				Detail:   fmt.Sprintf("not accessible: %v", err),
				Plan: &PlanStep{
					ID:       "enable-tun",
					Priority: PriorityBlocker,
					Title:    "Make the TUN device available",
					Why:      "The daemon creates a WireGuard interface via /dev/net/tun. Without it, the tunnel cannot come up.",
					Options: []PlanOption{{
						Label: "Load the tun kernel module",
						Steps: []string{
							"# Try loading the module:",
							"sudo modprobe tun",
							"# Persist across reboots (location varies by distro):",
							"echo tun | sudo tee /etc/modules-load.d/tun.conf",
							"# Confirm:",
							"ls -l /dev/net/tun",
						},
					}},
				},
			}
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			return Result{
				Category: CategoryHost,
				Name:     "/dev/net/tun",
				Status:   StatusFail,
				Detail:   "exists but is not a character device; the file is corrupted or the wrong path",
			}
		}
		return Result{
			Category: CategoryHost,
			Name:     "/dev/net/tun",
			Status:   StatusOK,
			Detail:   "character device present and accessible",
		}
	})
}
