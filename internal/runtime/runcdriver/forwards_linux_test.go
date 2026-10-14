//go:build linux

package runcdriver

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime"
)

func TestRuncEgressForwardUsesJobPolicy(t *testing.T) {
	d := &Driver{egressOpenSocket: "/fake/open.sock", forwards: []Forward{
		{Listen: "127.0.0.1:11500", HostSocket: "/fake/gateway.sock"},
		{Listen: "127.0.0.1:11502", HostSocket: "/fake/allowlist.sock", Egress: true},
	}}
	for _, policy := range []string{"none", "allowlist", "open"} {
		t.Run(policy, func(t *testing.T) {
			forwards, cleanup, err := d.prepareForwards(t.Context(), t.TempDir(), runtime.Spec{JobID: "job_example", EgressPolicy: policy})
			require.NoError(t, err)
			defer cleanup()
			if policy == "none" {
				require.Len(t, forwards, 1)
			} else {
				require.Len(t, forwards, 2)
				want := "/fake/allowlist.sock"
				if policy == "open" {
					want = "/fake/open.sock"
				}
				require.Equal(t, want, forwards[1].HostSocket)
			}
		})
	}
}

func TestRuncMCPForwardRequiresHostAuthority(t *testing.T) {
	d := &Driver{forwards: []Forward{{MCP: true}}}
	_, _, err := d.prepareForwards(t.Context(), t.TempDir(), runtime.Spec{JobID: "job_example"})
	require.ErrorContains(t, err, "host identity authority")
}
