package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/gateway"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// CatalogBuilder is the consumer-defined interface the ModelsService
// needs: produce the current catalog snapshot. Production wires this to
// the gateway's BuildCatalog function via a closure that captures the
// gateway's base URL.
type CatalogBuilder interface {
	Catalog() gateway.Catalog
}

// ModelsService implements fletcherv1connect.ModelServiceHandler. It
// surfaces the same catalog data the gateway serves at /v1/catalog.json
// so the CLI (`fletcher model list`) can render it without going
// through the gateway's TCP listener.
type ModelsService struct {
	fletcherv1connect.UnimplementedModelServiceHandler
	builder CatalogBuilder
}

// NewModelsService wires a ModelsService to a catalog source.
func NewModelsService(builder CatalogBuilder) *ModelsService {
	return &ModelsService{builder: builder}
}

// ListModels returns the catalog as a proto response. The catalog is
// recomputed on every call so changes (future multi-provider config)
// are reflected without restarting the daemon.
func (s *ModelsService) ListModels(_ context.Context, _ *connect.Request[fletcherv1.ListModelsRequest]) (*connect.Response[fletcherv1.ListModelsResponse], error) {
	cat := s.builder.Catalog()

	endpoints := make([]*fletcherv1.Endpoint, len(cat.Endpoints))
	for i, e := range cat.Endpoints {
		endpoints[i] = &fletcherv1.Endpoint{
			Kind:   e.Kind,
			Url:    e.URL,
			EnvVar: e.EnvVar,
		}
	}

	models := make([]*fletcherv1.Model, len(cat.Models))
	for i, m := range cat.Models {
		models[i] = &fletcherv1.Model{
			Id:       m.ID,
			Label:    m.Label,
			Upstream: m.Upstream,
		}
	}

	//nolint:gosec // catalog schema version is bounded by CatalogSchemaVersion
	return connect.NewResponse(&fletcherv1.ListModelsResponse{
		SchemaVersion: int32(cat.SchemaVersion),
		Endpoints:     endpoints,
		Models:        models,
	}), nil
}
