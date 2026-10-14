package session_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/session"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// pairedReads forces two callers to read the same pre-lock lifecycle state.
type pairedReads struct {
	sqliteq.Querier
	mu    sync.Mutex
	reads int
	ready chan struct{}
}

func (q *pairedReads) GetSessionByRef(ctx context.Context, ref string) (sqliteq.Session, error) {
	row, err := q.Querier.GetSessionByRef(ctx, ref)
	q.mu.Lock()
	wait := ref == "app" && q.reads < 2
	if wait {
		q.reads++
		if q.reads == 2 {
			close(q.ready)
		}
	}
	q.mu.Unlock()
	if wait {
		select {
		case <-q.ready:
		case <-ctx.Done():
			return sqliteq.Session{}, ctx.Err()
		}
	}
	return row, err
}

func TestConcurrentRedeployUsesLatestRollback(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	_, base := newManagerWithQuerier(t, rt, snap)
	q := &pairedReads{Querier: base, reads: 2, ready: make(chan struct{})}
	mgr := session.NewManager(q, snap, rt, session.AgentEnv{}, slog.New(slog.NewTextHandler(io.Discard, nil)), session.Options{})
	created, err := mgr.Create(t.Context(), session.CreateParams{Name: "app", Image: "base", RunApp: true})
	require.NoError(t, err)
	q.reads = 0
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := mgr.Redeploy(t.Context(), "app", "")
			results <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	row, err := base.GetSessionByRef(t.Context(), created.ID)
	require.NoError(t, err)
	require.Len(t, rt.started, 3)
	require.Equal(t, rt.started[1], *row.PrevForkPath)
	require.Len(t, snap.live, 2)
	require.Len(t, snap.deleted, 1)
}

func TestConcurrentRollbackSwapsCurrentForks(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	_, base := newManagerWithQuerier(t, rt, snap)
	q := &pairedReads{Querier: base, reads: 2, ready: make(chan struct{})}
	mgr := session.NewManager(q, snap, rt, session.AgentEnv{}, slog.New(slog.NewTextHandler(io.Discard, nil)), session.Options{})
	created, err := mgr.Create(t.Context(), session.CreateParams{Name: "app", Image: "base", RunApp: true})
	require.NoError(t, err)
	_, err = mgr.Redeploy(t.Context(), created.ID, "")
	require.NoError(t, err)
	q.reads = 0
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := mgr.Rollback(t.Context(), "app")
			results <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	require.Len(t, rt.started, 4)
	require.Equal(t, rt.started[0], rt.started[2])
	require.Equal(t, rt.started[1], rt.started[3])
	require.Len(t, snap.live, 2)
}
