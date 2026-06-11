package api

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

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
	Create(ctx context.Context, name, image, egressPolicy, gateway string, runApp bool) (session.Session, error)
	Get(ctx context.Context, ref string) (session.Session, error)
	List(ctx context.Context) ([]session.Session, error)
	Start(ctx context.Context, ref string) (session.Session, error)
	Stop(ctx context.Context, ref string) (session.Session, error)
	Delete(ctx context.Context, ref string) (bool, error)
	UpdateSession(ctx context.Context, ref, egressPolicy, gateway string) (session.Session, bool, error)
	Exec(ctx context.Context, ref, command string) (session.ExecResult, error)
	Shell(ctx context.Context, ref string, spec runtime.ShellSpec, stdin io.Reader, stdout io.Writer, resize <-chan runtime.WinSize) (int32, error)
	DialSSH(ctx context.Context, ref string) (net.Conn, error)
	Publish(ctx context.Context, ref string, guestPort int, name string, public bool, host string) (session.PublishedPort, error)
	Unpublish(ctx context.Context, ref string, guestPort int) error
	ListPorts(ctx context.Context, ref string) ([]session.PublishedPort, error)
	Restart(ctx context.Context, ref string) (session.Session, error)
	Redeploy(ctx context.Context, ref, newImage string) (session.Session, error)
	Rollback(ctx context.Context, ref string) (session.Session, error)
	Logs(ctx context.Context, ref string, tailLines int) (string, error)
	StreamLogs(ctx context.Context, ref string, tailLines int, follow bool, w io.Writer) error
	AppRestartCount(ctx context.Context, ref string) (int64, bool)
	CommitImage(ctx context.Context, ref string, p session.CommitImageParams) (string, error)
}

// DeployInfoResolver returns the image-derived deploy detail for a run_app
// session's image (its effective entrypoint and lowest EXPOSE port), or ok
// false when the image has no recorded template metadata.
type DeployInfoResolver interface {
	DeployInfo(image string) (entrypoint []string, exposedPort int, ok bool)
}

// CertStatusResolver reports the public TLS cert state for a published public
// hostname (status string + NotAfter Unix seconds). Backed by the public web
// server; nil when public web is off.
type CertStatusResolver interface {
	CertStatus(ctx context.Context, host string) (status string, expiresAt int64)
}

// ImageRefresher attempts a best-effort re-pull of a registry-sourced image
// before a redeploy re-forks from it, refreshing the on-disk template. Returns
// true when a pull updated the template; false for a local image or any
// skip/failure (which it logs).
type ImageRefresher interface {
	RefreshImage(ctx context.Context, image string) bool
	// HasTemplate reports whether an imported template of this name exists, so
	// a redeploy retarget can tell a local template from a registry ref.
	HasTemplate(name string) bool
	// ImportRef imports a registry ref under the given template name (replacing
	// it), for a redeploy that retargets the session's image source.
	ImportRef(ctx context.Context, ref, name string) error
}

// SessionsService implements fletcherv1connect.SessionServiceHandler.
type SessionsService struct {
	fletcherv1connect.UnimplementedSessionServiceHandler
	backend SessionsBackend
	// publicIP is the daemon's discovered public IP (host of the public
	// endpoint), surfaced so the client can tell the operator the exact A record
	// to create for a --public port. Empty when no public endpoint is known.
	publicIP string
	// deploy resolves image-derived deploy detail for GetSession; nil disables it.
	deploy DeployInfoResolver
	// certs resolves public TLS cert state for ListPorts; nil disables it.
	certs CertStatusResolver
	// refresher re-pulls a registry image before RedeploySession; nil disables it.
	refresher ImageRefresher
}

// SessionsDeps are the optional collaborators the SessionsService uses beyond
// its core backend; each is independently nil-able.
type SessionsDeps struct {
	// PublicIP is the daemon's discovered public IP for --public DNS guidance.
	PublicIP string
	// Deploy resolves run_app deploy detail for GetSession.
	Deploy DeployInfoResolver
	// Certs resolves public-port TLS status for ListPorts.
	Certs CertStatusResolver
	// Refresher re-pulls a registry-sourced image before RedeploySession.
	Refresher ImageRefresher
}

// NewSessionsService wires a SessionsService to a backend and its optional deps.
func NewSessionsService(backend SessionsBackend, deps SessionsDeps) *SessionsService {
	return &SessionsService{
		backend:   backend,
		publicIP:  deps.PublicIP,
		deploy:    deps.Deploy,
		certs:     deps.Certs,
		refresher: deps.Refresher,
	}
}

