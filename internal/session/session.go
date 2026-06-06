// Package session manages durable sessions: persistent microVMs you create,
// exec into, stop, start, and delete. Sessions share the runtime and snapshot
// machinery with jobs (one execution engine, DESIGN.md §4) but have their own
// lifecycle - the fork persists across stop/start instead of being torn down.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/snapshot"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// idPrefix is the typeid prefix for session IDs (e.g. "session_01h...").
const idPrefix = "session"

// State is a session's lifecycle state.
type State string

const (
	// StateRunning means the session's VM is booted and exec-ready.
	StateRunning State = "running"
	// StateStopped means the VM is down but its fork persists on disk.
	StateStopped State = "stopped"
)

// Session is the domain view of a session (the fork id/path stay internal).
type Session struct {
	ID         string
	Name       string
	Image      string
	State      State
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastUsedAt *time.Time
	DiskBytes  int64
}

// ErrNotFound is returned when a session ref matches nothing.
var ErrNotFound = errs.New(errs.CategoryNotFound, "session not found")

// Options tune session limits and the work-based idle auto-stop.
type Options struct {
	// IdleTimeout auto-stops a session idle (no work in flight) this long; 0
	// disables the reaper.
	IdleTimeout time.Duration
	// MaxCount caps the number of sessions; 0 disables the cap.
	MaxCount int
	// MaxDiskBytes caps total session disk; 0 disables the cap.
	MaxDiskBytes int64
}

// idleLoadThreshold is the guest 1-minute load average below which a session
// with no active host operation counts as having no work in flight. A truly
// idle VM sits near zero; a running agent or build pushes it well above this.
const idleLoadThreshold = 0.2

// Manager owns session lifecycle and the live VM handles. The runtime is the
// session-capable driver (nil when the configured runtime cannot host
// sessions); snapshot creates and destroys the persistent forks.
type Manager struct {
	q        sqliteq.Querier
	snapshot snapshot.Driver
	runtime  runtime.SessionRuntime
	env      []string
	logger   *slog.Logger
	opts     Options

	mu      sync.Mutex
	handles map[string]runtime.SessionHandle
	busy    map[string]int // sessions with an in-flight host op (exec/shell/ssh)
}

// NewManager constructs a Manager. rt may be nil if the configured runtime does
// not support sessions, in which case lifecycle calls fail with a clear error.
func NewManager(q sqliteq.Querier, snap snapshot.Driver, rt runtime.SessionRuntime, env []string, logger *slog.Logger, opts Options) *Manager {
	return &Manager{
		q:        q,
		snapshot: snap,
		runtime:  rt,
		env:      env,
		logger:   logger,
		opts:     opts,
		handles:  make(map[string]runtime.SessionHandle),
		busy:     make(map[string]int),
	}
}

// ExecResult is the captured output of a command run in a session.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int32
}

func (m *Manager) requireRuntime() error {
	if m.runtime == nil {
		return errs.New(errs.CategoryFailedPrecondition,
			"sessions require the firecracker runtime (set it with `fletcher settings set runtime firecracker`)")
	}
	return nil
}

// Create provisions a session's persistent fork, boots its VM, and records it.
func (m *Manager) Create(ctx context.Context, name, image string) (Session, error) {
	if err := m.requireRuntime(); err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(name) == "" {
		return Session{}, errs.New(errs.CategoryInvalidArgument, "name is required")
	}
	if strings.TrimSpace(image) == "" {
		return Session{}, errs.New(errs.CategoryInvalidArgument, "image is required")
	}
	if err := m.checkCaps(ctx); err != nil {
		return Session{}, err
	}

	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Session{}, fmt.Errorf("generate session id: %w", err)
	}
	sessionID := id.String()

	fork, err := m.snapshot.Create(ctx, image)
	if err != nil {
		return Session{}, fmt.Errorf("create session fork: %w", err)
	}

	handle, err := m.runtime.StartSession(ctx, runtime.SessionSpec{
		SessionID:  sessionID,
		RootfsPath: fork.Path,
		Env:        m.env,
	})
	if err != nil {
		_ = m.snapshot.Delete(context.WithoutCancel(ctx), fork.ID)
		return Session{}, fmt.Errorf("start session vm: %w", err)
	}

	now := time.Now().Unix()
	row, err := m.q.CreateSession(ctx, sqliteq.CreateSessionParams{
		ID:        sessionID,
		Name:      name,
		Image:     image,
		State:     string(StateRunning),
		ForkID:    fork.ID,
		ForkPath:  fork.Path,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		_ = handle.Stop(context.WithoutCancel(ctx))
		_ = m.snapshot.Delete(context.WithoutCancel(ctx), fork.ID)
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Session{}, errs.Newf(errs.CategoryConflict, "a session named %q already exists", name)
		}
		return Session{}, fmt.Errorf("record session: %w", err)
	}

	m.putHandle(sessionID, handle)
	return sessionFromRow(row), nil
}

