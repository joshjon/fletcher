package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

func TestHealthReportsStartedAtAndStatus(t *testing.T) {
	const startedAt = int64(1_700_000_000)
	svc := api.NewAdminService(startedAt, stubEndpoint("vpn.example.com:51820"))

	resp, err := svc.Health(context.Background(), connect.NewRequest(&fletcherv1.HealthRequest{}))
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Msg.GetStatus())
	require.Equal(t, startedAt, resp.Msg.GetStartedAt())
	require.NotEmpty(t, resp.Msg.GetVersion())
	require.NotEmpty(t, resp.Msg.GetCommit())
	require.Equal(t, "vpn.example.com:51820", resp.Msg.GetPublicEndpoint())
}

// TestHealthReportsEmptyEndpointWhenUnset covers the nil-provider path
// (the endpoint provider is optional) and the empty-endpoint case doctor
// keys its restart remediation off.
func TestHealthReportsEmptyEndpointWhenUnset(t *testing.T) {
	svc := api.NewAdminService(0, nil)
	resp, err := svc.Health(context.Background(), connect.NewRequest(&fletcherv1.HealthRequest{}))
	require.NoError(t, err)
	require.Empty(t, resp.Msg.GetPublicEndpoint())
}

// stubEndpoint is a fixed PublicEndpointProvider for tests.
type stubEndpoint string

func (s stubEndpoint) PublicEndpoint() string { return string(s) }
