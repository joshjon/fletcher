package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/project"
	"github.com/joshjon/fletcher/internal/runtime"
)

type cliProjects struct {
	api.ProjectsBackend
	p       project.Project
	created chan project.CreateParams
	opened  chan string
	deleted chan string
}

func (p cliProjects) Create(_ context.Context, params project.CreateParams) (project.Project, error) {
	p.created <- params
	return p.p, nil
}

func (p cliProjects) Open(_ context.Context, ref string) (project.Project, error) {
	p.opened <- ref
	return p.p, nil
}

func (p cliProjects) Delete(_ context.Context, ref string) error {
	p.deleted <- ref
	return nil
}

type cliProjectShell struct {
	api.SessionsBackend
	specs chan runtime.ShellSpec
	refs  chan string
}

func (f cliProjectShell) Shell(_ context.Context, ref string, spec runtime.ShellSpec, _ io.Reader, w io.Writer, _ <-chan runtime.WinSize) (int32, error) {
	f.specs <- spec
	f.refs <- ref
	_, err := io.WriteString(w, "project terminal ready\n")
	return 0, err
}

func TestProjectCLIRegistersAndOpensAnExistingVM(t *testing.T) {
	p := cliProjects{
		p:       project.Project{ID: "project_test", Name: "app", SessionID: "session_test", WorkingDir: "/workspace/app"},
		created: make(chan project.CreateParams, 1), opened: make(chan string, 2), deleted: make(chan string, 1),
	}
	f := cliProjectShell{specs: make(chan runtime.ShellSpec, 2), refs: make(chan string, 2)}
	mux := http.NewServeMux()
	path, handler := fletcherv1connect.NewProjectServiceHandler(api.NewProjectsService(p))
	mux.Handle(path, handler)
	path, handler = fletcherv1connect.NewSessionServiceHandler(api.NewSessionsService(f, api.SessionsDeps{}))
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	defer srv.Close()
	run := func(args ...string) string {
		return captureStdout(t, func() {
			command := append([]string{"fletcher", "--remote", strings.TrimPrefix(srv.URL, "http://"), "--token", "test", "project"}, args...)
			require.NoError(t, newApp().Run(t.Context(), command))
		})
	}
	out := run("create", "--name", "app", "--session", "dev", "--workdir", "/workspace/app", "-o", "json")
	require.Contains(t, out, "project_test")
	require.Equal(t, project.CreateParams{Name: "app", SessionRef: "dev", WorkingDir: "/workspace/app"}, <-p.created)

	// A non-interactive input lets the bidi shell finish without reading the
	// test runner's terminal; the real PTY remains a runtime responsibility.
	original := os.Stdin
	input, err := os.Open(os.DevNull)
	require.NoError(t, err)
	defer input.Close()
	os.Stdin = input
	defer func() { os.Stdin = original }()
	for _, label := range []string{"main", "build"} {
		out = run("open", "app", "--terminal", label)
		require.Contains(t, out, "project terminal ready")
		require.Equal(t, "app", <-p.opened)
		require.Equal(t, "session_test", <-f.refs)
		spec := <-f.specs
		require.Equal(t, "project_test-"+label, spec.TerminalName)
		require.Equal(t, "/workspace/app", spec.WorkDir)
	}
	out = run("delete", "app", "--yes")
	require.Contains(t, out, "removed project registration")
	require.Equal(t, "app", <-p.deleted)
}

func TestProjectOpenRejectsInvalidTerminalBeforeContactingDaemon(t *testing.T) {
	for _, label := range []string{"", "bad:name", strings.Repeat("a", 65)} {
		err := newApp().Run(t.Context(), []string{"fletcher", "project", "open", "app", "--terminal", label})
		require.Error(t, err)
		require.Contains(t, err.Error(), "terminal")
	}
}
