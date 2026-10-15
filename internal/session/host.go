package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

const (
	// ExecutionVM is the default isolated session environment.
	ExecutionVM = "vm"
	// ExecutionHost explicitly selects the locally authorized host account.
	ExecutionHost = "host"
)

// HostAccount returns the verified, locally authorized executor identity.
func (m *Manager) HostAccount(ctx context.Context) (runtime.HostAccount, error) {
	if m.opt().HostRuntime == nil {
		return runtime.HostAccount{}, hostDisabled()
	}
	return m.opt().HostRuntime.HostAccount(ctx)
}

func hostDisabled() error {
	return errs.New(errs.CategoryFailedPrecondition, "host sessions are disabled; a local administrator must authorize an account executor and configure --host-user")
}

func requireVM(row sqliteq.Session) error {
	if row.ExecutionKind == ExecutionHost {
		return errs.New(errs.CategoryFailedPrecondition, "this operation requires a VM; host sessions have no images, snapshots, VM network controls or deployments")
	}
	return nil
}

func (m *Manager) createHost(ctx context.Context, p CreateParams) (Session, error) {
	if strings.TrimSpace(p.Name) == "" {
		return Session{}, errs.New(errs.CategoryInvalidArgument, "name is required")
	}
	if p.Image != "" || p.EgressPolicy != "" || p.Gateway != "" || p.RunApp || p.VolumeRef != "" || len(p.Credentials) != 0 || len(p.EnvVars) != 0 {
		return Session{}, errs.New(errs.CategoryInvalidArgument, "host sessions do not accept images, VM network settings, gateway injection, deployments, volumes, credential seeding or environment injection")
	}
	account, err := m.HostAccount(ctx)
	if err != nil {
		return Session{}, err
	}
	if p.HostUser != account.User || p.HostUID != account.UID || account.UID <= 0 {
		return Session{}, errs.New(errs.CategoryFailedPrecondition, "confirm the locally authorized host account before creating a host session")
	}
	if p.WorkingDir == "" {
		p.WorkingDir = account.Home
	}
	if err := (runtime.ShellSpec{WorkDir: p.WorkingDir, TerminalName: "main"}).Validate(); err != nil {
		return Session{}, err
	}
	if cap := m.opt().MaxCount; cap > 0 {
		count, err := m.q.CountSessions(ctx)
		if err != nil {
			return Session{}, fmt.Errorf("count sessions: %w", err)
		}
		if count >= int64(cap) {
			return Session{}, errs.New(errs.CategoryFailedPrecondition, "session count cap reached; delete a session to make room")
		}
	}
	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Session{}, fmt.Errorf("generate host session id: %w", err)
	}
	spec := runtime.HostSessionSpec{SessionID: id.String(), Account: account, WorkDir: p.WorkingDir}
	handle, err := m.opt().HostRuntime.StartHostSession(ctx, spec)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = m.opt().HostRuntime.StopHostSession(cleanup, spec)
		return Session{}, fmt.Errorf("start host session: %w", err)
	}
	row, err := m.q.CreateHostSession(ctx, sqliteq.CreateHostSessionParams{
		ID: spec.SessionID, Name: p.Name, Now: time.Now().Unix(), HostUser: account.User,
		HostUid: account.UID, HostHome: account.Home, WorkingDir: p.WorkingDir,
	})
	if err != nil {
		_ = stopHandle(ctx, handle)
		if sqlite.IsUniqueViolation(err) {
			return Session{}, errs.Newf(errs.CategoryConflict, "a session named %q already exists", p.Name)
		}
		return Session{}, fmt.Errorf("record host session: %w", err)
	}
	m.putHandle(row.ID, handle)
	m.publishEvent(string(StateRunning), row.ID, row.Name)
	return sessionFromRow(row), nil
}

func hostSpec(row sqliteq.Session) runtime.HostSessionSpec {
	return runtime.HostSessionSpec{
		SessionID: row.ID, WorkDir: row.WorkingDir,
		Account: runtime.HostAccount{User: row.HostUser, UID: row.HostUid, Home: row.HostHome},
	}
}

func (m *Manager) startHost(ctx context.Context, row sqliteq.Session) (Session, error) {
	if m.opt().HostRuntime == nil {
		return Session{}, hostDisabled()
	}
	if State(row.State) == StateRunning && m.getHandle(row.ID) != nil {
		account, err := m.HostAccount(ctx)
		if err != nil {
			return Session{}, err
		}
		if account != hostSpec(row).Account {
			return Session{}, errs.New(errs.CategoryFailedPrecondition, "host account binding changed; review local authorization")
		}
		return sessionFromRow(row), nil
	}
	handle, err := m.opt().HostRuntime.StartHostSession(ctx, hostSpec(row))
	if err != nil {
		return Session{}, fmt.Errorf("start host session: %w", err)
	}
	if err := m.setState(ctx, row.ID, StateRunning); err != nil {
		_ = stopHandle(ctx, handle)
		return Session{}, err
	}
	m.putHandle(row.ID, handle)
	row.State = string(StateRunning)
	return sessionFromRow(row), nil
}

func (m *Manager) stopEnvironment(ctx context.Context, row sqliteq.Session, syncDisk bool) error {
	if handle := m.takeHandle(row.ID); handle != nil {
		if syncDisk && row.ExecutionKind != ExecutionHost {
			m.syncGuest(ctx, handle, row.ID)
		}
		if err := stopHandle(ctx, handle); err != nil {
			m.putHandle(row.ID, handle)
			return fmt.Errorf("stop session: %w", err)
		}
	} else if row.ExecutionKind == ExecutionHost {
		if m.opt().HostRuntime == nil {
			if State(row.State) == StateRunning {
				return hostDisabled()
			}
			return nil
		}
		// Reconciliation can discard a local handle while an executor still has
		// its namespace. Confirm cleanup even when the metadata says stopped.
		if err := m.opt().HostRuntime.StopHostSession(ctx, hostSpec(row)); err != nil {
			return fmt.Errorf("stop host session: %w", err)
		}
	}
	return nil
}
