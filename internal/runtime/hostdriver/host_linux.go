//go:build linux

package hostdriver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/joshjon/fletcher/internal/background"
	"github.com/joshjon/fletcher/internal/runtime"
)

// New selects one local account; the socket path cannot be supplied by an API client.
func New(name string) (*Driver, error) {
	account, _, err := accountInfo(name)
	if err != nil {
		return nil, err
	}
	if account.UID == int64(os.Geteuid()) {
		return nil, unavailable("host execution cannot use the daemon service account")
	}
	return &Driver{account: account, socket: "/run/fletcher-host/" + name + ".sock"}, nil
}

func accountInfo(name string) (runtime.HostAccount, string, error) {
	if !userPattern.MatchString(name) {
		return runtime.HostAccount{}, "", unavailable("host execution requires a valid local account name")
	}
	u, err := osuser.Lookup(name)
	if err != nil {
		return runtime.HostAccount{}, "", fmt.Errorf("lookup host account: %w", err)
	}
	uid, err := strconv.ParseInt(u.Uid, 10, 64)
	if err != nil || uid <= 0 {
		return runtime.HostAccount{}, "", unavailable("host execution requires a non-root local account")
	}
	if !filepath.IsAbs(u.HomeDir) {
		return runtime.HostAccount{}, "", unavailable("host account requires an absolute home directory")
	}
	if err := (runtime.ShellSpec{WorkDir: u.HomeDir, TerminalName: "host"}).Validate(); err != nil {
		return runtime.HostAccount{}, "", err
	}
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return runtime.HostAccount{}, "", fmt.Errorf("read local account shell: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 7 || fields[0] != name || fields[2] != u.Uid {
			continue
		}
		shell := fields[6]
		if !filepath.IsAbs(shell) || strings.HasSuffix(shell, "/nologin") || strings.HasSuffix(shell, "/false") {
			return runtime.HostAccount{}, "", unavailable("host execution requires a local account with a login shell")
		}
		return runtime.HostAccount{User: name, UID: uid, Home: u.HomeDir}, shell, nil
	}
	return runtime.HostAccount{}, "", unavailable("host account must exist in the local account database")
}

// ServeActivated serves the systemd-owned Unix socket as the configured unprivileged account.
func ServeActivated(ctx context.Context, daemonUser string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if os.Geteuid() == 0 {
		return unavailable("the host executor must not run as root")
	}
	current, err := osuser.Current()
	if err != nil {
		return fmt.Errorf("lookup executor account: %w", err)
	}
	account, shell, err := accountInfo(current.Username)
	if err != nil {
		return err
	}
	daemon, err := osuser.Lookup(daemonUser)
	if err != nil {
		return fmt.Errorf("lookup daemon account: %w", err)
	}
	uid, err := strconv.ParseUint(daemon.Uid, 10, 32)
	if err != nil || uid == 0 || strconv.FormatUint(uid, 10) == current.Uid {
		return unavailable("executor and daemon must use distinct non-root accounts")
	}
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) || os.Getenv("LISTEN_FDS") != "1" {
		return unavailable("host executor requires its locally installed systemd socket unit")
	}
	file := os.NewFile(3, "host-executor-listener")
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return fmt.Errorf("open activated host socket: %w", err)
	}
	defer func() { _ = listener.Close() }()
	if _, ok := listener.(*net.UnixListener); !ok {
		return unavailable("host executor accepts Unix sockets only")
	}
	dir := "/run/fletcher-host-agent-" + account.User
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open executor runtime directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("tmux.conf", []byte("set-option -g history-limit 50000\n"), 0o600); err != nil {
		return err
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return unavailable("host sessions require tmux installed on the daemon host")
	}
	s := &executor{account: account, shell: shell, tmux: tmux, dir: dir, sessions: make(map[string]*hostSession)}
	return s.serveConnections(ctx, listener, uint32(uid))
}

func (s *executor) serveConnections(ctx context.Context, listener net.Listener, uid uint32) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var clients sync.WaitGroup
	defer func() { cancel(); clients.Wait() }()
	slots := make(chan struct{}, 64)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept host executor connection: %w", err)
		}
		if !peerAllowed(conn, uid) {
			_ = conn.Close()
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		clients.Add(1)
		background.GoNamed(ctx, "host.executor.connection", func(ctx context.Context) {
			defer clients.Done()
			defer func() { <-slots }()
			defer func() { _ = conn.Close() }()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			s.serve(ctx, conn)
		})
	}
}

func peerAllowed(conn net.Conn, uid uint32) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return false
	}
	return credErr == nil && cred != nil && cred.Uid == uid
}

