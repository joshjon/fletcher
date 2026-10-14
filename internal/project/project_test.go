package project_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/project"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

type fakeSessions struct {
	q        *sqliteq.Queries
	starts   []string
	dirs     []string
	startErr error
	dirErr   error
	afterGet func()
}

func (f *fakeSessions) Get(ctx context.Context, ref string) (session.Session, error) {
	r, err := f.q.GetSessionByRef(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Session{}, session.ErrNotFound
	}
	if err != nil {
		return session.Session{}, err
	}
	if f.afterGet != nil {
		f.afterGet()
	}
	return session.Session{ID: r.ID, Name: r.Name, RunApp: r.RunApp != 0}, nil
}

func (f *fakeSessions) Start(ctx context.Context, ref string) (session.Session, error) {
	f.starts = append(f.starts, ref)
	if f.startErr != nil {
		return session.Session{}, f.startErr
	}
	return f.Get(ctx, ref)
}

func (f *fakeSessions) ListDir(_ context.Context, ref, dir string) (runtime.DirListing, error) {
	f.dirs = append(f.dirs, ref+":"+dir)
	return runtime.DirListing{Path: dir}, f.dirErr
}

func fixture(t *testing.T) (*project.Manager, *sqliteq.Queries, *fakeSessions, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sqlite.Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, sqlite.Migrate(db))
	q := sqliteq.New(db)
	_, err = q.CreateSession(t.Context(), sqliteq.CreateSessionParams{
		ID: "session_test", Name: "dev", Image: "base", State: "stopped",
		ForkID: "fork_test", ForkPath: "/unused", EgressPolicy: "none", Gateway: "off", EnvVars: "[]",
	})
	require.NoError(t, err)
	f := &fakeSessions{q: q}
	return project.NewManager(q, f), q, f, path
}

func create(t *testing.T, m *project.Manager, name, dir string) project.Project {
	t.Helper()
	p, err := m.Create(t.Context(), project.CreateParams{Name: name, SessionRef: "dev", WorkingDir: dir})
	require.NoError(t, err)
	return p
}

func TestRegistrationPersistsWithoutBootingOrChangingFiles(t *testing.T) {
	m, q, f, path := fixture(t)
	p := create(t, m, "app", "/workspace/app")
	require.Equal(t, "session_test", p.SessionID)
	require.Equal(t, "/workspace/app", p.WorkingDir)
	require.True(t, strings.HasPrefix(p.ID, "project_"))
	require.False(t, p.CreatedAt.IsZero())
	require.Empty(t, f.starts)
	require.Empty(t, f.dirs)

	// Another connection/manager models a daemon restart and another client.
	db, err := sqlite.Open(t.Context(), path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	other := project.NewManager(sqliteq.New(db), f)
	got, err := other.Get(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, p, got)
	byName, err := other.Get(t.Context(), "app")
	require.NoError(t, err)
	require.Equal(t, p, byName)
	listed, err := other.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []project.Project{p}, listed)
	r, err := q.GetSessionByRef(t.Context(), "dev")
	require.NoError(t, err)
	require.Equal(t, "stopped", r.State)
	require.Equal(t, "fork_test", r.ForkID)
}

func TestProjectIDWinsOverACollidingName(t *testing.T) {
	m, _, _, _ := fixture(t)
	p := create(t, m, "app", "")
	other := create(t, m, p.ID, "")
	require.NotEqual(t, p.ID, other.ID)
	got, err := m.Get(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, p.ID, got.ID)
}

func TestOpenReusesTheExistingSession(t *testing.T) {
	m, _, f, _ := fixture(t)
	p := create(t, m, "app", "")
	require.Equal(t, "/workspace", p.WorkingDir)
	for range 2 {
		opened, err := m.Open(t.Context(), "app")
		require.NoError(t, err)
		require.Equal(t, p, opened)
	}
	require.Equal(t, []string{p.SessionID, p.SessionID}, f.starts)
	require.Equal(t, []string{p.SessionID + ":/workspace", p.SessionID + ":/workspace"}, f.dirs)
}

func TestDeleteOnlyRemovesRegistration(t *testing.T) {
	m, q, f, _ := fixture(t)
	p := create(t, m, "app", "")
	create(t, m, "another", "/volume/another")
	require.NoError(t, m.Delete(t.Context(), p.ID))
	require.NoError(t, m.Delete(t.Context(), p.ID))
	_, err := m.Get(t.Context(), p.ID)
	require.ErrorIs(t, err, project.ErrNotFound)
	_, err = q.GetSessionByRef(t.Context(), p.SessionID)
	require.NoError(t, err)
	listed, err := m.List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Empty(t, f.starts)
	require.Empty(t, f.dirs)
}

