package session_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	mu        sync.Mutex
	started   []string // rootfs paths, in order
	handles   []*fakeHandle
	discarded int
	nextLoad  float64 // load stamped on handles the next StartSession hands out
}

func (r *fakeRuntime) StartSession(_ context.Context, spec runtime.SessionSpec) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, spec.RootfsPath)
	h := &fakeHandle{load: r.nextLoad}
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeRuntime) DiscardSession(_ context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discarded++
	return nil
}

// fakeHandle echoes the command back on stdout and counts Stop calls.
type fakeHandle struct {
	execs   []string
	stopped int
	load    float64
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

func (h *fakeHandle) DialSSH(_ context.Context) (net.Conn, error) {
	c, _ := net.Pipe()
	return c, nil
}

func (h *fakeHandle) DialPort(_ context.Context, _ uint16) (net.Conn, error) {
	c, _ := net.Pipe()
	return c, nil
}

func (h *fakeHandle) Load(_ context.Context) (float64, error) {
	return h.load, nil
}

func (h *fakeHandle) Stop(_ context.Context) error {
	h.stopped++
	return nil
}

// fakeBroker records the published-port forwarders the manager opens and closes,
// assigning a fake tunnel port per open.
type fakeBroker struct {
	mu     sync.Mutex
	opened map[string]int // published-port id -> tunnel port
	closed []string
	next   int
}

func newFakeBroker() *fakeBroker { return &fakeBroker{opened: map[string]int{}} }

func (b *fakeBroker) Open(pp session.PublishedPort) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	port := 40000 + b.next
	b.opened[pp.ID] = port
	return port, nil
}

func (b *fakeBroker) Close(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = append(b.closed, id)
	delete(b.opened, id)
}

func newManager(t *testing.T, rt runtime.SessionRuntime, snap snapshot.Driver) *session.Manager {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fletcher.db")
	db, err := sqlite.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return session.NewManager(sqliteq.New(db), snap, rt, nil, nil, logger, session.Options{})
}

func newManagerWithOpts(t *testing.T, rt runtime.SessionRuntime, snap snapshot.Driver, opts session.Options) *session.Manager {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fletcher.db")
	db, err := sqlite.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return session.NewManager(sqliteq.New(db), snap, rt, nil, nil, logger, opts)
}

func TestCreateBootsAndRecords(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	s, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)
	_, err = mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
}

func TestStopThenStartReusesFork(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	created, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)

	deleted, err := mgr.Delete(ctx, "dev")
	require.NoError(t, err)
	require.True(t, deleted)
	require.Len(t, snap.deleted, 1, "delete must destroy the fork")
	require.Len(t, snap.live, 0)
	require.Equal(t, 1, rt.handles[0].stopped, "delete must stop a running VM")
	require.Equal(t, 1, rt.discarded, "delete must discard on-disk VM state")

	_, err = mgr.Get(ctx, "dev")
	require.ErrorIs(t, err, session.ErrNotFound)
}

func TestReconcileOnBootResetsRunning(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	created, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)
	require.Equal(t, session.StateRunning, created.State)

	// Simulate a daemon restart: the in-memory handle is gone, so the row's
	// "running" state is stale and must be reset.
	require.NoError(t, mgr.ReconcileOnBoot(ctx))

	got, err := mgr.Get(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateStopped, got.State)
}

func TestCreateRefusedAtCountCap(t *testing.T) {
	mgr := newManagerWithOpts(t, &fakeRuntime{}, newFakeSnapshot(), session.Options{MaxCount: 1})
	ctx := context.Background()

	_, err := mgr.Create(ctx, "a", "ubuntu", "", "")
	require.NoError(t, err)
	_, err = mgr.Create(ctx, "b", "ubuntu", "", "")
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}

func TestReapIdleStopsIdleSession(t *testing.T) {
	rt := &fakeRuntime{} // handles report load 0 (idle)
	mgr := newManagerWithOpts(t, rt, newFakeSnapshot(), session.Options{IdleTimeout: time.Millisecond})
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond) // let it age past the idle timeout

	n, err := mgr.ReapIdle(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "an idle session should be auto-stopped")

	got, err := mgr.Get(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateStopped, got.State)
}

func TestReapIdleKeepsBusySession(t *testing.T) {
	rt := &fakeRuntime{nextLoad: 1.0} // handle reports a busy guest
	mgr := newManagerWithOpts(t, rt, newFakeSnapshot(), session.Options{IdleTimeout: time.Millisecond})
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	n, err := mgr.ReapIdle(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "a session with work in flight must not be stopped")

	got, err := mgr.Get(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateRunning, got.State)
}

func TestReapIdleDisabledIsNoop(t *testing.T) {
	mgr := newManagerWithOpts(t, &fakeRuntime{}, newFakeSnapshot(), session.Options{}) // IdleTimeout 0
	ctx := context.Background()
	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)

	n, err := mgr.ReapIdle(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestPublishOpensListsAndUnpublishes(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	broker := newFakeBroker()
	mgr.SetBroker(broker)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)

	pp, err := mgr.Publish(ctx, "dev", 3000, "")
	require.NoError(t, err)
	require.Equal(t, "port-3000", pp.Name, "name defaults to port-<n>")
	require.NotZero(t, pp.TunnelPort, "broker assigns a tunnel port")
	require.Contains(t, broker.opened, pp.ID, "broker opened the forwarder")

	ports, err := mgr.ListPorts(ctx, "dev")
	require.NoError(t, err)
	require.Len(t, ports, 1)
	require.Equal(t, 3000, ports[0].GuestPort)

	// Re-publishing the same port conflicts.
	_, err = mgr.Publish(ctx, "dev", 3000, "")
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))

	require.NoError(t, mgr.Unpublish(ctx, "dev", 3000))
	require.Contains(t, broker.closed, pp.ID, "broker closed the forwarder")
	ports, err = mgr.ListPorts(ctx, "dev")
	require.NoError(t, err)
	require.Empty(t, ports)
}

func TestUnpublishMissingPortNotFound(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	mgr.SetBroker(newFakeBroker())
	ctx := context.Background()
	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)

	err = mgr.Unpublish(ctx, "dev", 3000)
	require.Error(t, err)
	require.Equal(t, errs.CategoryNotFound, errs.CategoryOf(err))
}

func TestDialPortWakesStoppedSession(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)
	_, err = mgr.Stop(ctx, "dev")
	require.NoError(t, err)

	conn, err := mgr.DialPort(ctx, "dev", 3000)
	require.NoError(t, err)
	require.NotNil(t, conn)
	_ = conn.Close()

	got, err := mgr.Get(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, session.StateRunning, got.State, "an inbound connection wakes the session")
	require.Len(t, rt.started, 2, "create boot + wake boot")
}

func TestDeleteClosesPublishedPorts(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	broker := newFakeBroker()
	mgr.SetBroker(broker)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "")
	require.NoError(t, err)
	pp, err := mgr.Publish(ctx, "dev", 8080, "web")
	require.NoError(t, err)

	_, err = mgr.Delete(ctx, "dev")
	require.NoError(t, err)
	require.Contains(t, broker.closed, pp.ID, "delete closes the session's port forwarders")
}

func TestSessionsRequireSessionRuntime(t *testing.T) {
	// A nil runtime models a non-session-capable runtime (e.g. mock/runc).
	mgr := newManager(t, nil, newFakeSnapshot())
	_, err := mgr.Create(context.Background(), "dev", "ubuntu", "", "")
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}
