package api

import (
	"context"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/project"
)

// ProjectsBackend is the project registration and existing-VM access the API needs.
type ProjectsBackend interface {
	Create(context.Context, project.CreateParams) (project.Project, error)
	Get(context.Context, string) (project.Project, error)
	List(context.Context) ([]project.Project, error)
	Update(context.Context, string, project.UpdateParams) (project.Project, error)
	Delete(context.Context, string) error
	Open(context.Context, string) (project.Project, error)
}

// ProjectsService exposes saved guest directories to paired clients.
type ProjectsService struct {
	fletcherv1connect.UnimplementedProjectServiceHandler
	backend ProjectsBackend
}

// NewProjectsService wires the API to its project backend.
func NewProjectsService(backend ProjectsBackend) *ProjectsService {
	return &ProjectsService{backend: backend}
}

// CreateProject registers a directory in an existing VM.
func (s *ProjectsService) CreateProject(ctx context.Context, req *connect.Request[fletcherv1.CreateProjectRequest]) (*connect.Response[fletcherv1.CreateProjectResponse], error) {
	p, err := s.backend.Create(ctx, project.CreateParams{
		Name: req.Msg.GetName(), SessionRef: req.Msg.GetSessionRef(), WorkingDir: req.Msg.GetWorkingDir(),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.CreateProjectResponse{Project: projectToProto(p)}), nil
}

// GetProject fetches a registration without waking its VM.
func (s *ProjectsService) GetProject(ctx context.Context, req *connect.Request[fletcherv1.GetProjectRequest]) (*connect.Response[fletcherv1.GetProjectResponse], error) {
	p, err := s.backend.Get(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetProjectResponse{Project: projectToProto(p)}), nil
}

// ListProjects lists saved guest directories.
func (s *ProjectsService) ListProjects(ctx context.Context, _ *connect.Request[fletcherv1.ListProjectsRequest]) (*connect.Response[fletcherv1.ListProjectsResponse], error) {
	projects, err := s.backend.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.Project, len(projects))
	for i, p := range projects {
		out[i] = projectToProto(p)
	}
	return connect.NewResponse(&fletcherv1.ListProjectsResponse{Projects: out}), nil
}

// UpdateProject changes metadata without changing files or live terminals.
func (s *ProjectsService) UpdateProject(ctx context.Context, req *connect.Request[fletcherv1.UpdateProjectRequest]) (*connect.Response[fletcherv1.UpdateProjectResponse], error) {
	p, err := s.backend.Update(ctx, req.Msg.GetRef(), project.UpdateParams{Name: req.Msg.GetName(), WorkingDir: req.Msg.GetWorkingDir()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.UpdateProjectResponse{Project: projectToProto(p)}), nil
}

// DeleteProject removes only the registration.
func (s *ProjectsService) DeleteProject(ctx context.Context, req *connect.Request[fletcherv1.DeleteProjectRequest]) (*connect.Response[fletcherv1.DeleteProjectResponse], error) {
	if err := s.backend.Delete(ctx, req.Msg.GetRef()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteProjectResponse{}), nil
}

// OpenProject wakes the existing VM and verifies its project directory.
func (s *ProjectsService) OpenProject(ctx context.Context, req *connect.Request[fletcherv1.OpenProjectRequest]) (*connect.Response[fletcherv1.OpenProjectResponse], error) {
	p, err := s.backend.Open(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.OpenProjectResponse{Project: projectToProto(p)}), nil
}

func projectToProto(p project.Project) *fletcherv1.Project {
	return &fletcherv1.Project{
		Id: p.ID, Name: p.Name, SessionId: p.SessionID, WorkingDir: p.WorkingDir,
		CreatedAt: p.CreatedAt.Unix(), UpdatedAt: p.UpdatedAt.Unix(),
	}
}