// Get returns the session matching ref (an ID or name).
func (m *Manager) Get(ctx context.Context, ref string) (Session, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}
	return sessionFromRow(row), nil
}

// List returns all sessions, newest first.
func (m *Manager) List(ctx context.Context) ([]Session, error) {
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]Session, len(rows))
	for i, r := range rows {
		out[i] = sessionFromRow(r)
	}
	return out, nil
}

// Start boots a stopped session's VM against its persisted fork.
func (m *Manager) Start(ctx context.Context, ref string) (Session, error) {
	if err := m.requireRuntime(); err != nil {
		return Session{}, err
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}
	if State(row.State) == StateRunning {
		return sessionFromRow(row), nil // already running
	}

	handle, err := m.runtime.StartSession(ctx, runtime.SessionSpec{
		SessionID:  row.ID,
		RootfsPath: row.ForkPath,
		Env:        m.env,
	})
	if err != nil {
		return Session{}, fmt.Errorf("start session vm: %w", err)
	}
	m.putHandle(row.ID, handle)

	if err := m.setState(ctx, row.ID, StateRunning); err != nil {
		return Session{}, err
	}
	row.State = string(StateRunning)
	return sessionFromRow(row), nil
}

// Stop stops a running session's VM, keeping its fork on disk.
func (m *Manager) Stop(ctx context.Context, ref string) (Session, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}
	if handle := m.takeHandle(row.ID); handle != nil {
		if serr := handle.Stop(ctx); serr != nil {
			m.logger.Warn("stop session vm", slog.String("session_id", row.ID), slog.String("err", serr.Error()))
		}
	}
	if err := m.setState(ctx, row.ID, StateStopped); err != nil {
		return Session{}, err
	}
	row.State = string(StateStopped)
	return sessionFromRow(row), nil
}

// Delete stops the VM (if running) and destroys the fork.
func (m *Manager) Delete(ctx context.Context, ref string) (bool, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if handle := m.takeHandle(row.ID); handle != nil {
		if serr := handle.Stop(ctx); serr != nil {
			m.logger.Warn("stop session vm on delete", slog.String("session_id", row.ID), slog.String("err", serr.Error()))
		}
	}
	if derr := m.snapshot.Delete(context.WithoutCancel(ctx), row.ForkID); derr != nil {
		m.logger.Warn("delete session fork", slog.String("session_id", row.ID), slog.String("err", derr.Error()))
	}
	// Drop any on-disk VM state (e.g. a hibernation snapshot) for the session.
	if m.runtime != nil {
		if derr := m.runtime.DiscardSession(context.WithoutCancel(ctx), row.ID); derr != nil {
			m.logger.Warn("discard session vm state", slog.String("session_id", row.ID), slog.String("err", derr.Error()))
		}
	}
	if _, derr := m.q.DeleteSession(ctx, row.ID); derr != nil {
		return false, fmt.Errorf("delete session: %w", derr)
	}
	return true, nil
}

// Exec runs command inside a running session and returns its captured output.
func (m *Manager) Exec(ctx context.Context, ref, command string) (ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, errs.New(errs.CategoryInvalidArgument, "command is required")
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return ExecResult{}, err
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return ExecResult{}, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running; start it with `fletcher session start`", row.Name)
	}

	m.markBusy(row.ID)
	defer m.unmarkBusy(row.ID)

	var stdout, stderr strings.Builder
	res, err := handle.Exec(ctx, runtime.Spec{Command: command, Env: m.env}, &stdout, &stderr)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec in session: %w", err)
	}
	m.touch(ctx, row.ID)
	return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: res.ExitCode}, nil
}

