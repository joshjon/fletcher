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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oklog/run"

	"connectrpc.com/connect"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/joshjon/fletcher/internal/api"
	"github.com/joshjon/fletcher/internal/approval"
	"github.com/joshjon/fletcher/internal/audit"
	"github.com/joshjon/fletcher/internal/buildinfo"
	"github.com/joshjon/fletcher/internal/egress"
	"github.com/joshjon/fletcher/internal/events"
	"github.com/joshjon/fletcher/internal/gateway"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
	"github.com/joshjon/fletcher/internal/image"
	"github.com/joshjon/fletcher/internal/job"
	fletchermcp "github.com/joshjon/fletcher/internal/mcp"
	"github.com/joshjon/fletcher/internal/network/pairingtls"
	"github.com/joshjon/fletcher/internal/network/portmap"
	"github.com/joshjon/fletcher/internal/network/wireguard"
	"github.com/joshjon/fletcher/internal/peer"
	"github.com/joshjon/fletcher/internal/report"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver"
	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/vmm"
	runtimemock "github.com/joshjon/fletcher/internal/runtime/mockdriver"
	"github.com/joshjon/fletcher/internal/runtime/runcdriver"

	"github.com/joshjon/fletcher/internal/secrets"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/settings"
	"github.com/joshjon/fletcher/internal/snapshot"
	"github.com/joshjon/fletcher/internal/snapshot/btrfsdriver"
	"github.com/joshjon/fletcher/internal/snapshot/ext4driver"
	snapmock "github.com/joshjon/fletcher/internal/snapshot/mockdriver"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
	"github.com/joshjon/fletcher/internal/volume"
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

// imageUpdateState carries the background image-update check's outcome from Run,
// which launches the check early (before the network setup in buildServices),
// into the admin service. available is whether the registry is ahead of the
// imported template; checked is whether the check has finished, so Health and
// `fletcher doctor` can tell "no update" apart from "not checked yet" and avoid
// a misleading all-clear in the brief window right after a restart.
type imageUpdateState struct {
	available *atomic.Bool
	checked   *atomic.Bool
}

// checkForImageUpdate asks the registry, in the background at boot, whether the
// default image's imported template is older than the registry's current
// version. On a positive result it logs a non-fatal hint and flips available so
// Health (and `fletcher doctor`) can surface it. A local-only image (no recorded
// registry digest) or any registry error leaves available false - the daemon
// must not fail to start because a registry is unreachable. It always marks
// checked when done (success, no-update, or error), since it does not retry
// until the next boot.
func checkForImageUpdate(ctx context.Context, cfg Config, logger *slog.Logger, state imageUpdateState) {
	defer state.checked.Store(true)
	root := cfg.BtrfsRoot
	if root == "" {
		root = filepath.Join(filepath.Dir(cfg.DatabasePath), "snapshots")
	}
	imagesDir := filepath.Join(root, "images")
	available, source, err := image.CheckForUpdate(ctx, imagesDir, cfg.DefaultImage)
	if err != nil {
		logger.Debug("image update check skipped", slog.String("err", err.Error()))
		return
	}
	if !available {
		return
	}
	state.available.Store(true)
	logger.Info("a newer version of the default image is available",
		slog.String("image", cfg.DefaultImage),
		slog.String("source", source),
		slog.String("update", "sudo fletcher image update"),
	)
}

// Config holds boot-time daemon settings.
type Config struct {
	SocketPath        string
	DatabasePath      string
	LogLevel          string
	GatewayListenAddr string
	MCPListenAddr     string
	ProxyListenAddr   string
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
	// PairingPort is the public TCP port the pairing listener binds for
	// CompletePair over TLS (the pre-tunnel bootstrap native clients use).
	// Defaults to defaultPairingPort; UPnP forwards the same port.
	// Settings-managed (pairing_port).
	PairingPort int
	// DisableUPnP turns off the automatic router-port-forward attempt
	// at startup. Useful when running behind a router known to mishandle
	// UPnP, or in test environments. Default false (UPnP enabled).
	DisableUPnP bool
	// RemoteAPIListen, when set, binds the token-gated remote API to an extra
	// address beyond the WireGuard tunnel IP - typically the box's address on a
	// VPN the operator already runs (e.g. a Tailscale IP, "100.x.y.z:11700").
	// This is the daemon half of Mode B: a client reaches the API over that VPN
	// instead of Fletcher's own tunnel, so it never has to stand up a second VPN
	// (iOS allows only one active). Empty disables it and the default stays
	// tunnel-only. The bind retries because the VPN interface can come up after
	// the daemon; per-peer token auth applies exactly as on the tunnel listener.
	RemoteAPIListen string
	// SessionIdleTimeout auto-stops a session idle (no work in flight) this
	// long; 0 disables the reaper. Settings-managed (session_idle_timeout).
	SessionIdleTimeout time.Duration
	// SessionMaxCount caps the number of sessions; 0 disables the cap.
	SessionMaxCount int
	// SessionMaxDiskGB caps total session disk in GB; 0 disables the cap.
	SessionMaxDiskGB int
	// DefaultImage is the base image used by job/session create when --image is
	// omitted; empty makes --image required. Settings-managed (default_image).
	DefaultImage string
	// DefaultEgressPolicy is the fork egress policy used when a job/session is
	// created without an explicit --egress: "none" | "allowlist" | "open".
	// Settings-managed (default_egress_policy); empty resolves to "allowlist".
	DefaultEgressPolicy string
	// VMMemoryMB is the guest memory (MB) for each job/session microVM.
	// Settings-managed (vm_memory_mb); 512 is too small for an interactive agent.
	VMMemoryMB int
	// DefaultGateway is the model-gateway wiring used when a job/session is
	// created without an explicit --gateway: "on" injects the gateway env, "off"
	// omits it. Settings-managed (default_gateway); empty resolves to "on".
	DefaultGateway string
	// PublicWeb enables the public HTTPS listener that serves `session publish
	// --public` ports on the internet (binds 443/80). Settings-managed
	// (public_web); default false - the new public attack surface is opt-in.
	PublicWeb bool
	// ACMEStaging uses Let's Encrypt's staging CA for public certs (untrusted,
	// no rate limits - for testing). Settings-managed (acme_staging).
	ACMEStaging bool
	// ACMEEmail is the optional contact email for the ACME account.
	// Settings-managed (acme_email).
	ACMEEmail string
	// APNs config for pushing approval notifications to the iOS app. The daemon
	// pushes directly to Apple with the operator's own key; empty APNSKeyPath
	// disables push. All settings-managed (apns_*).
	APNSKeyPath string // path to the APNs auth key (.p8)
	APNSKeyID   string // the key's ID (Apple Developer)
	APNSTeamID  string // the Apple Developer team ID
	APNSTopic   string // the app's bundle ID (apns-topic)
	APNSSandbox bool   // use Apple's sandbox APNs host (apns_environment=sandbox)
}

