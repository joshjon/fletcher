package session_test

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/project"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
)

func TestProjectTerminalsReuseOneForkAndVM(t *testing.T) {
	rt, snap := &fakeRuntime{}, newFakeSnapshot()
	mgr, q := newManagerWithQuerier(t, rt, snap)
	s, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
	require.NoError(t, err)
	projects := project.NewManager(q, mgr)
	p, err := projects.Create(t.Context(), project.CreateParams{Name: "app", SessionRef: s.ID, WorkingDir: "/workspace/app"})
	require.NoError(t, err)
	for _, label := range []string{"main", "build", "main"} {
		opened, err := projects.Open(t.Context(), p.ID)
		require.NoError(t, err)
		spec := runtime.ShellSpec{TerminalName: p.ID + "-" + label, WorkDir: opened.WorkingDir}
		_, err = mgr.Shell(t.Context(), opened.SessionID, spec, strings.NewReader(""), io.Discard, nil)
		require.NoError(t, err)
		require.Equal(t, spec, rt.handles[0].lastShell)
	}
	require.Len(t, rt.started, 1)
	require.Len(t, snap.live, 1)
	originalFork := rt.started[0]
	_, err = mgr.Stop(t.Context(), s.ID)
	require.NoError(t, err)
	_, err = projects.Open(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, []string{originalFork, originalFork}, rt.started)
	require.Len(t, snap.live, 1)
	require.Empty(t, snap.deleted)
}

func TestInvalidShellTargetNeverReachesRuntime(t *testing.T) {
	rt := &fakeRuntime{}
	mgr := newManager(t, rt, newFakeSnapshot())
	s, err := mgr.Create(t.Context(), session.CreateParams{Name: "dev", Image: "base"})
	require.NoError(t, err)
	_, err = mgr.Shell(t.Context(), s.ID, runtime.ShellSpec{TerminalName: "a:b"}, strings.NewReader(""), io.Discard, nil)
	require.Equal(t, errs.CategoryInvalidArgument, errs.CategoryOf(err))
	require.Empty(t, rt.handles[0].lastShell.TerminalName)
}
