package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
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

// The Mode B listener binds when its address is available and shuts down
// cleanly on interrupt.
func TestRemoteAPIListenActorServesAndShutsDown(t *testing.T) {
	// Grab a free port, then hand its address to the actor to bind.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.Addr().String()
	require.NoError(t, probe.Close())

	var hit atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})}

	execute, interrupt := remoteAPIListenActor(context.Background(), addr, srv, discardLogger())
	done := make(chan error, 1)
	go func() { done <- execute() }()

	var status int
	require.Eventually(t, func() bool {
		resp, derr := http.Get("http://" + addr + "/")
		if derr != nil {
			return false
		}
		status = resp.StatusCode
		_ = resp.Body.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, http.StatusNoContent, status)
	require.True(t, hit.Load())

	interrupt(nil)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("actor did not exit after interrupt")
	}
}

// An address that is not yet bindable (the VPN interface is down) must keep the
// actor retrying rather than failing, and the retry loop must exit promptly on
// ctx cancel so shutdown does not hang.
func TestRemoteAPIListenActorRetriesUntilCancelled(t *testing.T) {
	first, max := remoteBindFirstBackoff, remoteBindMaxBackoff
	remoteBindFirstBackoff = 5 * time.Millisecond
	remoteBindMaxBackoff = 10 * time.Millisecond
	t.Cleanup(func() { remoteBindFirstBackoff, remoteBindMaxBackoff = first, max })

	// 240.0.0.0/4 is reserved and assigned to no local interface, so the bind
	// fails immediately and the actor stays in its retry loop.
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{Handler: http.NewServeMux()}
	execute, _ := remoteAPIListenActor(ctx, "240.0.0.1:11700", srv, discardLogger())

	done := make(chan error, 1)
	go func() { done <- execute() }()

	time.Sleep(30 * time.Millisecond) // let it spin through a few retries
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("actor did not exit after ctx cancel")
	}
}

func TestLooksLikeRegistryRef(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/joshjon/app:v1":  true,
		"docker.io/library/nginx": true,
		"registry:5000/app":       true,
		"localhost/app":           true,
		"myapp:latest":            false, // bare local tag
		"myapp":                   false,
		"library/nginx":           false, // Docker Hub implicit, not registry-qualified
		"":                        false,
	}
	for ref, want := range cases {
		require.Equal(t, want, looksLikeRegistryRef(ref), ref)
	}
}