// Defaults for the session settings, applied when not explicitly set.
const (
	defaultSessionIdleTimeout = 30 * time.Minute
	defaultSessionMaxCount    = 10
	defaultSessionMaxDiskGB   = 50
	defaultDefaultImage       = "fletcher-base"
	defaultDefaultAgent       = "pi"
	defaultEgressPolicy       = egress.PolicyAllowlist
	// defaultVMMemoryMB is the per-microVM guest memory. An interactive agent
	// (Claude Code, Opus, large context) needs well above the old 512 MB - that
	// OOM-killed the TUI; 2 GB gives comfortable headroom.
	defaultVMMemoryMB = 2048
	defaultGatewayOn  = "on"
)

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
	// Snapshot the flag/env config before the settings overlay so ReloadSettings
	// can re-derive the current effective config the same way boot does.
	flagCfg := cfg
	if err := applySettings(ctx, &cfg, settings.NewStore(queries), logger); err != nil {
		return err
	}
	logger = newLogger(cfg.LogLevel) // reflect a log_level setting

	// Launch the image-update check here, before buildServices does the slower
	// network setup, so its result is usually ready by the first `fletcher
	// doctor` after a restart. Run owns the atomics; buildServices wires them
	// into the admin service so Health reflects them.
	imageUpdate := imageUpdateState{available: &atomic.Bool{}, checked: &atomic.Bool{}}
	go checkForImageUpdate(ctx, cfg, logger, imageUpdate)

	svcs, err := buildServices(ctx, cfg, flagCfg, queries, logger, imageUpdate)
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
	cfg             Config
	supervisor      *job.Supervisor
	sessions        *session.Manager
	notify          notifyRouter
	portBroker      *session.Broker
	portMapper      *portmap.Mapper
	publicWeb       *publicWebServers
	connectSrv      *http.Server
	gatewaySrv      *http.Server
	mcpSrv          *http.Server
	connectLn       net.Listener
	gatewayLn       net.Listener
	mcpLn           net.Listener
	gatewayUnixLn   net.Listener
	mcpUnixLn       net.Listener
	proxySrv        *http.Server
	proxyUnixLn     net.Listener
	proxyUnix       string
	proxyOpenSrv    *http.Server
	proxyOpenUnixLn net.Listener
	proxyOpenUnix   string
	remoteSrv       *http.Server
	remoteLn        net.Listener
	remoteAddr      string
	vpnAPISrv       *http.Server
	vpnAPIAddr      string
	pairingSrv      *http.Server
	pairingLn       net.Listener
	pairingAddr     string
	gatewayUnix     string
	mcpUnix         string
	gatewayBaseURL  string
	mcpBaseURL      string
	tunnel          wireguard.Tunnel
}

