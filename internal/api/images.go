package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/image"
)

// ImageBuilder builds a project's Dockerfile out of a running session into a
// template (M19), implemented by the session manager. BuildImageFromSession
// blocks (the CLI path); StartBuildFromSession runs it detached and is polled
// via BuildStatus (the mobile path, robust to backgrounding).
type ImageBuilder interface {
	BuildImageFromSession(ctx context.Context, devRef, subdir, name string, force bool) (resultName string, exposedPort int, err error)
	StartBuildFromSession(ctx context.Context, devRef, subdir, name string, force bool) (buildID string, err error)
	BuildStatus(buildID string) (state, name string, exposedPort int, errMsg, log string)
}

// ImagesService implements fletcherv1connect.ImageServiceHandler: it imports a
// registry image into a template on the daemon's host, so a remote client can
// deploy without local docker or filesystem access to the box.
type ImagesService struct {
	fletcherv1connect.UnimplementedImageServiceHandler
	imagesDir string
	format    string
	builder   ImageBuilder
}

// NewImagesService wires the service to the daemon's images directory, snapshot
// format (server-side import currently supports ext4 / Firecracker), and the
// session-native image builder.
func NewImagesService(imagesDir, format string, builder ImageBuilder) *ImagesService {
	return &ImagesService{imagesDir: imagesDir, format: format, builder: builder}
}

// BuildFromSession builds a session's project Dockerfile into a template (M19).
func (s *ImagesService) BuildFromSession(ctx context.Context, req *connect.Request[fletcherv1.BuildFromSessionRequest]) (*connect.Response[fletcherv1.BuildFromSessionResponse], error) {
	if s.format != "ext4" {
		return nil, errs.New(errs.CategoryFailedPrecondition,
			"session-native build requires the firecracker runtime (ext4 snapshots)")
	}
	if s.builder == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "this daemon cannot build from a session")
	}
	ref := req.Msg.GetDevSessionRef()
	if ref == "" {
		return nil, errs.New(errs.CategoryInvalidArgument, "dev_session_ref is required")
	}
	name, port, err := s.builder.BuildImageFromSession(ctx, ref, req.Msg.GetSubdir(), req.Msg.GetName(), req.Msg.GetForce())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.BuildFromSessionResponse{
		Name:        name,
		ExposedPort: uint32(port), //nolint:gosec // port is 0..65535
	}), nil
}

// StartBuildFromSession kicks off a detached build and returns its id to poll.
func (s *ImagesService) StartBuildFromSession(ctx context.Context, req *connect.Request[fletcherv1.StartBuildFromSessionRequest]) (*connect.Response[fletcherv1.StartBuildFromSessionResponse], error) {
	if s.format != "ext4" {
		return nil, errs.New(errs.CategoryFailedPrecondition,
			"session-native build requires the firecracker runtime (ext4 snapshots)")
	}
	if s.builder == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "this daemon cannot build from a session")
	}
	if req.Msg.GetDevSessionRef() == "" {
		return nil, errs.New(errs.CategoryInvalidArgument, "dev_session_ref is required")
	}
	buildID, err := s.builder.StartBuildFromSession(ctx, req.Msg.GetDevSessionRef(), req.Msg.GetSubdir(), req.Msg.GetName(), req.Msg.GetForce())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.StartBuildFromSessionResponse{BuildId: buildID}), nil
}

// GetBuildStatus reports a detached build's current state for polling.
func (s *ImagesService) GetBuildStatus(_ context.Context, req *connect.Request[fletcherv1.GetBuildStatusRequest]) (*connect.Response[fletcherv1.GetBuildStatusResponse], error) {
	if s.builder == nil {
		return nil, errs.New(errs.CategoryFailedPrecondition, "this daemon cannot build from a session")
	}
	if req.Msg.GetBuildId() == "" {
		return nil, errs.New(errs.CategoryInvalidArgument, "build_id is required")
	}
	state, name, port, errMsg, log := s.builder.BuildStatus(req.Msg.GetBuildId())
	return connect.NewResponse(&fletcherv1.GetBuildStatusResponse{
		State:       state,
		Name:        name,
		ExposedPort: uint32(port), //nolint:gosec // port is 0..65535
		Error:       errMsg,
		Log:         log,
	}), nil
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
