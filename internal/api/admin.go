// Package api hosts the daemon's Connect-RPC service implementations.
package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/buildinfo"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// AdminService implements the daemon-administration RPCs (health checks etc.)
// exposed over the local Unix socket.
type AdminService struct {
	fletcherv1connect.UnimplementedAdminServiceHandler
	startedAt int64
}

// NewAdminService builds an admin service that reports startedAt as the
// daemon's start time (Unix epoch seconds).
func NewAdminService(startedAt int64) *AdminService {
	return &AdminService{startedAt: startedAt}
}

// Health returns the daemon's liveness state plus build identity.
func (s *AdminService) Health(_ context.Context, _ *connect.Request[fletcherv1.HealthRequest]) (*connect.Response[fletcherv1.HealthResponse], error) {
	info := buildinfo.Info()
	return connect.NewResponse(&fletcherv1.HealthResponse{
		Status:    "ok",
		Version:   info.Version,
		Commit:    info.Commit,
		StartedAt: s.startedAt,
	}), nil
}