//nolint:funlen // the single construction hub that wires every subsystem; splitting further would scatter the boot sequence
func buildServices(ctx context.Context, cfg, flagCfg Config, queries *sqliteq.Queries, logger *slog.Logger, imageUpdate imageUpdateState) (*services, error) {
	connectLn, err := listenUnix(ctx, cfg.SocketPath)
	if err != nil {
		return nil, err
	}

	secretsStore, err := secrets.Open(queries, cfg.AgeIdentityPath)
	if err != nil {
		return nil, fmt.Errorf("open secrets store: %w", err)
	}

	// With no explicit choice, pick the runtime by capability: Firecracker on a
	// KVM host with the VMM bundled, otherwise mock. runc stays an explicit
	// opt-in (it needs a provisioned btrfs root).
	resolveRuntimeDefaults(&cfg, logger)

	snapDriver, err := buildSnapshotDriver(cfg)
	if err != nil {
		return nil, fmt.Errorf("init snapshot driver: %w", err)
	}
	rtDriver, err := buildRuntimeDriver(cfg, logger)
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

	// Egress forward-proxy: a fork with egress points HTTP_PROXY/HTTPS_PROXY at
	// the in-fork forwarder, which relays to this unix socket. The proxy gates
	// each connection on the policy + the netguard LAN guard. B2 enforces a
	// single global allowlist; Phase B3 makes it per-job.
	proxyUnix := proxySocketPath(cfg)
	proxyUnixLn, err := listenUnix(ctx, proxyUnix)
	if err != nil {
		return nil, fmt.Errorf("egress proxy unix listener: %w", err)
	}
	egressProxy := egress.New(egress.NewAllowlist(defaultEgressAllowlist), logger)
	proxyOpenUnix := proxyOpenSocketPath(cfg)
	proxyOpenUnixLn, err := listenUnix(ctx, proxyOpenUnix)
	if err != nil {
		return nil, fmt.Errorf("egress open proxy unix listener: %w", err)
	}
	egressOpenProxy := egress.New(egress.Open{}, logger)
	logger.Info("egress proxies ready",
		slog.Int("allowlist_size", len(defaultEgressAllowlist)),
		slog.String("default_policy", egressDefaultPolicy(cfg)),
	)

	// In-process event bus: lifecycle changes fan out to WatchEvents clients
	// so they update live instead of polling.
	eventBus := events.NewBus()

	deviceTokens := deviceTokenStore{q: queries}
	apnsSender := buildAPNSSender(cfg, logger)
	approvalSvc := approval.NewService(queries, approval.ServiceOptions{
		Notifier: approvalNotifier{
			store:    deviceTokens,
			sender:   apnsSender,
			settings: settings.NewStore(queries),
			logger:   logger,
		},
		Events: eventBus,
	})
	apiEndpoint := remoteAPIAddr()
	peerSvc := peer.NewService(queries, peer.Options{
		PublicEndpoint:    cfg.PublicEndpoint,
		APIEndpoint:       apiEndpoint,
		RemoteAPIEndpoint: cfg.RemoteAPIListen,
	})
	wgKeyProvider := newServerKeyProvider(secretsStore)

	// One Mapper owns every router port-forward: it installs them via
	// NAT-PMP/UPnP, refreshes them so they never lapse, and releases them on
	// shutdown so Fletcher leaves no stale forwards behind.
	portMapper := portmap.NewMapper(logger)

	netSetup, err := bringUpNetwork(ctx, cfg, logger, peerSvc, wgKeyProvider, portMapper)
	if err != nil {
		return nil, fmt.Errorf("bring up network: %w", err)
	}

	// Expose the Connect API on the tunnel interface so paired clients can
	// drive the daemon. Requires a per-peer token (the unix socket stays
	// local + auth-free). Best-effort: nil when the tunnel is not up.
	remoteLn := listenRemoteAPI(ctx, netSetup, apiEndpoint, logger)

	// Tool registration happens once the session manager exists below (the
	// publish_image tool commits session forks); the server starts serving later.
	mcpServer := fletchermcp.NewServer("fletcher", buildinfo.Version, auditRecorder, logger)
	logger.Info("mcp server ready", slog.String("base_url", mcpURL))

	// Env every agent inherits, split so a fork can opt out of the model gateway
	// (DESIGN.md §6). baseAgentEnv (MCP + egress proxy) is always injected;
	// gatewayAgentEnv (the model-gateway base-URLs + placeholder keys) is added
	// only when the fork's gateway toggle is on. A gateway-off fork uses its own
	// auth (e.g. a subscription login) and reaches providers via egress. Shared
	// by ephemeral jobs and durable sessions (one environment, §4).
	proxyURL := "http://" + proxyListenAddr(cfg)
	baseAgentEnv := []string{
		"FLETCHER_MCP_URL=" + mcpURL,
		// Fork egress: agents' HTTP(S) clients (npm, pip, git, curl, WebFetch,
		// and Claude Code's startup connectivity check) route through the daemon
		// forward-proxy, which enforces the egress policy + LAN guard. Loopback
		// stays direct (NO_PROXY) so the gateway/MCP calls are not proxied. Both
		// cases set because tools vary on which they read.
		"HTTP_PROXY=" + proxyURL,
		"HTTPS_PROXY=" + proxyURL,
		"NO_PROXY=127.0.0.1,localhost,::1",
		"http_proxy=" + proxyURL,
		"https_proxy=" + proxyURL,
		"no_proxy=127.0.0.1,localhost,::1",
	}
	gatewayAgentEnv := []string{
		// OpenAI-compatible path (Codex, Aider, OpenHands, pi). The
		// gateway's /v1/chat/completions handler translates to Anthropic.
		"OPENAI_BASE_URL=" + gatewayURL + "/v1",
		"OPENAI_API_KEY=fletcher-gateway", // placeholder; real key lives in secrets store
		// Anthropic-native path (Claude Code). The gateway's /v1/messages
		// handler proxies the raw Messages request to api.anthropic.com.
		"ANTHROPIC_BASE_URL=" + gatewayURL,
		"ANTHROPIC_API_KEY=fletcher-gateway", // placeholder; real key lives in secrets store
		// Model catalog (Phase 14) - pi-extension and other agents fetch
		// this on startup to discover providers without per-job config.
		"FLETCHER_CATALOG_URL=" + gatewayURL + "/v1/catalog.json",
	}

	supervisor := job.NewSupervisor(queries, rtDriver, snapDriver, logger, job.SupervisorOptions{
		JobEnv:          baseAgentEnv,
		JobGatewayEnv:   gatewayAgentEnv,
		DefaultGateway:  defaultGateway(cfg),
		CredentialsRoot: cfg.CredentialsDir,
	})

	// Durable sessions reuse the runtime + snapshot drivers. The session-capable
	// runtime (firecracker) implements runtime.SessionRuntime; other runtimes
	// leave it nil and session lifecycle calls return a clear error.
	sessionRuntime, _ := rtDriver.(runtime.SessionRuntime)
	sessionMgr := session.NewManager(queries, snapDriver, sessionRuntime, baseAgentEnv, gatewayAgentEnv, logger, session.Options{
		IdleTimeout:         cfg.SessionIdleTimeout,
		MaxCount:            cfg.SessionMaxCount,
		MaxDiskBytes:        int64(cfg.SessionMaxDiskGB) << 30,
		DefaultImage:        cfg.DefaultImage,
		DefaultEgressPolicy: egressDefaultPolicy(cfg),
		DefaultGateway:      defaultGateway(cfg),
		PublicWeb:           cfg.PublicWeb,
		ImagesDir:           filepath.Join(snapshotRootDir(cfg), "images"),
		CredentialsRoot:     cfg.CredentialsDir,
	})
	if err := sessionMgr.ReconcileOnBoot(ctx); err != nil {
		return nil, fmt.Errorf("reconcile sessions: %w", err)
	}

	// Persistent volumes (Milestone 12): first-class disks attached to sessions
	// as a second drive, outliving forks and redeploys. The provisioner is the
	// snapshot driver's optional capability (ext4 only today).
	volumeProvisioner, _ := snapDriver.(snapshot.VolumeProvisioner)
	volumeMgr := volume.NewManager(queries, volumeProvisioner, logger)
	sessionMgr.SetVolumes(volumeMgr)
	sessionMgr.SetEvents(eventBus)
	supervisor.SetEvents(eventBus)

	// publish_image backend: agents commit their session's fork (or import a
	// registry ref) as a template, behind an approval. Only meaningful on a
	// session-capable runtime; elsewhere the tool is not registered.
	var publisher fletchermcp.ImagePublisher
	if sessionRuntime != nil {
		publisher = &imagePublisher{
			sessions:  sessionMgr,
			imagesDir: filepath.Join(snapshotRootDir(cfg), "images"),
			format:    driverKind(cfg.SnapshotKind),
		}
	}
	// Reports: structured results agents post; stored, event-published, and
	// pushed (the surviving half of the inbox idea).
	reportSvc := report.NewService(queries, eventBus)
	fletchermcp.RegisterBuiltinTools(mcpServer, startedAt, fletchermcp.NewEgressHTTPClient(30*time.Second), approvalSvc, publisher,
		&reportPublisher{reports: reportSvc, sessions: sessionMgr})

	// Published-port broker: forwards a session's published port to the service
	// inside its VM, dialing in via the session manager over vsock so the VM
	// stays unroutable (the preview-proxy pattern, for an arbitrary port). It
	// binds the WireGuard tunnel IP, so published ports are reachable by paired
	// clients over the tunnel; it is inert until the tunnel is up. Phase 2 adds
	// the public frontend on top.
	brokerTunnelIP := ""
	if netSetup.Tunnel != nil {
		if host, _, herr := net.SplitHostPort(apiEndpoint); herr == nil {
			brokerTunnelIP = host
		}
	}
	portBroker := session.NewBroker(ctx, brokerTunnelIP, sessionMgr.DialPort, logger)
	sessionMgr.SetBroker(portBroker)
	if err := sessionMgr.ReconcilePorts(ctx); err != nil {
		return nil, fmt.Errorf("reconcile published ports: %w", err)
	}
	// Bring deployed app sessions (created --app) back up after a restart, in the
	// background so booting their VMs does not delay daemon startup.
	go sessionMgr.StartDeployedOnBoot(ctx)

	// Public web (Milestone 8 Phase 2): serve `session publish --public` ports on
	// the internet over HTTPS, certmagic terminating TLS and reverse-proxying into
	// the VM over vsock. Opt-in (the new public attack surface), and best-effort:
	// if 443/80 cannot be bound (no CAP_NET_BIND_SERVICE) the daemon still runs,
	// just without public serving.
	publicWeb := buildPublicWeb(ctx, cfg, sessionMgr, portMapper, logger)

	var certStatus api.CertStatusResolver
	if publicWeb != nil {
		certStatus = publicWeb.pub
	}
	jobSvc := job.NewService(queries, supervisor, cfg.DefaultImage, egressDefaultPolicy(cfg), defaultGateway(cfg))
	reloader := &settingsReloader{
		flagCfg:       flagCfg,
		bootEffective: settingsDefaults(cfg),
		store:         settings.NewStore(queries),
		sessions:      sessionMgr,
		jobs:          jobSvc,
		logger:        logger,
	}

	connectDeps := connectDeps{
		jobs:             jobSvc,
		sessions:         sessionMgr,
		volumes:          volumeMgr,
		credentials:      sessionMgr,
		events:           eventBus,
		reports:          reportSvc,
		publicIP:         publicEndpointHost(netSetup.EffectivePublicEndpoint),
		imagesDir:        filepath.Join(snapshotRootDir(cfg), "images"),
		snapshotKind:     driverKind(cfg.SnapshotKind),
		imageBuilder:     sessionMgr,
		secrets:          secretsStore,
		approvals:        approvalSvc,
		push:             deviceTokens,
		peers:            peerSvc,
		serverKey:        wgKeyProvider,
		models:           gatewayCatalog{baseURL: gatewayURL},
		peerSync:         &tunnelPeerSyncer{peers: peerSvc, tunnel: netSetup.Tunnel, logger: logger},
		settings:         settings.NewStore(queries),
		settingsDefaults: settingsDefaults(cfg),
		settingsReloader: reloader,
		certStatus:       certStatus,
		runtimeStatus: api.RuntimeStatus{
			Runtime:            driverKind(cfg.RuntimeKind),
			Snapshot:           driverKind(cfg.SnapshotKind),
			BaseImageAvailable: baseImageAvailable(cfg),
			BaseImageUpdate:    imageUpdate.available,
			BaseImageChecked:   imageUpdate.checked,
		},
	}

	connectSrv := newHTTPServer(startedAt.Unix(), connectDeps, logger)
	remoteSrv := newRemoteServer(remoteLn, peerSvc, connectSrv.Handler)

	// Mode B (BYO-VPN): expose the same token-gated API on an operator-run VPN
	// address (e.g. a Tailscale IP) so a client can reach the box without
	// standing up Fletcher's own tunnel. Independent of remoteSrv - it comes up
	// even when there is no Fletcher tunnel.
	var vpnAPISrv *http.Server
	if cfg.RemoteAPIListen != "" {
		vpnAPISrv = newRemoteAPIServer(peerSvc, connectSrv.Handler)
	}

	// Public pairing listener: the pre-tunnel bootstrap native clients use.
	// CompletePair cannot travel over the tunnel it is trying to establish
	// (the daemon only learns the client key at CompletePair), so it is
	// exposed here over TLS with a pinned self-signed cert, gated by the
	// one-time pairing code. Only meaningful when the tunnel is up.
	pairingSrv, pairingLn, pairingAddr := buildPairingListener(ctx, cfg, netSetup, peerSvc, connectDeps, portMapper, logger)

	return &services{
		cfg:        cfg,
		supervisor: supervisor,
		sessions:   sessionMgr,
		notify: notifyRouter{
			bus:      eventBus,
			store:    deviceTokens,
			sender:   apnsSender,
			settings: settings.NewStore(queries),
			reports:  reportSvc,
			logger:   logger,
		},
		portBroker:      portBroker,
		portMapper:      portMapper,
		publicWeb:       publicWeb,
		connectSrv:      connectSrv,
		gatewaySrv:      newGatewayHTTPServer(gw),
		mcpSrv:          newMCPHTTPServer(mcpServer),
		connectLn:       connectLn,
		gatewayLn:       gatewayLn,
		mcpLn:           mcpLn,
		gatewayUnixLn:   gatewayUnixLn,
		mcpUnixLn:       mcpUnixLn,
		proxySrv:        newProxyHTTPServer(egressProxy),
		proxyUnixLn:     proxyUnixLn,
		proxyUnix:       proxyUnix,
		proxyOpenSrv:    newProxyHTTPServer(egressOpenProxy),
		proxyOpenUnixLn: proxyOpenUnixLn,
		proxyOpenUnix:   proxyOpenUnix,
		remoteSrv:       remoteSrv,
		remoteLn:        remoteLn,
		remoteAddr:      apiEndpoint,
		vpnAPISrv:       vpnAPISrv,
		vpnAPIAddr:      cfg.RemoteAPIListen,
		pairingSrv:      pairingSrv,
		pairingLn:       pairingLn,
		pairingAddr:     pairingAddr,
		gatewayUnix:     gatewayUnix,
		mcpUnix:         mcpUnix,
		gatewayBaseURL:  gatewayURL,
		mcpBaseURL:      mcpURL,
		tunnel:          netSetup.Tunnel,
	}, nil
}

