package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/volume"
)

// VolumesBackend is what the VolumeService handler needs from the volume
// manager.
type VolumesBackend interface {
	Create(ctx context.Context, name string, sizeBytes int64) (volume.Volume, error)
	Get(ctx context.Context, ref string) (volume.Volume, error)
	List(ctx context.Context) ([]volume.Volume, error)
	Delete(ctx context.Context, ref string) error
}

// VolumesService implements fletcherv1connect.VolumeServiceHandler.
type VolumesService struct {
	fletcherv1connect.UnimplementedVolumeServiceHandler
	backend VolumesBackend
}

// NewVolumesService wires the service to its backend.
func NewVolumesService(backend VolumesBackend) *VolumesService {
	return &VolumesService{backend: backend}
}

// CreateVolume provisions a new blank volume.
func (s *VolumesService) CreateVolume(ctx context.Context, req *connect.Request[fletcherv1.CreateVolumeRequest]) (*connect.Response[fletcherv1.CreateVolumeResponse], error) {
	if req.Msg.GetSizeBytes() < 0 {
		return nil, errs.New(errs.CategoryInvalidArgument, "size must not be negative")
	}
	v, err := s.backend.Create(ctx, req.Msg.GetName(), req.Msg.GetSizeBytes())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.CreateVolumeResponse{Volume: volumeToProto(v)}), nil
}

// GetVolume fetches a volume by id or name.
func (s *VolumesService) GetVolume(ctx context.Context, req *connect.Request[fletcherv1.GetVolumeRequest]) (*connect.Response[fletcherv1.GetVolumeResponse], error) {
	v, err := s.backend.Get(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetVolumeResponse{Volume: volumeToProto(v)}), nil
}

// ListVolumes returns all volumes, newest first.
func (s *VolumesService) ListVolumes(ctx context.Context, _ *connect.Request[fletcherv1.ListVolumesRequest]) (*connect.Response[fletcherv1.ListVolumesResponse], error) {
	volumes, err := s.backend.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.Volume, len(volumes))
	for i, v := range volumes {
		out[i] = volumeToProto(v)
	}
	return connect.NewResponse(&fletcherv1.ListVolumesResponse{Volumes: out}), nil
}

// DeleteVolume destroys a volume and its data (refused while attached).
func (s *VolumesService) DeleteVolume(ctx context.Context, req *connect.Request[fletcherv1.DeleteVolumeRequest]) (*connect.Response[fletcherv1.DeleteVolumeResponse], error) {
	if err := s.backend.Delete(ctx, req.Msg.GetRef()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteVolumeResponse{}), nil
}

func volumeToProto(v volume.Volume) *fletcherv1.Volume {
	return &fletcherv1.Volume{
		Id:              v.ID,
		Name:            v.Name,
		SizeBytes:       v.SizeBytes,
		UsedBytes:       v.UsedBytes,
		AttachedSession: v.AttachedSession,
		CreatedAt:       v.CreatedAt.Unix(),
	}
}
