package doctor

import (
	"context"
	"runtime"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/vmm"
)

// CheckFirecrackerVMM reports whether this build carries the Firecracker VMM
// (the firecracker binary and a guest kernel), which the Firecracker runtime
// needs. Release binaries always bundle it; a from-source build needs
// `make fetch-vmm` first. Paired with the /dev/kvm check, this tells the
// operator whether the strong-isolation runtime is available at all.
func CheckFirecrackerVMM() Checker {
	return CheckerFunc(func(_ context.Context) Result {
		const name = "Firecracker VMM"
		if runtime.GOOS != "linux" {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusSkip,
				Detail:   "not applicable on " + runtime.GOOS + "; Firecracker is Linux only",
			}
		}
		if !vmm.Available() {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusWarn,
				Detail:   "not bundled in this build; the Firecracker runtime is unavailable (runc/mock still work)",
				Plan: &PlanStep{
					ID:       "bundle-vmm",
					Priority: PriorityFollowup,
					Title:    "Bundle the Firecracker VMM",
					Why:      "The Firecracker runtime boots microVMs from a bundled firecracker binary and guest kernel. A from-source build does not include them until they are fetched.",
					Options: []PlanOption{{
						Label: "Fetch the VMM and rebuild (from-source builds)",
						Steps: []string{
							"make fetch-vmm",
							"make install",
						},
					}},
				},
			}
		}
		return Result{
			Category: CategoryHost,
			Name:     name,
			Status:   StatusOK,
			Detail:   "bundled (firecracker binary + guest kernel)",
		}
	})
}
