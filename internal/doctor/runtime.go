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

// CheckRuntimeReady asks the daemon (via Health) whether jobs and sessions can
// actually run: the runtime gives real isolation, and a base image is imported.
// It catches at doctor time the two things that otherwise surface as a raw error
// only when a job or session is first created.
func CheckRuntimeReady(socketPath string) Checker {
	return CheckerFunc(func(ctx context.Context) Result {
		const name = "Job runtime"
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
			// CheckDaemon already reports an unreachable daemon; don't duplicate.
			return Result{Category: CategoryHost, Name: name, Status: StatusSkip, Detail: "daemon unreachable; runtime readiness not assessed"}
		}

		runtimeKind := resp.Msg.GetRuntime()
		snapshotKind := resp.Msg.GetSnapshot()
		stack := fmt.Sprintf("%s runtime / %s snapshot", runtimeKind, snapshotKind)

		if runtimeKind == "mock" {
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusWarn,
				Detail:   "mock runtime: jobs run without real isolation on this host",
				Plan:     mockRuntimePlan(),
			}
		}
		if !resp.Msg.GetBaseImageAvailable() {
			// A blocker, not a warning: without a base image every job and
			// session creation fails, so the runtime is not usable at all.
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusFail,
				Detail:   fmt.Sprintf("%s, but no base image is imported; jobs and sessions can't boot until you import one", stack),
				Plan:     noBaseImagePlan(snapshotKind),
			}
		}
		if resp.Msg.GetBaseImageUpdateAvailable() {
			// Not a blocker: the imported template still boots, but the registry
			// has a newer build (e.g. a security update to the base rootfs).
			return Result{
				Category: CategoryHost,
				Name:     name,
				Status:   StatusWarn,
				Detail:   fmt.Sprintf("%s, base image imported (a newer version is available)", stack),
				Plan:     imageUpdatePlan(),
			}
		}
		return Result{
			Category: CategoryHost,
			Name:     name,
			Status:   StatusOK,
			Detail:   fmt.Sprintf("%s, base image imported", stack),
		}
	})
}

func imageUpdatePlan() *PlanStep {
	return &PlanStep{
		ID:       "update-base-image",
		Priority: PriorityFollowup,
		Title:    "Update the base image",
		Why:      "The registry has a newer build of the default image than the imported template (e.g. a rebuilt rootfs with package updates). Existing jobs and sessions keep their already-cloned forks; new ones will use the updated template.",
		Options: []PlanOption{{
			Label: "Re-pull and re-import the default image",
			Steps: []string{
				"sudo fletcher image update",
			},
		}},
	}
}

func mockRuntimePlan() *PlanStep {
	return &PlanStep{
		ID:       "real-runtime",
		Priority: PriorityFollowup,
		Title:    "Run jobs with real isolation",
		Why:      "The mock runtime executes job commands without a VM or container boundary. It is meant for development on a host without KVM, not for running untrusted agents.",
		Options: []PlanOption{
			{
				Label: "Enable KVM for the Firecracker runtime (preferred)",
				Steps: []string{
					"# Enable VT-x/AMD-V in BIOS/UEFI (bare metal) or nested virt (a guest VM).",
					"lsmod | grep kvm   # confirm the module is loaded",
					"fletcher daemon restart",
				},
			},
			{
				Label: "Or opt into the runc (shared-kernel) runtime",
				Steps: []string{
					"fletcher settings set runtime runc",
					"fletcher settings set snapshot btrfs",
					"fletcher daemon restart",
				},
			},
		},
	}
}

func noBaseImagePlan(snapshotKind string) *PlanStep {
	format := "ext4"
	if snapshotKind == "btrfs" {
		format = "subvolume"
	}
	return &PlanStep{
		ID:       "import-base-image",
		Priority: PriorityBlocker,
		Title:    "Import a base image",
		Why:      "A job or session boots from an imported base-image rootfs. Without one, creating a job or session fails when it tries to clone a template that does not exist.",
		Options: []PlanOption{{
			Label: "Import the published base image",
			Steps: []string{
				fmt.Sprintf("sudo fletcher image import ghcr.io/joshjon/fletcher-base:debian-13 \\\n  --format %s --btrfs-root /var/lib/fletcher/snapshots --name fletcher-base", format),
			},
		}},
	}
}