func TestSessionDeletionRemovesItsRegistrations(t *testing.T) {
	m, q, _, _ := fixture(t)
	p := create(t, m, "app", "")
	_, err := q.DeleteSession(t.Context(), p.SessionID)
	require.NoError(t, err)
	listed, err := m.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = m.Open(t.Context(), p.ID)
	require.ErrorIs(t, err, project.ErrNotFound)
}

func TestUpdatePreservesIdentityAndDoesNotTouchGuest(t *testing.T) {
	m, _, f, _ := fixture(t)
	p := create(t, m, "app", "")
	updated, err := m.Update(t.Context(), p.ID, project.UpdateParams{Name: "renamed", WorkingDir: "/workspace/new"})
	require.NoError(t, err)
	require.Equal(t, p.ID, updated.ID)
	require.Equal(t, p.SessionID, updated.SessionID)
	require.Equal(t, "renamed", updated.Name)
	require.Equal(t, "/workspace/new", updated.WorkingDir)
	updated, err = m.Update(t.Context(), p.ID, project.UpdateParams{})
	require.NoError(t, err)
	require.Equal(t, "renamed", updated.Name)
	require.Empty(t, f.starts)
	require.Empty(t, f.dirs)
}

func TestValidationAndConflicts(t *testing.T) {
	m, _, f, _ := fixture(t)
	for _, name := range []string{"", " ", ".hidden", "-bad", "Bad", "a/b", strings.Repeat("a", 65)} {
		_, err := m.Create(t.Context(), project.CreateParams{Name: name, SessionRef: "dev"})
		require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err), name)
	}
	for _, dir := range []string{"relative", "/a/../b", "/a//b", "/a\n", "/a\x00", "/" + strings.Repeat("a", 4096)} {
		_, err := m.Create(t.Context(), project.CreateParams{Name: "app", SessionRef: "dev", WorkingDir: dir})
		require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err), dir)
	}
	_, err := m.Create(t.Context(), project.CreateParams{Name: "app", SessionRef: "missing"})
	require.ErrorIs(t, err, session.ErrNotFound)
	_, err = m.Create(t.Context(), project.CreateParams{Name: "app"})
	require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
	create(t, m, "app", "")
	_, err = m.Create(t.Context(), project.CreateParams{Name: "app", SessionRef: "dev"})
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
	p := create(t, m, "other", "")
	_, err = m.Update(t.Context(), p.ID, project.UpdateParams{Name: "app"})
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
	require.Empty(t, f.starts)
}

func TestOpenReportsStartAndDirectoryFailures(t *testing.T) {
	m, _, f, _ := fixture(t)
	create(t, m, "app", "")
	f.startErr = errors.New("boot unavailable")
	_, err := m.Open(t.Context(), "app")
	require.ErrorIs(t, err, f.startErr)
	require.Empty(t, f.dirs)
	f.startErr = nil
	f.dirErr = errors.New("not a directory")
	_, err = m.Open(t.Context(), "app")
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
	require.Contains(t, err.Error(), "update the project path")
}

func TestCreateRejectsDeploysAndSessionDeletionRace(t *testing.T) {
	m, q, f, _ := fixture(t)
	_, err := q.CreateSession(t.Context(), sqliteq.CreateSessionParams{
		ID: "session_app", Name: "deploy", Image: "base", State: "stopped", ForkID: "fork_app",
		ForkPath: "/unused", EgressPolicy: "none", Gateway: "off", EnvVars: "[]", RunApp: 1,
	})
	require.NoError(t, err)
	_, err = m.Create(t.Context(), project.CreateParams{Name: "app", SessionRef: "deploy"})
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
	f.afterGet = func() {
		_, derr := q.DeleteSession(t.Context(), "session_test")
		require.NoError(t, derr)
	}
	_, err = m.Create(t.Context(), project.CreateParams{Name: "app", SessionRef: "dev"})
	require.Equal(t, errs.CategoryNotFound, errs.CategoryOf(err))
	listed, err := m.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, listed)
}

type gatedStore struct {
	project.Store
	arrived chan struct{}
	release chan struct{}
}

func (g gatedStore) GetProjectByRef(ctx context.Context, ref string) (sqliteq.Project, error) {
	r, err := g.Store.GetProjectByRef(ctx, ref)
	g.arrived <- struct{}{}
	<-g.release
	return r, err
}

func TestConcurrentPartialUpdatesDoNotOverwriteEachOther(t *testing.T) {
	m, q, f, _ := fixture(t)
	p := create(t, m, "app", "")
	g := gatedStore{Store: q, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	concurrent := project.NewManager(g, f)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, params := range []project.UpdateParams{{Name: "renamed"}, {WorkingDir: "/workspace/new"}} {
		wg.Go(func() { _, err := concurrent.Update(t.Context(), p.ID, params); errCh <- err })
	}
	<-g.arrived
	<-g.arrived
	close(g.release)
	wg.Wait()
	for range 2 {
		require.NoError(t, <-errCh)
	}
	got, err := m.Get(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
	require.Equal(t, "/workspace/new", got.WorkingDir)
}
