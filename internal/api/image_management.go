package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/image"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// StartImport starts a reconnectable registry pull or update.
func (s *ImagesService) StartImport(ctx context.Context, req *connect.Request[fletcherv1.StartImportRequest]) (*connect.Response[fletcherv1.StartImportResponse], error) {
	if s.manager == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "image management unavailable")
	}
	r := req.Msg
	op, err := s.manager.Start(ctx, r.GetRequestId(), image.ImportOptions{Ref: r.GetRef(), Name: r.GetName(), Force: r.GetForce(), Username: r.GetRegistryUsername(), Password: r.GetRegistryPassword()}, r.GetUpdate())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.StartImportResponse{Operation: importProto(op)}), nil
}

// ListImports returns recent imports including interrupted operations.
func (s *ImagesService) ListImports(ctx context.Context, _ *connect.Request[fletcherv1.ListImportsRequest]) (*connect.Response[fletcherv1.ListImportsResponse], error) {
	if s.manager == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "image management unavailable")
	}
	ops, err := s.manager.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.ImageImport, len(ops))
	for i, op := range ops {
		out[i] = importProto(op)
	}
	return connect.NewResponse(&fletcherv1.ListImportsResponse{Operations: out}), nil
}

// DeleteImage removes an unused template after daemon-side reference checks.
func (s *ImagesService) DeleteImage(ctx context.Context, req *connect.Request[fletcherv1.DeleteImageRequest]) (*connect.Response[fletcherv1.DeleteImageResponse], error) {
	if s.manager == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "image management unavailable")
	}
	if err := s.manager.Delete(ctx, req.Msg.GetName()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteImageResponse{}), nil
}

func importProto(op sqliteq.ImageImport) *fletcherv1.ImageImport {
	return &fletcherv1.ImageImport{Id: op.ID, Ref: op.Ref, Name: op.Name, State: op.State, Error: op.Error, CreatedAt: op.CreatedAt}
}