// Shell opens an interactive PTY in a running session, bridging the caller's
// stdin/stdout and window resizes to the VM, and returns the shell's exit code.
func (m *Manager) Shell(ctx context.Context, ref string, spec runtime.ShellSpec, stdin io.Reader, stdout io.Writer, resize <-chan runtime.WinSize) (int32, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return 0, err
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return 0, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running; start it with `fletcher session start`", row.Name)
	}
	if len(spec.Env) == 0 {
		spec.Env = m.env
	}
	m.markBusy(row.ID)
	defer m.unmarkBusy(row.ID)
	code, err := handle.Shell(ctx, spec, stdin, stdout, resize)
	if err != nil {
		return 0, fmt.Errorf("shell in session: %w", err)
	}
	m.touch(ctx, row.ID)
	return code, nil
}

// DialSSH opens a raw byte stream to a running session's SSH server for the
// daemon to proxy a brokered SSH connection through.
func (m *Manager) DialSSH(ctx context.Context, ref string) (net.Conn, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return nil, err
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return nil, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running; start it with `fletcher session start`", row.Name)
	}
	conn, err := handle.DialSSH(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial session ssh: %w", err)
	}
	m.touch(ctx, row.ID)
	// The SSH session is in flight for the life of the connection; keep the
	// session marked busy (and so safe from idle auto-stop) until it closes.
	m.markBusy(row.ID)
	id := row.ID
	return &busyConn{Conn: conn, release: func() { m.unmarkBusy(id) }}, nil
}

// ReconcileOnBoot marks every "running" session as "stopped" at daemon start:
// their VMs died with the previous daemon process. The forks persist, so the
// operator can start them again. Mirrors the job supervisor's boot reconcile.
func (m *Manager) ReconcileOnBoot(ctx context.Context) error {
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("reconcile sessions: %w", err)
	}
	for _, r := range rows {
		if State(r.State) != StateRunning {
			continue
		}
		if err := m.setState(ctx, r.ID, StateStopped); err != nil {
			return err
		}
		m.logger.Info("reset orphaned running session to stopped", slog.String("session_id", r.ID))
	}
	return nil
}

// lookup resolves ref (id or name) to a row, returning ErrNotFound if missing.
func (m *Manager) lookup(ctx context.Context, ref string) (sqliteq.Session, error) {
	row, err := m.q.GetSessionByRef(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return sqliteq.Session{}, ErrNotFound
	}
	if err != nil {
		return sqliteq.Session{}, fmt.Errorf("get session: %w", err)
	}
	return row, nil
}

func (m *Manager) setState(ctx context.Context, id string, state State) error {
	if err := m.q.UpdateSessionState(ctx, sqliteq.UpdateSessionStateParams{
		State:     string(state),
		UpdatedAt: time.Now().Unix(),
		ID:        id,
	}); err != nil {
		return fmt.Errorf("update session state: %w", err)
	}
	return nil
}

func (m *Manager) touch(ctx context.Context, id string) {
	now := time.Now().Unix()
	if err := m.q.TouchSession(ctx, sqliteq.TouchSessionParams{
		LastUsedAt: &now,
		UpdatedAt:  now,
		ID:         id,
	}); err != nil {
		m.logger.Debug("touch session", slog.String("session_id", id), slog.String("err", err.Error()))
	}
}

func (m *Manager) putHandle(id string, h runtime.SessionHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handles[id] = h
}

func (m *Manager) getHandle(id string) runtime.SessionHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handles[id]
}

func (m *Manager) takeHandle(id string) runtime.SessionHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.handles[id]
	delete(m.handles, id)
	return h
}

