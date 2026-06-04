// Package api hosts the daemon's Connect-RPC service implementations.
package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/buildinfo"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// PublicEndpointProvider reports the host:port the daemon will advertise to
// paired devices. The peer service satisfies it; Health surfaces the value so
// doctor can diagnose a stale or missing endpoint.
type PublicEndpointProvider interface {
	PublicEndpoint() string
}

// AdminService implements the daemon-administration RPCs (health checks etc.)
// exposed over the local Unix socket.
type AdminService struct {
	fletcherv1connect.UnimplementedAdminServiceHandler
	startedAt int64
	endpoint  PublicEndpointProvider
}

// NewAdminService builds an admin service that reports startedAt as the
// daemon's start time (Unix epoch seconds). endpoint may be nil (e.g. in
// tests), in which case Health reports an empty public endpoint.
func NewAdminService(startedAt int64, endpoint PublicEndpointProvider) *AdminService {
	return &AdminService{startedAt: startedAt, endpoint: endpoint}
}

// Health returns the daemon's liveness state plus build identity and the
// effective public endpoint.
func (s *AdminService) Health(_ context.Context, _ *connect.Request[fletcherv1.HealthRequest]) (*connect.Response[fletcherv1.HealthResponse], error) {
	info := buildinfo.Info()
	endpoint := ""
	if s.endpoint != nil {
		endpoint = s.endpoint.PublicEndpoint()
	}
	return connect.NewResponse(&fletcherv1.HealthResponse{
		Status:         "ok",
		Version:        info.Version,
		Commit:         info.Commit,
		StartedAt:      s.startedAt,
		PublicEndpoint: endpoint,
	}), nil
}
