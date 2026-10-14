package session_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
)

type cancellableRuntime struct {
	*fakeRuntime
	entered   chan struct{}
	blockSync bool
}

func (r *cancellableRuntime) StartSession(ctx context.Context, spec runtime.SessionSpec) (runtime.SessionHandle, error) {
	h, err := r.fakeRuntime.StartSession(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &cancellableHandle{fakeHandle: h.(*fakeHandle), entered: r.entered, blockSync: r.blockSync}, nil
}

type cancellableHandle struct {
	*fakeHandle
	entered   chan struct{}
	blockSync bool
}

func (h *cancellableHandle) Exec(ctx context.Context, spec runtime.Spec, stdout, stderr io.Writer) (runtime.Result, error) {
	if spec.Command == "block" || strings.HasPrefix(spec.Command, "tar ") || (h.blockSync && spec.Command == "sync") {
		close(h.entered)
		<-ctx.Done()
		return runtime.Result{}, ctx.Err()
	}
	return h.fakeHandle.Exec(ctx, spec, stdout, stderr)
}

func (h *cancellableHandle) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.fakeHandle.Stop(ctx)
}

func TestShutdownCancelsOperationsThenSyncsAndStops(t *testing.T) {
	rt := &cancellableRuntime{fakeRuntime: &fakeRuntime{}, entered: make(chan struct{})}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := mgr.Exec(t.Context(), "dev", "block"); done <- err }()
	<-rt.entered
	require.NoError(t, mgr.Shutdown(t.Context()))
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, []string{"sync"}, rt.handles[0].execs)
	require.Equal(t, 1, rt.handles[0].stopped)
	require.Len(t, snap.live, 1, "shutdown must preserve the session disk")
	got, err := mgr.Get(t.Context(), "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateStopped, got.State)
	_, err = mgr.Start(t.Context(), "dev")
	require.Equal(t, errs.CategoryUnavailable, errs.CategoryOf(err))
	require.NoError(t, mgr.Shutdown(t.Context()), "shutdown is idempotent")
}

func TestShutdownGivesInFlightStopAFreshCleanupContext(t *testing.T) {
	rt := &cancellableRuntime{fakeRuntime: &fakeRuntime{}, entered: make(chan struct{}), blockSync: true}
	mgr := newManager(t, rt, newFakeSnapshot())
	_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := mgr.Stop(t.Context(), "dev"); done <- err }()
	<-rt.entered
	require.NoError(t, mgr.Shutdown(t.Context()))
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, 1, rt.handles[0].stopped, "request cancellation must not skip VM cleanup")
}

func TestShutdownCancelsInFlightCreate(t *testing.T) {
	rt := &gatedRuntime{fakeRuntime: &fakeRuntime{}, entered: make(chan struct{}), proceed: make(chan struct{})}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
		done <- err
	}()
	<-rt.entered
	require.NoError(t, mgr.Shutdown(t.Context()))
	require.ErrorIs(t, <-done, context.Canceled)
	require.Empty(t, snap.live)
	rows, err := mgr.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestDetachedBuildOutlivesClientButDrainsOnShutdown(t *testing.T) {
	rt := &cancellableRuntime{fakeRuntime: &fakeRuntime{}, entered: make(chan struct{})}
	mgr := newManagerWithOpts(t, rt, newFakeSnapshot(), session.Options{ImagesDir: t.TempDir()})
	_, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	id, err := mgr.StartBuildFromSession(ctx, "dev", "", "example", false)
	require.NoError(t, err)
	<-rt.entered
	cancel()
	require.Equal(t, "building", mgr.BuildStatus(id).State)
	require.NoError(t, mgr.Shutdown(t.Context()))
	require.Equal(t, "failed", mgr.BuildStatus(id).State)
	require.Equal(t, 1, rt.handles[0].stopped)
}
