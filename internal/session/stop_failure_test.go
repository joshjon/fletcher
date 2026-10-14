package session_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
)

type stopFailureRuntime struct {
	*fakeRuntime
	stopErr error
}

func (r *stopFailureRuntime) StartSession(ctx context.Context, spec runtime.SessionSpec) (runtime.SessionHandle, error) {
	h, err := r.fakeRuntime.StartSession(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &stopFailureHandle{SessionHandle: h, owner: r}, nil
}

type stopFailureHandle struct {
	runtime.SessionHandle
	owner *stopFailureRuntime
}

func (h *stopFailureHandle) Stop(ctx context.Context) error {
	if h.owner.stopErr != nil {
		return h.owner.stopErr
	}
	return h.SessionHandle.Stop(ctx)
}

func TestFailedStopNeverDeletesOrSwapsLiveDisks(t *testing.T) {
	for _, operation := range []string{"stop", "delete", "restart", "redeploy", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			rt := &stopFailureRuntime{fakeRuntime: &fakeRuntime{}}
			snap := newFakeSnapshot()
			mgr, q := newManagerWithQuerier(t, rt, snap)
			created, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
			require.NoError(t, err)
			if operation == "rollback" {
				created, err = mgr.Redeploy(t.Context(), "dev", "next")
				require.NoError(t, err)
			}
			before, err := q.GetSessionByRef(t.Context(), created.ID)
			require.NoError(t, err)
			forks, boots := len(snap.live), len(rt.started)
			rt.stopErr = errors.New("stop failed")
			switch operation {
			case "stop":
				_, err = mgr.Stop(t.Context(), "dev")
			case "delete":
				_, err = mgr.Delete(t.Context(), "dev")
			case "restart":
				_, err = mgr.Restart(t.Context(), "dev")
			case "redeploy":
				_, err = mgr.Redeploy(t.Context(), "dev", "next")
			case "rollback":
				_, err = mgr.Rollback(t.Context(), "dev")
			}
			require.ErrorIs(t, err, rt.stopErr)
			got, err := mgr.Get(t.Context(), "dev")
			require.NoError(t, err)
			after, err := q.GetSessionByRef(t.Context(), created.ID)
			require.NoError(t, err)
			require.Equal(t, before.ForkID, after.ForkID)
			require.Equal(t, before.PrevForkID, after.PrevForkID)
			require.Equal(t, session.StateRunning, got.State)
			require.Equal(t, forks, len(snap.live))
			require.Equal(t, boots, len(rt.started))
			_, err = mgr.Exec(t.Context(), "dev", "true")
			require.NoError(t, err, "the failed stop must retain the live handle")
		})
	}
}
