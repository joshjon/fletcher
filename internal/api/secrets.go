package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/secrets"
)

// SecretsBackend is the consumer-defined interface the SecretsService
// handler needs from the secrets layer.
type SecretsBackend interface {
	Set(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]secrets.Metadata, error)
}

// SecretsService implements fletcherv1connect.SecretServiceHandler.
type SecretsService struct {
	fletcherv1connect.UnimplementedSecretServiceHandler
	backend SecretsBackend
}

// NewSecretsService wires a SecretsService to a backend.
func NewSecretsService(backend SecretsBackend) *SecretsService {
	return &SecretsService{backend: backend}
}

// SetSecret stores name=value in the encrypted store.
func (s *SecretsService) SetSecret(ctx context.Context, req *connect.Request[fletcherv1.SetSecretRequest]) (*connect.Response[fletcherv1.SetSecretResponse], error) {
	if err := s.backend.Set(ctx, req.Msg.GetName(), req.Msg.GetValue()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.SetSecretResponse{}), nil
}

// DeleteSecret removes name from the store. Missing secrets are reported
// via existed=false rather than an error.
func (s *SecretsService) DeleteSecret(ctx context.Context, req *connect.Request[fletcherv1.DeleteSecretRequest]) (*connect.Response[fletcherv1.DeleteSecretResponse], error) {
	// We don't have a "did the row exist" return path on the backend, so
	// the wire field is mostly informational; treat any not-found as
	// "existed=false" and still succeed.
	if err := s.backend.Delete(ctx, req.Msg.GetName()); err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return connect.NewResponse(&fletcherv1.DeleteSecretResponse{Existed: false}), nil
		}
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteSecretResponse{Existed: true}), nil
}

// ListSecrets returns metadata (no plaintext) for every stored secret.
func (s *SecretsService) ListSecrets(ctx context.Context, _ *connect.Request[fletcherv1.ListSecretsRequest]) (*connect.Response[fletcherv1.ListSecretsResponse], error) {
	rows, err := s.backend.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.SecretMetadata, len(rows))
	for i, r := range rows {
		out[i] = &fletcherv1.SecretMetadata{
			Name:      r.Name,
			CreatedAt: r.CreatedAt.Unix(),
			UpdatedAt: r.UpdatedAt.Unix(),
		}
	}
	return connect.NewResponse(&fletcherv1.ListSecretsResponse{Secrets: out}), nil
}
