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
	"github.com/joshjon/fletcher/internal/image"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/snapshot"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
	"github.com/joshjon/fletcher/internal/volume"
)

// fakeSnapshot records forks in memory so the manager's create/delete plumbing
// can be exercised without a real filesystem.
type fakeSnapshot struct {
	mu        sync.Mutex
	n         int
	live      map[string]string // id -> path
	deleted   []string
	templates map[string]string // committed template name -> snapshot id
	// committedFiles are the extraFiles of the most recent CommitTemplate.
	committedFiles map[string][]byte
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

// CommitTemplate records committed templates so CommitImage can be exercised
// (satisfies snapshot.TemplateCommitter).
func (f *fakeSnapshot) CommitTemplate(_ context.Context, id, name string, force bool, extraFiles map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[id]; !ok {
		return fmt.Errorf("snapshot %s not found", id)
	}
	if f.templates == nil {
		f.templates = map[string]string{}
	}
	if _, exists := f.templates[name]; exists && !force {
		return fmt.Errorf("template %q already exists", name)
	}
	f.templates[name] = id
	f.committedFiles = extraFiles
	return nil
}

// nonCommittingSnapshot hides fakeSnapshot's CommitTemplate so the
// missing-capability path can be tested.
type nonCommittingSnapshot struct{ inner *fakeSnapshot }

func (s nonCommittingSnapshot) Create(ctx context.Context, image string) (snapshot.Snapshot, error) {
	return s.inner.Create(ctx, image)
}

func (s nonCommittingSnapshot) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

// fakeRuntime hands out fakeHandles and records the forks it was asked to boot.
type fakeRuntime struct {
	mu        sync.Mutex
	started   []string   // rootfs paths, in order
	runApps   []bool     // spec.RunApp per StartSession, in order
	envs      [][]string // spec.Env per StartSession, in order
	volumes   []string   // spec.VolumePath per StartSession, in order
	handles   []*fakeHandle
	discarded int
	nextLoad  float64 // load stamped on handles the next StartSession hands out
}

func (r *fakeRuntime) StartSession(_ context.Context, spec runtime.SessionSpec) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, spec.RootfsPath)
	r.runApps = append(r.runApps, spec.RunApp)
	r.envs = append(r.envs, spec.Env)
	r.volumes = append(r.volumes, spec.VolumePath)
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

func (r *fakeRuntime) ReclaimOrphans(_ context.Context, _ []string) (int, error) {
	return 0, nil
}

// fakeHandle echoes the command back on stdout and counts Stop calls.
type fakeHandle struct {
	execs    []string
	stopped  int
	load     float64
	restarts int64
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

func (h *fakeHandle) AppRestarts(_ context.Context) (int64, error) {
	return h.restarts, nil
}

func (h *fakeHandle) WriteFile(_ context.Context, spec runtime.FileWriteSpec, content io.Reader) (runtime.FileWriteResult, error) {
	n, err := io.Copy(io.Discard, io.LimitReader(content, spec.Size))
	return runtime.FileWriteResult{BytesWritten: n}, err
}

func (h *fakeHandle) ReadFile(_ context.Context, _ string, onInfo func(runtime.FileReadResult) error, w io.Writer) error {
	if onInfo != nil {
		return onInfo(runtime.FileReadResult{Size: 0, Mode: 0o644})
	}
	return nil
}

func (h *fakeHandle) ListDir(_ context.Context, _ string) (runtime.DirListing, error) {
	return runtime.DirListing{}, nil
}

func (h *fakeHandle) FileOp(_ context.Context, _ runtime.FileOpSpec) error {
	return nil
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

func newManagerWithQuerier(t *testing.T, rt runtime.SessionRuntime, snap snapshot.Driver) (*session.Manager, sqliteq.Querier) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fletcher.db")
	db, err := sqlite.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := sqliteq.New(db)
	return session.NewManager(q, snap, rt, nil, nil, logger, session.Options{}), q
}

func TestCreateBootsAndRecords(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	s, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)
	_, err = mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
}

func TestStopThenStartReusesFork(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	created, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	created, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	_, err := mgr.Create(ctx, "a", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)
	_, err = mgr.Create(ctx, "b", "ubuntu", "", "", false, "", nil, nil)
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}

