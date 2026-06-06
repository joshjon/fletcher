package session_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/snapshot"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// fakeSnapshot records forks in memory so the manager's create/delete plumbing
// can be exercised without a real filesystem.
type fakeSnapshot struct {
	mu      sync.Mutex
	n       int
	live    map[string]string // id -> path
	deleted []string
}

func newFakeSnapshot() *fakeSnapshot { return &fakeSnapshot{live: map[string]string{}} }

func (f *fakeSnapshot) Create(_ context.Context, image string) (snapshot.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("fork_%s_%d", image, f.n)
	path := "/tmp/" + id
	f.live[id] = path
	return snapshot.Snapshot{ID: id, Path: path}, nil
}

func (f *fakeSnapshot) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeRuntime hands out fakeHandles and records the forks it was asked to boot.
type fakeRuntime struct {
	mu      sync.Mutex
	started []string // rootfs paths, in order
	handles []*fakeHandle
}

func (r *fakeRuntime) StartSession(_ context.Context, spec runtime.SessionSpec) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, spec.RootfsPath)
	h := &fakeHandle{}
	r.handles = append(r.handles, h)
	return h, nil
}

// fakeHandle echoes the command back on stdout and counts Stop calls.
type fakeHandle struct {
	execs   []string
	stopped int
}

func (h *fakeHandle) Exec(_ context.Context, spec runtime.Spec, stdout, _ io.Writer) (runtime.Result, error) {
	h.execs = append(h.execs, spec.Command)
	_, _ = io.WriteString(stdout, "ran: "+spec.Command)
	return runtime.Result{ExitCode: 0}, nil
}

func (h *fakeHandle) Shell(_ context.Context, _ runtime.ShellSpec, _ io.Reader, stdout io.Writer, _ <-chan runtime.WinSize) (int32, error) {
	_, _ = io.WriteString(stdout, "shell")
	return 0, nil
}

func (h *fakeHandle) Stop(_ context.Context) error {
	h.stopped++
	return nil
}

func newManager(t *testing.T, rt runtime.SessionRuntime, snap snapshot.Driver) *session.Manager {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fletcher.db")
	db, err := sqlite.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return session.NewManager(sqliteq.New(db), snap, rt, nil, logger)
}

func TestCreateBootsAndRecords(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	s, err := mgr.Create(ctx, "dev", "ubuntu")
	require.NoError(t, err)
	require.NotEmpty(t, s.ID)
	require.Equal(t, "dev", s.Name)
	require.Equal(t, session.StateRunning, s.State)
	require.Len(t, rt.started, 1, "should boot exactly one VM")
	require.Len(t, snap.live, 1, "should create exactly one fork")

	got, err := mgr.Get(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, s.ID, got.ID)
}

func TestCreateDuplicateNameConflicts(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu")
	require.NoError(t, err)
	_, err = mgr.Create(ctx, "dev", "ubuntu")
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
}

func TestStopThenStartReusesFork(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	created, err := mgr.Create(ctx, "dev", "ubuntu")
	require.NoError(t, err)
	forkPath := rt.started[0]

	stopped, err := mgr.Stop(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateStopped, stopped.State)
	require.Equal(t, 1, rt.handles[0].stopped, "VM should be stopped")
	require.Len(t, snap.deleted, 0, "stop must keep the fork")

	started, err := mgr.Start(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateRunning, started.State)
	require.Equal(t, created.ID, started.ID)
	require.Len(t, rt.started, 2, "start should boot a second VM")
	require.Equal(t, forkPath, rt.started[1], "start must reuse the same fork path")
}

func TestExecRequiresRunning(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu")
	require.NoError(t, err)

	res, err := mgr.Exec(ctx, "dev", "echo hi")
	require.NoError(t, err)
	require.Equal(t, "ran: echo hi", res.Stdout)
	require.Equal(t, int32(0), res.ExitCode)

	_, err = mgr.Stop(ctx, "dev")
	require.NoError(t, err)

	_, err = mgr.Exec(ctx, "dev", "echo hi")
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}

func TestDeleteDestroysFork(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu")
	require.NoError(t, err)

	deleted, err := mgr.Delete(ctx, "dev")
	require.NoError(t, err)
	require.True(t, deleted)
	require.Len(t, snap.deleted, 1, "delete must destroy the fork")
	require.Len(t, snap.live, 0)
	require.Equal(t, 1, rt.handles[0].stopped, "delete must stop a running VM")

	_, err = mgr.Get(ctx, "dev")
	require.ErrorIs(t, err, session.ErrNotFound)
}

func TestReconcileOnBootResetsRunning(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	created, err := mgr.Create(ctx, "dev", "ubuntu")
	require.NoError(t, err)
	require.Equal(t, session.StateRunning, created.State)

	// Simulate a daemon restart: the in-memory handle is gone, so the row's
	// "running" state is stale and must be reset.
	require.NoError(t, mgr.ReconcileOnBoot(ctx))

	got, err := mgr.Get(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateStopped, got.State)
}

func TestSessionsRequireSessionRuntime(t *testing.T) {
	// A nil runtime models a non-session-capable runtime (e.g. mock/runc).
	mgr := newManager(t, nil, newFakeSnapshot())
	_, err := mgr.Create(context.Background(), "dev", "ubuntu")
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}
