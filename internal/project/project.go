// Package project saves guest working directories in existing durable VMs without owning their files or execution lifecycle.
package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// ErrNotFound is returned when a project ref matches nothing.
var ErrNotFound = errs.New(errs.CategoryNotFound, "project not found")

// Project registers a guest directory in an existing session, not a separate sandbox.
type Project struct {
	ID         string
	Name       string
	SessionID  string
	WorkingDir string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store is the persistent registration access the manager needs.
type Store interface {
	CreateProject(context.Context, sqliteq.CreateProjectParams) (sqliteq.Project, error)
	GetProjectByRef(context.Context, string) (sqliteq.Project, error)
	ListProjects(context.Context) ([]sqliteq.Project, error)
	UpdateProject(context.Context, sqliteq.UpdateProjectParams) (sqliteq.Project, error)
	DeleteProject(context.Context, string) (int64, error)
}

// Sessions is the existing VM lifecycle and guest directory access used by projects.
type Sessions interface {
	Get(context.Context, string) (session.Session, error)
	Start(context.Context, string) (session.Session, error)
	ListDir(context.Context, string, string) (runtime.DirListing, error)
}

// Manager owns project registrations but never creates, clones or destroys a VM.
type Manager struct {
	q        Store
	sessions Sessions
}

// NewManager wires project storage to the existing session manager.
func NewManager(q Store, sessions Sessions) *Manager {
	return &Manager{q: q, sessions: sessions}
}

// CreateParams selects an existing VM and initial guest directory.
type CreateParams struct {
	Name       string
	SessionRef string
	WorkingDir string
}

// Create saves a project without booting its VM or modifying guest files.
func (m *Manager) Create(ctx context.Context, p CreateParams) (Project, error) {
	name := strings.TrimSpace(p.Name)
	if err := validateName(name); err != nil {
		return Project{}, err
	}
	if strings.TrimSpace(p.SessionRef) == "" {
		return Project{}, errs.New(errs.CategoryInvalidArgument, "an existing session is required")
	}
	dir := p.WorkingDir
	if dir == "" {
		dir = "/workspace"
	}
	if err := validateDir(dir); err != nil {
		return Project{}, err
	}
	s, err := m.sessions.Get(ctx, p.SessionRef)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project session: %w", err)
	}
	if s.RunApp {
		return Project{}, errs.New(errs.CategoryFailedPrecondition, "projects require a development session, not an app deployment")
	}
	id, err := typeid.WithPrefix("project")
	if err != nil {
		return Project{}, fmt.Errorf("generate project id: %w", err)
	}
	now := time.Now().Unix()
	row, err := m.q.CreateProject(ctx, sqliteq.CreateProjectParams{
		ID: id.String(), Name: name, SessionID: s.ID, WorkingDir: dir,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		if sqlite.IsUniqueViolation(err) {
			return Project{}, errs.Newf(errs.CategoryConflict, "a project named %q already exists", name)
		}
		if _, serr := m.sessions.Get(ctx, s.ID); errors.Is(serr, session.ErrNotFound) {
			return Project{}, errs.New(errs.CategoryNotFound, "project session no longer exists")
		}
		return Project{}, fmt.Errorf("record project: %w", err)
	}
	return fromRow(row), nil
}

// Get returns a registration by ID or name without waking its VM.
func (m *Manager) Get(ctx context.Context, ref string) (Project, error) {
	row, err := m.q.GetProjectByRef(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return fromRow(row), nil
}

// List returns registrations, newest first, without inspecting guest files.
func (m *Manager) List(ctx context.Context) ([]Project, error) {
	rows, err := m.q.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	out := make([]Project, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

// UpdateParams changes metadata without moving files or redirecting live terminals.
type UpdateParams struct {
	Name       string
	WorkingDir string
}

// Update changes a registration, preserving its VM and immutable project identity.
func (m *Manager) Update(ctx context.Context, ref string, p UpdateParams) (Project, error) {
	current, err := m.Get(ctx, ref)
	if err != nil {
		return Project{}, err
	}
	var name, dir *string
	if p.Name != "" {
		p.Name = strings.TrimSpace(p.Name)
		if err := validateName(p.Name); err != nil {
			return Project{}, err
		}
		name = &p.Name
	}
	if p.WorkingDir != "" {
		if err := validateDir(p.WorkingDir); err != nil {
			return Project{}, err
		}
		dir = &p.WorkingDir
	}
	row, err := m.q.UpdateProject(ctx, sqliteq.UpdateProjectParams{
		ID: current.ID, Name: name, WorkingDir: dir, UpdatedAt: time.Now().Unix(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if sqlite.IsUniqueViolation(err) {
		return Project{}, errs.Newf(errs.CategoryConflict, "a project named %q already exists", p.Name)
	}
	if err != nil {
		return Project{}, fmt.Errorf("update project: %w", err)
	}
	return fromRow(row), nil
}

// Delete removes only the registration and leaves all VM files and processes untouched.
func (m *Manager) Delete(ctx context.Context, ref string) error {
	p, err := m.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := m.q.DeleteProject(ctx, p.ID); err != nil {
		return fmt.Errorf("delete project registration: %w", err)
	}
	return nil
}

// Open wakes the existing VM and verifies the saved directory without running setup or an agent.
func (m *Manager) Open(ctx context.Context, ref string) (Project, error) {
	p, err := m.Get(ctx, ref)
	if err != nil {
		return Project{}, err
	}
	if _, err := m.sessions.Start(ctx, p.SessionID); err != nil {
		return Project{}, fmt.Errorf("start project session: %w", err)
	}
	// Re-read after waking so a removed registration is not returned and a
	// concurrent directory change is checked rather than silently ignored.
	p, err = m.Get(ctx, p.ID)
	if err != nil {
		return Project{}, err
	}
	if _, err := m.sessions.ListDir(ctx, p.SessionID, p.WorkingDir); err != nil {
		if ctx.Err() != nil {
			return Project{}, ctx.Err()
		}
		return Project{}, errs.Newf(errs.CategoryFailedPrecondition,
			"project directory %q is unavailable in the guest; open the session terminal to repair it or update the project path", p.WorkingDir)
	}
	return p, nil
}

func validateDir(dir string) error {
	return (runtime.ShellSpec{WorkDir: dir, TerminalName: "project"}).Validate()
}

func validateName(name string) error {
	if name == "" || len(name) > 64 || name[0] == '.' || name[0] == '-' {
		return errs.New(errs.CategoryInvalidArgument, "project name must be 1-64 characters using lowercase letters, digits, '.', '_' or '-', without a leading '.' or '-'")
	}
	for _, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !valid {
			return errs.New(errs.CategoryInvalidArgument, "project name must use lowercase letters, digits, '.', '_' or '-'")
		}
	}
	return nil
}

func fromRow(r sqliteq.Project) Project {
	return Project{
		ID: r.ID, Name: r.Name, SessionID: r.SessionID, WorkingDir: r.WorkingDir,
		CreatedAt: time.Unix(r.CreatedAt, 0), UpdatedAt: time.Unix(r.UpdatedAt, 0),
	}
}