func TestReapIdleStopsIdleSession(t *testing.T) {
	rt := &fakeRuntime{} // handles report load 0 (idle)
	mgr := newManagerWithOpts(t, rt, newFakeSnapshot(), session.Options{IdleTimeout: time.Millisecond})
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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
	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)

	pp, err := mgr.Publish(ctx, "dev", 3000, "", false, "")
	require.NoError(t, err)
	require.Equal(t, "port-3000", pp.Name, "name defaults to port-<n>")
	require.NotZero(t, pp.TunnelPort, "broker assigns a tunnel port")
	require.Contains(t, broker.opened, pp.ID, "broker opened the forwarder")

	ports, err := mgr.ListPorts(ctx, "dev")
	require.NoError(t, err)
	require.Len(t, ports, 1)
	require.Equal(t, 3000, ports[0].GuestPort)

	// Re-publishing the same port conflicts.
	_, err = mgr.Publish(ctx, "dev", 3000, "", false, "")
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
	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)

	err = mgr.Unpublish(ctx, "dev", 3000)
	require.Error(t, err)
	require.Equal(t, errs.CategoryNotFound, errs.CategoryOf(err))
}

func TestDialPortWakesStoppedSession(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
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

	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)
	pp, err := mgr.Publish(ctx, "dev", 8080, "web", false, "")
	require.NoError(t, err)

	_, err = mgr.Delete(ctx, "dev")
	require.NoError(t, err)
	require.Contains(t, broker.closed, pp.ID, "delete closes the session's port forwarders")
}

func TestPublishPublicRequiresEnable(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot()) // PublicWeb defaults off
	mgr.SetBroker(newFakeBroker())
	ctx := context.Background()
	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)

	_, err = mgr.Publish(ctx, "dev", 8080, "", true, "app.example.com")
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}

func TestPublishPublicValidatesHostAndResolves(t *testing.T) {
	mgr := newManagerWithOpts(t, &fakeRuntime{}, newFakeSnapshot(), session.Options{PublicWeb: true})
	mgr.SetBroker(newFakeBroker())
	ctx := context.Background()
	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)

	// A malformed host is rejected before anything is recorded.
	_, err = mgr.Publish(ctx, "dev", 8080, "", true, "not a host")
	require.Error(t, err)
	require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))

	// A valid host is accepted, lowercased, and persisted as public.
	pp, err := mgr.Publish(ctx, "dev", 8080, "", true, "App.Example.com")
	require.NoError(t, err)
	require.True(t, pp.Public)
	require.Equal(t, "app.example.com", pp.Host)

	// The public listener resolves the host to this port; unknown hosts 404.
	got, err := mgr.LookupPublicPort(ctx, "app.example.com")
	require.NoError(t, err)
	require.Equal(t, pp.ID, got.ID)
	_, err = mgr.LookupPublicPort(ctx, "nope.example.com")
	require.ErrorIs(t, err, session.ErrNotFound)
}

func TestPublishHostWithoutPublicRejected(t *testing.T) {
	mgr := newManagerWithOpts(t, &fakeRuntime{}, newFakeSnapshot(), session.Options{PublicWeb: true})
	mgr.SetBroker(newFakeBroker())
	ctx := context.Background()
	_, err := mgr.Create(ctx, "dev", "ubuntu", "", "", false, "", nil, nil)
	require.NoError(t, err)

	_, err = mgr.Publish(ctx, "dev", 8080, "", false, "app.example.com")
	require.Error(t, err)
	require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
}

func TestCreateAppRunsAndPersists(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	ctx := context.Background()

	s, err := mgr.Create(ctx, "web", "nginx", "", "", true, "", nil, nil)
	require.NoError(t, err)
	require.True(t, s.RunApp)
	require.Equal(t, []bool{true}, rt.runApps, "create should boot the VM in app mode")

	// App mode is persisted, so a stop/start re-runs the app rather than coming
	// up bare (the footgun this fixes).
	_, err = mgr.Stop(ctx, "web")
	require.NoError(t, err)
	_, err = mgr.Start(ctx, "web")
	require.NoError(t, err)
	require.Equal(t, []bool{true, true}, rt.runApps, "restart must re-run the app")

	got, err := mgr.Get(ctx, "web")
	require.NoError(t, err)
	require.True(t, got.RunApp)
}