// ReapIdle stops (hibernates) every running session with no work in flight that
// has been idle longer than the configured timeout. "Work in flight" means an
// active host operation (exec/shell/ssh) or a busy guest (load average), so a
// running agent or task is never stopped mid-work, even with no user attached.
// It is a no-op when the idle timeout is disabled.
func (m *Manager) ReapIdle(ctx context.Context) (int, error) {
	if m.opts.IdleTimeout <= 0 {
		return 0, nil
	}
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap idle sessions: %w", err)
	}
	stopped := 0
	for _, r := range rows {
		if State(r.State) != StateRunning || m.busyCount(r.ID) > 0 {
			continue
		}
		handle := m.getHandle(r.ID)
		if handle == nil {
			continue
		}
		if load, lerr := handle.Load(ctx); lerr == nil && load >= idleLoadThreshold {
			m.touch(ctx, r.ID) // working with no host op attached; reset the idle clock
			continue
		}
		if time.Since(lastActivity(r)) < m.opts.IdleTimeout {
			continue
		}
		if _, serr := m.Stop(ctx, r.ID); serr != nil {
			m.logger.Warn("auto-stop idle session", slog.String("session_id", r.ID), slog.String("err", serr.Error()))
			continue
		}
		m.logger.Info("auto-stopped idle session", slog.String("session_id", r.ID), slog.String("name", r.Name))
		stopped++
	}
	return stopped, nil
}

// checkCaps refuses a new session when a configured count or disk cap is hit,
// reporting what is using the space rather than deleting anything.
func (m *Manager) checkCaps(ctx context.Context) error {
	if m.opts.MaxCount <= 0 && m.opts.MaxDiskBytes <= 0 {
		return nil
	}
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("check session caps: %w", err)
	}
	if m.opts.MaxCount > 0 && len(rows) >= m.opts.MaxCount {
		return errs.Newf(errs.CategoryFailedPrecondition,
			"session count cap reached (%d/%d). Delete one to make room:\n%s",
			len(rows), m.opts.MaxCount, usageReport(rows))
	}
	if m.opts.MaxDiskBytes > 0 {
		var total int64
		for _, r := range rows {
			total += forkBytes(r.ForkPath)
		}
		if total >= m.opts.MaxDiskBytes {
			return errs.Newf(errs.CategoryFailedPrecondition,
				"session disk cap reached (%d/%d GiB). Delete one to make room:\n%s",
				total>>30, m.opts.MaxDiskBytes>>30, usageReport(rows))
		}
	}
	return nil
}

// usageReport lists each session's disk use and last-used time for a cap error.
func usageReport(rows []sqliteq.Session) string {
	var b strings.Builder
	for _, r := range rows {
		last := "never"
		if r.LastUsedAt != nil {
			last = time.Unix(*r.LastUsedAt, 0).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "  %s  %s  %d MiB  last used %s\n",
			r.Name, r.State, forkBytes(r.ForkPath)>>20, last)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Manager) markBusy(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy[id]++
}

func (m *Manager) unmarkBusy(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[id] <= 1 {
		delete(m.busy, id)
		return
	}
	m.busy[id]--
}

func (m *Manager) busyCount(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[id]
}

// lastActivity is when a session was last used, falling back to when it last
// changed state (its start) for one that has never had an operation.
func lastActivity(r sqliteq.Session) time.Time {
	if r.LastUsedAt != nil {
		return time.Unix(*r.LastUsedAt, 0)
	}
	return time.Unix(r.UpdatedAt, 0)
}

// forkBytes is the on-disk size of a session fork, 0 if it cannot be stat-ed.
func forkBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// busyConn keeps a session marked busy until the SSH connection it wraps closes.
type busyConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *busyConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

func sessionFromRow(r sqliteq.Session) Session {
	return Session{
		ID:         r.ID,
		Name:       r.Name,
		Image:      r.Image,
		State:      State(r.State),
		CreatedAt:  time.Unix(r.CreatedAt, 0),
		UpdatedAt:  time.Unix(r.UpdatedAt, 0),
		LastUsedAt: timePtrFromUnix(r.LastUsedAt),
		DiskBytes:  forkBytes(r.ForkPath),
	}
}

func timePtrFromUnix(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.Unix(*v, 0)
	return &t
}
