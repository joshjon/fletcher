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
	"strconv"
	"strings"
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
	"github.com/joshjon/fletcher/internal/network/wireguard"
	"github.com/joshjon/fletcher/internal/peer"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/vmm"
	runtimemock "github.com/joshjon/fletcher/internal/runtime/mockdriver"
	"github.com/joshjon/fletcher/internal/runtime/runcdriver"
	"github.com/joshjon/fletcher/internal/secrets"
	"github.com/joshjon/fletcher/internal/settings"
	"github.com/joshjon/fletcher/internal/snapshot"
	"github.com/joshjon/fletcher/internal/snapshot/btrfsdriver"
	"github.com/joshjon/fletcher/internal/snapshot/ext4driver"
	snapmock "github.com/joshjon/fletcher/internal/snapshot/mockdriver"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// auditRecorder is the daemon's privileged-op audit sink. Phase 4 wires
// the Noop recorder; future phases will replace it with the SQLite-backed
// log without changing any call sites.
var auditRecorder audit.Recorder = audit.Noop{}

// checkForUpgrade hits GitHub Releases in the background at boot and
// logs a hint if a newer Fletcher version is published. Failures are
// silent at info level (debug level if you want to see them) - the
// daemon should not fail to start because GitHub is unreachable.
func checkForUpgrade(ctx context.Context, logger *slog.Logger) {
	release, err := buildinfo.CheckLatest(ctx, nil)
	if err != nil {
		logger.Debug("release check skipped", slog.String("err", err.Error()))
		return
	}
	if !buildinfo.UpgradeAvailable(buildinfo.Version, release.TagName) {
		return
	}
	logger.Info("a newer fletcher release is available",
		slog.String("current", buildinfo.Version),
		slog.String("latest", release.TagName),
		slog.String("url", release.HTMLURL),
		slog.String("upgrade", "curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh"),
	)
}

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
	// WireGuardListenPort is the UDP port wireguard-go binds on for the
	// hub-side tunnel. Defaults to 51820. The same value is what UPnP
	// asks the router to forward.
	WireGuardListenPort int
	// DisableUPnP turns off the automatic router-port-forward attempt
	// at startup. Useful when running behind a router known to mishandle
	// UPnP, or in test environments. Default false (UPnP enabled).
	DisableUPnP bool
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
		slog.String("version", buildinfo.Version),
	)
	if err := ensureDirs(cfg); err != nil {
		return err
	}
	go checkForUpgrade(ctx, logger)

	db, err := sqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(db); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	logger.Info("migrations up to date")

	queries := sqliteq.New(db)
	if err := applySettings(ctx, &cfg, settings.NewStore(queries), logger); err != nil {
		return err
	}
	logger = newLogger(cfg.LogLevel) // reflect a log_level setting

	svcs, err := buildServices(ctx, cfg, queries, logger)
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
	gatewayUnixLn  net.Listener
	mcpUnixLn      net.Listener
	remoteSrv      *http.Server
	remoteLn       net.Listener
	remoteAddr     string
	gatewayUnix    string
	mcpUnix        string
	gatewayBaseURL string
	mcpBaseURL     string
	tunnel         wireguard.Tunnel
}

