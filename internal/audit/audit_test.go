package audit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/audit"
)

func TestNoopRecorderAcceptsEvents(t *testing.T) {
	var r audit.Recorder = audit.Noop{}
	require.NoError(t, r.Record(context.Background(), audit.Event{
		Kind:    "mcp.tool_call",
		Subject: "job_abc",
		Actor:   "agent_xyz",
		Detail:  map[string]any{"tool": "egress.http"},
	}))
}
