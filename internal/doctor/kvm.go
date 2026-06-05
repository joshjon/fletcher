package doctor

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"slices"
	"strconv"
)

// daemonUser is the unprivileged system user the daemon runs as. It - not the
// CLI invoker running `fletcher doctor` - is what must be able to open /dev/kvm
// for the Firecracker runtime, so that is whose group membership we check.
const daemonUser = "fletcher"

// CheckKVM verifies the host can run the Firecracker runtime: /dev/kvm must
// exist and the daemon user must be able to open it. Firecracker boots each
// fork as a KVM microVM (the default, strong-isolation tier); without KVM the
// operator falls back to the degraded runc runtime, so a missing device is a
// warning, not a hard failure. Linux-only.
func CheckKVM() Checker {
	return CheckerFunc(func(_ context.Context) Result {
		const name = "/dev/kvm"
		if runtime.GOOS != "linux" {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusSkip,
				Detail:   fmt.Sprintf("not applicable on %s; Firecracker is Linux + KVM only", runtime.GOOS),
			}
		}
		info, err := os.Stat("/dev/kvm")
		if err != nil {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusWarn,
				Detail:   "absent; the Firecracker runtime is unavailable (the runc fallback still works)",
				Plan:     kvmAbsentPlan(),
			}
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusFail,
				Detail:   "exists but is not a character device; the path is wrong or the node is corrupted",
			}
		}
		// Present. The daemon runs as daemonUser, which normally reaches the
		// device through membership in its owning group (typically "kvm").
		gid, ok := deviceGID(info)
		if !ok {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusOK,
				Detail:   "present (group ownership not determinable; assuming accessible)",
			}
		}
		if detail, accessible := daemonKVMAccess(gid); !accessible {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusWarn,
				Detail:   detail,
				Plan:     kvmGroupPlan(groupName(gid)),
			}
		}
		return Result{
			Category: CategoryHost,
			Name:     name,
			Status:   StatusOK,
			Detail:   "present and accessible to the daemon",
		}
	})
}

// daemonKVMAccess reports whether daemonUser can reach a /dev/kvm owned by
// deviceGID, with a human-readable detail when it cannot. When the daemon user
// does not exist (e.g. running from source without `make install`) it does not
// cry wolf: there is no daemon to lack access.
func daemonKVMAccess(deviceGID uint32) (detail string, accessible bool) {
	u, err := user.Lookup(daemonUser)
	if err != nil {
		return "present (daemon user not installed; cannot assess group access)", true
	}
	gids, err := u.GroupIds()
	if err != nil {
		return "present (could not read daemon user groups; assuming accessible)", true
	}
	if slices.Contains(gids, strconv.FormatUint(uint64(deviceGID), 10)) {
		return "", true
	}
	return fmt.Sprintf("daemon user %q is not in the %q group that owns /dev/kvm",
		daemonUser, groupName(deviceGID)), false
}

func groupName(gid uint32) string {
	if g, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10)); err == nil {
		return g.Name
	}
	return strconv.FormatUint(uint64(gid), 10)
}

func kvmGroupPlan(group string) *PlanStep {
	return &PlanStep{
		ID:       "kvm-group",
		Priority: PriorityFollowup,
		Title:    "Grant the daemon access to /dev/kvm",
		Why:      "The Firecracker runtime boots each fork as a KVM microVM. The daemon runs as an unprivileged user that must belong to the group owning /dev/kvm, or VM creation fails with a permission error.",
		Options: []PlanOption{{
			Label: fmt.Sprintf("Add the daemon user to the %q group", group),
			Steps: []string{
				fmt.Sprintf("sudo usermod -aG %s %s", group, daemonUser),
				"# Restart so the daemon picks up the new group:",
				"fletcher daemon restart",
			},
		}},
	}
}

func kvmAbsentPlan() *PlanStep {
	return &PlanStep{
		ID:       "enable-kvm",
		Priority: PriorityFollowup,
		Title:    "Enable KVM to use the Firecracker runtime",
		Why:      "Firecracker needs hardware virtualization (/dev/kvm) for its microVM isolation. Without it Fletcher can still run on the degraded-isolation runc runtime, but you lose the VM boundary.",
		Options: []PlanOption{
			{
				Label: "Enable virtualization for this machine",
				Steps: []string{
					"# Bare metal: enable VT-x/AMD-V in the BIOS/UEFI.",
					"# A guest VM: enable nested virtualization on the hypervisor,",
					"# then confirm the module is loaded:",
					"lsmod | grep kvm",
				},
			},
			{
				Label: "Or stay on the runc fallback",
				Steps: []string{
					"fletcher settings set runtime runc",
				},
			},
		},
	}
}
