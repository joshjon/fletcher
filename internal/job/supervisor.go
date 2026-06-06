package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/snapshot"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// Supervisor owns the daemon's job-execution loop: it polls SQLite for
// queued jobs, claims them via an atomic queued→running transition, then
// runs each one inside a fresh snapshot using the configured runtime.
// It also reconciles on boot (resetting orphan "running" rows back to
// "queued") and supports surgical cancellation of in-flight jobs.
type Supervisor struct {
	q        sqliteq.Querier
	runtime  runtime.Driver
	snapshot snapshot.Driver
	logger   *slog.Logger

	pollInterval    time.Duration
	drainDeadline   time.Duration
	jobEnv          []string
	credentialsRoot string

	mu     sync.Mutex
	active map[string]context.CancelFunc

	wg     sync.WaitGroup
	wakeup chan struct{}
}

// SupervisorOptions configures non-essential Supervisor behaviour.
type SupervisorOptions struct {
	// PollInterval is how often the supervisor scans for queued jobs in the
	// absence of an explicit Notify. Default 2s.
	PollInterval time.Duration
	// DrainDeadline caps how long Run will wait for in-flight jobs to finish
	// once ctx is cancelled. Default 30s.
	DrainDeadline time.Duration
	// JobEnv is appended to every job's runtime.Spec.Env. The daemon uses
	// this to inject OPENAI_BASE_URL pointing at the local model gateway
	// (so agents inside forks never see real API keys).
	JobEnv []string
	// CredentialsRoot is the host directory under which each credential's
	// HostRelPath (see AllowedCredentials) is resolved. Empty disables
	// trusted-credential mode: jobs that request credentials fail at start.
	CredentialsRoot string
}

// NewSupervisor wires a Supervisor to its dependencies.
func NewSupervisor(q sqliteq.Querier, rt runtime.Driver, sn snapshot.Driver, logger *slog.Logger, opts SupervisorOptions) *Supervisor {
	if opts.PollInterval == 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.DrainDeadline == 0 {
		opts.DrainDeadline = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{
		q:               q,
		runtime:         rt,
		snapshot:        sn,
		logger:          logger,
		pollInterval:    opts.PollInterval,
		drainDeadline:   opts.DrainDeadline,
		jobEnv:          append([]string(nil), opts.JobEnv...),
		credentialsRoot: opts.CredentialsRoot,
		active:          make(map[string]context.CancelFunc),
		wakeup:          make(chan struct{}, 1),
	}
}

// Run blocks until ctx is cancelled. On entry it reconciles any rows left
// "running" from a previous boot back to "queued". On exit it waits up to
// DrainDeadline for in-flight runOne goroutines to finish so DB writes do
// not race the daemon's db.Close.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.reconcileOnBoot(ctx); err != nil {
		return fmt.Errorf("supervisor reconcile: %w", err)
	}

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		s.scheduleDueCronJobs(ctx)
		s.pickAndRun(ctx)
		select {
		case <-ctx.Done():
			s.drain()
			return ctx.Err()
		case <-s.wakeup:
		case <-ticker.C:
		}
	}
}

// scheduleDueCronJobs fires every cron job whose next_run_at has arrived. Each
// fire creates a child ephemeral run (which the same loop then executes) and
// advances the definition's next_run_at to the following match. The run is a
// plain job, so runOne needs no cron-awareness - one engine (DESIGN.md §4). A
// missed window while the daemon was down fires once on recovery (no backfill).
func (s *Supervisor) scheduleDueCronJobs(ctx context.Context) {
	now := time.Now()
	nowUnix := now.Unix()
	due, err := s.q.ListDueCronJobs(ctx, &nowUnix)
	if err != nil {
		s.logger.Error("list due cron jobs", slog.String("err", err.Error()))
		return
	}
	for _, c := range due {
		// Parse before firing: a definition that no longer parses is marked
		// failed so it stops being due instead of re-firing every tick.
		sched, perr := ParseSchedule(c.Schedule)
		if perr != nil {
			s.logger.Error("cron job has an invalid schedule; marking failed",
				slog.String("job_id", c.ID), slog.String("err", perr.Error()))
			s.failCronDefinition(ctx, c.ID, perr.Error(), nowUnix)
			continue
		}

		childID, idErr := typeid.WithPrefix(idPrefix)
		if idErr != nil {
			s.logger.Error("generate cron run id", slog.String("job_id", c.ID), slog.String("err", idErr.Error()))
			continue
		}
		parentID := c.ID
		if _, cErr := s.q.CreateJob(ctx, sqliteq.CreateJobParams{
			ID:          childID.String(),
			Status:      string(StatusQueued),
			TriggerKind: string(TriggerEphemeral),
			Name:        c.Name,
			Command:     c.Command,
			Image:       c.Image,
			Credentials: c.Credentials,
			CreatedAt:   nowUnix,
			UpdatedAt:   nowUnix,
			ParentID:    &parentID,
		}); cErr != nil {
			s.logger.Error("create cron run", slog.String("job_id", c.ID), slog.String("err", cErr.Error()))
			continue
		}

		next := sched.Next(now).Unix()
		if uErr := s.q.SetJobNextRun(ctx, sqliteq.SetJobNextRunParams{
			NextRunAt: &next,
			UpdatedAt: nowUnix,
			ID:        c.ID,
		}); uErr != nil {
			s.logger.Error("advance cron next run", slog.String("job_id", c.ID), slog.String("err", uErr.Error()))
			continue
		}
		s.logger.Info("scheduled cron run",
			slog.String("cron_job_id", c.ID),
			slog.String("run_id", childID.String()),
			slog.Time("next_run", time.Unix(next, 0)))
	}
}

