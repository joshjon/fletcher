package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/buildinfo"
	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/host"
)

// HostService exposes only bounded, explicit daemon-management operations.
type HostService struct {
	fletcherv1connect.UnimplementedHostServiceHandler
	manager *host.Manager
}

// NewHostService wires the daemon's host manager to Connect.
func NewHostService(manager *host.Manager) *HostService { return &HostService{manager: manager} }

// GetHost reports version, supervision and readiness checks.
func (s *HostService) GetHost(ctx context.Context, _ *connect.Request[fletcherv1.GetHostRequest]) (*connect.Response[fletcherv1.GetHostResponse], error) {
	can, reason := s.manager.CanRestart(ctx)
	out := &fletcherv1.GetHostResponse{Version: buildinfo.Version, Commit: buildinfo.Commit, StartedAt: s.manager.StartedAt, CanRestart: can, RestartUnavailableReason: reason}
	if s.manager.Checks != nil {
		for _, c := range s.manager.Checks(ctx) {
			out.Checks = append(out.Checks, &fletcherv1.HostCheck{Name: c.Name, Status: c.Status, Detail: c.Detail})
		}
	}
	return connect.NewResponse(out), nil
}

// GetDaemonLogs returns a bounded tail of this process's structured log output.
func (s *HostService) GetDaemonLogs(_ context.Context, req *connect.Request[fletcherv1.GetDaemonLogsRequest]) (*connect.Response[fletcherv1.GetDaemonLogsResponse], error) {
	lines := req.Msg.GetLines()
	if lines < 0 || lines > 1000 {
		return nil, errs.New(errs.CategoryInvalidArgument, "lines must be 0-1000")
	}
	if lines == 0 {
		lines = 100
	}
	return connect.NewResponse(&fletcherv1.GetDaemonLogsResponse{Text: s.manager.Logs.Tail(int(lines)), Generation: s.manager.StartedAt}), nil
}

// RestartDaemon requests a generation-checked, supervised restart.
func (s *HostService) RestartDaemon(ctx context.Context, req *connect.Request[fletcherv1.RestartDaemonRequest]) (*connect.Response[fletcherv1.RestartDaemonResponse], error) {
	if err := s.manager.RequestRestart(ctx, req.Msg.GetRequestId(), req.Msg.GetExpectedStartedAt()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.RestartDaemonResponse{Accepted: true}), nil
}

// GetStorage reports capacity and non-exclusive allocated-block totals.
func (s *HostService) GetStorage(ctx context.Context, _ *connect.Request[fletcherv1.GetStorageRequest]) (*connect.Response[fletcherv1.GetStorageResponse], error) {
	st, err := s.manager.Storage(ctx)
	if err != nil {
		return nil, err
	}
	out := &fletcherv1.GetStorageResponse{TotalBytes: st.Total, AvailableBytes: st.Available, MeasuredAt: time.Now().Unix()}
	for _, c := range st.Categories {
		out.Categories = append(out.Categories, &fletcherv1.StorageCategory{Name: c.Name, AllocatedBytes: c.Allocated, LogicalBytes: c.Logical, Files: c.Files})
	}
	return connect.NewResponse(out), nil
}

// ClearBuildCache clears only the regenerable build cache when it is idle.
func (s *HostService) ClearBuildCache(ctx context.Context, _ *connect.Request[fletcherv1.ClearBuildCacheRequest]) (*connect.Response[fletcherv1.ClearBuildCacheResponse], error) {
	if s.manager.ClearCache == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "build cache management unavailable")
	}
	if err := s.manager.ClearCache(ctx); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.ClearBuildCacheResponse{}), nil
}
