package api_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/project"
)

type projectBackend struct {
	api.ProjectsBackend
	created project.CreateParams
	updated project.UpdateParams
	ref     string
	p       project.Project
	err     error
	calls   int
}

func (f *projectBackend) Create(_ context.Context, p project.CreateParams) (project.Project, error) {
	f.created = p
	f.calls++
	return f.p, f.err
}

func (f *projectBackend) Get(_ context.Context, ref string) (project.Project, error) {
	f.ref = ref
	return f.p, f.err
}

func (f *projectBackend) List(context.Context) ([]project.Project, error) {
	return []project.Project{f.p}, f.err
}

func (f *projectBackend) Update(_ context.Context, ref string, p project.UpdateParams) (project.Project, error) {
	f.ref, f.updated = ref, p
	return f.p, f.err
}

func (f *projectBackend) Delete(_ context.Context, ref string) error {
	f.ref = ref
	return f.err
}

func (f *projectBackend) Open(_ context.Context, ref string) (project.Project, error) {
	f.ref = ref
	return f.p, f.err
}

func TestProjectRPCs(t *testing.T) {
	f := &projectBackend{p: project.Project{
		ID: "project_test", Name: "app", SessionID: "session_test", WorkingDir: "/workspace/app",
		CreatedAt: time.Unix(7, 0), UpdatedAt: time.Unix(8, 0),
	}}
	_, handler := fletcherv1connect.NewProjectServiceHandler(api.NewProjectsService(f),
		connect.WithInterceptors(api.ErrorInterceptor(slog.Default()), api.ValidationInterceptor()))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	client := fletcherv1connect.NewProjectServiceClient(srv.Client(), srv.URL)
	ctx := t.Context()
	created, err := client.CreateProject(ctx, connect.NewRequest(&fletcherv1.CreateProjectRequest{
		Name: "app", SessionRef: "dev", WorkingDir: "/workspace/app",
	}))
	require.NoError(t, err)
	require.Equal(t, project.CreateParams{Name: "app", SessionRef: "dev", WorkingDir: "/workspace/app"}, f.created)
	require.Equal(t, "session_test", created.Msg.GetProject().GetSessionId())
	require.EqualValues(t, 7, created.Msg.GetProject().GetCreatedAt())
	got, err := client.GetProject(ctx, connect.NewRequest(&fletcherv1.GetProjectRequest{Ref: "app"}))
	require.NoError(t, err)
	require.Equal(t, "app", f.ref)
	require.Equal(t, "/workspace/app", got.Msg.GetProject().GetWorkingDir())
	listed, err := client.ListProjects(ctx, connect.NewRequest(&fletcherv1.ListProjectsRequest{}))
	require.NoError(t, err)
	require.Len(t, listed.Msg.GetProjects(), 1)
	_, err = client.UpdateProject(ctx, connect.NewRequest(&fletcherv1.UpdateProjectRequest{Ref: "app", WorkingDir: "/volume/app"}))
	require.NoError(t, err)
	require.Equal(t, project.UpdateParams{WorkingDir: "/volume/app"}, f.updated)
	_, err = client.OpenProject(ctx, connect.NewRequest(&fletcherv1.OpenProjectRequest{Ref: "project_test"}))
	require.NoError(t, err)
	require.Equal(t, "project_test", f.ref)
	_, err = client.DeleteProject(ctx, connect.NewRequest(&fletcherv1.DeleteProjectRequest{Ref: "app"}))
	require.NoError(t, err)
	require.Equal(t, "app", f.ref)
	f.err = project.ErrNotFound
	_, err = client.OpenProject(ctx, connect.NewRequest(&fletcherv1.OpenProjectRequest{Ref: "missing"}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	f.err = errs.New(errs.CategoryFailedPrecondition, "directory unavailable")
	_, err = client.OpenProject(ctx, connect.NewRequest(&fletcherv1.OpenProjectRequest{Ref: "app"}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
}

func TestProjectValidationRunsBeforeBackend(t *testing.T) {
	f := &projectBackend{}
	_, handler := fletcherv1connect.NewProjectServiceHandler(api.NewProjectsService(f), connect.WithInterceptors(api.ValidationInterceptor()))
	srv := httptest.NewServer(handler)
	defer srv.Close()
	client := fletcherv1connect.NewProjectServiceClient(srv.Client(), srv.URL)
	for _, msg := range []*fletcherv1.CreateProjectRequest{
		{Name: "bad/name", SessionRef: "dev"}, {Name: "app"}, {},
	} {
		_, err := client.CreateProject(t.Context(), connect.NewRequest(msg))
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}
	require.Zero(t, f.calls)
}