// CreateSession provisions a session and boots its VM. Categorised errors
// (e.g. a duplicate name) map to the wire code via the ErrorInterceptor.
func (s *SessionsService) CreateSession(ctx context.Context, req *connect.Request[fletcherv1.CreateSessionRequest]) (*connect.Response[fletcherv1.CreateSessionResponse], error) {
	sess, err := s.backend.Create(ctx, req.Msg.GetName(), req.Msg.GetImage(), req.Msg.GetEgressPolicy(), req.Msg.GetGateway(), req.Msg.GetRunApp())
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
	p := sessionToProto(sess)
	// Deploy detail is image-derived and only meaningful for a run_app session;
	// resolve it here (GetSession only) so the list stays cheap.
	if sess.RunApp && s.deploy != nil {
		if entrypoint, port, ok := s.deploy.DeployInfo(sess.Image); ok {
			var exposed uint32
			if port > 0 && port <= 65535 {
				exposed = uint32(port)
			}
			di := &fletcherv1.DeployInfo{
				Entrypoint:  entrypoint,
				ExposedPort: exposed,
			}
			// restart_count is runtime state from the guest, only for a running
			// app session; absent (0) when stopped.
			if n, ok := s.backend.AppRestartCount(ctx, req.Msg.GetRef()); ok && n > 0 && n <= 1<<32-1 {
				di.RestartCount = uint32(n)
			}
			p.Deploy = di
		}
	}
	return connect.NewResponse(&fletcherv1.GetSessionResponse{Session: p}), nil
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

// UpdateSession changes a session's egress policy and/or gateway (empty leaves
// a field unchanged); restart_required flags that a running session needs a
// restart for the change to take effect.
func (s *SessionsService) UpdateSession(ctx context.Context, req *connect.Request[fletcherv1.UpdateSessionRequest]) (*connect.Response[fletcherv1.UpdateSessionResponse], error) {
	sess, restartRequired, err := s.backend.UpdateSession(ctx, req.Msg.GetRef(), req.Msg.GetEgressPolicy(), req.Msg.GetGateway())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.UpdateSessionResponse{
		Session:         sessionToProto(sess),
		RestartRequired: restartRequired,
	}), nil
}

// RestartSession stops a running session's VM and starts it again against the
// same fork (re-running the app for a run_app deploy).
func (s *SessionsService) RestartSession(ctx context.Context, req *connect.Request[fletcherv1.RestartSessionRequest]) (*connect.Response[fletcherv1.RestartSessionResponse], error) {
	sess, err := s.backend.Restart(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.RestartSessionResponse{Session: sessionToProto(sess)}), nil
}

// RedeploySession re-pulls a registry-sourced image (best-effort), then
// replaces the session's disk with a fresh fork of the current template and
// restarts it.
func (s *SessionsService) RedeploySession(ctx context.Context, req *connect.Request[fletcherv1.RedeploySessionRequest]) (*connect.Response[fletcherv1.RedeploySessionResponse], error) {
	ref := req.Msg.GetRef()
	target := strings.TrimSpace(req.Msg.GetImage())

	var refreshed bool
	var retargetImage string
	switch {
	case target == "":
		// Re-pull the current image's source before re-forking so the fresh fork
		// picks up the new template. Best-effort: a failed pull is logged by the
		// refresher and we redeploy the current template. A lookup miss surfaces
		// from Redeploy below with the proper error.
		if s.refresher != nil {
			if sess, err := s.backend.Get(ctx, ref); err == nil {
				refreshed = s.refresher.RefreshImage(ctx, sess.Image)
			}
		}
	case s.refresher != nil && s.refresher.HasTemplate(target):
		// An imported template: retarget the session to it.
		retargetImage = target
	case s.refresher != nil:
		// A registry ref: import it under the session's current template name.
		// Unlike the best-effort same-ref refresh, an explicit ref must not
		// silently fall back to the old image.
		sess, err := s.backend.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		if err := s.refresher.ImportRef(ctx, target, sess.Image); err != nil {
			return nil, errs.Newf(errs.CategoryFailedPrecondition,
				"import %q for redeploy: %s", target, err)
		}
		refreshed = true
	default:
		return nil, errs.New(errs.CategoryFailedPrecondition,
			"this daemon cannot retarget a redeploy (no image importer wired)")
	}

	sess, err := s.backend.Redeploy(ctx, ref, retargetImage)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.RedeploySessionResponse{
		Session:        sessionToProto(sess),
		ImageRefreshed: refreshed,
	}), nil
}

// RollbackSession swaps a session back to the fork its last redeploy retired.
func (s *SessionsService) RollbackSession(ctx context.Context, req *connect.Request[fletcherv1.RollbackSessionRequest]) (*connect.Response[fletcherv1.RollbackSessionResponse], error) {
	sess, err := s.backend.Rollback(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.RollbackSessionResponse{Session: sessionToProto(sess)}), nil
}