func TestSessionsRequireSessionRuntime(t *testing.T) {
	// A nil runtime models a non-session-capable runtime (e.g. mock/runc).
	mgr := newManager(t, nil, newFakeSnapshot())
	_, err := mgr.Create(context.Background(), "dev", "ubuntu", "", "", false, "", nil, nil)
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}

func TestUpdateSession(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "ubuntu", "allowlist", "on", false, "", nil, nil)
	require.NoError(t, err)

	// Change egress; empty gateway leaves it unchanged. A running session needs
	// a restart to apply (the policy is baked into the fork at boot).
	s, restart, err := mgr.UpdateSession(ctx, "dev", "open", "", nil, false)
	require.NoError(t, err)
	require.True(t, restart)
	require.Equal(t, "open", s.EgressPolicy)
	require.Equal(t, "on", s.Gateway)

	got, err := mgr.Get(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, "open", got.EgressPolicy)

	// Invalid values are rejected.
	_, _, err = mgr.UpdateSession(ctx, "dev", "bogus", "", nil, false)
	require.Error(t, err)
	_, _, err = mgr.UpdateSession(ctx, "dev", "", "maybe", nil, false)
	require.Error(t, err)

	// A stopped session applies on next start, so no restart flag.
	_, err = mgr.Stop(ctx, "dev")
	require.NoError(t, err)
	_, restart, err = mgr.UpdateSession(ctx, "dev", "none", "off", nil, false)
	require.NoError(t, err)
	require.False(t, restart)
}

func TestCommitImageCommitsRunningSession(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	imagesDir := t.TempDir()
	mgr := newManagerWithOpts(t, rt, snap, session.Options{ImagesDir: imagesDir})
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev-1", "fletcher-base", "", "", false, "", nil, nil)
	require.NoError(t, err)

	img, err := mgr.CommitImage(ctx, "dev-1", session.CommitImageParams{
		Name:        "webapp",
		Entrypoint:  []string{"node", "/workspace/server.js"},
		ExposedPort: 3000,
	})
	require.NoError(t, err)
	require.Equal(t, "webapp", img)

	// The running guest was synced before the commit.
	require.Len(t, rt.handles, 1)
	require.Equal(t, []string{"sync"}, rt.handles[0].execs)

	// The fork was committed under the requested template name, with the app
	// launch spec injected into the image.
	require.NotEmpty(t, snap.templates["webapp"])
	require.Contains(t, snap.committedFiles, "/etc/fletcher/app.json")
	require.Contains(t, string(snap.committedFiles["/etc/fletcher/app.json"]), "node")

	// Sidecar metadata records the entrypoint, port, and source session.
	meta, found, err := image.ReadMeta(imagesDir, "webapp")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "session:dev-1", meta.Source)
	require.Equal(t, []string{"node", "/workspace/server.js"}, meta.Entrypoint)
	require.Equal(t, 3000, meta.ExposedPort)
}

func TestCommitImageStoppedSessionCommitsWithoutExec(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "base", "", "", false, "", nil, nil)
	require.NoError(t, err)
	_, err = mgr.Stop(ctx, "dev")
	require.NoError(t, err)

	img, err := mgr.CommitImage(ctx, "dev", session.CommitImageParams{Name: "frozen"})
	require.NoError(t, err)
	require.Equal(t, "frozen", img)
	require.Equal(t, []string{"sync"}, rt.handles[0].execs, "one sync from the stop, none from the commit")
	require.NotEmpty(t, snap.templates["frozen"])
}

func TestCommitImageStoppedSessionAcceptsEntrypoint(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "base", "", "", false, "", nil, nil)
	require.NoError(t, err)
	_, err = mgr.Stop(ctx, "dev")
	require.NoError(t, err)

	_, err = mgr.CommitImage(ctx, "dev", session.CommitImageParams{
		Name:       "frozen2",
		Entrypoint: []string{"node"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"sync"}, rt.handles[0].execs, "one sync from the stop, none from the commit")
	require.Contains(t, snap.committedFiles, "/etc/fletcher/app.json")
}

