// Package daemon orchestrates the long-running fletcher process: opens
// SQLite, runs migrations, registers Connect handlers, and serves them on
// a local Unix socket inside an oklog/run group.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/run"

	"connectrpc.com/connect"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/joshjon/fletcher/internal/api"
	"github.com/joshjon/fletcher/internal/approval"
	"github.com/joshjon/fletcher/internal/audit"
	"github.com/joshjon/fletcher/internal/buildinfo"
	"github.com/joshjon/fletcher/internal/gateway"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/job"
	fletchermcp "github.com/joshjon/fletcher/internal/mcp"
	"github.com/joshjon/fletcher/internal/peer"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver"
	runtimemock "github.com/joshjon/fletcher/internal/runtime/mockdriver"
	"github.com/joshjon/fletcher/internal/runtime/runcdriver"
	"github.com/joshjon/fletcher/internal/secrets"
	"github.com/joshjon/fletcher/internal/snapshot"
	"github.com/joshjon/fletcher/internal/snapshot/btrfsdriver"
	snapmock "github.com/joshjon/fletcher/internal/snapshot/mockdriver"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// auditRecorder is the daemon's privileged-op audit sink. Phase 4 wires
// the Noop recorder; future phases will replace it with the SQLite-backed
// log without changing any call sites.
var auditRecorder audit.Recorder = audit.Noop{}

// Config holds boot-time daemon settings.
type Config struct {
	SocketPath        string
	DatabasePath      string
	LogLevel          string
	GatewayListenAddr string
	MCPListenAddr     string
	AgeIdentityPath   string

	// RuntimeKind selects the runtime.Driver: "mock" (default), "runc",
	// or "firecracker". Non-mock drivers are Linux-only.
	RuntimeKind string
	// SnapshotKind selects the snapshot.Driver: "mock" (default) or
	// "btrfs". Non-mock drivers are Linux-only.
	SnapshotKind string
	// BtrfsRoot is the on-disk root for btrfs subvolumes; required when
	// SnapshotKind=btrfs.
	BtrfsRoot string
	// RuncBinary overrides the runc executable path when RuntimeKind=runc.
	RuncBinary string
	// CredentialsDir is the host directory under which trusted-credential
	// mode (Phase 12) resolves each credential's HostRelPath. Defaults to
	// the operator's $HOME; empty disables credential mounting entirely.
	CredentialsDir string
	// PublicEndpoint is the host:port peers dial to reach this daemon
	// over WireGuard from outside the LAN. Set once at install (e.g.
	// "home.example.com:51820"); empty means peer pairing fails with a
	// clear error pointing at how to set it. Operator-knowledge config:
	// the daemon can't reliably auto-detect this in every NAT setup.
	PublicEndpoint string
}

// shutdownTimeout caps how long the daemon waits for in-flight work before
// forcing exit. Matches STANDARDS.md.
const shutdownTimeout = 30 * time.Second

