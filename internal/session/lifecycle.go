package session

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
)

func (m *Manager) beginOperation(ctx context.Context) (context.Context, func(), error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.closing {
		return nil, nil, errs.New(errs.CategoryUnavailable, "daemon is shutting down; reconnect before retrying")
	}
	m.operations.Add(1)
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.lifetime, cancel)
	return opCtx, func() {
		stop()
		cancel()
		m.operations.Done()
	}, nil
}

// Give cleanup a short flush window even when the request is cancelled. Keep
// an existing deadline, particularly the daemon's shutdown budget, as a ceiling.
func stopHandle(ctx context.Context, handle runtime.SessionHandle) error {
	budget := 30 * time.Second
	if ctx.Err() != nil {
		budget = 5 * time.Second
	}
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	return handle.Stop(ctx)
}

// Shutdown drains operations, syncs and stops VMs, and records externally supervised host sessions as stopped before SQLite closes.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.lifecycleMu.Lock()
	m.closing = true
	m.cancelLifetime()
	m.lifecycleMu.Unlock()
	m.shutdownOnce.Do(func() {
		background.GoNamed(ctx, "session.drain", func(context.Context) {
			m.operations.Wait()
			close(m.drained)
		})
	})
	select {
	case <-m.drained:
	case <-ctx.Done():
		return fmt.Errorf("drain session operations: %w", ctx.Err())
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.handles))
	for id := range m.handles {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var group errgroup.Group
	group.SetLimit(4)
	for _, id := range ids {
		group.Go(func() error {
			lock := m.startLock(id)
			lock.Lock()
			defer lock.Unlock()
			row, err := m.lookup(ctx, id)
			if err != nil {
				return err
			}
			if row.ExecutionKind == ExecutionHost {
				// The account service owns host process teardown and is stopped
				// before the daemon by systemd. Never reactivate it during drain.
				m.takeHandle(id)
				return m.setState(ctx, id, StateStopped)
			}
			_, err = m.stopLocked(ctx, id)
			return err
		})
	}
	return group.Wait()
}
