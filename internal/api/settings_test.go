package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/settings"
)

type fakeSettingsBackend struct{ views []settings.View }

func (f fakeSettingsBackend) Set(context.Context, string, string) error    { return nil }
func (f fakeSettingsBackend) Delete(context.Context, string) (bool, error) { return false, nil }
func (f fakeSettingsBackend) Describe(context.Context) ([]settings.View, error) {
	return f.views, nil
}

// TestListSettingsSurfacesDefaults verifies that an unset key reports the
// daemon's resolved default value (not an empty string), while an explicitly
// set key keeps its stored value.
func TestListSettingsSurfacesDefaults(t *testing.T) {
	backend := fakeSettingsBackend{views: []settings.View{
		{Key: "runtime", Value: "", Description: "d", Set: false},
		{Key: "log_level", Value: "debug", Description: "d", Set: true},
	}}
	svc := api.NewSettingsService(backend, map[string]string{
		"runtime":   "firecracker", // the effective default
		"log_level": "info",        // would be the default, but this key is set
	})

	resp, err := svc.ListSettings(context.Background(), connect.NewRequest(&fletcherv1.ListSettingsRequest{}))
	require.NoError(t, err)

	got := make(map[string]*fletcherv1.Setting)
	for _, s := range resp.Msg.GetSettings() {
		got[s.GetKey()] = s
	}

	require.Equal(t, "firecracker", got["runtime"].GetValue(), "unset key should surface the default")
	require.False(t, got["runtime"].GetSet())

	require.Equal(t, "debug", got["log_level"].GetValue(), "set key keeps its stored value")
	require.True(t, got["log_level"].GetSet())
}