// connectDeps bundles the backends newHTTPServer wires onto the Connect
// mux. Grouping them in a struct keeps newHTTPServer's signature tight
// as more services land.
type connectDeps struct {
	jobs        api.JobsBackend
	sessions    api.SessionsBackend
	volumes     api.VolumesBackend
	credentials api.CredentialsBackend
	events      *events.Bus
	reports     api.ReportsBackend
	secrets     api.SecretsBackend
	approvals   api.ApprovalsBackend
	push        api.PushBackend
	peers       api.PeersBackend
	serverKey   api.ServerKeyProvider
	models      api.CatalogBuilder
	peerSync    api.PeerSyncer
	settings    api.SettingsBackend
	// publicIP is the daemon's discovered public IP (host of the effective public
	// endpoint), passed to the sessions service for --public DNS guidance.
	publicIP string
	// imagesDir and snapshotKind let the image service import registry images
	// server-side into the daemon's snapshot root.
	imagesDir    string
	snapshotKind string
	// imageBuilder builds a session's project Dockerfile into a template (M19).
	imageBuilder api.ImageBuilder
	// settingsDefaults maps each setting key to the daemon's resolved default,
	// so `fletcher settings list` shows the effective value, not just "(default)".
	settingsDefaults map[string]string
	// settingsReloader live-applies the reloadable settings (ReloadSettings).
	settingsReloader api.SettingsReloader
	// certStatus reports public-port TLS cert state for ListPorts; nil when
	// public web is off.
	certStatus api.CertStatusResolver
	// runtimeStatus is the effective runtime config surfaced via Health for
	// `fletcher doctor`.
	runtimeStatus api.RuntimeStatus
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
	//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
	g.Add(httpServeActor(logger, "egress-unix", s.proxySrv, s.proxyUnixLn, "unix:"+s.proxyUnix))
	//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
	g.Add(httpServeActor(logger, "egress-open-unix", s.proxyOpenSrv, s.proxyOpenUnixLn, "unix:"+s.proxyOpenUnix))
	if s.remoteSrv != nil {
		//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
		g.Add(httpServeActor(logger, "remote-api", s.remoteSrv, s.remoteLn, "http://"+s.remoteAddr))
	}
	if s.vpnAPISrv != nil {
		g.Add(remoteAPIListenActor(ctx, s.vpnAPIAddr, s.vpnAPISrv, logger))
	}
	if s.pairingSrv != nil {
		//nolint:contextcheck // same: shutdown must outlive the cancelled parent ctx
		g.Add(tlsServeActor(logger, "pairing", s.pairingSrv, s.pairingLn, "https://"+s.pairingAddr))
	}
	g.Add(supervisorActor(ctx, s.supervisor))
	// Always on: ReapIdle no-ops when the idle timeout is 0, and the same tick
	// drives the deploy-health sweep.
	g.Add(sessionReaperActor(ctx, logger, s.sessions, s.cfg.SessionIdleTimeout))
	g.Add(notifyRouterActor(ctx, s.notify))
	if s.tunnel != nil {
		g.Add(tunnelActor(ctx, logger, s.tunnel))
	}
	if s.portMapper != nil && !s.cfg.DisableUPnP {
		g.Add(portMapperActor(ctx, s.portMapper))
	}
	if s.portBroker != nil {
		g.Add(portBrokerActor(s.portBroker))
	}
	if s.publicWeb != nil {
		//nolint:contextcheck // shutdown must outlive the cancelled parent ctx
		g.Add(tlsServeActor(logger, "public-https", s.publicWeb.httpsSrv, s.publicWeb.httpsLn, "https://"+publicHTTPSAddr))
		//nolint:contextcheck // same
		g.Add(httpServeActor(logger, "public-http", s.publicWeb.httpSrv, s.publicWeb.httpLn, "http://"+publicHTTPAddr))
	}
	g.Add(signalActor(ctx))
	return g.Run()
}

// notifyRouterActor runs the push-notification router until shutdown.
func notifyRouterActor(ctx context.Context, r notifyRouter) (func() error, func(error)) {
	runCtx, cancel := context.WithCancel(ctx)
	return func() error {
			r.run(runCtx)
			return nil
		}, func(error) {
			cancel()
		}
}

// sessionReaperActor periodically hibernates idle sessions (no work in flight)
// to reclaim host RAM and sweeps deploy health on the same tick. The tick is
// half the idle timeout capped to a sane range, so a session is stopped within
// roughly one timeout of going idle.
func sessionReaperActor(ctx context.Context, logger *slog.Logger, mgr *session.Manager, idleTimeout time.Duration) (func() error, func(error)) {
	interval := idleTimeout / 2
	if interval < time.Minute {
		interval = time.Minute
	}
	if interval > 10*time.Minute {
		interval = 10 * time.Minute
	}
	reapCtx, cancel := context.WithCancel(ctx)
	return func() error {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-reapCtx.Done():
					return nil
				case <-ticker.C:
					if _, err := mgr.ReapIdle(reapCtx); err != nil {
						logger.Warn("session idle reaper", slog.String("err", err.Error()))
					}
					// Same cadence: spot crash-looping deploys (publishes a
					// "crash-looping" event the notify router pushes).
					mgr.SweepDeployHealth(reapCtx)
				}
			}
		}, func(error) {
			cancel()
		}
}

// portBrokerActor keeps the published-port forwarders alive until shutdown,
// then closes every listener so the daemon does not leave them bound.
func portBrokerActor(b *session.Broker) (func() error, func(error)) {
	done := make(chan struct{})
	return func() error {
			<-done
			return nil
		}, func(error) {
			b.CloseAll()
			close(done)
		}
}

// Public web listen addresses. Bound on all interfaces so the box is the public
// edge (UPnP forwards the same ports); binding 80/443 needs CAP_NET_BIND_SERVICE.
const (
	publicHTTPSAddr = ":443"
	publicHTTPAddr  = ":80"
	publicHTTPSPort = 443
	publicHTTPPort  = 80
)

// publicWebServers holds the public HTTPS (TLS) and HTTP (ACME + redirect)
// servers and their listeners.
type publicWebServers struct {
	httpsSrv *http.Server
	httpSrv  *http.Server
	httpsLn  net.Listener
	httpLn   net.Listener
	// pub backs the listeners; held so the sessions service can read per-host
	// TLS cert status for ListPorts.
	pub *session.PublicServer
}

