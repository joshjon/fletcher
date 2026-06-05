package api

import (
	"context"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/settings"
)

// SettingsBackend is what the SettingsService handler needs from the settings
// store.
type SettingsBackend interface {
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) (bool, error)
	Describe(ctx context.Context) ([]settings.View, error)
}

// SettingsService implements fletcherv1connect.SettingsServiceHandler.
type SettingsService struct {
	fletcherv1connect.UnimplementedSettingsServiceHandler
	backend SettingsBackend
}

// NewSettingsService wires a SettingsService to a backend.
func NewSettingsService(backend SettingsBackend) *SettingsService {
	return &SettingsService{backend: backend}
}

// SetSetting validates and stores key=value.
func (s *SettingsService) SetSetting(ctx context.Context, req *connect.Request[fletcherv1.SetSettingRequest]) (*connect.Response[fletcherv1.SetSettingResponse], error) {
	if err := s.backend.Set(ctx, req.Msg.GetKey(), req.Msg.GetValue()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.SetSettingResponse{}), nil
}

// DeleteSetting removes a setting, reverting it to the flag/env default.
func (s *SettingsService) DeleteSetting(ctx context.Context, req *connect.Request[fletcherv1.DeleteSettingRequest]) (*connect.Response[fletcherv1.DeleteSettingResponse], error) {
	existed, err := s.backend.Delete(ctx, req.Msg.GetKey())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteSettingResponse{Existed: existed}), nil
}

// ListSettings returns every known setting with its value and help.
func (s *SettingsService) ListSettings(ctx context.Context, _ *connect.Request[fletcherv1.ListSettingsRequest]) (*connect.Response[fletcherv1.ListSettingsResponse], error) {
	views, err := s.backend.Describe(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.Setting, len(views))
	for i, v := range views {
		out[i] = &fletcherv1.Setting{
			Key:         v.Key,
			Value:       v.Value,
			Description: v.Description,
			Set:         v.Set,
		}
	}
	return connect.NewResponse(&fletcherv1.ListSettingsResponse{Settings: out}), nil
}