// failCronDefinition marks a cron definition failed (e.g. its stored schedule no
// longer parses) so it stops being selected as due.
func (s *Supervisor) failCronDefinition(ctx context.Context, id, message string, nowUnix int64) {
	code := int64(-1)
	if err := s.q.MarkJobFailed(ctx, sqliteq.MarkJobFailedParams{
		ExitCode:     &code,
		ErrorMessage: &message,
		CompletedAt:  &nowUnix,
		UpdatedAt:    nowUnix,
		ID:           id,
	}); err != nil {
		s.logger.Error("mark cron definition failed", slog.String("job_id", id), slog.String("err", err.Error()))
	}
}

// Notify wakes the supervisor early. It is non-blocking and coalesces
// rapid notifications: only one extra wakeup is buffered.
func (s *Supervisor) Notify() {
	select {
	case s.wakeup <- struct{}{}:
	default:
	}
}

// CancelRunning signals a running job's process tree to terminate. It
// returns true iff a process was actually tracked and cancelled. The DB
// transition to "cancelled" is the caller's responsibility (see
// Service.Cancel) - this method only kills the child.
func (s *Supervisor) CancelRunning(jobID string) bool {
	s.mu.Lock()
	cancel, ok := s.active[jobID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// reconcileOnBoot resets any orphan "running" rows to "queued". They were
// left in that state by an ungraceful daemon exit and need to be re-run.
func (s *Supervisor) reconcileOnBoot(ctx context.Context) error {
	rows, err := s.q.ListJobsByStatus(ctx, sqliteq.ListJobsByStatusParams{
		Status: string(StatusRunning),
		Limit:  1000,
	})
	if err != nil {
		return fmt.Errorf("list running on boot: %w", err)
	}
	now := time.Now().Unix()
	for _, r := range rows {
		s.logger.Info("resetting orphan running job to queued", slog.String("job_id", r.ID))
		if err := s.q.UpdateJobStatus(ctx, sqliteq.UpdateJobStatusParams{
			Status:    string(StatusQueued),
			UpdatedAt: now,
			ID:        r.ID,
		}); err != nil {
			return fmt.Errorf("reset running job %s: %w", r.ID, err)
		}
	}
	return nil
}

// pickAndRun fetches a batch of queued jobs and starts each one. The DB
// transition itself is the atomic claim: MarkJobStarted's WHERE clause
// only matches rows that are still "queued".
func (s *Supervisor) pickAndRun(ctx context.Context) {
	rows, err := s.q.ListJobsByStatus(ctx, sqliteq.ListJobsByStatusParams{
		Status: string(StatusQueued),
		Limit:  10,
	})
	if err != nil {
		s.logger.Error("list queued jobs", slog.String("err", err.Error()))
		return
	}
	for _, r := range rows {
		s.startJob(ctx, r)
	}
}

func (s *Supervisor) startJob(parentCtx context.Context, row sqliteq.Job) {
	now := time.Now().Unix()
	if err := s.q.MarkJobStarted(parentCtx, sqliteq.MarkJobStartedParams{
		StartedAt: &now,
		UpdatedAt: now,
		ID:        row.ID,
	}); err != nil {
		s.logger.Error("mark job started", slog.String("job_id", row.ID), slog.String("err", err.Error()))
		return
	}

	jobCtx, cancel := context.WithCancel(parentCtx)
	s.mu.Lock()
	s.active[row.ID] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.active, row.ID)
			s.mu.Unlock()
			cancel()
		}()
		s.runOne(jobCtx, row)
	}()
}