// buildPublicWeb constructs the public web servers when public_web is enabled.
// Returns nil when disabled, or when 443/80 cannot be bound (logged, non-fatal:
// the daemon still serves everything else) - the usual cause is a missing
// CAP_NET_BIND_SERVICE.
func buildPublicWeb(ctx context.Context, cfg Config, mgr *session.Manager, mapper *portmap.Mapper, logger *slog.Logger) *publicWebServers {
	if !cfg.PublicWeb {
		return nil
	}
	httpsLn, _, herr := listenTCP(ctx, publicHTTPSAddr, "public-https")
	if herr != nil {
		logger.Error("public web disabled: cannot bind 443 (grant CAP_NET_BIND_SERVICE?)", slog.String("err", herr.Error()))
		return nil
	}
	httpLn, _, herr := listenTCP(ctx, publicHTTPAddr, "public-http")
	if herr != nil {
		logger.Error("public web disabled: cannot bind 80 (grant CAP_NET_BIND_SERVICE?)", slog.String("err", herr.Error()))
		_ = httpsLn.Close()
		return nil
	}
	pub := session.NewPublicServer(session.PublicConfig{
		Backend: mgr,
		Logger:  logger,
		CertDir: filepath.Join(filepath.Dir(cfg.DatabasePath), "certmagic"),
		Email:   cfg.ACMEEmail,
		Staging: cfg.ACMEStaging,
	})
	// Forward the public ports on the router so the box is reachable from the
	// internet; best-effort (the operator may have forwarded them manually).
	mapTCPPort(ctx, mapper, publicHTTPSPort, "Fletcher (public web HTTPS)")
	mapTCPPort(ctx, mapper, publicHTTPPort, "Fletcher (public web HTTP)")
	logger.Info("public web serving enabled",
		slog.String("https", publicHTTPSAddr),
		slog.String("http", publicHTTPAddr),
		slog.Bool("acme_staging", cfg.ACMEStaging))
	return &publicWebServers{
		httpsSrv: &http.Server{Handler: pub.HTTPSHandler(), TLSConfig: pub.TLSConfig(), ReadHeaderTimeout: 10 * time.Second},
		httpSrv:  &http.Server{Handler: pub.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second},
		httpsLn:  httpsLn,
		httpLn:   httpLn,
		pub:      pub,
	}
}

