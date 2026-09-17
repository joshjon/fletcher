package host

import (
	"context"
	"sync"
	"time"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/errs"
)

// Check reports an actionable host readiness condition.
type Check struct{ Name, Status, Detail string }

// Options supplies daemon-owned paths and lifecycle hooks, never client paths.
type Options struct {
	StartedAt               int64
	SnapshotRoot, StateRoot string
	Logs                    *Logs
	Restart                 func()
	Validate                func(context.Context) error
	Checks                  func(context.Context) []Check
	ClearCache              func(context.Context) error
	Diagnose                func(context.Context, bool) ([]Check, string, error)
}

// Manager coordinates inspection and one acknowledged restart per process.
type Manager struct {
	ctx context.Context
	Options
	mu        sync.Mutex
	restartID string
	support   func(context.Context) (bool, string)
}

// NewManager builds a host manager using the daemon's lifetime context.
func NewManager(ctx context.Context, opts Options) *Manager {
	if opts.Logs == nil {
		opts.Logs = &Logs{}
	}
	return &Manager{ctx: ctx, Options: opts, support: RestartSupport}
}

// CanRestart checks the service supervisor rather than assuming a systemd host.
func (m *Manager) CanRestart(ctx context.Context) (bool, string) {
	if m.Restart == nil {
		return false, "remote restart unavailable"
	}
	return m.support(ctx)
}

// RequestRestart schedules a graceful shutdown after acknowledgement can flush.
func (m *Manager) RequestRestart(ctx context.Context, id string, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(id) < 16 || len(id) > 80 {
		return errs.New(errs.CategoryInvalidArgument, "request_id must be 16-80 characters")
	}
	if expected != m.StartedAt {
		return errs.New(errs.CategoryConflict, "daemon generation changed; refresh host status before restarting")
	}
	if m.restartID == id {
		return nil
	}
	if m.restartID != "" {
		return errs.New(errs.CategoryConflict, "daemon restart already scheduled")
	}
	if ok, reason := m.CanRestart(ctx); !ok {
		return errs.New(errs.CategoryFailedPrecondition, reason)
	}
	if m.Validate != nil {
		if err := m.Validate(ctx); err != nil {
			return err
		}
	}
	m.restartID = id
	//nolint:contextcheck // accepted restart belongs to the daemon lifetime, not the request
	background.GoNamed(m.ctx, "host.restart", func(ctx context.Context) {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.Restart()
		}
	})
	return nil
}

// Storage measures daemon-owned data with a bounded execution time.
func (m *Manager) Storage(ctx context.Context) (Storage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return MeasureStorage(ctx, m.SnapshotRoot, m.StateRoot)
}