//nolint:funlen // the single construction hub that wires every subsystem; splitting further would scatter the boot sequence
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
	// The gateway and MCP also listen on unix sockets so a fork (loopback
	// only) can reach them via bind-mounted sockets + the in-fork forwarder.
	gatewayUnix := gatewaySocketPath(cfg)
	gatewayUnixLn, err := listenUnix(ctx, gatewayUnix)
	if err != nil {
		return nil, fmt.Errorf("gateway unix listener: %w", err)
	}
	mcpUnix := mcpSocketPath(cfg)
	mcpUnixLn, err := listenUnix(ctx, mcpUnix)
	if err != nil {
		return nil, fmt.Errorf("mcp unix listener: %w", err)
	}

	approvalSvc := approval.NewService(queries, approval.ServiceOptions{})
	apiEndpoint := remoteAPIAddr()
	peerSvc := peer.NewService(queries, peer.Options{
		PublicEndpoint: cfg.PublicEndpoint,
		APIEndpoint:    apiEndpoint,
	})
	wgKeyProvider := newServerKeyProvider(secretsStore)

	netSetup, err := bringUpNetwork(ctx, cfg, logger, peerSvc, wgKeyProvider)
	if err != nil {
		return nil, fmt.Errorf("bring up network: %w", err)
	}

	// Expose the Connect API on the tunnel interface so paired clients can
	// drive the daemon. Requires a per-peer token (the unix socket stays
	// local + auth-free). Best-effort: nil when the tunnel is not up.
	remoteLn := listenRemoteAPI(ctx, netSetup, apiEndpoint, logger)

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
		peerSync:  &tunnelPeerSyncer{peers: peerSvc, tunnel: netSetup.Tunnel, logger: logger},
		settings:  settings.NewStore(queries),
	}

	connectSrv := newHTTPServer(startedAt.Unix(), connectDeps, logger)
	remoteSrv := newRemoteServer(remoteLn, peerSvc, connectSrv.Handler)

	return &services{
		cfg:            cfg,
		supervisor:     supervisor,
		connectSrv:     connectSrv,
		gatewaySrv:     newGatewayHTTPServer(gw),
		mcpSrv:         newMCPHTTPServer(mcpServer),
		connectLn:      connectLn,
		gatewayLn:      gatewayLn,
		mcpLn:          mcpLn,
		gatewayUnixLn:  gatewayUnixLn,
		mcpUnixLn:      mcpUnixLn,
		remoteSrv:      remoteSrv,
		remoteLn:       remoteLn,
		remoteAddr:     apiEndpoint,
		gatewayUnix:    gatewayUnix,
		mcpUnix:        mcpUnix,
		gatewayBaseURL: gatewayURL,
		mcpBaseURL:     mcpURL,
		tunnel:         netSetup.Tunnel,
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
	peerSync  api.PeerSyncer
	settings  api.SettingsBackend
}

// tunnelPeerSyncer is the production PeerSyncer: it pulls the current
// peer registry on every change and pushes the result into the running
// WireGuard tunnel.
type tunnelPeerSyncer struct {
	peers  *peer.Service
	tunnel wireguard.Tunnel
	logger *slog.Logger
}

// SyncPeers refreshes the tunnel's peer set. Returns nil if the tunnel
// is not configured (Mac dev / no public endpoint).
func (t *tunnelPeerSyncer) SyncPeers(ctx context.Context) error {
	if t == nil || t.tunnel == nil {
		return nil
	}
	configs, err := loadPeerConfigs(ctx, t.peers)
	if err != nil {
		t.logger.Error("load peers for tunnel sync", slog.String("err", err.Error()))
		return err
	}
	if err := t.tunnel.SetPeers(ctx, configs); err != nil {
		t.logger.Error("apply peers to tunnel", slog.String("err", err.Error()))
		return err
	}
	return nil
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
	// The same gateway/MCP servers also serve their unix sockets (for forks).
	//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
	g.Add(httpServeActor(logger, "gateway-unix", s.gatewaySrv, s.gatewayUnixLn, "unix:"+s.gatewayUnix))
	//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
	g.Add(httpServeActor(logger, "mcp-unix", s.mcpSrv, s.mcpUnixLn, "unix:"+s.mcpUnix))
	if s.remoteSrv != nil {
		//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
		g.Add(httpServeActor(logger, "remote-api", s.remoteSrv, s.remoteLn, "http://"+s.remoteAddr))
	}
	g.Add(supervisorActor(ctx, s.supervisor))
	if s.tunnel != nil {
		g.Add(tunnelActor(ctx, logger, s.tunnel))
	}
	g.Add(signalActor(ctx))
	return g.Run()
}