// tlsServeActor serves an HTTPS server (TLS via the server's TLSConfig, e.g.
// certmagic's on-demand certs) until shutdown.
func tlsServeActor(logger *slog.Logger, role string, srv *http.Server, ln net.Listener, baseURL string) (func() error, func(error)) {
	execute := func() error {
		logger.Info(role+" listening", slog.String("base_url", baseURL))
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// portMapperActor refreshes the router port-forwards on a timer until the
// run group shuts down, then releases them so Fletcher does not leave stale
// forwards on the router (addressing the "what if I forget to remove these"
// worry: Fletcher cleans up its own mappings on exit).
func portMapperActor(ctx context.Context, mapper *portmap.Mapper) (func() error, func(error)) {
	rctx, cancel := context.WithCancel(ctx)
	return func() error {
			return mapper.Run(rctx)
		}, func(error) {
			cancel()
		}
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
		api.NewAdminService(startedAt, deps.peers, deps.runtimeStatus), interceptors,
	)
	mux.Handle(adminPath, adminHandler)

	jobsPath, jobsHandler := fletcherv1connect.NewJobServiceHandler(
		api.NewJobsService(deps.jobs), interceptors,
	)
	mux.Handle(jobsPath, jobsHandler)

	sessionsPath, sessionsHandler := fletcherv1connect.NewSessionServiceHandler(
		api.NewSessionsService(deps.sessions, api.SessionsDeps{
			PublicIP:  deps.publicIP,
			Deploy:    imageDeployResolver{imagesDir: deps.imagesDir},
			Certs:     deps.certStatus,
			Refresher: imageRefresher{imagesDir: deps.imagesDir, logger: logger},
		}), interceptors,
	)
	mux.Handle(sessionsPath, sessionsHandler)

	secretsPath, secretsHandler := fletcherv1connect.NewSecretServiceHandler(
		api.NewSecretsService(deps.secrets), interceptors,
	)
	mux.Handle(secretsPath, secretsHandler)

	approvalsPath, approvalsHandler := fletcherv1connect.NewApprovalServiceHandler(
		api.NewApprovalsService(deps.approvals), interceptors,
	)
	mux.Handle(approvalsPath, approvalsHandler)

	pushPath, pushHandler := fletcherv1connect.NewPushServiceHandler(
		api.NewPushService(deps.push), interceptors,
	)
	mux.Handle(pushPath, pushHandler)

	peersPath, peersHandler := fletcherv1connect.NewPeerServiceHandler(
		api.NewPeersService(deps.peers, deps.serverKey, deps.peerSync), interceptors,
	)
	mux.Handle(peersPath, peersHandler)

	settingsPath, settingsHandler := fletcherv1connect.NewSettingsServiceHandler(
		api.NewSettingsService(deps.settings, deps.settingsDefaults, deps.settingsReloader), interceptors,
	)
	mux.Handle(settingsPath, settingsHandler)

	modelsPath, modelsHandler := fletcherv1connect.NewModelServiceHandler(
		api.NewModelsService(deps.models), interceptors,
	)
	mux.Handle(modelsPath, modelsHandler)

	imagesPath, imagesHandler := fletcherv1connect.NewImageServiceHandler(
		api.NewImagesService(deps.imagesDir, deps.snapshotKind, deps.imageBuilder), interceptors,
	)
	mux.Handle(imagesPath, imagesHandler)

	volumesPath, volumesHandler := fletcherv1connect.NewVolumeServiceHandler(
		api.NewVolumesService(deps.volumes), interceptors,
	)
	mux.Handle(volumesPath, volumesHandler)

	credentialsPath, credentialsHandler := fletcherv1connect.NewCredentialServiceHandler(
		api.NewCredentialsService(deps.credentials), interceptors,
	)
	mux.Handle(credentialsPath, credentialsHandler)

	eventsPath, eventsHandler := fletcherv1connect.NewEventServiceHandler(
		api.NewEventsService(deps.events), interceptors,
	)
	mux.Handle(eventsPath, eventsHandler)

	reportsPath, reportsHandler := fletcherv1connect.NewReportServiceHandler(
		api.NewReportsService(deps.reports), interceptors,
	)
	mux.Handle(reportsPath, reportsHandler)

	// Serve cleartext HTTP/2 (h2c) alongside HTTP/1.1 so Connect bidi streams -
	// the interactive shell (SessionService.ShellSession) - work over the unix
	// socket. Negotiation happens at the connection layer, so HTTP/1.1 unary
	// clients are unaffected.
	return &http.Server{
		Handler:           mux,
		Protocols:         h2cProtocols(),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// h2cProtocols enables HTTP/1.1 and cleartext HTTP/2 (no TLS) on a server or
// transport - the set the unix socket and tunnel API speak.
func h2cProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
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

// pairingProcedure is the single Connect procedure the public pairing
// listener exposes. Everything else 404s so the public surface is exactly
// "complete a pairing with a valid one-time code" and nothing more.
const pairingProcedure = "/fletcher.v1.PeerService/CompletePair"

// pairingCertDir is where the self-signed pairing certificate lives, next
// to the database so it shares the daemon's state directory and backups.
func pairingCertDir(cfg Config) string {
	return filepath.Join(filepath.Dir(cfg.DatabasePath), "pairing")
}

// buildPairingListener brings up the public TLS pairing endpoint when the
// tunnel is up and a public endpoint is known: it loads (or generates) a
// self-signed cert bound to the public endpoint host (so iOS's TLS checks
// pass alongside the app's pin), binds the public TCP port, forwards it via
// UPnP, and wires the peer service to advertise the endpoint and serve the
// cert's live fingerprint. Every failure is non-fatal (logged); the daemon
// still runs, mobile pairing just stays unavailable. Returns nil servers
// when pairing cannot be offered (no tunnel, no public endpoint, cert
// error, or bind failure).
func buildPairingListener(
	ctx context.Context,
	cfg Config,
	netSetup *networkSetup,
	peerSvc *peer.Service,
	deps connectDeps,
	mapper *portmap.Mapper,
	logger *slog.Logger,
) (*http.Server, net.Listener, string) {
	if netSetup.Tunnel == nil {
		// No tunnel means a paired client could not establish WireGuard
		// afterward, so there is nothing to bootstrap.
		return nil, nil, ""
	}
	host := publicEndpointHost(netSetup.EffectivePublicEndpoint)
	if host == "" {
		// The cert's SAN binds to this host, and the client has nowhere to
		// dial without it, so there is no pairing endpoint to offer.
		logger.Warn("pairing listener disabled: no public endpoint to bind the pairing certificate to")
		return nil, nil, ""
	}
	mgr := pairingtls.NewManager(pairingCertDir(cfg), logger)
	if err := mgr.EnsureHost(host); err != nil {
		logger.Error("pairing listener disabled: cannot load TLS cert", slog.String("err", err.Error()))
		return nil, nil, ""
	}
	port := pairingPort(cfg)
	ln, _, err := listenTCP(ctx, fmt.Sprintf("0.0.0.0:%d", port), "pairing")
	if err != nil {
		logger.Error("pairing listener disabled: cannot bind", slog.Int("port", port), slog.String("err", err.Error()))
		return nil, nil, ""
	}
	addr := ln.Addr().String()
	// Advertise only now that the listener is real, then open the router
	// port so the endpoint is reachable from outside the LAN. The manager
	// is also the rotation hook: a later SetPublicEndpoint re-issues the
	// cert for the new host through it.
	peerSvc.SetPairingCert(port, mgr)
	if !cfg.DisableUPnP {
		mapTCPPort(ctx, mapper, port, "Fletcher (iOS pairing)")
	}
	logger.Info("pairing listener ready",
		slog.String("addr", addr),
		slog.String("host", host),
		slog.String("tls_fingerprint", mgr.Fingerprint()))
	return newPairingServer(deps, mgr, logger), ln, addr
}

// newPairingServer builds the TLS-terminated pairing server. It serves
// ONLY CompletePair, authenticated by the one-time pairing code in the
// request body (not a per-peer token - the client has none yet), and
// 404s every other path. The same PeersService backends as the local API
// are reused, so a completed pairing syncs the new peer into the tunnel.
// The cert is served via the manager's GetCertificate, so a rotation takes
// effect on the next handshake without rebuilding the listener.
func newPairingServer(deps connectDeps, mgr *pairingtls.Manager, logger *slog.Logger) *http.Server {
	interceptors := connect.WithInterceptors(
		api.RequestIDInterceptor(),
		api.ErrorInterceptor(logger),
	)
	path, handler := fletcherv1connect.NewPeerServiceHandler(
		api.NewPeersService(deps.peers, deps.serverKey, deps.peerSync), interceptors,
	)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	return &http.Server{
		Handler:           restrictToPairing(mux),
		TLSConfig:         mgr.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// restrictToPairing wraps the PeerService handler so the public listener
// answers only CompletePair and 404s every other path. This is the public
// surface's guard rail: the full PeerService (ListPeers, DeletePeer, ...)
// must never be reachable without a per-peer token, and the pairing
// listener is deliberately token-free.
func restrictToPairing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pairingProcedure {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
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

func newProxyHTTPServer(p *egress.Proxy) *http.Server {
	return &http.Server{
		Handler:           p,
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

// resolveRuntimeDefaults picks the runtime and matching snapshot driver when
// the operator has not chosen one (via flag, env, or `fletcher settings`). On a
// KVM host with the Firecracker VMM bundled it selects the real microVM tier;
// otherwise it falls back to the mock driver, which runs everywhere. runc is
// not auto-selected because it needs a provisioned btrfs snapshot root.
func resolveRuntimeDefaults(cfg *Config, logger *slog.Logger) {
	if cfg.RuntimeKind != "" {
		return // operator chose explicitly
	}
	if kvmUsable() && vmm.Available() {
		cfg.RuntimeKind = "firecracker"
		if cfg.SnapshotKind == "" {
			cfg.SnapshotKind = "ext4"
		}
		logger.Info("runtime auto-selected",
			slog.String("runtime", cfg.RuntimeKind),
			slog.String("snapshot", cfg.SnapshotKind),
			slog.String("reason", "KVM available and VMM bundled"))
		return
	}
	cfg.RuntimeKind = "mock"
	if cfg.SnapshotKind == "" {
		cfg.SnapshotKind = "mock"
	}
	logger.Info("runtime auto-selected",
		slog.String("runtime", cfg.RuntimeKind),
		slog.String("reason", "no usable /dev/kvm or VMM not bundled; set runtime explicitly for runc"))
}

// kvmUsable reports whether the daemon can actually open /dev/kvm for the
// Firecracker runtime (presence plus permission, not just existence).
func kvmUsable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// imageDeployResolver resolves a run_app session's deploy detail (entrypoint,
// EXPOSE port) from its image template's sidecar metadata.
type imageDeployResolver struct{ imagesDir string }

func (r imageDeployResolver) DeployInfo(imageName string) (entrypoint []string, exposedPort int, ok bool) {
	meta, found, err := image.ReadMeta(r.imagesDir, imageName)
	if err != nil || !found {
		return nil, 0, false
	}
	return meta.Entrypoint, meta.ExposedPort, true
}

// imagePublisher adapts the session manager and the server-side registry
// import to the mcp.ImagePublisher the publish_image tool needs. Approval
// gating happens in the tool; this is just the do-it backend.
type imagePublisher struct {
	sessions  *session.Manager
	imagesDir string
	format    string
}

func (p *imagePublisher) CommitSessionImage(ctx context.Context, c fletchermcp.CommitImage) (string, error) {
	return p.sessions.CommitImage(ctx, c.SessionRef, session.CommitImageParams{
		Name:        c.Name,
		Entrypoint:  c.Entrypoint,
		Cmd:         c.Cmd,
		WorkingDir:  c.WorkingDir,
		ExposedPort: c.ExposedPort,
		Force:       c.Force,
	})
}

func (p *imagePublisher) ImportRegistryImage(ctx context.Context, ref, name string, force bool) (string, error) {
	if p.format != "ext4" {
		return "", fmt.Errorf("registry image publishing requires the firecracker runtime (ext4 snapshots), not %q", p.format)
	}
	if name == "" {
		name = image.DefaultName(ref)
	}
	res, err := image.ImportRegistry(ctx, image.ImportOptions{
		Ref:       ref,
		Name:      name,
		ImagesDir: p.imagesDir,
		Force:     force,
	})
	if err != nil {
		return "", err
	}
	return res.Name, nil
}

// imageRefresher re-pulls a registry-sourced template before a redeploy so the
// fresh fork picks up the latest image. A local image has nothing to pull.
type imageRefresher struct {
	imagesDir string
	logger    *slog.Logger
}

func (r imageRefresher) RefreshImage(ctx context.Context, name string) bool {
	meta, found, err := image.ReadMeta(r.imagesDir, name)
	if err != nil || !found || !looksLikeRegistryRef(meta.Source) {
		return false
	}
	if _, err := image.ImportRegistry(ctx, image.ImportOptions{
		Ref:       meta.Source,
		Name:      name,
		ImagesDir: r.imagesDir,
		Force:     true,
	}); err != nil {
		r.logger.Warn("redeploy: re-pull failed; redeploying the current template",
			slog.String("image", name),
			slog.String("source", meta.Source),
			slog.String("err", err.Error()),
		)
		return false
	}
	r.logger.Info("redeploy: re-pulled image to latest",
		slog.String("image", name), slog.String("source", meta.Source))
	return true
}

// HasTemplate reports whether an imported template of this name exists.
func (r imageRefresher) HasTemplate(name string) bool {
	templates, err := image.ListTemplates(r.imagesDir)
	if err != nil {
		return false
	}
	for _, t := range templates {
		if t.Name == name {
			return true
		}
	}
	return false
}

// ImportRef imports a registry ref under the given template name (replacing
// it), for a redeploy that retargets the session's image source.
func (r imageRefresher) ImportRef(ctx context.Context, ref, name string) error {
	_, err := image.ImportRegistry(ctx, image.ImportOptions{
		Ref:       ref,
		Name:      name,
		ImagesDir: r.imagesDir,
		Force:     true,
	})
	return err
}

// looksLikeRegistryRef reports whether ref is registry-qualified (so it can be
// pulled), vs a bare/local tag: the component before the first "/" must look
// like a registry host (contain "." or ":", or be "localhost").
func looksLikeRegistryRef(ref string) bool {
	host, _, ok := strings.Cut(ref, "/")
	if !ok {
		return false
	}
	return strings.ContainsAny(host, ".:") || host == "localhost"
}

// settingsReloader live-applies the reloadable settings to the running daemon
// without a restart. It re-derives the current effective config from the
// flag/env base plus the current store (the same path boot takes), pushes the
// live defaults and caps into the session and job managers, and reports which
// restart-required settings have drifted from what the daemon booted with.
type settingsReloader struct {
	flagCfg       Config            // flag/env config, before the settings overlay
	bootEffective map[string]string // effective settings at boot, for drift detection
	store         *settings.Store
	sessions      *session.Manager
	jobs          *job.Service
	logger        *slog.Logger
}

// Reload re-applies the live settings and returns the keys re-applied plus the
// restart-required keys whose value now differs from boot.
func (r *settingsReloader) Reload(ctx context.Context) (reloaded, pendingRestart []string, err error) {
	cur := r.flagCfg
	if err := applySettings(ctx, &cur, r.store, r.logger); err != nil {
		return nil, nil, err
	}
	r.sessions.ReloadDefaults(session.ReloadableDefaults{
		IdleTimeout:         cur.SessionIdleTimeout,
		MaxCount:            cur.SessionMaxCount,
		MaxDiskBytes:        int64(cur.SessionMaxDiskGB) << 30,
		DefaultImage:        cur.DefaultImage,
		DefaultEgressPolicy: egressDefaultPolicy(cur),
		DefaultGateway:      defaultGateway(cur),
	})
	r.jobs.ReloadDefaults(cur.DefaultImage, egressDefaultPolicy(cur), defaultGateway(cur))

	curEffective := settingsDefaults(cur)
	for key, boot := range r.bootEffective {
		if settings.RequiresRestart(key) && curEffective[key] != boot {
			pendingRestart = append(pendingRestart, key)
		}
	}
	sort.Strings(pendingRestart)
	return settings.LiveKeys(), pendingRestart, nil
}

// settingsDefaults maps each settings key to the daemon's resolved default value
// (what it falls back to when the key is not explicitly set), so the settings
// service can show the effective value rather than a bare "(default)". cfg is
// already fully resolved here, so for an unset key it holds exactly that default.
func settingsDefaults(cfg Config) map[string]string {
	btrfsRoot := cfg.BtrfsRoot
	if btrfsRoot == "" {
		btrfsRoot = filepath.Join(filepath.Dir(cfg.DatabasePath), "snapshots")
	}
	return map[string]string{
		settings.KeyRuntime:             cfg.RuntimeKind,
		settings.KeySnapshot:            cfg.SnapshotKind,
		settings.KeyBtrfsRoot:           btrfsRoot,
		settings.KeyPublicEndpoint:      cfg.PublicEndpoint,
		settings.KeyWireGuardPort:       strconv.Itoa(cfg.WireGuardListenPort),
		settings.KeyPairingPort:         strconv.Itoa(pairingPort(cfg)),
		settings.KeyLogLevel:            cfg.LogLevel,
		settings.KeyCredentialsDir:      cfg.CredentialsDir,
		settings.KeyNoUPnP:              strconv.FormatBool(cfg.DisableUPnP),
		settings.KeyGatewayListen:       cfg.GatewayListenAddr,
		settings.KeyMCPListen:           cfg.MCPListenAddr,
		settings.KeyRemoteAPIListen:     cfg.RemoteAPIListen,
		settings.KeySessionIdleTimeout:  sessionIdleTimeoutString(cfg.SessionIdleTimeout),
		settings.KeySessionMaxCount:     strconv.Itoa(cfg.SessionMaxCount),
		settings.KeySessionMaxDiskGB:    strconv.Itoa(cfg.SessionMaxDiskGB),
		settings.KeyDefaultImage:        cfg.DefaultImage,
		settings.KeyDefaultAgent:        defaultDefaultAgent,
		settings.KeyDefaultEgressPolicy: egressDefaultPolicy(cfg),
		settings.KeyVMMemoryMB:          strconv.Itoa(cfg.VMMemoryMB),
		settings.KeyDefaultGateway:      defaultGateway(cfg),
		settings.KeyPublicWeb:           strconv.FormatBool(cfg.PublicWeb),
		settings.KeyACMEStaging:         strconv.FormatBool(cfg.ACMEStaging),
		settings.KeyACMEEmail:           cfg.ACMEEmail,
		settings.KeyNotifyApprovals:     "true",
		settings.KeyNotifyReports:       "true",
		settings.KeyNotifyJobs:          "true",
		settings.KeyNotifySessionIdle:   "true",
		settings.KeyNotifyDeployHealth:  "true",
		settings.KeyAPNSKeyPath:         cfg.APNSKeyPath,
		settings.KeyAPNSKeyID:           cfg.APNSKeyID,
		settings.KeyAPNSTeamID:          cfg.APNSTeamID,
		settings.KeyAPNSTopic:           cfg.APNSTopic,
		settings.KeyAPNSEnvironment:     apnsEnvironment(cfg.APNSSandbox),
	}
}

// apnsEnvironment renders the apns_environment setting value from the bool.
func apnsEnvironment(sandbox bool) string {
	if sandbox {
		return "sandbox"
	}
	return "production"
}

// sessionIdleTimeoutString renders the idle timeout for display, showing "0"
// (rather than "0s") when the reaper is disabled.
func sessionIdleTimeoutString(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return d.String()
}

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
	// Session settings have no CLI flag; seed their defaults so a stored value
	// (including an explicit 0 to disable) overrides and absence keeps the default.
	cfg.SessionIdleTimeout = defaultSessionIdleTimeout
	cfg.SessionMaxCount = defaultSessionMaxCount
	cfg.SessionMaxDiskGB = defaultSessionMaxDiskGB
	cfg.DefaultImage = defaultDefaultImage
	cfg.DefaultEgressPolicy = defaultEgressPolicy
	cfg.VMMemoryMB = defaultVMMemoryMB
	cfg.DefaultGateway = defaultGatewayOn

	vals, err := store.Values(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	for k, v := range vals {
		if applySetting(cfg, k, v) {
			logger.Info("applied setting", slog.String("key", k), slog.String("value", v))
		}
	}
	return nil
}

// applySetting overlays one stored setting onto cfg, returning false for an
// unknown key (persisted by an older/newer version) so the caller skips it.
func applySetting(cfg *Config, k, v string) bool {
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
	case settings.KeyPairingPort:
		if n, perr := strconv.Atoi(v); perr == nil {
			cfg.PairingPort = n
		}
	case settings.KeyLogLevel:
		cfg.LogLevel = v
	case settings.KeyCredentialsDir:
		cfg.CredentialsDir = v
	case settings.KeyNoUPnP:
		if b, perr := strconv.ParseBool(v); perr == nil {
			cfg.DisableUPnP = b
		}
	case settings.KeyGatewayListen:
		cfg.GatewayListenAddr = v
	case settings.KeyMCPListen:
		cfg.MCPListenAddr = v
	case settings.KeyRemoteAPIListen:
		cfg.RemoteAPIListen = v
	case settings.KeySessionIdleTimeout:
		if v == "0" {
			cfg.SessionIdleTimeout = 0
		} else if d, perr := time.ParseDuration(v); perr == nil {
			cfg.SessionIdleTimeout = d
		}
	case settings.KeySessionMaxCount:
		if n, perr := strconv.Atoi(v); perr == nil {
			cfg.SessionMaxCount = n
		}
	case settings.KeySessionMaxDiskGB:
		if n, perr := strconv.Atoi(v); perr == nil {
			cfg.SessionMaxDiskGB = n
		}
	case settings.KeyDefaultImage:
		cfg.DefaultImage = v
	case settings.KeyDefaultEgressPolicy:
		cfg.DefaultEgressPolicy = v
	case settings.KeyVMMemoryMB:
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			cfg.VMMemoryMB = n
		}
	case settings.KeyDefaultGateway:
		cfg.DefaultGateway = v
	default:
		return applyPublicWebSetting(cfg, k, v)
	}
	return true
}

// applyPublicWebSetting applies the Milestone 8 public-web keys, returning false
// for any other (unknown) key. Split from applySetting to keep each readable.
func applyPublicWebSetting(cfg *Config, k, v string) bool {
	switch k {
	case settings.KeyPublicWeb:
		if b, perr := strconv.ParseBool(v); perr == nil {
			cfg.PublicWeb = b
		}
	case settings.KeyACMEStaging:
		if b, perr := strconv.ParseBool(v); perr == nil {
			cfg.ACMEStaging = b
		}
	case settings.KeyACMEEmail:
		cfg.ACMEEmail = v
	case settings.KeyAPNSKeyPath:
		cfg.APNSKeyPath = v
	case settings.KeyAPNSKeyID:
		cfg.APNSKeyID = v
	case settings.KeyAPNSTeamID:
		cfg.APNSTeamID = v
	case settings.KeyAPNSTopic:
		cfg.APNSTopic = v
	case settings.KeyAPNSEnvironment:
		cfg.APNSSandbox = v == "sandbox"
	default:
		return false // unknown key persisted by an older/newer version; ignore
	}
	return true
}

// publicEndpointHost extracts the host (IP) from an "ip:port" public endpoint,
// or returns the endpoint unchanged if it has no port. Empty stays empty. Used
// to tell a --public publisher the A record to create.
func publicEndpointHost(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		return host
	}
	return endpoint
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

// newRemoteAPIServer builds the network-API http.Server: the same handlers as
// the unix socket, gated by a per-peer token, with h2c so a streaming RPC (e.g.
// `session shell`) works. Shared by the tunnel-side listener and the Mode B
// VPN listener - both serve the identical handler over different transports.
func newRemoteAPIServer(auth tokenAuthenticator, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           authMiddleware(auth, handler),
		Protocols:         h2cProtocols(),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// newRemoteServer builds the tunnel-side network-API server, or nil when there
// is no tunnel listener.
func newRemoteServer(remoteLn net.Listener, auth tokenAuthenticator, handler http.Handler) *http.Server {
	if remoteLn == nil {
		return nil
	}
	return newRemoteAPIServer(auth, handler)
}

// Retry timing for remoteAPIListenActor; package vars so tests can shrink them.
var (
	remoteBindFirstBackoff = 2 * time.Second
	remoteBindMaxBackoff   = 30 * time.Second
)

// remoteAPIListenActor binds the Mode B remote-API listener on addr (the box's
// address on an operator-run VPN) and serves srv, retrying the bind for as long
// as the daemon runs. Unlike the tunnel listener it is bound here rather than
// at construction: the VPN interface (e.g. Tailscale) can come up after the
// daemon, and a bind that fails once must not leave the API unreachable for the
// whole process lifetime. The parent ctx is cancelled on shutdown, which breaks
// the retry loop; interrupt drains an in-flight server.
func remoteAPIListenActor(ctx context.Context, addr string, srv *http.Server, logger *slog.Logger) (func() error, func(error)) {
	execute := func() error {
		var lc net.ListenConfig
		backoff := remoteBindFirstBackoff
		for {
			ln, err := lc.Listen(ctx, "tcp", addr)
			if err == nil {
				logger.Info("remote-api (vpn) listening", slog.String("addr", addr))
				if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
					return fmt.Errorf("remote-api (vpn) serve: %w", serveErr)
				}
				return nil
			}
			if ctx.Err() != nil {
				return nil //nolint:nilerr // ctx cancelled mid-bind is a clean shutdown, not an error
			}
			logger.Info("remote-api (vpn) address not bindable yet; retrying once the VPN is up",
				slog.String("addr", addr),
				slog.Duration("retry_in", backoff),
				slog.String("err", err.Error()),
			)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > remoteBindMaxBackoff {
				backoff = remoteBindMaxBackoff
			}
		}
	}
	//nolint:contextcheck // shutdown must outlive the cancelled parent ctx
	interrupt := func(_ error) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	return execute, interrupt
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

// proxySocketPath is the daemon-side unix socket the egress forward-proxy
// listens on; a fork reaches it via the in-fork forwarder (HTTP_PROXY) and the
// loopback->vsock relay, the same way it reaches the gateway and MCP sockets.
func proxySocketPath(cfg Config) string {
	return filepath.Join(filepath.Dir(cfg.SocketPath), "egress.sock")
}

// proxyOpenSocketPath is the daemon-side unix socket of the "open" egress proxy
// (any public host, LAN-guarded). A fork with the "open" policy is relayed here
// instead of to the allowlist proxy at proxySocketPath.
func proxyOpenSocketPath(cfg Config) string {
	return filepath.Join(filepath.Dir(cfg.SocketPath), "egress-open.sock")
}

const defaultProxyListen = "127.0.0.1:11700"

// proxyListenAddr is the in-fork loopback address agents point HTTP_PROXY at;
// the in-fork forwarder relays it to proxySocketPath over vsock.
func proxyListenAddr(cfg Config) string {
	if cfg.ProxyListenAddr == "" {
		return defaultProxyListen
	}
	return cfg.ProxyListenAddr
}

// egressDefaultPolicy resolves the daemon's default fork egress policy (empty
// settings value falls back to "allowlist").
func egressDefaultPolicy(cfg Config) string {
	return egress.Normalize(cfg.DefaultEgressPolicy)
}

// defaultGateway resolves the daemon's default model-gateway wiring: "off" only
// when explicitly set, otherwise "on".
func defaultGateway(cfg Config) string {
	if cfg.DefaultGateway == "off" {
		return "off"
	}
	return "on"
}

// defaultEgressAllowlist is the curated host set the egress proxy permits under
// the default `allowlist` policy: Anthropic infrastructure (so Claude Code's
// startup connectivity check and API work), common package registries, and git
// hosts. The netguard LAN guard still applies on top. Per-job overrides
// (none / open / a custom allowlist) land in Phase B3.
var defaultEgressAllowlist = []string{
	// Anthropic infra: api / console / statsig under *.anthropic.com; the
	// Claude Code TUI also reaches claude.ai (OAuth) and platform.claude.com
	// (Console auth) for its startup checks.
	"anthropic.com", "*.anthropic.com",
	"claude.ai", "*.claude.ai",
	"claude.com", "*.claude.com",
	// Git hosts.
	"github.com", "*.github.com", "*.githubusercontent.com",
	"gitlab.com", "*.gitlab.com",
	// Package registries.
	"registry.npmjs.org", "*.npmjs.org",
	"pypi.org", "*.pypi.org", "files.pythonhosted.org",
	"crates.io", "static.crates.io",
	"proxy.golang.org", "sum.golang.org", "*.golang.org",
	// Debian apt (the base image is debian-based).
	"deb.debian.org", "*.debian.org",
}

// buildRuntimeDriver constructs the runtime.Driver chosen by cfg.
func buildRuntimeDriver(cfg Config, logger *slog.Logger) (runtime.Driver, error) {
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
				{Listen: proxyListenAddr(cfg), HostSocket: proxySocketPath(cfg)},
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
				{ListenAddr: proxyListenAddr(cfg), HostSocket: proxySocketPath(cfg), Egress: true},
			},
			EgressOpenSocket: proxyOpenSocketPath(cfg),
			MemSizeMib:       int64(cfg.VMMemoryMB),
			VcpuCount:        2,
			Logger:           logger,
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

// snapshotRootDir is the daemon's snapshot root: the configured btrfs_root, or
// a "snapshots" dir next to the database when unset.
func snapshotRootDir(cfg Config) string {
	if cfg.BtrfsRoot != "" {
		return cfg.BtrfsRoot
	}
	return filepath.Join(filepath.Dir(cfg.DatabasePath), "snapshots")
}

// baseImageAvailable reports whether at least one base-image rootfs template is
// imported for the active snapshot driver, so jobs and sessions can boot. The
// daemon runs as the user that owns the images directory, so it can stat it
// (the CLI running `fletcher doctor` usually cannot). Surfaced via Health.
func baseImageAvailable(cfg Config) bool {
	root := cfg.BtrfsRoot
	if root == "" {
		root = filepath.Join(filepath.Dir(cfg.DatabasePath), "snapshots")
	}
	entries, err := os.ReadDir(filepath.Join(root, "images"))
	if err != nil {
		return false
	}
	switch driverKind(cfg.SnapshotKind) {
	case "ext4":
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".ext4") {
				return true
			}
		}
		return false
	case "mock":
		return true // the mock snapshot driver needs no imported image
	default: // btrfs subvolume templates: any entry under images/
		return len(entries) > 0
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	base := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(api.NewContextLogHandler(base))
}