func TestCommitImageConflictWithoutForce(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "base", "", "", false, "", nil, nil)
	require.NoError(t, err)

	_, err = mgr.CommitImage(ctx, "dev", session.CommitImageParams{Name: "snap1"})
	require.NoError(t, err)
	_, err = mgr.CommitImage(ctx, "dev", session.CommitImageParams{Name: "snap1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
	_, err = mgr.CommitImage(ctx, "dev", session.CommitImageParams{Name: "snap1", Force: true})
	require.NoError(t, err)
}

func TestCommitImageRejectsInvalidName(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "base", "", "", false, "", nil, nil)
	require.NoError(t, err)

	for _, bad := range []string{"", "Has Caps", "../escape", ".hidden", "-flag", "a/b"} {
		_, err = mgr.CommitImage(ctx, "dev", session.CommitImageParams{Name: bad})
		require.Error(t, err, "name %q", bad)
	}
}

func TestCommitImageRequiresCommittingDriver(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, nonCommittingSnapshot{inner: newFakeSnapshot()})
	ctx := context.Background()

	_, err := mgr.Create(ctx, "dev", "base", "", "", false, "", nil, nil)
	require.NoError(t, err)

	_, err = mgr.CommitImage(ctx, "dev", session.CommitImageParams{Name: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot commit")
}

func TestSessionEnvCarriesIdentity(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	ctx := context.Background()

	created, err := mgr.Create(ctx, "dev", "base", "", "", false, "", nil, nil)
	require.NoError(t, err)
	require.Contains(t, rt.envs[0], "FLETCHER_SESSION_ID="+created.ID)
	require.Contains(t, rt.envs[0], "FLETCHER_SESSION_NAME=dev")

	// Identity survives stop/start (rebuilt from the row).
	_, err = mgr.Stop(ctx, "dev")
	require.NoError(t, err)
	_, err = mgr.Start(ctx, "dev")
	require.NoError(t, err)
	require.Contains(t, rt.envs[1], "FLETCHER_SESSION_NAME=dev")
}

func TestRedeployKeepsPreviousForkAndRollbackSwaps(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	created, err := mgr.Create(ctx, "app", "webapp", "", "", true, "", nil, nil)
	require.NoError(t, err)
	require.False(t, created.HasRollback)
	firstFork := rt.started[0]

	// No rollback target before any redeploy.
	_, err = mgr.Rollback(ctx, "app")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no previous fork")

	// Redeploy: a new fork boots, the old one is retired (not deleted).
	redeployed, err := mgr.Redeploy(ctx, "app", "")
	require.NoError(t, err)
	require.True(t, redeployed.HasRollback)
	require.Len(t, rt.started, 2)
	secondFork := rt.started[1]
	require.NotEqual(t, firstFork, secondFork)
	require.Empty(t, snap.deleted, "the retired fork is kept for rollback")

	// Rollback boots the retired fork again.
	rolled, err := mgr.Rollback(ctx, "app")
	require.NoError(t, err)
	require.True(t, rolled.HasRollback, "rolling forward again stays possible")
	require.Len(t, rt.started, 3)
	require.Equal(t, firstFork, rt.started[2])

	// A second rollback swaps forward to the redeployed fork.
	_, err = mgr.Rollback(ctx, "app")
	require.NoError(t, err)
	require.Equal(t, secondFork, rt.started[3])
}

func TestRedeployRetargetsImageAndDropsOlderPrev(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "app", "v1img", "", "", true, "", nil, nil)
	require.NoError(t, err)

	// Retarget to a different template.
	redeployed, err := mgr.Redeploy(ctx, "app", "v2img")
	require.NoError(t, err)
	require.Equal(t, "v2img", redeployed.Image)
	require.Contains(t, rt.started[1], "fork_v2img")

	// A second redeploy reclaims the oldest fork (only one rollback level).
	_, err = mgr.Redeploy(ctx, "app", "")
	require.NoError(t, err)
	require.Len(t, snap.deleted, 1)
	require.Contains(t, snap.deleted[0], "fork_v1img")
}