type executor struct {
	account          runtime.HostAccount
	shell, tmux, dir string
	mu               sync.Mutex
	sessions         map[string]*hostSession
}

type hostSession struct {
	spec   runtime.HostSessionSpec
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

func (s *executor) serve(ctx context.Context, conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var req request
	if err := readPacket(conn, &req); err != nil {
		return
	}
	if req.Version != protocolVersion {
		_ = writePacket(conn, packet{Error: "host executor protocol mismatch; update the daemon and account service together"})
		return
	}
	if req.Op == "info" {
		_ = writePacket(conn, packet{Account: s.account, Version: protocolVersion})
		return
	}
	if !sessionIDPattern.MatchString(req.Session.SessionID) || req.Session.Account != s.account {
		_ = writePacket(conn, packet{Error: "host session account or identity is invalid"})
		return
	}
	if req.Op == "start" {
		err := s.start(ctx, req.Session)
		_ = writePacket(conn, resultPacket(err))
		return
	}
	s.mu.Lock()
	hs := s.sessions[req.Session.SessionID]
	s.mu.Unlock()
	if req.Op == "stop" {
		_ = writePacket(conn, resultPacket(s.stop(ctx, req.Session.SessionID)))
		return
	}
	if hs == nil || hs.ctx.Err() != nil || hs.spec != req.Session {
		_ = writePacket(conn, packet{Error: "host session is stopped; restart the session to reconnect"})
		return
	}
	switch req.Op {
	case "list":
		listing, err := s.list(req.Path)
		p := resultPacket(err)
		p.Listing = listing
		_ = writePacket(conn, p)
	case "shell":
		_ = conn.SetDeadline(time.Time{})
		s.shellSession(ctx, conn, hs, req.Shell)
	default:
		_ = writePacket(conn, packet{Error: "unsupported host executor operation"})
	}
}

func resultPacket(err error) packet {
	if err != nil {
		return packet{Error: err.Error()}
	}
	return packet{}
}

func (s *executor) start(ctx context.Context, spec runtime.HostSessionSpec) error {
	if err := (runtime.ShellSpec{WorkDir: spec.WorkDir, TerminalName: "host"}).Validate(); err != nil {
		return err
	}
	if spec.WorkDir == "" {
		return unavailable("host session requires an initial directory")
	}
	info, err := os.Stat(spec.WorkDir)
	if err != nil || !info.IsDir() {
		return unavailable("host session directory is unavailable to the selected account")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior := s.sessions[spec.SessionID]; prior != nil && prior.ctx.Err() == nil {
		if prior.spec != spec {
			return unavailable("host session configuration does not match its existing terminals")
		}
		return nil
	}
	lifetime, cancel := context.WithCancel(ctx)
	s.sessions[spec.SessionID] = &hostSession{spec: spec, ctx: lifetime, cancel: cancel}
	return nil
}

func (s *executor) stop(ctx context.Context, id string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if err == nil {
			delete(s.sessions, id)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if hs := s.sessions[id]; hs != nil {
		hs.mu.Lock()
		defer hs.mu.Unlock()
		hs.cancel()
	}
	// Only this session's dedicated tmux server is stopped. Never remove a
	// working directory, invoke a global kill-server, or kill by account UID.
	socket := s.socket(id)
	if _, err := os.Lstat(socket); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	//nolint:gosec // locally resolved tmux binary and validated per-session socket, without a shell
	cmd := exec.CommandContext(ctx, s.tmux, "-S", socket, "kill-server")
	cmd.Env = s.env("")
	commandErr := cmd.Run()
	// tmux can acknowledge kill-server before closing its listener. A second
	// Stop can then see an exiting server that still accepts connections but
	// rejects commands. Confirm listener teardown before forgetting the namespace.
	if err := waitHostSocketClosed(ctx, socket); err != nil {
		return fmt.Errorf("stop host terminals: %w", errors.Join(commandErr, err))
	}
	return nil
}

func waitHostSocketClosed(ctx context.Context, socket string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	dialer := net.Dialer{Timeout: time.Second}
	for {
		conn, err := dialer.DialContext(ctx, "unix", socket)
		// A stale socket after a crashed or exiting server is not a live terminal.
		if errors.Is(err, unix.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("confirm terminal socket shutdown: %w", err)
		}
		_ = conn.Close()
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for terminal socket shutdown: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *executor) socket(id string) string { return filepath.Join(s.dir, id+".sock") }

func (s *executor) env(term string) []string {
	if term == "" {
		term = "xterm-256color"
	}
	return []string{
		"HOME=" + s.account.Home, "USER=" + s.account.User, "LOGNAME=" + s.account.User,
		"SHELL=" + s.shell, "TERM=" + term, "LANG=C.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
	}
}
