package session_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/volume"
)

type gatedRuntime struct {
	*fakeRuntime
	entered chan struct{}
	proceed chan struct{}
	bootErr error
}

func (r *gatedRuntime) StartSession(ctx context.Context, spec runtime.SessionSpec) (runtime.SessionHandle, error) {
	close(r.entered)
	select {
	case <-r.proceed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if r.bootErr != nil {
		return nil, r.bootErr
	}
	return r.fakeRuntime.StartSession(ctx, spec)
}

func TestConcurrentCreateReservesVolumeBeforeBoot(t *testing.T) {
	rt := &gatedRuntime{fakeRuntime: &fakeRuntime{}, entered: make(chan struct{}), proceed: make(chan struct{})}
	mgr, q := newManagerWithQuerier(t, rt, newFakeSnapshot())
	vols := volume.NewManager(q, fakeVolumeProvisioner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.SetVolumes(vols)
	v, err := vols.Create(t.Context(), "data", 0)
	require.NoError(t, err)
	results := make(chan error, 2)
	go func() {
		_, err := mgr.Create(t.Context(), session.CreateParams{Name: "first", Image: "base", VolumeRef: v.ID})
		results <- err
	}()
	<-rt.entered
	go func() {
		_, err := mgr.Create(t.Context(), session.CreateParams{Name: "second", Image: "base", VolumeRef: v.Name})
		results <- err
	}()
	close(rt.proceed)
	var successes, conflicts int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else {
			require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
			conflicts++
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	require.Len(t, rt.volumes, 1, "a conflicting VM must never boot against the disk")
	rows, err := q.ListSessionsByVolume(t.Context(), &v.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestFailedBootReleasesVolumeReservation(t *testing.T) {
	rt := &gatedRuntime{fakeRuntime: &fakeRuntime{}, entered: make(chan struct{}), proceed: make(chan struct{}), bootErr: errors.New("boot rejected")}
	close(rt.proceed)
	mgr, q := newManagerWithQuerier(t, rt, newFakeSnapshot())
	vols := volume.NewManager(q, fakeVolumeProvisioner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.SetVolumes(vols)
	_, err := vols.Create(t.Context(), "data", 0)
	require.NoError(t, err)
	_, err = mgr.Create(t.Context(), session.CreateParams{Name: "failed", Image: "base", VolumeRef: "data"})
	require.Error(t, err)
	_, _, release, err := vols.ReserveAttachment(t.Context(), "data")
	require.NoError(t, err)
	release()
}
