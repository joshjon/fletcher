package session_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
)

func TestShutdownClosesBackpressuredSSHAndPreviewConnections(t *testing.T) {
	for _, kind := range []string{"ssh", "preview"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt := &fakeRuntime{}
				snap := newFakeSnapshot()
				mgr := newManager(t, rt, snap)
				_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
				require.NoError(t, err)
				dialCtx, cancel := context.WithCancel(t.Context())
				var conn net.Conn
				if kind == "ssh" {
					conn, err = mgr.DialSSH(dialCtx, "dev")
				} else {
					conn, err = mgr.DialPort(dialCtx, "dev", 8080)
				}
				require.NoError(t, err)
				defer conn.Close()
				cancel() // a broker cancels the setup context after dialing
				done := make(chan error, 1)
				go func() { _, err := conn.Write([]byte("blocked")); done <- err }()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("dial cancellation closed an established connection: %v", err)
				default:
				}
				require.NoError(t, mgr.Shutdown(t.Context()))
				require.Error(t, <-done, "shutdown must release the blocked network writer")
				require.Equal(t, 1, rt.handles[0].stopped)
				require.Len(t, snap.live, 1)
				_, err = mgr.DialSSH(t.Context(), "dev")
				require.Equal(t, errs.CategoryUnavailable, errs.CategoryOf(err))
				_, err = mgr.DialPort(t.Context(), "dev", 8080)
				require.Equal(t, errs.CategoryUnavailable, errs.CategoryOf(err))
			})
		})
	}
}

type slowStopRuntime struct {
	*fakeRuntime
	block atomic.Bool
}

func (r *slowStopRuntime) StartSession(ctx context.Context, spec runtime.SessionSpec) (runtime.SessionHandle, error) {
	h, err := r.fakeRuntime.StartSession(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &slowStopHandle{SessionHandle: h, owner: r}, nil
}

type slowStopHandle struct {
	runtime.SessionHandle
	owner *slowStopRuntime
}

func (h *slowStopHandle) Stop(ctx context.Context) error {
	if h.owner.block.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return h.SessionHandle.Stop(ctx)
}

func TestShutdownDeadlineRetainsFailedHandlesForRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &slowStopRuntime{fakeRuntime: &fakeRuntime{}}
		snap := newFakeSnapshot()
		mgr := newManager(t, rt, snap)
		_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
		require.NoError(t, err)
		rt.block.Store(true)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		started := time.Now()
		require.ErrorIs(t, mgr.Shutdown(ctx), context.DeadlineExceeded)
		require.Equal(t, time.Second, time.Since(started), "cleanup must not extend the shutdown deadline")
		row, err := mgr.Get(t.Context(), "dev")
		require.NoError(t, err)
		require.Equal(t, session.StateRunning, row.State)
		require.Len(t, snap.live, 1)
		rt.block.Store(false)
		require.NoError(t, mgr.Shutdown(t.Context()))
		require.Equal(t, 1, rt.handles[0].stopped)
	})
}