func TestDeleteReclaimsPreviousFork(t *testing.T) {
	rt := &fakeRuntime{}
	snap := newFakeSnapshot()
	mgr := newManager(t, rt, snap)
	ctx := context.Background()

	_, err := mgr.Create(ctx, "app", "img", "", "", true, "", nil, nil)
	require.NoError(t, err)
	_, err = mgr.Redeploy(ctx, "app", "")
	require.NoError(t, err)

	ok, err := mgr.Delete(ctx, "app")
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, snap.live, "both the active and retired forks are reclaimed")
}

// fakeVolumes is a VolumeResolver with one volume that tracks attachment via
// the resolved flag (mirroring what the real manager derives from session rows).
type fakeVolumes struct {
	id, name, path string
	attachedTo     string
}

func (f *fakeVolumes) ResolveAttachable(_ context.Context, ref string) (string, string, error) {
	if ref != f.id && ref != f.name {
		return "", "", errs.New(errs.CategoryNotFound, "volume not found")
	}
	if f.attachedTo != "" {
		return "", "", errs.Newf(errs.CategoryConflict, "volume %q is already attached to session %q", f.name, f.attachedTo)
	}
	return f.id, f.path, nil
}

func (f *fakeVolumes) PathFor(_ context.Context, id string) (string, error) {
	if id != f.id {
		return "", errs.New(errs.CategoryNotFound, "volume not found")
	}
	return f.path, nil
}

func (f *fakeVolumes) NameFor(_ context.Context, id string) (string, error) {
	if id != f.id {
		return "", errs.New(errs.CategoryNotFound, "volume not found")
	}
	return f.name, nil
}

func TestCreateAttachesVolumeAndItSurvivesLifecycle(t *testing.T) {
	rt := &fakeRuntime{}
	mgr, q := newManagerWithQuerier(t, rt, newFakeSnapshot())
	volMgr := volume.NewManager(q, fakeVolumeProvisioner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.SetVolumes(volMgr)
	ctx := context.Background()

	vol, err := volMgr.Create(ctx, "data", 0)
	require.NoError(t, err)

	created, err := mgr.Create(ctx, "dev", "img", "", "", false, "data", nil, nil)
	require.NoError(t, err)
	require.Equal(t, vol.ID, created.VolumeID)
	require.Equal(t, "data", created.VolumeName)
	require.Equal(t, vol.Path, rt.volumes[0])

	// Attached: the volume is not attachable elsewhere and not deletable.
	_, err = mgr.Create(ctx, "dev2", "img", "", "", false, "data", nil, nil)
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
	require.Error(t, volMgr.Delete(ctx, "data"))

	// The volume rides through stop/start...
	_, err = mgr.Stop(ctx, "dev")
	require.NoError(t, err)
	_, err = mgr.Start(ctx, "dev")
	require.NoError(t, err)
	require.Equal(t, vol.Path, rt.volumes[1])

	// ...and through a redeploy (fresh fork, same volume).
	redeployed, err := mgr.Redeploy(ctx, "dev", "")
	require.NoError(t, err)
	require.Equal(t, vol.ID, redeployed.VolumeID)
	require.Equal(t, vol.Path, rt.volumes[2])

	// Deleting the session detaches; the volume outlives it and is deletable.
	ok, err := mgr.Delete(ctx, "dev")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, volMgr.Delete(ctx, "data"))
}

// fakeVolumeProvisioner satisfies snapshot.VolumeProvisioner in memory.
type fakeVolumeProvisioner struct{}

func (fakeVolumeProvisioner) CreateVolume(_ context.Context, id string, _ int64) (string, error) {
	return "/fake/volumes/" + id + ".ext4", nil
}

func (fakeVolumeProvisioner) DeleteVolume(context.Context, string) error { return nil }

func TestCreateWithVolumeRequiresResolver(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	_, err := mgr.Create(context.Background(), "dev", "img", "", "", false, "data", nil, nil)
	require.Error(t, err)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
}

func TestCreateWithAttachedVolumeConflicts(t *testing.T) {
	mgr := newManager(t, &fakeRuntime{}, newFakeSnapshot())
	mgr.SetVolumes(&fakeVolumes{id: "vol_1", name: "data", path: "/fake/v", attachedTo: "other"})
	_, err := mgr.Create(context.Background(), "dev", "img", "", "", false, "data", nil, nil)
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
}
