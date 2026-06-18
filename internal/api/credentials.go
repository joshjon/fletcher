package api

import (
	"context"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// CredentialsBackend is what the CredentialService handler needs from the
// session manager: save a git host login as a reusable box credential, list the
// saved ones, and delete one. (Agent login seeding was removed - see M16.)
type CredentialsBackend interface {
	SaveGitCredential(host, username, token, gitName, gitEmail string) error
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

// SaveGitCredential saves a git host login (host + username + token) into the
// box's saved logins from structured fields, so new sessions seeded with the
// "git" credential can clone over HTTPS.
func (s *CredentialsService) SaveGitCredential(_ context.Context, req *connect.Request[fletcherv1.SaveGitCredentialRequest]) (*connect.Response[fletcherv1.SaveGitCredentialResponse], error) {
	if err := s.backend.SaveGitCredential(
		req.Msg.GetHost(), req.Msg.GetUsername(), req.Msg.GetToken(),
		req.Msg.GetGitUserName(), req.Msg.GetGitUserEmail(),
	); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.SaveGitCredentialResponse{}), nil
}

// ListCredentials returns the box's saved logins (today, the git login).
func (s *CredentialsService) ListCredentials(_ context.Context, _ *connect.Request[fletcherv1.ListCredentialsRequest]) (*connect.Response[fletcherv1.ListCredentialsResponse], error) {
	return connect.NewResponse(&fletcherv1.ListCredentialsResponse{
		Names: s.backend.SavedCredentials(),
	}), nil
}

// DeleteCredential removes a saved login.
func (s *CredentialsService) DeleteCredential(_ context.Context, req *connect.Request[fletcherv1.DeleteCredentialRequest]) (*connect.Response[fletcherv1.DeleteCredentialResponse], error) {
	if err := s.backend.DeleteSavedCredential(req.Msg.GetName()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteCredentialResponse{}), nil
}
