package api

import (
	"context"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// CredentialsBackend is what the CredentialService handler needs from the
// session manager: save a session's login as a reusable box credential, list
// the saved ones, and delete one.
type CredentialsBackend interface {
	ExportCredential(ctx context.Context, ref, name string) error
	SavedCredentials() []string
	DeleteSavedCredential(name string) error
}

// CredentialsService implements fletcherv1connect.CredentialServiceHandler.
type CredentialsService struct {
	fletcherv1connect.UnimplementedCredentialServiceHandler
	backend CredentialsBackend
}

// NewCredentialsService wires the service to its backend.
func NewCredentialsService(backend CredentialsBackend) *CredentialsService {
	return &CredentialsService{backend: backend}
}

// SaveSessionLogin copies an agent login out of a running session into the box's
// saved logins.
func (s *CredentialsService) SaveSessionLogin(ctx context.Context, req *connect.Request[fletcherv1.SaveSessionLoginRequest]) (*connect.Response[fletcherv1.SaveSessionLoginResponse], error) {
	if err := s.backend.ExportCredential(ctx, req.Msg.GetSessionRef(), req.Msg.GetName()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.SaveSessionLoginResponse{}), nil
}

// ListCredentials returns the names of the box's saved logins.
func (s *CredentialsService) ListCredentials(_ context.Context, _ *connect.Request[fletcherv1.ListCredentialsRequest]) (*connect.Response[fletcherv1.ListCredentialsResponse], error) {
	return connect.NewResponse(&fletcherv1.ListCredentialsResponse{Names: s.backend.SavedCredentials()}), nil
}

// DeleteCredential removes a saved login.
func (s *CredentialsService) DeleteCredential(_ context.Context, req *connect.Request[fletcherv1.DeleteCredentialRequest]) (*connect.Response[fletcherv1.DeleteCredentialResponse], error) {
	if err := s.backend.DeleteSavedCredential(req.Msg.GetName()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteCredentialResponse{}), nil
}
