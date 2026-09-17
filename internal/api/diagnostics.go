package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

// RunDiagnostics runs bounded, read-only doctor checks on the host.
func (s *HostService) RunDiagnostics(ctx context.Context, req *connect.Request[fletcherv1.RunDiagnosticsRequest]) (*connect.Response[fletcherv1.RunDiagnosticsResponse], error) {
	if s.manager.Diagnose == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "host diagnostics unavailable")
	}
	checks, plan, err := s.manager.Diagnose(ctx, req.Msg.GetIncludeExternalChecks())
	if err != nil {
		return nil, err
	}
	out := &fletcherv1.RunDiagnosticsResponse{ActionPlan: plan, CheckedAt: time.Now().Unix()}
	for _, c := range checks {
		out.Checks = append(out.Checks, &fletcherv1.HostCheck{Name: c.Name, Status: c.Status, Detail: c.Detail})
	}
	return connect.NewResponse(out), nil
}