// CommitSessionImage commits a session's fork as a new image template.
func (s *SessionsService) CommitSessionImage(ctx context.Context, req *connect.Request[fletcherv1.CommitSessionImageRequest]) (*connect.Response[fletcherv1.CommitSessionImageResponse], error) {
	img, err := s.backend.CommitImage(ctx, req.Msg.GetRef(), session.CommitImageParams{
		Name:        req.Msg.GetName(),
		Entrypoint:  req.Msg.GetEntrypoint(),
		Cmd:         req.Msg.GetCmd(),
		WorkingDir:  req.Msg.GetWorkingDir(),
		ExposedPort: int(req.Msg.GetExposedPort()),
		Force:       req.Msg.GetForce(),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.CommitSessionImageResponse{Image: img}), nil
}

// GetSessionLogs returns the tail of a run_app session's app log.
func (s *SessionsService) GetSessionLogs(ctx context.Context, req *connect.Request[fletcherv1.GetSessionLogsRequest]) (*connect.Response[fletcherv1.GetSessionLogsResponse], error) {
	content, err := s.backend.Logs(ctx, req.Msg.GetRef(), int(req.Msg.GetTailLines()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetSessionLogsResponse{Content: content}), nil
}

// StreamSessionLogs streams a run_app session's app log to the client, with an
// optional live follow that ends when the client disconnects.
func (s *SessionsService) StreamSessionLogs(ctx context.Context, req *connect.Request[fletcherv1.StreamSessionLogsRequest], stream *connect.ServerStream[fletcherv1.StreamSessionLogsResponse]) error {
	w := writerFunc(func(p []byte) (int, error) {
		// Copy: the runtime's frame payload may be reused after Write returns.
		if err := stream.Send(&fletcherv1.StreamSessionLogsResponse{Data: append([]byte(nil), p...)}); err != nil {
			return 0, err
		}
		return len(p), nil
	})
	return s.backend.StreamLogs(ctx, req.Msg.GetRef(), int(req.Msg.GetTailLines()), req.Msg.GetFollow(), w)
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

// PublishPort exposes a port the session serves, brokered by the daemon.
func (s *SessionsService) PublishPort(ctx context.Context, req *connect.Request[fletcherv1.PublishPortRequest]) (*connect.Response[fletcherv1.PublishPortResponse], error) {
	pp, err := s.backend.Publish(ctx, req.Msg.GetRef(), int(req.Msg.GetGuestPort()), req.Msg.GetName(), req.Msg.GetPublic(), req.Msg.GetHost())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.PublishPortResponse{Port: publishedToProto(pp), PublicIp: s.publicIP}), nil
}

// UnpublishPort stops forwarding a session's published port.
func (s *SessionsService) UnpublishPort(ctx context.Context, req *connect.Request[fletcherv1.UnpublishPortRequest]) (*connect.Response[fletcherv1.UnpublishPortResponse], error) {
	if err := s.backend.Unpublish(ctx, req.Msg.GetRef(), int(req.Msg.GetGuestPort())); err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.UnpublishPortResponse{}), nil
}

// ListPorts returns a session's published ports.
func (s *SessionsService) ListPorts(ctx context.Context, req *connect.Request[fletcherv1.ListPortsRequest]) (*connect.Response[fletcherv1.ListPortsResponse], error) {
	ports, err := s.backend.ListPorts(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	out := make([]*fletcherv1.PublishedPort, len(ports))
	for i, pp := range ports {
		p := publishedToProto(pp)
		// TLS status applies only to a public port with a hostname, and only
		// when public web (the certmagic terminator) is up.
		if pp.Public && pp.Host != "" && s.certs != nil {
			p.TlsStatus, p.TlsExpiresAt = s.certs.CertStatus(ctx, pp.Host)
		}
		out[i] = p
	}
	return connect.NewResponse(&fletcherv1.ListPortsResponse{Ports: out, PublicIp: s.publicIP}), nil
}

func publishedToProto(p session.PublishedPort) *fletcherv1.PublishedPort {
	return &fletcherv1.PublishedPort{
		Id:         p.ID,
		SessionId:  p.SessionID,
		GuestPort:  uint32(p.GuestPort), //nolint:gosec // guest port validated 1..65535
		Name:       p.Name,
		TunnelPort: uint32(p.TunnelPort), //nolint:gosec // tunnel port is an OS-assigned 1..65535 value
		Public:     p.Public,
		Host:       p.Host,
		CreatedAt:  p.CreatedAt.Unix(),
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
		Gateway:      s.Gateway,
		RunApp:       s.RunApp,
		HasRollback:  s.HasRollback,
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
