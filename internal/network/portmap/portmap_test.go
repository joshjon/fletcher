package portmap_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/network/portmap"
)

func TestMapRejectsInvalidProtocol(t *testing.T) {
	_, err := portmap.Map(context.Background(), portmap.Request{
		InternalPort: 11500,
		Protocol:     "ICMP",
	})
	require.Error(t, err)
}

func TestMapRequiresInternalPort(t *testing.T) {
	_, err := portmap.Map(context.Background(), portmap.Request{
		Protocol: portmap.ProtocolTCP,
	})
	require.Error(t, err)
}

// TestMapDiscoveryRunsInProcess verifies that a real (or absent) UPnP
// IGD on the LAN doesn't crash discovery. Pass means either:
//   - a mapping was installed (test-machine has a UPnP router; rare in CI)
//   - or the call returned an error before the timeout (the expected case)
func TestMapDiscoveryRunsInProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	_, err := portmap.Map(ctx, portmap.Request{
		InternalPort:  11500,
		Protocol:      portmap.ProtocolUDP,
		LeaseDuration: time.Hour,
		Description:   "fletcher test",
	})
	// We don't assert on err's content — absence of a UPnP router is
	// the common case. The test just confirms discovery doesn't panic.
	_ = err
}
