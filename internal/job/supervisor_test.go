package job_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/job"
	runtimemock "github.com/joshjon/fletcher/internal/runtime/mockdriver"
	snapmock "github.com/joshjon/fletcher/internal/snapshot/mockdriver"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// supervisorRig wires a supervisor to in-memory storage and the mock
// runtime + snapshot drivers.
type supervisorRig struct {
	db      *sql.DB
	queries *sqliteq.Queries
	svc     *job.Service
	sup     *job.Supervisor
}

func newSupervisorRig(t *testing.T) *supervisorRig {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fletcher.db")
	db, err := sqlite.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))

	queries := sqliteq.New(db)
	snapDriver, err := snapmock.New(filepath.Join(dir, "snapshots"))
	require.NoError(t, err)
	rtDriver := runtimemock.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := job.NewSupervisor(queries, rtDriver, snapDriver, logger, job.SupervisorOptions{
		PollInterval:  50 * time.Millisecond,
		DrainDeadline: 5 * time.Second,
	})
	svc := job.NewService(queries, sup)
	return &supervisorRig{db: db, queries: queries, svc: svc, sup: sup}
}

// start runs the supervisor in a goroutine; the returned func blocks until
// the supervisor has fully exited after ctx cancellation.
func (r *supervisorRig) start(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.sup.Run(ctx)
	}()
	return func() { <-done }
}

func waitForStatus(t *testing.T, svc *job.Service, id string, want job.Status, timeout time.Duration) job.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, err := svc.Get(context.Background(), id)
		require.NoError(t, err)
		if got.Status == want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	last, _ := svc.Get(context.Background(), id)
	t.Fatalf("job %s never reached status %q (last=%q)", id, want, last.Status)
	return job.Job{}
}

func TestSupervisorRunsSuccessfulJob(t *testing.T) {
	r := newSupervisorRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait := r.start(ctx)

	created, err := r.svc.Create(ctx, job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Name:    "ok",
		Command: "exit 0",
		Image:   "mock",
	})
	require.NoError(t, err)

	got := waitForStatus(t, r.svc, created.ID, job.StatusSucceeded, 5*time.Second)
	require.NotNil(t, got.StartedAt)
	require.NotNil(t, got.CompletedAt)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, int32(0), *got.ExitCode)

	cancel()
	wait()
}

func TestSupervisorMarksFailingJob(t *testing.T) {
	r := newSupervisorRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait := r.start(ctx)

	created, err := r.svc.Create(ctx, job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Name:    "fail",
		Command: "exit 7",
		Image:   "mock",
	})
	require.NoError(t, err)

	got := waitForStatus(t, r.svc, created.ID, job.StatusFailed, 5*time.Second)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, int32(7), *got.ExitCode)

	cancel()
	wait()
}

func TestSupervisorCancelsRunningJob(t *testing.T) {
	r := newSupervisorRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait := r.start(ctx)

	created, err := r.svc.Create(ctx, job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Name:    "long",
		Command: "sleep 60",
		Image:   "mock",
	})
	require.NoError(t, err)

	waitForStatus(t, r.svc, created.ID, job.StatusRunning, 3*time.Second)

	ok, err := r.svc.Cancel(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, ok)

	got := waitForStatus(t, r.svc, created.ID, job.StatusCancelled, 3*time.Second)
	require.NotNil(t, got.CompletedAt)

	cancel()
	wait()
}

func TestSupervisorReconcilesOrphanRunningOnBoot(t *testing.T) {
	r := newSupervisorRig(t)
	ctx := context.Background()

	created, err := r.svc.Create(ctx, job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Name:    "orphan",
		Command: "exit 0",
		Image:   "mock",
	})
	require.NoError(t, err)

	// Simulate a daemon crash: promote queued → running directly, leaving
	// the row in the orphan state the supervisor should reconcile on boot.
	now := time.Now().Unix()
	require.NoError(t, r.queries.MarkJobStarted(ctx, sqliteq.MarkJobStartedParams{
		StartedAt: &now,
		UpdatedAt: now,
		ID:        created.ID,
	}))

	runCtx, cancel := context.WithCancel(ctx)
	wait := r.start(runCtx)

	got := waitForStatus(t, r.svc, created.ID, job.StatusSucceeded, 5*time.Second)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, int32(0), *got.ExitCode)

	cancel()
	wait()
}
