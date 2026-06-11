package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/image"
)

// ImagesService implements fletcherv1connect.ImageServiceHandler: it imports a
// registry image into a template on the daemon's host, so a remote client can
// deploy without local docker or filesystem access to the box.
type ImagesService struct {
	fletcherv1connect.UnimplementedImageServiceHandler
	imagesDir string
	format    string
}

// NewImagesService wires the service to the daemon's images directory and
// snapshot format (server-side import currently supports ext4 / Firecracker).
func NewImagesService(imagesDir, format string) *ImagesService {
	return &ImagesService{imagesDir: imagesDir, format: format}
}

// ListImages lists the imported templates so a client can offer a picker.
func (s *ImagesService) ListImages(_ context.Context, _ *connect.Request[fletcherv1.ListImagesRequest]) (*connect.Response[fletcherv1.ListImagesResponse], error) {
	templates, err := image.ListTemplates(s.imagesDir)
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.Image, len(templates))
	for i, t := range templates {
		var port uint32
		if t.ExposedPort > 0 && t.ExposedPort <= 65535 {
			port = uint32(t.ExposedPort)
		}
		out[i] = &fletcherv1.Image{
			Name:        t.Name,
			Format:      t.Format,
			Source:      t.Source,
			Digest:      t.Digest,
			ImportedAt:  t.ImportedAt,
			ExposedPort: port,
			Entrypoint:  t.Entrypoint,
		}
	}
	return connect.NewResponse(&fletcherv1.ListImagesResponse{Images: out}), nil
}

// Import pulls a registry image and flattens it into a template.
func (s *ImagesService) Import(ctx context.Context, req *connect.Request[fletcherv1.ImportRequest]) (*connect.Response[fletcherv1.ImportResponse], error) {
	if s.format != "ext4" {
		return nil, errs.New(errs.CategoryFailedPrecondition,
			"server-side image import requires the firecracker runtime (ext4 snapshots); build/import locally for other runtimes")
	}
	ref := req.Msg.GetRef()
	if ref == "" {
		return nil, errs.New(errs.CategoryInvalidArgument, "image ref is required")
	}
	name := req.Msg.GetName()
	if name == "" {
		name = image.DefaultName(ref)
	}
	res, err := image.ImportRegistry(ctx, image.ImportOptions{
		Ref:       ref,
		Name:      name,
		ImagesDir: s.imagesDir,
		Username:  req.Msg.GetRegistryUsername(),
		Password:  req.Msg.GetRegistryPassword(),
		Force:     req.Msg.GetForce(),
	})
	if err != nil {
		// Surface the descriptive message (bad ref / auth / exists) with a clean
		// non-internal code rather than an opaque Internal error.
		return nil, errs.Newf(errs.CategoryFailedPrecondition, "%s", err.Error())
	}
	return connect.NewResponse(&fletcherv1.ImportResponse{
		Name:        res.Name,
		Digest:      res.Digest,
		ExposedPort: uint32(res.ExposedPort), //nolint:gosec // port is 0..65535
	}), nil
}
