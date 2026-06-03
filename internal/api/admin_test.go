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
	svc := api.NewAdminService(startedAt)

	resp, err := svc.Health(context.Background(), connect.NewRequest(&fletcherv1.HealthRequest{}))
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Msg.GetStatus())
	require.Equal(t, startedAt, resp.Msg.GetStartedAt())
	require.NotEmpty(t, resp.Msg.GetVersion())
	require.NotEmpty(t, resp.Msg.GetCommit())
}
