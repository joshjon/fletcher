package session_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/project"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

type fakeHostRuntime struct {
	account     runtime.HostAccount
	specs       []runtime.HostSessionSpec
	stops       []runtime.HostSessionSpec
	handles     []*hostTestHandle
	unavailable bool
}

type hostTestHandle struct {
	*fakeHandle
	stopErr error
}

func (h *hostTestHandle) Stop(ctx context.Context) error {
	if h.stopErr != nil {
		return h.stopErr
	}
	return h.fakeHandle.Stop(ctx)
}

func (r *fakeHostRuntime) HostAccount(context.Context) (runtime.HostAccount, error) {
	if r.unavailable {
		return runtime.HostAccount{}, errors.New("executor unavailable")
	}
	return r.account, nil
}

func (r *fakeHostRuntime) StartHostSession(ctx context.Context, spec runtime.HostSessionSpec) (runtime.SessionHandle, error) {
	account, err := r.HostAccount(ctx)
	if err != nil {
		return nil, err
	}
	if spec.Account != account {
		return nil, errors.New("account mismatch")
	}
	r.specs = append(r.specs, spec)
	h := &hostTestHandle{fakeHandle: &fakeHandle{}}
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeHostRuntime) StopHostSession(ctx context.Context, spec runtime.HostSessionSpec) error {
	account, err := r.HostAccount(ctx)
	if err != nil {
		return err
	}
	if spec.Account != account {
		return errors.New("account mismatch")
	}
	r.stops = append(r.stops, spec)
	return nil
}

func hostFixture(t *testing.T) (*session.Manager, *fakeHostRuntime, *fakeSnapshot, *fakeRuntime, *sqliteq.Queries) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, sqlite.Migrate(db))
	q := sqliteq.New(db)
	r := &fakeHostRuntime{account: runtime.HostAccount{User: "operator", UID: 1000, Home: t.TempDir()}}
	snap, vm := newFakeSnapshot(), &fakeRuntime{}
	m := session.NewManager(q, snap, vm, session.AgentEnv{Base: []string{"SECRET=must-not-inject"}}, slog.New(slog.NewTextHandler(io.Discard, nil)), session.Options{HostRuntime: r, DefaultImage: "unused", DefaultGateway: "on", IdleTimeout: time.Nanosecond})
	return m, r, snap, vm, q
}

func hostParams(r *fakeHostRuntime, name string) session.CreateParams {
	return session.CreateParams{Name: name, ExecutionKind: session.ExecutionHost, HostUser: r.account.User, HostUID: r.account.UID}
}

func TestHostSessionLifecycleAndBookmarks(t *testing.T) {
	m, r, snap, vm, q := hostFixture(t)
	ctx := t.Context()
	s, err := m.Create(ctx, hostParams(r, "host-work"))
	require.NoError(t, err)
	require.Equal(t, session.ExecutionHost, s.ExecutionKind)
	require.Equal(t, r.account, s.HostAccount)
	require.Equal(t, r.account.Home, s.WorkingDir)
	require.Empty(t, s.Image)
	require.Empty(t, s.EgressPolicy)
	require.Empty(t, s.Gateway)
	require.Zero(t, s.DiskBytes)
	file := filepath.Join(s.WorkingDir, "keep.txt")
	require.NoError(t, os.WriteFile(file, []byte("keep"), 0o600))
	projects := project.NewManager(q, m)
	p, err := projects.Create(ctx, project.CreateParams{Name: "bookmark", SessionRef: s.ID})
	require.NoError(t, err)
	require.Equal(t, s.WorkingDir, p.WorkingDir)
	opened, err := projects.Open(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p.SessionID, opened.SessionID)
	require.Len(t, r.specs, 1, "opening a bookmark reuses its running host environment")
	_, err = m.Stop(ctx, s.ID)
	require.NoError(t, err)
	require.Empty(t, r.handles[0].execs, "host stop must not invoke guest sync")
	_, err = m.Start(ctx, s.ID)
	require.NoError(t, err)
	_, err = m.Restart(ctx, s.ID)
	require.NoError(t, err)
	n, err := m.ReapIdle(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "host processes must not be reaped using VM load semantics")
	deleted, err := m.Delete(ctx, s.ID)
	require.NoError(t, err)
	require.True(t, deleted)
	_, err = projects.Get(ctx, p.ID)
	require.ErrorIs(t, err, project.ErrNotFound)
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
	require.Zero(t, snap.n)
	require.Empty(t, snap.deleted)
	require.Empty(t, vm.started)
	require.Zero(t, vm.discarded)
}

func TestHostSessionCleanupAfterReconciliation(t *testing.T) {
	m, r, snap, vm, q := hostFixture(t)
	ctx := t.Context()
	s, err := m.Create(ctx, hostParams(r, "host"))
	require.NoError(t, err)
	fresh := session.NewManager(q, snap, vm, session.AgentEnv{}, slog.Default(), session.Options{HostRuntime: r})
	require.NoError(t, fresh.ReconcileOnBoot(ctx))

	// A lost daemon-side handle does not prove the executor namespace is gone.
	r.unavailable = true
	deleted, err := fresh.Delete(ctx, s.ID)
	require.Error(t, err)
	require.False(t, deleted)
	still, err := fresh.Get(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, session.StateStopped, still.State)
	r.unavailable = false
	deleted, err = fresh.Delete(ctx, s.ID)
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, r.specs, r.stops)
	require.Empty(t, snap.deleted)
}