// tunnelActor keeps the WireGuard interface alive until the run group
// shuts down; on interrupt it tears the interface back down so the
// kernel doesn't keep an orphaned link around between restarts.
func tunnelActor(ctx context.Context, logger *slog.Logger, t wireguard.Tunnel) (func() error, func(error)) {
	done := make(chan struct{})
	return func() error {
			<-done
			return nil
		}, func(error) {
			if err := t.Stop(); err != nil {
				logger.Warn("stop wireguard tunnel", slog.String("err", err.Error()))
			}
			close(done)
			_ = ctx // shutdown is driven by run.Group interrupt; ctx is unused here
		}
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
// file left behind by a previous crash. The socket is chmod'd to 0660 so the
// owning user and members of the owning group can talk to the daemon. Under
// systemd the daemon runs as fletcher:fletcher, so granting the group access
// is what lets an operator added to the fletcher group reach the socket - a
// 0600 socket would deny every group member regardless of membership, since
// connect() on a Unix socket needs write permission on the socket inode.
func listenUnix(ctx context.Context, socketPath string) (net.Listener, error) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o660); err != nil { //nolint:gosec // 0660 is deliberate: group members (the operator) must reach the socket
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
		api.NewAdminService(startedAt, deps.peers), interceptors,
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
		api.NewPeersService(deps.peers, deps.serverKey, deps.peerSync), interceptors,
	)
	mux.Handle(peersPath, peersHandler)

	settingsPath, settingsHandler := fletcherv1connect.NewSettingsServiceHandler(
		api.NewSettingsService(deps.settings), interceptors,
	)
	mux.Handle(settingsPath, settingsHandler)

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
	case "ext4":
		// The Firecracker rootfs substrate: per-job ext4 image clones. Shares
		// the btrfs root so clones are cheap reflinks (a full copy elsewhere).
		root := cfg.BtrfsRoot
		if root == "" {
			root = filepath.Join(filepath.Dir(cfg.DatabasePath), "snapshots")
		}
		return ext4driver.New(ext4driver.Options{RootDir: root})
	default:
		return nil, fmt.Errorf("unknown snapshot kind %q", cfg.SnapshotKind)
	}
}

// applySettings overlays the stored runtime settings onto cfg, so an operator's
// `fletcher settings set` overrides the flag/env default. Bootstrap config
// (database, socket, age key, listen addresses) is not settable and untouched.
func applySettings(ctx context.Context, cfg *Config, store *settings.Store, logger *slog.Logger) error {
	vals, err := store.Values(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	for k, v := range vals {
		switch k {
		case settings.KeyRuntime:
			cfg.RuntimeKind = v
		case settings.KeySnapshot:
			cfg.SnapshotKind = v
		case settings.KeyBtrfsRoot:
			cfg.BtrfsRoot = v
		case settings.KeyPublicEndpoint:
			cfg.PublicEndpoint = v
		case settings.KeyWireGuardPort:
			if n, perr := strconv.Atoi(v); perr == nil {
				cfg.WireGuardListenPort = n
			}
		case settings.KeyLogLevel:
			cfg.LogLevel = v
		case settings.KeyCredentialsDir:
			cfg.CredentialsDir = v
		default:
			continue // unknown key persisted by an older/newer version; ignore
		}
		logger.Info("applied setting", slog.String("key", k), slog.String("value", v))
	}
	return nil
}

// remoteAPIPort is the TCP port the daemon exposes its Connect API on, bound to
// the WireGuard tunnel interface for paired clients.
const remoteAPIPort = 11700

// remoteAPIAddr is the tunnel-side host:port for the network API: the WireGuard
// server tunnel IP (the .1 of the peer subnet) so only tunnel peers can reach
// it. Falls back to loopback if the subnet cannot be parsed.
func remoteAPIAddr() string {
	addr, err := serverTunnelAddress(peer.DefaultTunnelCIDR)
	if err != nil {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(remoteAPIPort))
	}
	ip, _, _ := strings.Cut(addr, "/")
	return net.JoinHostPort(ip, strconv.Itoa(remoteAPIPort))
}

