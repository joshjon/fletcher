package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/joshjon/fletcher/internal/doctor"
	"github.com/joshjon/fletcher/internal/host"
)

func hostDiagnostics(cfg Config) func(context.Context, bool) ([]host.Check, string, error) {
	return func(ctx context.Context, external bool) ([]host.Check, string, error) {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		checkers := []doctor.Checker{
			doctor.CheckDaemon(cfg.SocketPath), doctor.CheckTUN(), doctor.CheckKVM(),
			doctor.CheckFirecrackerVMM(), doctor.CheckJobRuntime(cfg.SocketPath),
			doctor.CheckBaseImage(cfg.SocketPath), doctor.CheckDefaultRoutes(),
			doctor.CheckPublicEndpoint(cfg.SocketPath), doctor.CheckPairingEndpoint(cfg.SocketPath),
		}
		if external {
			checkers = append(checkers, doctor.CheckPublicIP(), doctor.CheckProviderReachability())
		}
		results := doctor.Run(ctx, checkers)
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		out := make([]host.Check, 0, len(results))
		for _, r := range results {
			status := "ok"
			switch r.Status {
			case doctor.StatusFail:
				status = "error"
			case doctor.StatusWarn, doctor.StatusSkip:
				status = "warning"
			}
			out = append(out, host.Check{Name: string(r.Category) + ": " + r.Name, Status: status, Detail: r.Detail})
		}
		var plan strings.Builder
		for _, step := range doctor.CollectPlan(results) {
			fmt.Fprintf(&plan, "%s\n%s\n", step.Title, step.Why)
			for _, option := range step.Options {
				fmt.Fprintf(&plan, "%s\n", option.Label)
				for _, line := range option.Steps {
					fmt.Fprintf(&plan, "  %s\n", line)
				}
			}
			plan.WriteString("\n")
		}
		return out, plan.String(), nil
	}
}
