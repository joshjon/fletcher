package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/network/portmap"
)

// shrinkDeriveTiming swaps the retry timing for sub-second values so the retry
// paths run fast, restoring the originals when the test ends.
func shrinkDeriveTiming(t *testing.T) {
	t.Helper()
	w, f, m := deriveRetryWindow, deriveFirstBackoff, deriveMaxBackoff
	deriveRetryWindow = 80 * time.Millisecond
	deriveFirstBackoff = 5 * time.Millisecond
	deriveMaxBackoff = 10 * time.Millisecond
	t.Cleanup(func() {
		deriveRetryWindow, deriveFirstBackoff, deriveMaxBackoff = w, f, m
	})
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The reboot race: the WAN lags network-online.target, so the first few
// derivation attempts fail. deriveEndpoint must keep retrying and succeed once
// the router answers, instead of leaving the daemon tunnel-less.
func TestDeriveEndpointRetriesUntilRouterReady(t *testing.T) {
	shrinkDeriveTiming(t)

	var calls atomic.Int32
	ensure := func(context.Context, portmap.Request) (portmap.Result, error) {
		if calls.Add(1) < 3 {
			return portmap.Result{}, errors.New("no gateway response")
		}
		return portmap.Result{ExternalIP: "203.0.113.5", ExternalPort: 51820}, nil
	}

	res, derived := deriveEndpoint(context.Background(), discardLogger(), ensure, portmap.Request{}, true)
	require.NotNil(t, res)
	require.Equal(t, "203.0.113.5:51820", derived)
	require.EqualValues(t, 3, calls.Load())
}

// A WAN that never comes back must not retry forever: deriveEndpoint gives up
// after the bounded window so boot completes (tunnel-less, as before).
func TestDeriveEndpointGivesUpAfterWindow(t *testing.T) {
	shrinkDeriveTiming(t)

	var calls atomic.Int32
	ensure := func(context.Context, portmap.Request) (portmap.Result, error) {
		calls.Add(1)
		return portmap.Result{}, errors.New("no gateway response")
	}

	res, derived := deriveEndpoint(context.Background(), discardLogger(), ensure, portmap.Request{}, true)
	require.Nil(t, res)
	require.Empty(t, derived)
	require.GreaterOrEqual(t, calls.Load(), int32(2))
}

// With retry off (operator endpoint set, or a dev host) a single failed
// attempt returns immediately - boot never blocks on the router.
func TestDeriveEndpointSingleAttemptWhenNotRetrying(t *testing.T) {
	shrinkDeriveTiming(t)

	var calls atomic.Int32
	ensure := func(context.Context, portmap.Request) (portmap.Result, error) {
		calls.Add(1)
		return portmap.Result{}, errors.New("no gateway response")
	}

	res, derived := deriveEndpoint(context.Background(), discardLogger(), ensure, portmap.Request{}, false)
	require.Nil(t, res)
	require.Empty(t, derived)
	require.EqualValues(t, 1, calls.Load())
}

// A successful mapping with no external address (e.g. behind CGNAT) still
// returns the result so the port-forward is tracked, just no endpoint.
func TestDeriveEndpointReturnsMappingWithoutEndpoint(t *testing.T) {
	shrinkDeriveTiming(t)

	ensure := func(context.Context, portmap.Request) (portmap.Result, error) {
		return portmap.Result{ExternalPort: 51820}, nil
	}

	res, derived := deriveEndpoint(context.Background(), discardLogger(), ensure, portmap.Request{}, false)
	require.NotNil(t, res)
	require.Empty(t, derived)
}
