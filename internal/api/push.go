package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// PushBackend stores the device push tokens the PushService manages.
type PushBackend interface {
	RegisterToken(ctx context.Context, token string) error
	UnregisterToken(ctx context.Context, token string) (bool, error)
}

// PushService implements fletcherv1connect.PushServiceHandler.
type PushService struct {
	fletcherv1connect.UnimplementedPushServiceHandler
	backend PushBackend
}

// NewPushService wires a PushService to its token store.
func NewPushService(backend PushBackend) *PushService {
	return &PushService{backend: backend}
}

// RegisterPushToken records a device's APNs token (idempotent).
func (s *PushService) RegisterPushToken(ctx context.Context, req *connect.Request[fletcherv1.RegisterPushTokenRequest]) (*connect.Response[fletcherv1.RegisterPushTokenResponse], error) {
	if req.Msg.GetToken() == "" {
		return nil, errs.New(errs.CategoryInvalidArgument, "token is required")
	}
	if err := s.backend.RegisterToken(ctx, req.Msg.GetToken()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.RegisterPushTokenResponse{}), nil
}

// UnregisterPushToken removes a device's APNs token.
func (s *PushService) UnregisterPushToken(ctx context.Context, req *connect.Request[fletcherv1.UnregisterPushTokenRequest]) (*connect.Response[fletcherv1.UnregisterPushTokenResponse], error) {
	existed, err := s.backend.UnregisterToken(ctx, req.Msg.GetToken())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.UnregisterPushTokenResponse{Existed: existed}), nil
}