func (s *Supervisor) runOne(jobCtx context.Context, row sqliteq.Job) {
	log := s.logger.With(
		slog.String("job_id", row.ID),
		slog.String("job_name", row.Name),
	)
	log.Info("starting job")

	mounts, err := s.resolveCredentials(row.Credentials)
	if err != nil {
		log.Error("resolve credentials", slog.String("err", err.Error()))
		s.markFailed(row.ID, -1, fmt.Sprintf("resolve credentials: %s", err))
		return
	}

	snap, err := s.snapshot.Create(jobCtx, row.Image)
	if err != nil {
		log.Error("create snapshot", slog.String("err", err.Error()))
		s.markFailed(row.ID, -1, fmt.Sprintf("create snapshot: %s", err))
		return
	}
	defer s.deleteSnapshot(snap.ID, log)

	// Capture the tail of the job's stdout+stderr so a failure is not
	// opaque: it is logged and stored in the job's error message. stdout and
	// stderr share one buffer (interleaved, as a terminal would show them).
	out := &cappedBuffer{max: 16 << 10}
	result, err := s.runtime.Run(jobCtx, runtime.Spec{
		JobID:   row.ID,
		Image:   row.Image,
		Command: row.Command,
		WorkDir: snap.Path,
		Env:     s.jobEnv,
		Mounts:  mounts,
	}, out, out)

	// Two cancellation paths share ctx.Canceled: targeted CancelRunning
	// (DB already says "cancelled") vs daemon shutdown (DB still says
	// "running"). We distinguish by reading the DB.
	if errors.Is(jobCtx.Err(), context.Canceled) {
		current, gerr := s.q.GetJob(context.Background(), row.ID)
		if gerr == nil && Status(current.Status) == StatusCancelled {
			log.Info("job cancelled by user")
			return
		}
		log.Info("job interrupted (daemon shutting down); leaving as running for boot reconcile")
		return
	}

	if err != nil {
		log.Error("runtime error", slog.String("err", err.Error()), slog.String("output", out.String()))
		s.markFailed(row.ID, -1, joinErrOutput(err.Error(), out.String()))
		return
	}

	if result.ExitCode == 0 {
		log.Info("job succeeded")
		s.markSucceeded(row.ID, result.ExitCode)
		return
	}

	log.Info("job exited non-zero",
		slog.Int64("exit_code", int64(result.ExitCode)),
		slog.String("output", out.String()))
	s.markFailed(row.ID, result.ExitCode, out.String())
}

// cappedBuffer is an io.Writer that retains only the last max bytes written,
// so a job's output can be surfaced on failure without unbounded memory. It is
// safe for the concurrent stdout/stderr writes a runtime driver performs.
type cappedBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if len(c.buf) > c.max {
		c.buf = c.buf[len(c.buf)-c.max:]
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(string(c.buf))
}

// joinErrOutput combines a driver error with any captured job output for the
// stored failure message.
func joinErrOutput(errMsg, output string) string {
	if output == "" {
		return errMsg
	}
	return errMsg + ": " + output
}

func (s *Supervisor) markSucceeded(jobID string, exitCode int32) {
	now := time.Now().Unix()
	ec := int64(exitCode)
	if err := s.q.MarkJobSucceeded(context.Background(), sqliteq.MarkJobSucceededParams{
		ExitCode:    &ec,
		CompletedAt: &now,
		UpdatedAt:   now,
		ID:          jobID,
	}); err != nil {
		s.logger.Error("mark succeeded", slog.String("job_id", jobID), slog.String("err", err.Error()))
	}
}

func (s *Supervisor) markFailed(jobID string, exitCode int32, message string) {
	now := time.Now().Unix()
	ec := int64(exitCode)
	params := sqliteq.MarkJobFailedParams{
		ExitCode:    &ec,
		CompletedAt: &now,
		UpdatedAt:   now,
		ID:          jobID,
	}
	if message != "" {
		params.ErrorMessage = &message
	}
	if err := s.q.MarkJobFailed(context.Background(), params); err != nil {
		s.logger.Error("mark failed", slog.String("job_id", jobID), slog.String("err", err.Error()))
	}
}

func (s *Supervisor) deleteSnapshot(id string, log *slog.Logger) {
	// Use a fresh context: the job's ctx is typically cancelled by the time
	// we're cleaning up.
	if err := s.snapshot.Delete(context.Background(), id); err != nil {
		log.Warn("delete snapshot", slog.String("snapshot_id", id), slog.String("err", err.Error()))
	}
}

// resolveCredentials turns the row's stored credential list (JSON-encoded
// allowlist names) into concrete runtime.Mount entries by joining each
// credential's HostRelPath onto the supervisor's CredentialsRoot. Each
// resolved source path must exist on the host; missing dirs fail the
// job early with a clear message rather than producing a confusing
// bind-mount error from the runtime.
func (s *Supervisor) resolveCredentials(encoded string) ([]runtime.Mount, error) {
	names, err := decodeCredentials(encoded)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	if s.credentialsRoot == "" {
		return nil, fmt.Errorf("job requests credentials %v but daemon has no credentials root configured", names)
	}
	mounts := make([]runtime.Mount, 0, len(names))
	for _, name := range names {
		spec, ok := AllowedCredentials[name]
		if !ok {
			return nil, fmt.Errorf("unknown credential %q stored on job (allowed: %s)", name, allowedCredentialNames())
		}
		src := filepath.Join(s.credentialsRoot, spec.HostRelPath)
		if _, statErr := os.Stat(src); statErr != nil {
			return nil, fmt.Errorf("credential %q: host path %s: %w", name, src, statErr)
		}
		mounts = append(mounts, runtime.Mount{
			Source:      src,
			Destination: spec.GuestPath,
			ReadOnly:    false,
		})
	}
	return mounts, nil
}

// drain waits up to drainDeadline for in-flight runOne goroutines to
// finish. Past the deadline it returns regardless, accepting that some DB
// writes may race db.Close (the OS reaps the process either way).
func (s *Supervisor) drain() {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(s.drainDeadline):
		s.logger.Warn("supervisor drain timeout; abandoning in-flight jobs")
	}
}
