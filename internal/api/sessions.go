package api

import (
	"context"
	"errors"
	"io"
	"net"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/session"
)

// SessionsBackend is what the SessionService handler needs from the session
// manager.
type SessionsBackend interface {
	Create(ctx context.Context, name, image, egressPolicy string) (session.Session, error)
	Get(ctx context.Context, ref string) (session.Session, error)
	List(ctx context.Context) ([]session.Session, error)
	Start(ctx context.Context, ref string) (session.Session, error)
	Stop(ctx context.Context, ref string) (session.Session, error)
	Delete(ctx context.Context, ref string) (bool, error)
	Exec(ctx context.Context, ref, command string) (session.ExecResult, error)
	Shell(ctx context.Context, ref string, spec runtime.ShellSpec, stdin io.Reader, stdout io.Writer, resize <-chan runtime.WinSize) (int32, error)
	DialSSH(ctx context.Context, ref string) (net.Conn, error)
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
	sess, err := s.backend.Create(ctx, req.Msg.GetName(), req.Msg.GetImage(), req.Msg.GetEgressPolicy())
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

// ShellSession bridges a Connect bidi stream to an interactive PTY in the
// session VM. The first client message carries ShellStart (ref + window size);
// subsequent messages carry stdin or resize events. Terminal output flows back
// as data messages, and a final exit_code message closes the stream.
func (s *SessionsService) ShellSession(ctx context.Context, stream *connect.BidiStream[fletcherv1.ShellSessionRequest, fletcherv1.ShellSessionResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || start.GetRef() == "" {
		return errs.New(errs.CategoryInvalidArgument, "first shell message must carry start with a session ref")
	}

	// stdin: client messages feed a pipe the backend reads as the PTY's stdin.
	pr, pw := io.Pipe()
	resize := make(chan runtime.WinSize, 8)
	stdout := writerFunc(func(p []byte) (int, error) {
		if serr := stream.Send(&fletcherv1.ShellSessionResponse{
			Msg: &fletcherv1.ShellSessionResponse_Data{Data: p},
		}); serr != nil {
			return 0, serr
		}
		return len(p), nil
	})

	// Forward later client messages (keystrokes, resizes) until it hangs up.
	go func() {
		defer func() { _ = pw.Close() }()
		defer close(resize)
		for {
			msg, rerr := stream.Receive()
			if rerr != nil {
				return
			}
			switch m := msg.Msg.(type) {
			case *fletcherv1.ShellSessionRequest_Stdin:
				if _, werr := pw.Write(m.Stdin); werr != nil {
					return
				}
			case *fletcherv1.ShellSessionRequest_Resize:
				select {
				case resize <- runtime.WinSize{Cols: clampUint16(m.Resize.GetCols()), Rows: clampUint16(m.Resize.GetRows())}:
				default: // drop a resize if the backend is mid-write; the next one wins
				}
			}
		}
	}()

	spec := runtime.ShellSpec{
		Term: start.GetTerm(),
		Cols: clampUint16(start.GetCols()),
		Rows: clampUint16(start.GetRows()),
	}
	code, err := s.backend.Shell(ctx, start.GetRef(), spec, pr, stdout, resize)
	_ = pr.Close() // unblock the receive goroutine's pipe writes
	if err != nil {
		return err
	}
	return stream.Send(&fletcherv1.ShellSessionResponse{
		Msg: &fletcherv1.ShellSessionResponse_ExitCode{ExitCode: code},
	})
}

// ProxySession brokers a raw byte stream between the client and a running
// session's SSH server (relayed over vsock). It backs `fletcher session ssh`
// as an SSH ProxyCommand; the VM needs no network route.
func (s *SessionsService) ProxySession(ctx context.Context, stream *connect.BidiStream[fletcherv1.ProxySessionRequest, fletcherv1.ProxySessionResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil || open.GetRef() == "" {
		return errs.New(errs.CategoryInvalidArgument, "first proxy message must carry open with a session ref")
	}
	conn, err := s.backend.DialSSH(ctx, open.GetRef())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	go proxyClientToConn(stream, conn)

	// Session -> client drives the handler's lifetime.
	buf := make([]byte, 32<<10)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			if serr := stream.Send(&fletcherv1.ProxySessionResponse{Data: append([]byte(nil), buf[:n]...)}); serr != nil {
				return serr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// proxyClientToConn copies client bytes into the session connection. On the
// client's half-close it half-closes toward sshd so it sees EOF, not a reset.
func proxyClientToConn(stream *connect.BidiStream[fletcherv1.ProxySessionRequest, fletcherv1.ProxySessionResponse], conn net.Conn) {
	for {
		msg, err := stream.Receive()
		if err != nil {
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			return
		}
		if d := msg.GetData(); len(d) > 0 {
			if _, werr := conn.Write(d); werr != nil {
				return
			}
		}
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// clampUint16 narrows a proto uint32 window dimension to the uint16 the PTY
// ioctl expects, saturating rather than wrapping.
func clampUint16(v uint32) uint16 {
	if v > 0xffff {
		return 0xffff
	}
	return uint16(v)
}

func sessionToProto(s session.Session) *fletcherv1.Session {
	p := &fletcherv1.Session{
		Id:           s.ID,
		Name:         s.Name,
		Image:        s.Image,
		State:        stateToProto(s.State),
		CreatedAt:    s.CreatedAt.Unix(),
		UpdatedAt:    s.UpdatedAt.Unix(),
		DiskBytes:    s.DiskBytes,
		EgressPolicy: s.EgressPolicy,
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