func TestHostSessionShutdown(t *testing.T) {
	m, r, _, _, _ := hostFixture(t)
	s, err := m.Create(t.Context(), hostParams(r, "host"))
	require.NoError(t, err)
	require.NoError(t, m.Shutdown(t.Context()))
	require.Equal(t, 1, r.handles[0].stopped)
	require.Empty(t, r.handles[0].execs, "shutdown must not invoke guest sync on a host")
	stopped, err := m.Get(t.Context(), s.ID)
	require.NoError(t, err)
	require.Equal(t, session.StateStopped, stopped.State)
	_, err = m.Start(t.Context(), s.ID)
	require.ErrorContains(t, err, "shutting down")
}

func TestHostSessionRejectsVMFeaturesAndMissingConsent(t *testing.T) {
	m, r, snap, vm, _ := hostFixture(t)
	mutations := []func(*session.CreateParams){
		func(p *session.CreateParams) { p.Image = "base" },
		func(p *session.CreateParams) { p.Gateway = "off" },
		func(p *session.CreateParams) { p.EgressPolicy = "open" },
		func(p *session.CreateParams) { p.RunApp = true },
		func(p *session.CreateParams) { p.VolumeRef = "data" },
		func(p *session.CreateParams) { p.Credentials = []string{"git"} },
		func(p *session.CreateParams) { p.EnvVars = []session.EnvVar{{Name: "TEST", Value: "x"}} },
		func(p *session.CreateParams) { p.HostUser = "other" },
		func(p *session.CreateParams) { p.HostUID = 0 },
		func(p *session.CreateParams) { p.WorkingDir = "relative" },
		func(p *session.CreateParams) { p.ExecutionKind = "" },
		func(p *session.CreateParams) { p.ExecutionKind = "unknown" },
	}
	for _, mutate := range mutations {
		p := hostParams(r, "invalid")
		mutate(&p)
		_, err := m.Create(t.Context(), p)
		require.Error(t, err)
	}
	require.Empty(t, r.specs)
	s, err := m.Create(t.Context(), hostParams(r, "host"))
	require.NoError(t, err)
	_, _, err = m.Update(t.Context(), s.ID, session.UpdateParams{Gateway: "on"})
	require.Error(t, err)
	_, err = m.CommitImage(t.Context(), s.ID, session.CommitImageParams{Name: "never"})
	require.Error(t, err)
	_, err = m.Redeploy(t.Context(), s.ID, "never")
	require.Error(t, err)
	_, err = m.Rollback(t.Context(), s.ID)
	require.Error(t, err)
	_, err = m.Publish(t.Context(), s.ID, session.PublishParams{GuestPort: 8080})
	require.Error(t, err)
	_, _, err = m.BuildImageFromSession(t.Context(), s.ID, "", "never", false)
	require.Error(t, err)
	_, err = m.StartBuildFromSession(t.Context(), s.ID, "", "never", false)
	require.Error(t, err)
	require.Zero(t, snap.n)
	require.Empty(t, snap.deleted)
	require.Empty(t, vm.started)
	require.Zero(t, vm.discarded)
}

func TestHostSessionFailureAndRecordedAccountBinding(t *testing.T) {
	m, r, snap, vm, q := hostFixture(t)
	ctx := t.Context()
	s, err := m.Create(ctx, hostParams(r, "host"))
	require.NoError(t, err)
	_, err = m.Create(ctx, hostParams(r, "host"))
	require.Error(t, err)
	require.Equal(t, 1, r.handles[1].stopped, "unrecorded namespace must be stopped")
	r.handles[0].stopErr = errors.New("stop unavailable")
	deleted, err := m.Delete(ctx, s.ID)
	require.Error(t, err)
	require.False(t, deleted)
	still, err := m.Get(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, session.StateRunning, still.State)
	r.handles[0].stopErr = nil
	_, err = m.Stop(ctx, s.ID)
	require.NoError(t, err)
	r.account.UID++
	_, err = m.Start(ctx, s.ID)
	require.ErrorContains(t, err, "account mismatch")
	r.account.UID--
	r.unavailable = true
	_, err = m.Start(ctx, s.ID)
	require.Error(t, err)
	require.Empty(t, vm.started, "unavailable host execution must never start a VM")
	require.Zero(t, snap.n)

	// Persisted account and path survive a manager restart without becoming VM fields.
	fresh := session.NewManager(q, nil, nil, session.AgentEnv{}, slog.Default(), session.Options{})
	require.NoError(t, fresh.ReconcileOnBoot(ctx))
	got, err := fresh.Get(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, s.HostAccount, got.HostAccount)
	require.Equal(t, s.WorkingDir, got.WorkingDir)
	_, err = fresh.Start(ctx, s.ID)
	require.ErrorContains(t, err, "disabled")
	deleted, err = fresh.Delete(ctx, s.ID)
	require.NoError(t, err)
	require.True(t, deleted)
}