// newRemoteServer builds the network-API http.Server (the same handlers as the
// unix socket, gated by a per-peer token), or nil when there is no listener.
func newRemoteServer(remoteLn net.Listener, auth tokenAuthenticator, handler http.Handler) *http.Server {
	if remoteLn == nil {
		return nil
	}
	return &http.Server{
		Handler:           authMiddleware(auth, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// listenRemoteAPI binds the tunnel-side TCP listener for the network API, or
// returns nil (logged) when the tunnel is down or the bind fails - the daemon
// still serves the local unix socket.
func listenRemoteAPI(ctx context.Context, netSetup *networkSetup, addr string, logger *slog.Logger) net.Listener {
	if netSetup.Tunnel == nil {
		return nil
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		logger.Warn("remote API listener not started", slog.String("addr", addr), slog.String("err", err.Error()))
		return nil
	}
	return ln
}

// tokenAuthenticator verifies that a bearer token belongs to a paired peer.
type tokenAuthenticator interface {
	AuthenticateToken(ctx context.Context, token string) (peer.Peer, error)
}

// authMiddleware requires a valid per-peer bearer token before passing the
// request to next. Used only on the network-exposed remote listener; the local
// unix socket is file-permission gated and stays auth-free.
func authMiddleware(auth tokenAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := auth.AuthenticateToken(r.Context(), bearerToken(r.Header.Get("Authorization"))); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) string {
	if after, ok := strings.CutPrefix(header, "Bearer "); ok {
		return after
	}
	return ""
}

// gatewaySocketPath and mcpSocketPath are the daemon-side unix sockets the
// gateway and MCP also listen on (next to the Connect socket), so a fork -
// which has only loopback - can reach them through bind-mounted sockets and
// the in-fork forwarder.
func gatewaySocketPath(cfg Config) string {
	return filepath.Join(filepath.Dir(cfg.SocketPath), "gateway.sock")
}

func mcpSocketPath(cfg Config) string {
	return filepath.Join(filepath.Dir(cfg.SocketPath), "mcp.sock")
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
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve daemon binary for fork forwarder: %w", err)
		}
		return runcdriver.New(runcdriver.Options{
			Binary:          cfg.RuncBinary,
			ForwarderBinary: self,
			Forwards: []runcdriver.Forward{
				{Listen: cfg.GatewayListenAddr, HostSocket: gatewaySocketPath(cfg)},
				{Listen: cfg.MCPListenAddr, HostSocket: mcpSocketPath(cfg)},
			},
		})
	case "firecracker":
		// Extract the bundled VMM (firecracker binary + guest kernel) on first
		// use, then point the driver at the extracted paths.
		stateDir := filepath.Dir(cfg.DatabasePath)
		bundle, err := vmm.Extract(filepath.Join(stateDir, "vmm"))
		if err != nil {
			return nil, fmt.Errorf("extract firecracker VMM: %w", err)
		}
		return firecrackerdriver.New(firecrackerdriver.Options{
			FirecrackerBinary: bundle.FirecrackerPath,
			KernelPath:        bundle.KernelPath,
			RunDir:            filepath.Join(stateDir, "firecracker"),
			// Same loopback services as runc, relayed over vsock instead of a
			// bind-mounted socket: the agent reaches only the daemon, no egress.
			Forwards: []firecrackerdriver.Forward{
				{ListenAddr: cfg.GatewayListenAddr, HostSocket: gatewaySocketPath(cfg)},
				{ListenAddr: cfg.MCPListenAddr, HostSocket: mcpSocketPath(cfg)},
			},
		})
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
