// Package api hosts the daemon's Connect-RPC service implementations.
package api

import (
	"context"
	"sync/atomic"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/buildinfo"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// PublicEndpointProvider reports the host:port endpoints the daemon will
// advertise to paired devices: PublicEndpoint for the WireGuard tunnel and
// PairingEndpoint for the native-client pairing listener. The peer service
// satisfies it; Health surfaces the values so doctor can diagnose a stale or
// missing endpoint or pairing listener.
type PublicEndpointProvider interface {
	PublicEndpoint() string
	PairingEndpoint() string
}

// RuntimeStatus is the daemon's effective runtime configuration resolved at
// startup, surfaced via Health so `fletcher doctor` can assess job readiness.
type RuntimeStatus struct {
	// Runtime is the effective runtime driver: "firecracker", "runc", or "mock".
	Runtime string
	// Snapshot is the effective snapshot driver: "ext4", "btrfs", or "mock".
	Snapshot string
	// BaseImageAvailable is true when at least one base-image template exists for
	// the active snapshot driver.
	BaseImageAvailable bool
	// BaseImageUpdate is set by a background registry check when the default
	// image's template is older than the registry's current version. It may be
	// nil (no check wired up), which Health reports as no update available.
	BaseImageUpdate *atomic.Bool
	// BaseImageChecked is set once that background check has run to completion,
	// so Health can distinguish "no update" from "not checked yet". It may be nil
	// (no check wired up), which Health reports as not yet checked.
	BaseImageChecked *atomic.Bool
}

// AdminService implements the daemon-administration RPCs (health checks etc.)
// exposed over the local Unix socket.
type AdminService struct {
	fletcherv1connect.UnimplementedAdminServiceHandler
	startedAt int64
	endpoint  PublicEndpointProvider
	runtime   RuntimeStatus
}

// NewAdminService builds an admin service that reports startedAt as the
// daemon's start time (Unix epoch seconds). endpoint may be nil (e.g. in
// tests), in which case Health reports an empty public endpoint.
func NewAdminService(startedAt int64, endpoint PublicEndpointProvider, runtime RuntimeStatus) *AdminService {
	return &AdminService{startedAt: startedAt, endpoint: endpoint, runtime: runtime}
}

// Health returns the daemon's liveness state plus build identity, the effective
// public endpoint, and the effective runtime configuration.
func (s *AdminService) Health(_ context.Context, _ *connect.Request[fletcherv1.HealthRequest]) (*connect.Response[fletcherv1.HealthResponse], error) {
	info := buildinfo.Info()
	endpoint := ""
	pairingEndpoint := ""
	if s.endpoint != nil {
		endpoint = s.endpoint.PublicEndpoint()
		pairingEndpoint = s.endpoint.PairingEndpoint()
	}
	updateAvailable := s.runtime.BaseImageUpdate != nil && s.runtime.BaseImageUpdate.Load()
	updateChecked := s.runtime.BaseImageChecked != nil && s.runtime.BaseImageChecked.Load()
	return connect.NewResponse(&fletcherv1.HealthResponse{
		Status:                   "ok",
		Version:                  info.Version,
		Commit:                   info.Commit,
		StartedAt:                s.startedAt,
		PublicEndpoint:           endpoint,
		Runtime:                  s.runtime.Runtime,
		Snapshot:                 s.runtime.Snapshot,
		BaseImageAvailable:       s.runtime.BaseImageAvailable,
		BaseImageUpdateAvailable: updateAvailable,
		BaseImageUpdateChecked:   updateChecked,
		PairingEndpoint:          pairingEndpoint,
	}), nil
}