// Run starts the daemon and blocks until ctx is cancelled or a fatal error
// occurs. On shutdown it closes the listener, removes the socket file, and
// closes the database.
func Run(ctx context.Context, cfg Config) error {
	logger := newLogger(cfg.LogLevel)
	logger.Info("starting fletcher daemon",
		slog.String("socket", cfg.SocketPath),
		slog.String("database", cfg.DatabasePath),
	)
	if err := ensureDirs(cfg); err != nil {
		return err
	}

	db, err := sqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(db); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	logger.Info("migrations up to date")

	svcs, err := buildServices(ctx, cfg, sqliteq.New(db), logger)
	if err != nil {
		return err
	}

	if err := svcs.run(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("daemon stopped")
	return nil
}

// services bundles everything Run needs to wire into the oklog/run group.
// Splitting construction out keeps Run's funlen reasonable while still
// surfacing every component in one place.
type services struct {
	cfg            Config
	supervisor     *job.Supervisor
	connectSrv     *http.Server
	gatewaySrv     *http.Server
	mcpSrv         *http.Server
	connectLn      net.Listener
	gatewayLn      net.Listener
	mcpLn          net.Listener
	gatewayBaseURL string
	mcpBaseURL     string
}

func buildServices(ctx context.Context, cfg Config, queries *sqliteq.Queries, logger *slog.Logger) (*services, error) {
	connectLn, err := listenUnix(ctx, cfg.SocketPath)
	if err != nil {
		return nil, err
	}

	secretsStore, err := secrets.Open(queries, cfg.AgeIdentityPath)
	if err != nil {
		return nil, fmt.Errorf("open secrets store: %w", err)
	}

	snapDriver, err := buildSnapshotDriver(cfg)
	if err != nil {
		return nil, fmt.Errorf("init snapshot driver: %w", err)
	}
	rtDriver, err := buildRuntimeDriver(cfg)
	if err != nil {
		return nil, fmt.Errorf("init runtime driver: %w", err)
	}
	logger.Info("drivers selected",
		slog.String("runtime", driverKind(cfg.RuntimeKind)),
		slog.String("snapshot", driverKind(cfg.SnapshotKind)),
	)

	gatewayLn, gatewayURL, err := listenTCP(ctx, cfg.GatewayListenAddr, "gateway")
	if err != nil {
		return nil, err
	}
	gw := gateway.New(secretsStore, gateway.NewAnthropicBackend(), gatewayURL, logger)
	logger.Info("model gateway ready", slog.String("base_url", gatewayURL))

	startedAt := time.Now()
	mcpLn, mcpURL, err := listenTCP(ctx, cfg.MCPListenAddr, "mcp")
	if err != nil {
		return nil, err
	}
	approvalSvc := approval.NewService(queries, approval.ServiceOptions{})
	peerSvc := peer.NewService(queries, peer.Options{
		PublicEndpoint: cfg.PublicEndpoint,
	})
	wgKeyProvider := newServerKeyProvider(secretsStore)

	mcpServer := fletchermcp.NewServer("fletcher", buildinfo.Version, auditRecorder, logger)
	fletchermcp.RegisterBuiltinTools(mcpServer, startedAt, &http.Client{Timeout: 30 * time.Second}, approvalSvc)
	logger.Info("mcp server ready", slog.String("base_url", mcpURL))

	supervisor := job.NewSupervisor(queries, rtDriver, snapDriver, logger, job.SupervisorOptions{
		JobEnv: []string{
			// OpenAI-compatible path (Codex, Aider, OpenHands, pi). The
			// gateway's /v1/chat/completions handler translates to Anthropic.
			"OPENAI_BASE_URL=" + gatewayURL + "/v1",
			"OPENAI_API_KEY=fletcher-gateway", // placeholder; real key lives in secrets store
			// Anthropic-native path (Claude Code). The gateway's /v1/messages
			// handler proxies the raw Messages request to api.anthropic.com.
			"ANTHROPIC_BASE_URL=" + gatewayURL,
			"ANTHROPIC_API_KEY=fletcher-gateway", // placeholder; real key lives in secrets store
			"FLETCHER_MCP_URL=" + mcpURL,
			// Model catalog (Phase 14) - pi-extension and other agents fetch
			// this on startup to discover providers without per-job config.
			"FLETCHER_CATALOG_URL=" + gatewayURL + "/v1/catalog.json",
		},
		CredentialsRoot: cfg.CredentialsDir,
	})

	connectDeps := connectDeps{
		jobs:      job.NewService(queries, supervisor),
		secrets:   secretsStore,
		approvals: approvalSvc,
		peers:     peerSvc,
		serverKey: wgKeyProvider,
		models:    gatewayCatalog{baseURL: gatewayURL},
	}
	return &services{
		cfg:            cfg,
		supervisor:     supervisor,
		connectSrv:     newHTTPServer(startedAt.Unix(), connectDeps, logger),
		gatewaySrv:     newGatewayHTTPServer(gw),
		mcpSrv:         newMCPHTTPServer(mcpServer),
		connectLn:      connectLn,
		gatewayLn:      gatewayLn,
		mcpLn:          mcpLn,
		gatewayBaseURL: gatewayURL,
		mcpBaseURL:     mcpURL,
	}, nil
}

// connectDeps bundles the backends newHTTPServer wires onto the Connect
// mux. Grouping them in a struct keeps newHTTPServer's signature tight
// as more services land.
type connectDeps struct {
	jobs      api.JobsBackend
	secrets   api.SecretsBackend
	approvals api.ApprovalsBackend
	peers     api.PeersBackend
	serverKey api.ServerKeyProvider
	models    api.CatalogBuilder
}

// gatewayCatalog adapts the gateway-base-URL closure into the
// api.CatalogBuilder interface. The base URL is captured at daemon start
// (after the listener binds) so random-port (":0") setups are handled.
type gatewayCatalog struct{ baseURL string }

// Catalog returns the current catalog snapshot.
func (g gatewayCatalog) Catalog() gateway.Catalog { return gateway.BuildCatalog(g.baseURL) }

func (s *services) run(ctx context.Context, logger *slog.Logger) error {
	var g run.Group
	// serveActor's interrupt path uses a fresh context for graceful shutdown
	// because the parent ctx is already cancelled by the time interrupt fires.
	//nolint:contextcheck // shutdown must outlive the cancelled parent ctx
	g.Add(serveActor(logger, s.connectSrv, s.connectLn, s.cfg.SocketPath))
	//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
	g.Add(httpServeActor(logger, "gateway", s.gatewaySrv, s.gatewayLn, s.gatewayBaseURL))
	//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
	g.Add(httpServeActor(logger, "mcp", s.mcpSrv, s.mcpLn, s.mcpBaseURL))
	g.Add(supervisorActor(ctx, s.supervisor))
	g.Add(signalActor(ctx))
	return g.Run()
}

func ensureDirs(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	return nil
}

// listenUnix opens a Unix-domain listener at socketPath, removing any stale
// file left behind by a previous crash. The socket is chmod'd to 0600 so only
// the owning user can talk to the daemon.
func listenUnix(ctx context.Context, socketPath string) (net.Listener, error) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

func newHTTPServer(startedAt int64, deps connectDeps, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()

	interceptors := connect.WithInterceptors(
		api.RequestIDInterceptor(),
		api.ErrorInterceptor(logger),
	)

	adminPath, adminHandler := fletcherv1connect.NewAdminServiceHandler(
		api.NewAdminService(startedAt), interceptors,
	)
	mux.Handle(adminPath, adminHandler)

	jobsPath, jobsHandler := fletcherv1connect.NewJobServiceHandler(
		api.NewJobsService(deps.jobs), interceptors,
	)
	mux.Handle(jobsPath, jobsHandler)

	secretsPath, secretsHandler := fletcherv1connect.NewSecretServiceHandler(
		api.NewSecretsService(deps.secrets), interceptors,
	)
	mux.Handle(secretsPath, secretsHandler)

	approvalsPath, approvalsHandler := fletcherv1connect.NewApprovalServiceHandler(
		api.NewApprovalsService(deps.approvals), interceptors,
	)
	mux.Handle(approvalsPath, approvalsHandler)

	peersPath, peersHandler := fletcherv1connect.NewPeerServiceHandler(
		api.NewPeersService(deps.peers, deps.serverKey), interceptors,
	)
	mux.Handle(peersPath, peersHandler)

	modelsPath, modelsHandler := fletcherv1connect.NewModelServiceHandler(
		api.NewModelsService(deps.models), interceptors,
	)
	mux.Handle(modelsPath, modelsHandler)

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// listenTCP binds a TCP listener and resolves the base URL callers should
// target. Resolving here (rather than echoing the config) means random-
// port (":0") setups still produce a usable URL.
func listenTCP(ctx context.Context, addr, role string) (net.Listener, string, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("listen %s %s: %w", role, addr, err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, "", fmt.Errorf("%s listener returned unexpected addr type %T", role, ln.Addr())
	}
	host := tcp.IP.String()
	if host == "<nil>" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return ln, fmt.Sprintf("http://%s:%d", host, tcp.Port), nil
}

func newGatewayHTTPServer(gw *gateway.Gateway) *http.Server {
	return &http.Server{
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

func newMCPHTTPServer(mcp *fletchermcp.Server) *http.Server {
	streamable := mcpserver.NewStreamableHTTPServer(mcp.Inner())
	return &http.Server{
		Handler:           streamable,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// httpServeActor is a generic run.Group actor for HTTP servers behind a
// TCP listener. Used for the gateway and MCP listeners; the Connect
// surface (Unix socket) keeps its own actor because it removes the socket
// file on shutdown.
func httpServeActor(logger *slog.Logger, role string, srv *http.Server, ln net.Listener, baseURL string) (func() error, func(error)) {
	execute := func() error {
		logger.Info(role+" listening", slog.String("base_url", baseURL))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%s serve: %w", role, err)
		}
		return nil
	}
	interrupt := func(_ error) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	return execute, interrupt
}

// serveActor returns the run.Group actor pair that owns the HTTP server.
func serveActor(logger *slog.Logger, srv *http.Server, ln net.Listener, socketPath string) (func() error, func(error)) {
	execute := func() error {
		logger.Info("listening", slog.String("socket", socketPath))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
	interrupt := func(_ error) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = os.Remove(socketPath)
	}
	return execute, interrupt
}

// supervisorActor wraps the job supervisor's Run as an oklog/run actor.
// The supervisor's drain() honours ctx cancellation and waits for in-flight
// runOne goroutines, so the interrupt closure has nothing to do here.
func supervisorActor(ctx context.Context, sup *job.Supervisor) (func() error, func(error)) {
	cancelCh := make(chan struct{})
	execute := func() error {
		err := sup.Run(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	interrupt := func(_ error) { close(cancelCh) }
	return execute, interrupt
}

// signalActor returns the run.Group actor pair that observes ctx (typically
// wired with signal.NotifyContext in main) and triggers group shutdown when
// the signal fires.
func signalActor(ctx context.Context) (func() error, func(error)) {
	cancelCh := make(chan struct{})
	execute := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cancelCh:
			return nil
		}
	}
	interrupt := func(_ error) { close(cancelCh) }
	return execute, interrupt
}

// defaultDriverKind is the fallback when neither config nor flag selects
// one. "mock" everywhere so an unconfigured daemon still boots on macOS.
const defaultDriverKind = "mock"

// buildSnapshotDriver constructs the snapshot.Driver chosen by cfg. The
// btrfs driver is only meaningful on Linux; on darwin it constructs to a
// shim whose New returns "not supported on darwin".
func buildSnapshotDriver(cfg Config) (snapshot.Driver, error) {
	kind := cfg.SnapshotKind
	if kind == "" {
		kind = defaultDriverKind
	}
	switch kind {
	case "mock":
		snapRoot := filepath.Join(filepath.Dir(cfg.DatabasePath), "snapshots")
		return snapmock.New(snapRoot)
	case "btrfs":
		root := cfg.BtrfsRoot
		if root == "" {
			root = filepath.Join(filepath.Dir(cfg.DatabasePath), "snapshots")
		}
		return btrfsdriver.New(btrfsdriver.Options{RootDir: root})
	default:
		return nil, fmt.Errorf("unknown snapshot kind %q", cfg.SnapshotKind)
	}
}

// buildRuntimeDriver constructs the runtime.Driver chosen by cfg.
func buildRuntimeDriver(cfg Config) (runtime.Driver, error) {
	kind := cfg.RuntimeKind
	if kind == "" {
		kind = defaultDriverKind
	}
	switch kind {
	case "mock":
		return runtimemock.New(), nil
	case "runc":
		return runcdriver.New(runcdriver.Options{Binary: cfg.RuncBinary})
	case "firecracker":
		return firecrackerdriver.New(firecrackerdriver.Options{})
	default:
		return nil, fmt.Errorf("unknown runtime kind %q", cfg.RuntimeKind)
	}
}

func driverKind(v string) string {
	if v == "" {
		return defaultDriverKind
	}
	return v
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	base := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(api.NewContextLogHandler(base))
}
