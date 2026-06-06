package api

import (
	"context"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/session"
)

// SessionsBackend is what the SessionService handler needs from the session
// manager.
type SessionsBackend interface {
	Create(ctx context.Context, name, image string) (session.Session, error)
	Get(ctx context.Context, ref string) (session.Session, error)
	List(ctx context.Context) ([]session.Session, error)
	Start(ctx context.Context, ref string) (session.Session, error)
	Stop(ctx context.Context, ref string) (session.Session, error)
	Delete(ctx context.Context, ref string) (bool, error)
	Exec(ctx context.Context, ref, command string) (session.ExecResult, error)
}

// SessionsService implements fletcherv1connect.SessionServiceHandler.
type SessionsService struct {
	fletcherv1connect.UnimplementedSessionServiceHandler
	backend SessionsBackend
}

// NewSessionsService wires a SessionsService to a backend.
func NewSessionsService(backend SessionsBackend) *SessionsService {
	return &SessionsService{backend: backend}
}

// CreateSession provisions a session and boots its VM. Categorised errors
// (e.g. a duplicate name) map to the wire code via the ErrorInterceptor.
func (s *SessionsService) CreateSession(ctx context.Context, req *connect.Request[fletcherv1.CreateSessionRequest]) (*connect.Response[fletcherv1.CreateSessionResponse], error) {
	sess, err := s.backend.Create(ctx, req.Msg.GetName(), req.Msg.GetImage())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.CreateSessionResponse{Session: sessionToProto(sess)}), nil
}

// GetSession fetches a session by id or name.
func (s *SessionsService) GetSession(ctx context.Context, req *connect.Request[fletcherv1.GetSessionRequest]) (*connect.Response[fletcherv1.GetSessionResponse], error) {
	sess, err := s.backend.Get(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetSessionResponse{Session: sessionToProto(sess)}), nil
}

// ListSessions returns all sessions, newest first.
func (s *SessionsService) ListSessions(ctx context.Context, _ *connect.Request[fletcherv1.ListSessionsRequest]) (*connect.Response[fletcherv1.ListSessionsResponse], error) {
	sessions, err := s.backend.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.Session, len(sessions))
	for i, sess := range sessions {
		out[i] = sessionToProto(sess)
	}
	return connect.NewResponse(&fletcherv1.ListSessionsResponse{Sessions: out}), nil
}

// StartSession boots a stopped session's VM against its persisted disk.
func (s *SessionsService) StartSession(ctx context.Context, req *connect.Request[fletcherv1.StartSessionRequest]) (*connect.Response[fletcherv1.StartSessionResponse], error) {
	sess, err := s.backend.Start(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.StartSessionResponse{Session: sessionToProto(sess)}), nil
}

// StopSession stops a running session's VM, keeping its disk on hand.
func (s *SessionsService) StopSession(ctx context.Context, req *connect.Request[fletcherv1.StopSessionRequest]) (*connect.Response[fletcherv1.StopSessionResponse], error) {
	sess, err := s.backend.Stop(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.StopSessionResponse{Session: sessionToProto(sess)}), nil
}

// DeleteSession stops a session (if running) and destroys its disk.
func (s *SessionsService) DeleteSession(ctx context.Context, req *connect.Request[fletcherv1.DeleteSessionRequest]) (*connect.Response[fletcherv1.DeleteSessionResponse], error) {
	deleted, err := s.backend.Delete(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DeleteSessionResponse{Deleted: deleted}), nil
}

// ExecSession runs a command in a running session and returns its output.
func (s *SessionsService) ExecSession(ctx context.Context, req *connect.Request[fletcherv1.ExecSessionRequest]) (*connect.Response[fletcherv1.ExecSessionResponse], error) {
	res, err := s.backend.Exec(ctx, req.Msg.GetRef(), req.Msg.GetCommand())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.ExecSessionResponse{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}), nil
}

func sessionToProto(s session.Session) *fletcherv1.Session {
	p := &fletcherv1.Session{
		Id:        s.ID,
		Name:      s.Name,
		Image:     s.Image,
		State:     stateToProto(s.State),
		CreatedAt: s.CreatedAt.Unix(),
		UpdatedAt: s.UpdatedAt.Unix(),
	}
	if s.LastUsedAt != nil {
		v := s.LastUsedAt.Unix()
		p.LastUsedAt = &v
	}
	return p
}

func stateToProto(s session.State) fletcherv1.SessionState {
	switch s {
	case session.StateRunning:
		return fletcherv1.SessionState_SESSION_STATE_RUNNING
	case session.StateStopped:
		return fletcherv1.SessionState_SESSION_STATE_STOPPED
	default:
		return fletcherv1.SessionState_SESSION_STATE_UNSPECIFIED
	}
}
