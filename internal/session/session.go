// Package session manages durable sessions: persistent microVMs you create,
// exec into, stop, start, and delete. Sessions share the runtime and snapshot
// machinery with jobs (one execution engine, DESIGN.md §4) but have their own
// lifecycle - the fork persists across stop/start instead of being torn down.
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/appspec"
	"github.com/joshjon/fletcher/internal/egress"
	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/image"
	"github.com/joshjon/fletcher/internal/runtime"
	"github.com/joshjon/fletcher/internal/snapshot"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// idPrefix is the typeid prefix for session IDs (e.g. "session_01h...").
const idPrefix = "session"

// State is a session's lifecycle state.
type State string

const (
	// StateRunning means the session's VM is booted and exec-ready.
	StateRunning State = "running"
	// StateStopped means the VM is down but its fork persists on disk.
	StateStopped State = "stopped"
)

// Session is the domain view of a session (the fork id/path stay internal).
type Session struct {
	ID         string
	Name       string
	Image      string
	State      State
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastUsedAt *time.Time
	DiskBytes  int64
	// EgressPolicy gates the fork's outbound network: "none" | "allowlist" |
	// "open". Fixed at create time.
	EgressPolicy string
	// Gateway is "on" or "off": whether the model-gateway env is injected.
	Gateway string
	// RunApp is whether the session runs the image's own app on boot (M9).
	RunApp bool
	// HasRollback is whether a redeploy retired a fork this session can roll
	// back to.
	HasRollback bool
	// VolumeID and VolumeName identify the persistent volume attached to this
	// session (empty when none). The volume outlives forks and the session.
	VolumeID   string
	VolumeName string
}

// ErrNotFound is returned when a session ref matches nothing.
var ErrNotFound = errs.New(errs.CategoryNotFound, "session not found")

// Options tune session limits and the work-based idle auto-stop.
type Options struct {
	// IdleTimeout auto-stops a session idle (no work in flight) this long; 0
	// disables the reaper.
	IdleTimeout time.Duration
	// MaxCount caps the number of sessions; 0 disables the cap.
	MaxCount int
	// MaxDiskBytes caps total session disk; 0 disables the cap.
	MaxDiskBytes int64
	// DefaultImage is used when a session is created with no image; empty makes
	// the image required.
	DefaultImage string
	// DefaultEgressPolicy is the egress policy used when create is called with an
	// empty policy: "none" | "allowlist" | "open".
	DefaultEgressPolicy string
	// DefaultGateway is the model-gateway wiring used when create is called with
	// an empty value: "on" | "off".
	DefaultGateway string
	// PublicWeb gates `Publish` with public=true: when false, publishing a port
	// publicly is refused (the public HTTPS listener is off).
	PublicWeb bool
	// ImagesDir is the daemon's images directory (<snapshot-root>/images). When
	// set, committing a session image records its TemplateMeta sidecar there so
	// pickers and deploys see the entrypoint and port.
	ImagesDir string
}

// idleLoadThreshold is the guest 1-minute load average below which a session
// with no active host operation counts as having no work in flight. A truly
// idle VM sits near zero; a running agent or build pushes it well above this.
const idleLoadThreshold = 0.2

// Manager owns session lifecycle and the live VM handles. The runtime is the
// session-capable driver (nil when the configured runtime cannot host
// sessions); snapshot creates and destroys the persistent forks.
type Manager struct {
	q        sqliteq.Querier
	snapshot snapshot.Driver
	runtime  runtime.SessionRuntime
	// baseEnv is injected into every session; gatewayEnv is added on top only
	// when the session's gateway toggle is on (DESIGN.md §6).
	baseEnv    []string
	gatewayEnv []string
	logger     *slog.Logger
	// opts holds the live-reloadable create-time defaults and caps. Stored
	// behind an atomic pointer so ReloadSettings can swap them while creates and
	// the idle reaper read them concurrently.
	opts atomic.Pointer[Options]

	mu      sync.Mutex
	handles map[string]runtime.SessionHandle
	busy    map[string]int // sessions with an in-flight host op (exec/shell/ssh)

	// startLocks serialises Start per session id so concurrent wakes (e.g.
	// several inbound connections to a published port at once) boot at most one
	// VM. A sync.Map's zero value is ready to use.
	startLocks sync.Map

	// broker opens/closes the host-side forwarders for published ports. nil when
	// the tunnel is down (published ports are recorded but unreachable until it
	// comes up). Set once at startup via SetBroker, before serving.
	broker PortBroker

	// volumes resolves persistent volumes for attachment and boot. nil when the
	// snapshot driver cannot provision volumes; sessions then refuse --volume.
	// Set once at startup via SetVolumes, before serving.
	volumes VolumeResolver
}

// VolumeResolver resolves persistent volumes for attachment and boot
// (implemented by volume.Manager; kept narrow per "consumers define
// interfaces").
type VolumeResolver interface {
	// ResolveAttachable resolves ref to a volume free to attach, refusing one
	// already attached to another session.
	ResolveAttachable(ctx context.Context, ref string) (id, path string, err error)
	// PathFor returns the backing path for an attached volume id.
	PathFor(ctx context.Context, id string) (string, error)
	// NameFor returns the display name for an attached volume id.
	NameFor(ctx context.Context, id string) (string, error)
}

// PortBroker opens and closes the host-side forwarders that make a session's
// published port reachable. The Manager owns the published_ports rows; the
// broker owns the live listeners. The split lets the broker dial back into the
// Manager (DialPort) to reach the guest without an import cycle.
type PortBroker interface {
	// Open starts forwarding for a published port, binding its host-side
	// listener (reusing pp.TunnelPort when non-zero, else picking a free port)
	// and returning the actual bound tunnel port.
	Open(pp PublishedPort) (tunnelPort int, err error)
	// Close stops forwarding for the published port with this id.
	Close(id string)
}

// PublishedPort is a port a session serves, brokered by the daemon. Phase 1
// makes it reachable over the tunnel at TunnelPort; Public/Host drive the
// Phase 2 public listener.
type PublishedPort struct {
	ID         string
	SessionID  string
	GuestPort  int
	Name       string
	TunnelPort int
	Public     bool
	Host       string
	CreatedAt  time.Time
}

// pubPortPrefix is the typeid prefix for published-port IDs.
const pubPortPrefix = "pubport"

// NewManager constructs a Manager. rt may be nil if the configured runtime does
// not support sessions, in which case lifecycle calls fail with a clear error.
func NewManager(q sqliteq.Querier, snap snapshot.Driver, rt runtime.SessionRuntime, baseEnv, gatewayEnv []string, logger *slog.Logger, opts Options) *Manager {
	m := &Manager{
		q:          q,
		snapshot:   snap,
		runtime:    rt,
		baseEnv:    baseEnv,
		gatewayEnv: gatewayEnv,
		logger:     logger,
		handles:    make(map[string]runtime.SessionHandle),
		busy:       make(map[string]int),
	}
	m.opts.Store(&opts)
	return m
}

// opt returns the current create-time defaults and caps.
func (m *Manager) opt() Options { return *m.opts.Load() }

// ReloadDefaults swaps the live-reloadable defaults and caps without a restart,
// preserving the boot-bound fields (PublicWeb, which gates public publishing
// only while its listener is up). Called by ReloadSettings.
func (m *Manager) ReloadDefaults(d ReloadableDefaults) {
	cur := *m.opts.Load()
	cur.IdleTimeout = d.IdleTimeout
	cur.MaxCount = d.MaxCount
	cur.MaxDiskBytes = d.MaxDiskBytes
	cur.DefaultImage = d.DefaultImage
	cur.DefaultEgressPolicy = d.DefaultEgressPolicy
	cur.DefaultGateway = d.DefaultGateway
	m.opts.Store(&cur)
}

// ReloadableDefaults is the subset of Options that ReloadSettings can apply
// live (the create-time defaults and caps).
type ReloadableDefaults struct {
	IdleTimeout         time.Duration
	MaxCount            int
	MaxDiskBytes        int64
	DefaultImage        string
	DefaultEgressPolicy string
	DefaultGateway      string
}

// envFor composes a session's environment: the base env always, plus the
// model-gateway env when the gateway is on.
func (m *Manager) envFor(gateway string) []string {
	env := append([]string(nil), m.baseEnv...)
	if gateway != "off" {
		env = append(env, m.gatewayEnv...)
	}
	return env
}

// sessionEnv is envFor plus the session's own identity, so an agent inside the
// fork can name itself to daemon tools (e.g. publish_image's session arg).
func (m *Manager) sessionEnv(gateway, id, name string) []string {
	return append(m.envFor(gateway),
		"FLETCHER_SESSION_ID="+id,
		"FLETCHER_SESSION_NAME="+name,
	)
}

// resolveGateway canonicalises a gateway value to "on"/"off"; empty uses the
// configured default, and anything other than "off" is "on".
func resolveGateway(v, def string) string {
	if strings.TrimSpace(v) == "" {
		v = def
	}
	if v == "off" {
		return "off"
	}
	return "on"
}

// ExecResult is the captured output of a command run in a session.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int32
}

func (m *Manager) requireRuntime() error {
	if m.runtime == nil {
		return errs.New(errs.CategoryFailedPrecondition,
			"sessions require the firecracker runtime (set it with `fletcher settings set runtime firecracker`)")
	}
	return nil
}

// Create provisions a session's persistent fork, boots its VM, and records it.
// egressPolicy is "none"|"allowlist"|"open"; empty resolves to the manager's
// configured default. volumeRef, when non-empty, attaches that persistent
// volume (mounted at /volume in the guest) for the session's lifetime.
func (m *Manager) Create(ctx context.Context, name, image, egressPolicy, gateway string, runApp bool, volumeRef string) (Session, error) {
	if err := m.requireRuntime(); err != nil {
		return Session{}, err
	}
	if strings.TrimSpace(name) == "" {
		return Session{}, errs.New(errs.CategoryInvalidArgument, "name is required")
	}
	if strings.TrimSpace(egressPolicy) == "" {
		egressPolicy = m.opt().DefaultEgressPolicy
	}
	egressPolicy = egress.Normalize(egressPolicy)
	gateway = resolveGateway(gateway, m.opt().DefaultGateway)
	if strings.TrimSpace(image) == "" {
		image = m.opt().DefaultImage
	}
	if strings.TrimSpace(image) == "" {
		return Session{}, errs.New(errs.CategoryInvalidArgument, "image is required")
	}
	if err := m.checkCaps(ctx); err != nil {
		return Session{}, err
	}

	var volumeID, volumePath string
	if strings.TrimSpace(volumeRef) != "" {
		if m.volumes == nil {
			return Session{}, errs.New(errs.CategoryFailedPrecondition,
				"this daemon cannot attach volumes (requires the firecracker runtime's ext4 snapshots)")
		}
		var err error
		volumeID, volumePath, err = m.volumes.ResolveAttachable(ctx, volumeRef)
		if err != nil {
			return Session{}, err
		}
	}

	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Session{}, fmt.Errorf("generate session id: %w", err)
	}
	sessionID := id.String()

	fork, err := m.snapshot.Create(ctx, image)
	if err != nil {
		return Session{}, fmt.Errorf("create session fork: %w", err)
	}

	handle, err := m.runtime.StartSession(ctx, runtime.SessionSpec{
		SessionID:    sessionID,
		RootfsPath:   fork.Path,
		Env:          m.sessionEnv(gateway, sessionID, name),
		EgressPolicy: egressPolicy,
		RunApp:       runApp,
		VolumePath:   volumePath,
	})
	if err != nil {
		_ = m.snapshot.Delete(context.WithoutCancel(ctx), fork.ID)
		return Session{}, fmt.Errorf("start session vm: %w", err)
	}

	now := time.Now().Unix()
	row, err := m.q.CreateSession(ctx, sqliteq.CreateSessionParams{
		ID:           sessionID,
		Name:         name,
		Image:        image,
		State:        string(StateRunning),
		ForkID:       fork.ID,
		ForkPath:     fork.Path,
		CreatedAt:    now,
		UpdatedAt:    now,
		EgressPolicy: egressPolicy,
		Gateway:      gateway,
		RunApp:       boolToInt(runApp),
		VolumeID:     nilIfEmptyStr(volumeID),
	})
	if err != nil {
		_ = handle.Stop(context.WithoutCancel(ctx))
		_ = m.snapshot.Delete(context.WithoutCancel(ctx), fork.ID)
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Session{}, errs.Newf(errs.CategoryConflict, "a session named %q already exists", name)
		}
		return Session{}, fmt.Errorf("record session: %w", err)
	}

	m.putHandle(sessionID, handle)
	return m.enrich(ctx, sessionFromRow(row)), nil
}

// Get returns the session matching ref (an ID or name).
func (m *Manager) Get(ctx context.Context, ref string) (Session, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}
	return m.enrich(ctx, sessionFromRow(row)), nil
}

// List returns all sessions, newest first.
func (m *Manager) List(ctx context.Context) ([]Session, error) {
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]Session, len(rows))
	for i, r := range rows {
		out[i] = m.enrich(ctx, sessionFromRow(r))
	}
	return out, nil
}

// enrich fills display-only derived fields (the attached volume's name).
// Best-effort: a failed lookup leaves the id, which is still actionable.
func (m *Manager) enrich(ctx context.Context, s Session) Session {
	if s.VolumeID != "" && m.volumes != nil {
		if name, err := m.volumes.NameFor(ctx, s.VolumeID); err == nil {
			s.VolumeName = name
		}
	}
	return s
}

// Start boots a stopped session's VM against its persisted fork. It is safe to
// call concurrently for the same session (e.g. several inbound connections
// waking a published port at once): a per-session lock serialises the
// check-and-boot so at most one VM is started.
func (m *Manager) Start(ctx context.Context, ref string) (Session, error) {
	if err := m.requireRuntime(); err != nil {
		return Session{}, err
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}

	lock := m.startLock(row.ID)
	lock.Lock()
	defer lock.Unlock()

	// Re-read under the lock: a concurrent caller may have started it already.
	row, err = m.lookup(ctx, row.ID)
	if err != nil {
		return Session{}, err
	}
	if State(row.State) == StateRunning {
		return sessionFromRow(row), nil // already running
	}

	volumePath, err := m.volumePathFor(ctx, row)
	if err != nil {
		return Session{}, err
	}
	handle, err := m.runtime.StartSession(ctx, runtime.SessionSpec{
		SessionID:    row.ID,
		RootfsPath:   row.ForkPath,
		Env:          m.sessionEnv(row.Gateway, row.ID, row.Name),
		EgressPolicy: row.EgressPolicy,
		RunApp:       row.RunApp != 0,
		VolumePath:   volumePath,
	})
	if err != nil {
		return Session{}, fmt.Errorf("start session vm: %w", err)
	}
	m.putHandle(row.ID, handle)

	if err := m.setState(ctx, row.ID, StateRunning); err != nil {
		return Session{}, err
	}
	row.State = string(StateRunning)
	return m.enrich(ctx, sessionFromRow(row)), nil
}

// volumePathFor resolves the backing path of a session's attached volume
// ("" when none). A missing volume row is an error: booting without the disk
// the session's app expects would look like silent data loss.
func (m *Manager) volumePathFor(ctx context.Context, row sqliteq.Session) (string, error) {
	if row.VolumeID == nil || *row.VolumeID == "" {
		return "", nil
	}
	if m.volumes == nil {
		return "", errs.New(errs.CategoryFailedPrecondition,
			"session has a volume attached but this daemon cannot mount volumes")
	}
	path, err := m.volumes.PathFor(ctx, *row.VolumeID)
	if err != nil {
		return "", fmt.Errorf("resolve session volume: %w", err)
	}
	return path, nil
}

// nilIfEmptyStr maps "" to a NULL-able column value.
func nilIfEmptyStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Stop stops a running session's VM, keeping its fork on disk.
func (m *Manager) Stop(ctx context.Context, ref string) (Session, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}
	if handle := m.takeHandle(row.ID); handle != nil {
		m.syncGuest(ctx, handle, row.ID)
		if serr := handle.Stop(ctx); serr != nil {
			m.logger.Warn("stop session vm", slog.String("session_id", row.ID), slog.String("err", serr.Error()))
		}
	}
	if err := m.setState(ctx, row.ID, StateStopped); err != nil {
		return Session{}, err
	}
	row.State = string(StateStopped)
	return sessionFromRow(row), nil
}

// Delete stops the VM (if running) and destroys the fork.
func (m *Manager) Delete(ctx context.Context, ref string) (bool, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if handle := m.takeHandle(row.ID); handle != nil {
		if serr := handle.Stop(ctx); serr != nil {
			m.logger.Warn("stop session vm on delete", slog.String("session_id", row.ID), slog.String("err", serr.Error()))
		}
	}
	// Close any published-port forwarders before the rows vanish (the DB rows
	// cascade-delete with the session; the in-memory listeners do not).
	m.closeSessionPorts(ctx, row.ID)
	if derr := m.snapshot.Delete(context.WithoutCancel(ctx), row.ForkID); derr != nil {
		m.logger.Warn("delete session fork", slog.String("session_id", row.ID), slog.String("err", derr.Error()))
	}
	if row.PrevForkID != nil {
		if derr := m.snapshot.Delete(context.WithoutCancel(ctx), *row.PrevForkID); derr != nil {
			m.logger.Warn("delete session previous fork", slog.String("session_id", row.ID), slog.String("err", derr.Error()))
		}
	}
	// Drop any on-disk VM state (e.g. a hibernation snapshot) for the session.
	if m.runtime != nil {
		if derr := m.runtime.DiscardSession(context.WithoutCancel(ctx), row.ID); derr != nil {
			m.logger.Warn("discard session vm state", slog.String("session_id", row.ID), slog.String("err", derr.Error()))
		}
	}
	if _, derr := m.q.DeleteSession(ctx, row.ID); derr != nil {
		return false, fmt.Errorf("delete session: %w", derr)
	}
	return true, nil
}

// Exec runs command inside a running session and returns its captured output.
func (m *Manager) Exec(ctx context.Context, ref, command string) (ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, errs.New(errs.CategoryInvalidArgument, "command is required")
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return ExecResult{}, err
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return ExecResult{}, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running; start it with `fletcher session start`", row.Name)
	}

	m.markBusy(row.ID)
	defer m.unmarkBusy(row.ID)

	var stdout, stderr strings.Builder
	// Leave Env unset so the handle uses the session's own environment (the
	// global agent env with the egress proxy vars resolved for this session's
	// policy), keeping exec consistent with the login-shell and shell paths.
	res, err := handle.Exec(ctx, runtime.Spec{Command: command}, &stdout, &stderr)
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec in session: %w", err)
	}
	m.touch(ctx, row.ID)
	return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: res.ExitCode}, nil
}

// appLogPath is where a run_app session's supervisor writes the app's merged
// stdout/stderr inside the guest.
const appLogPath = "/var/log/fletcher-app.log"

// defaultLogTailLines is how many trailing app-log lines Logs returns when the
// caller does not specify a bound.
const defaultLogTailLines = 200

// Restart stops a running session's VM and starts it again against the same
// fork. For a run_app (deploy) session this re-runs the image's app.
func (m *Manager) Restart(ctx context.Context, ref string) (Session, error) {
	if _, err := m.Stop(ctx, ref); err != nil {
		return Session{}, err
	}
	return m.Start(ctx, ref)
}

// StreamLogs tails the session's app log into w. With follow it stays open
// (like `tail -F`) until ctx is cancelled or the client disconnects; otherwise
// it writes the trailing tailLines and returns. A missing log - the session is
// not a run_app deploy, or its app has not written yet - yields empty output,
// not an error. The app log path is a fixed guest-side constant and tailLines
// is an int, so the tail command carries no caller-supplied text.
func (m *Manager) StreamLogs(ctx context.Context, ref string, tailLines int, follow bool, w io.Writer) error {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return err
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running; start it with `fletcher session start`", row.Name)
	}
	if tailLines <= 0 {
		tailLines = defaultLogTailLines
	}
	followFlag := ""
	if follow {
		// -F (not -f) so a follow stream survives the supervisor truncating the
		// log on an app restart.
		followFlag = "-F "
	}
	cmd := fmt.Sprintf("tail -n %d %s%s 2>/dev/null || true", tailLines, followFlag, appLogPath)

	m.markBusy(row.ID)
	defer m.unmarkBusy(row.ID)
	if _, err := handle.Exec(ctx, runtime.Spec{Command: cmd}, w, w); err != nil {
		// A follow stream ends when the client disconnects (ctx cancelled): the
		// daemon closes the vsock conn, the guest kills the tail, and exec
		// returns a context error - not a real failure.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("tail session log: %w", err)
	}
	return nil
}

// Logs returns the tail of the session's app log (no follow).
func (m *Manager) Logs(ctx context.Context, ref string, tailLines int) (string, error) {
	var buf strings.Builder
	if err := m.StreamLogs(ctx, ref, tailLines, false, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// AppRestartCount returns how many times a running run_app session's app has
// restarted (queried from the guest), and whether the count is available (the
// session is running with a live handle).
func (m *Manager) AppRestartCount(ctx context.Context, ref string) (int64, bool) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return 0, false
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return 0, false
	}
	n, err := handle.AppRestarts(ctx)
	if err != nil {
		return 0, false
	}
	return n, true
}

// UpdateSession changes a session's egress policy and/or gateway wiring (an
// empty value leaves that field unchanged). Both are baked into the fork at VM
// boot, so a change to a running session takes effect on its next start;
// restartRequired reports whether the session is currently running.
func (m *Manager) UpdateSession(ctx context.Context, ref, egressPolicy, gateway string) (Session, bool, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, false, err
	}

	newEgress := row.EgressPolicy
	if strings.TrimSpace(egressPolicy) != "" {
		switch egressPolicy {
		case egress.PolicyNone, egress.PolicyAllowlist, egress.PolicyOpen:
			newEgress = egressPolicy
		default:
			return Session{}, false, errs.Newf(errs.CategoryInvalidArgument,
				"invalid egress policy %q (want none | allowlist | open)", egressPolicy)
		}
	}
	newGateway := row.Gateway
	if strings.TrimSpace(gateway) != "" {
		switch gateway {
		case "on", "off":
			newGateway = gateway
		default:
			return Session{}, false, errs.Newf(errs.CategoryInvalidArgument,
				"invalid gateway %q (want on | off)", gateway)
		}
	}

	if err := m.q.UpdateSessionPolicy(ctx, sqliteq.UpdateSessionPolicyParams{
		EgressPolicy: newEgress,
		Gateway:      newGateway,
		UpdatedAt:    time.Now().Unix(),
		ID:           row.ID,
	}); err != nil {
		return Session{}, false, err
	}
	row.EgressPolicy = newEgress
	row.Gateway = newGateway
	return sessionFromRow(row), State(row.State) == StateRunning, nil
}

// CommitImageParams parameterise committing a session's fork as an image
// template (the docker-commit analogue).
type CommitImageParams struct {
	// Name is the template name jobs/sessions/deploys reference via --image.
	Name string
	// Entrypoint and Cmd describe how a deploy launches the committed image's
	// app. When either is set it is written into the committed image's app
	// launch spec (offline, so a stopped session works too). Empty keeps the
	// image's existing app spec.
	Entrypoint []string
	Cmd        []string
	// WorkingDir is the app's working directory (with Entrypoint/Cmd).
	WorkingDir string
	// ExposedPort is the port a deploy of this image publishes by default
	// (0 keeps the source template's, if known).
	ExposedPort int
	// Force replaces an existing template of the same name.
	Force bool
}

// CommitImage commits a session's fork as a new image template so jobs,
// sessions, and deploys can boot from it. A running session's guest page cache
// is flushed (sync) first, so the cloned disk is at worst crash-consistent: the
// ext4 journal replays on first boot. Returns the committed template name.
func (m *Manager) CommitImage(ctx context.Context, ref string, p CommitImageParams) (string, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return "", errs.New(errs.CategoryInvalidArgument, "image name is required")
	}
	if !validImageName(name) {
		return "", errs.Newf(errs.CategoryInvalidArgument,
			"invalid image name %q (use lowercase letters, digits, '.', '_', '-')", name)
	}
	committer, ok := m.snapshot.(snapshot.TemplateCommitter)
	if !ok {
		return "", errs.New(errs.CategoryFailedPrecondition,
			"the configured snapshot driver cannot commit session images (requires the firecracker runtime's ext4 snapshots)")
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return "", err
	}

	// Keep the session safe from the idle reaper for the duration.
	m.markBusy(row.ID)
	defer m.unmarkBusy(row.ID)

	// Flush a running guest's page cache so the cloned disk holds current
	// bytes; the result is at worst crash-consistent and the committer replays
	// the journal when it needs to edit the clone.
	if handle := m.getHandle(row.ID); handle != nil && State(row.State) == StateRunning {
		if res, xerr := m.execIn(ctx, handle, "sync"); xerr != nil {
			return "", fmt.Errorf("sync session disk before commit: %w", xerr)
		} else if res.ExitCode != 0 {
			return "", fmt.Errorf("sync session disk before commit: exit %d: %s", res.ExitCode, res.Stderr)
		}
	}

	// A new entrypoint is written into the committed image as its app launch
	// spec (the same /etc/fletcher/app.json a registry import bakes in).
	var extraFiles map[string][]byte
	if len(p.Entrypoint) > 0 || len(p.Cmd) > 0 {
		specJSON, jerr := json.Marshal(appspec.Spec{
			Entrypoint: p.Entrypoint,
			Cmd:        p.Cmd,
			WorkingDir: p.WorkingDir,
		})
		if jerr != nil {
			return "", fmt.Errorf("marshal app spec: %w", jerr)
		}
		extraFiles = map[string][]byte{appspec.Path: specJSON}
	}

	if err := committer.CommitTemplate(ctx, row.ForkID, name, p.Force, extraFiles); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return "", errs.Newf(errs.CategoryConflict, "image %q already exists (use force to replace it)", name)
		}
		return "", fmt.Errorf("commit session fork: %w", err)
	}
	m.writeCommittedMeta(row, p, name)
	m.touch(ctx, row.ID)
	return name, nil
}

// writeCommittedMeta records the committed template's sidecar metadata,
// inheriting the source template's entrypoint/port when the commit does not
// override them. Best-effort: the template is usable without it.
func (m *Manager) writeCommittedMeta(row sqliteq.Session, p CommitImageParams, name string) {
	imagesDir := m.opt().ImagesDir
	if imagesDir == "" {
		return
	}
	entrypoint := append(append([]string(nil), p.Entrypoint...), p.Cmd...)
	exposedPort := p.ExposedPort
	if parent, found, err := image.ReadMeta(imagesDir, row.Image); err == nil && found {
		if len(entrypoint) == 0 {
			entrypoint = parent.Entrypoint
		}
		if exposedPort == 0 {
			exposedPort = parent.ExposedPort
		}
	}
	meta := image.TemplateMeta{
		Source:      "session:" + row.Name,
		Format:      "ext4",
		ImportedAt:  time.Now().Unix(),
		Entrypoint:  entrypoint,
		ExposedPort: exposedPort,
	}
	if err := image.WriteMeta(imagesDir, name, meta); err != nil {
		m.logger.Warn("write committed image metadata",
			slog.String("session_id", row.ID), slog.String("image", name), slog.String("err", err.Error()))
	}
}

// syncGuest flushes a guest's page cache to its disk before the VM stops.
// Hibernation keeps memory, but the disk must stay the source of truth
// (DESIGN §5): a stale or discarded snapshot, a redeploy retiring the fork for
// rollback, or a commit must never lose writes that only lived in guest RAM.
// Best-effort: a guest that cannot sync still stops.
func (m *Manager) syncGuest(ctx context.Context, handle runtime.SessionHandle, id string) {
	if res, err := m.execIn(ctx, handle, "sync"); err != nil {
		m.logger.Warn("sync guest before stop", slog.String("session_id", id), slog.String("err", err.Error()))
	} else if res.ExitCode != 0 {
		m.logger.Warn("sync guest before stop", slog.String("session_id", id), slog.String("stderr", res.Stderr))
	}
}

// execIn runs cmd in the session VM behind handle, capturing its output.
func (m *Manager) execIn(ctx context.Context, handle runtime.SessionHandle, cmd string) (ExecResult, error) {
	var stdout, stderr strings.Builder
	res, err := handle.Exec(ctx, runtime.Spec{Command: cmd}, &stdout, &stderr)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: res.ExitCode}, nil
}

// validImageName reports whether name is safe as a template file name.
func validImageName(name string) bool {
	if name == "" || len(name) > 64 || name[0] == '.' || name[0] == '-' {
		return false
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Redeploy replaces a session's disk with a fresh fork of its template image
// and restarts it. newImage, when non-empty, retargets the session to that
// template first (it must already be imported); empty re-forks from the
// session's current image. The retired fork is kept as the session's previous
// fork (reflink-shared, so nearly free) so a bad redeploy can be rolled back;
// each redeploy replaces the one before it.
func (m *Manager) Redeploy(ctx context.Context, ref, newImage string) (Session, error) {
	if err := m.requireRuntime(); err != nil {
		return Session{}, err
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}
	image := row.Image
	if strings.TrimSpace(newImage) != "" {
		image = newImage
	}

	// Hold the per-session start lock across the disk swap so a concurrent wake
	// cannot boot the old fork mid-redeploy. Released before the final Start
	// (which re-acquires it); the new fork is committed by then, so whichever
	// caller boots uses it.
	lock := m.startLock(row.ID)
	lock.Lock()
	if handle := m.takeHandle(row.ID); handle != nil {
		// Flush the guest first: this disk becomes the rollback target.
		m.syncGuest(ctx, handle, row.ID)
		if serr := handle.Stop(ctx); serr != nil {
			m.logger.Warn("stop session vm for redeploy", slog.String("session_id", row.ID), slog.String("err", serr.Error()))
		}
	}
	if err := m.setState(ctx, row.ID, StateStopped); err != nil {
		lock.Unlock()
		return Session{}, err
	}

	fork, err := m.snapshot.Create(ctx, image)
	if err != nil {
		lock.Unlock()
		return Session{}, fmt.Errorf("re-fork session from image %q: %w", image, err)
	}
	retiredForkID, retiredForkPath := row.ForkID, row.ForkPath
	droppedPrev := row.PrevForkID
	if err := m.q.UpdateSessionForks(ctx, sqliteq.UpdateSessionForksParams{
		ForkID:       fork.ID,
		ForkPath:     fork.Path,
		PrevForkID:   &retiredForkID,
		PrevForkPath: &retiredForkPath,
		Image:        image,
		UpdatedAt:    time.Now().Unix(),
		ID:           row.ID,
	}); err != nil {
		_ = m.snapshot.Delete(context.WithoutCancel(ctx), fork.ID)
		lock.Unlock()
		return Session{}, fmt.Errorf("point session at new fork: %w", err)
	}
	// Only one rollback level is kept: reclaim the fork the retired one replaces.
	if droppedPrev != nil {
		if derr := m.snapshot.Delete(context.WithoutCancel(ctx), *droppedPrev); derr != nil {
			m.logger.Warn("delete dropped previous fork after redeploy", slog.String("session_id", row.ID), slog.String("err", derr.Error()))
		}
	}
	lock.Unlock()

	return m.Start(ctx, row.ID)
}

// Rollback swaps a session back to the fork its last redeploy retired and
// restarts it - the one-step undo for a bad redeploy. Swapping (rather than
// consuming) the forks means rolling forward again is the same operation.
// The session's image label is not changed: the fork is the source of truth
// for what runs; the label tracks the most recent explicit retarget.
func (m *Manager) Rollback(ctx context.Context, ref string) (Session, error) {
	if err := m.requireRuntime(); err != nil {
		return Session{}, err
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return Session{}, err
	}
	if row.PrevForkID == nil || row.PrevForkPath == nil {
		return Session{}, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q has no previous fork to roll back to (rollback undoes a redeploy)", row.Name)
	}

	lock := m.startLock(row.ID)
	lock.Lock()
	if handle := m.takeHandle(row.ID); handle != nil {
		// Flush the guest first: this disk becomes the swap-forward target.
		m.syncGuest(ctx, handle, row.ID)
		if serr := handle.Stop(ctx); serr != nil {
			m.logger.Warn("stop session vm for rollback", slog.String("session_id", row.ID), slog.String("err", serr.Error()))
		}
	}
	if err := m.setState(ctx, row.ID, StateStopped); err != nil {
		lock.Unlock()
		return Session{}, err
	}
	curID, curPath := row.ForkID, row.ForkPath
	if err := m.q.UpdateSessionForks(ctx, sqliteq.UpdateSessionForksParams{
		ForkID:       *row.PrevForkID,
		ForkPath:     *row.PrevForkPath,
		PrevForkID:   &curID,
		PrevForkPath: &curPath,
		Image:        row.Image,
		UpdatedAt:    time.Now().Unix(),
		ID:           row.ID,
	}); err != nil {
		lock.Unlock()
		return Session{}, fmt.Errorf("swap session forks: %w", err)
	}
	lock.Unlock()

	return m.Start(ctx, row.ID)
}

// Shell opens an interactive PTY in a running session, bridging the caller's
// stdin/stdout and window resizes to the VM, and returns the shell's exit code.
func (m *Manager) Shell(ctx context.Context, ref string, spec runtime.ShellSpec, stdin io.Reader, stdout io.Writer, resize <-chan runtime.WinSize) (int32, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return 0, err
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return 0, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running; start it with `fletcher session start`", row.Name)
	}
	// Leave spec.Env unset so the handle uses the session's own environment
	// (gateway/egress resolved for this session), consistent with Exec.
	m.markBusy(row.ID)
	defer m.unmarkBusy(row.ID)
	code, err := handle.Shell(ctx, spec, stdin, stdout, resize)
	if err != nil {
		return 0, fmt.Errorf("shell in session: %w", err)
	}
	m.touch(ctx, row.ID)
	return code, nil
}

// DialSSH opens a raw byte stream to a running session's SSH server for the
// daemon to proxy a brokered SSH connection through.
func (m *Manager) DialSSH(ctx context.Context, ref string) (net.Conn, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return nil, err
	}
	handle := m.getHandle(row.ID)
	if handle == nil || State(row.State) != StateRunning {
		return nil, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running; start it with `fletcher session start`", row.Name)
	}
	conn, err := handle.DialSSH(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial session ssh: %w", err)
	}
	m.touch(ctx, row.ID)
	// The SSH session is in flight for the life of the connection; keep the
	// session marked busy (and so safe from idle auto-stop) until it closes.
	m.markBusy(row.ID)
	id := row.ID
	return &busyConn{Conn: conn, release: func() { m.unmarkBusy(id) }}, nil
}

// SetBroker wires the port broker used to open/close published-port
// forwarders. Called once at startup before serving.
func (m *Manager) SetBroker(b PortBroker) { m.broker = b }

// SetVolumes wires the volume resolver used to attach and boot persistent
// volumes. Called once at startup before serving.
func (m *Manager) SetVolumes(v VolumeResolver) { m.volumes = v }

// DialPort opens a raw byte stream to a TCP port inside a session's VM for the
// daemon's port broker to proxy a published-port connection through. A stopped
// session is woken first (so a published port keeps serving across idle
// hibernation), and the session is marked busy for the connection's lifetime so
// the reaper does not stop it mid-traffic.
func (m *Manager) DialPort(ctx context.Context, ref string, port uint16) (net.Conn, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return nil, err
	}
	if State(row.State) != StateRunning {
		if _, serr := m.Start(ctx, row.ID); serr != nil {
			return nil, fmt.Errorf("wake session for published port: %w", serr)
		}
	}
	handle := m.getHandle(row.ID)
	if handle == nil {
		return nil, errs.Newf(errs.CategoryFailedPrecondition,
			"session %q is not running", row.Name)
	}
	conn, err := handle.DialPort(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("dial session port: %w", err)
	}
	m.touch(ctx, row.ID)
	// The connection is in flight for its lifetime; keep the session busy (and so
	// safe from idle auto-stop) until it closes.
	m.markBusy(row.ID)
	id := row.ID
	return &busyConn{Conn: conn, release: func() { m.unmarkBusy(id) }}, nil
}

// Publish exposes a port the session serves, brokered by the daemon. It records
// the port and (when a broker is wired) opens the host-side tunnel forwarder.
// When public is set, the port is also served on the public HTTPS listener under
// host - which requires the public_web setting to be enabled and a valid host.
func (m *Manager) Publish(ctx context.Context, ref string, guestPort int, name string, public bool, host string) (PublishedPort, error) {
	if guestPort < 1 || guestPort > 65535 {
		return PublishedPort{}, errs.New(errs.CategoryInvalidArgument, "guest port must be between 1 and 65535")
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if public {
		if !m.opt().PublicWeb {
			return PublishedPort{}, errs.New(errs.CategoryFailedPrecondition,
				"public web serving is disabled; enable it with `fletcher settings set public_web true`")
		}
		if err := validatePublicHost(host); err != nil {
			return PublishedPort{}, errs.Newf(errs.CategoryInvalidArgument, "invalid --host: %s", err)
		}
	} else if host != "" {
		return PublishedPort{}, errs.New(errs.CategoryInvalidArgument, "--host is only valid with --public")
	}
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return PublishedPort{}, err
	}
	if _, gerr := m.q.GetPublishedPortBySessionPort(ctx, sqliteq.GetPublishedPortBySessionPortParams{
		SessionID: row.ID, GuestPort: int64(guestPort),
	}); gerr == nil {
		return PublishedPort{}, errs.Newf(errs.CategoryConflict,
			"port %d is already published for session %q", guestPort, row.Name)
	} else if !errors.Is(gerr, sql.ErrNoRows) {
		return PublishedPort{}, fmt.Errorf("check published port: %w", gerr)
	}
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("port-%d", guestPort)
	}

	id, err := typeid.WithPrefix(pubPortPrefix)
	if err != nil {
		return PublishedPort{}, fmt.Errorf("generate published port id: %w", err)
	}
	pp := PublishedPort{ID: id.String(), SessionID: row.ID, GuestPort: guestPort, Name: name, Public: public, Host: host}

	// Open the tunnel forwarder first so it assigns the tunnel port we then
	// persist. For a private port the tunnel is the only path, so a broker
	// failure is fatal; a public port still serves over HTTPS without it, so it
	// is best-effort there.
	if m.broker != nil {
		tunnelPort, berr := m.broker.Open(pp)
		switch {
		case berr != nil && !public:
			return PublishedPort{}, errs.Newf(errs.CategoryFailedPrecondition,
				"open port forwarder: %v", berr)
		case berr != nil:
			m.logger.Warn("published port: tunnel forwarder unavailable, public access only",
				slog.String("session_id", row.ID), slog.Int("guest_port", guestPort), slog.String("err", berr.Error()))
		default:
			pp.TunnelPort = tunnelPort
		}
	}

	var hostArg *string
	if host != "" {
		hostArg = &host
	}
	created, err := m.q.CreatePublishedPort(ctx, sqliteq.CreatePublishedPortParams{
		ID:         pp.ID,
		SessionID:  pp.SessionID,
		GuestPort:  int64(pp.GuestPort),
		Name:       pp.Name,
		TunnelPort: int64(pp.TunnelPort),
		Public:     boolToInt(public),
		Host:       hostArg,
		CreatedAt:  time.Now().Unix(),
	})
	if err != nil {
		if m.broker != nil {
			m.broker.Close(pp.ID)
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed") && host != "" {
			return PublishedPort{}, errs.Newf(errs.CategoryConflict, "host %q is already in use by another published port", host)
		}
		return PublishedPort{}, fmt.Errorf("record published port: %w", err)
	}
	return publishedFromRow(created), nil
}

// LookupPublicPort resolves a public hostname to its published port (session +
// guest port) for the public listener's routing and certmagic's on-demand TLS
// decision. Returns ErrNotFound when no public port claims the host.
func (m *Manager) LookupPublicPort(ctx context.Context, host string) (PublishedPort, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	row, err := m.q.GetPublishedPublicPortByHost(ctx, &h)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedPort{}, ErrNotFound
	}
	if err != nil {
		return PublishedPort{}, fmt.Errorf("lookup public port: %w", err)
	}
	return publishedFromRow(row), nil
}

// validatePublicHost checks host is a plausible bare DNS name (no scheme, port,
// or path) for a public published port.
func validatePublicHost(host string) error {
	if host == "" {
		return errors.New("a hostname is required for a public port (e.g. app.example.com)")
	}
	if len(host) > 253 {
		return errors.New("hostname is too long")
	}
	if strings.ContainsAny(host, "/:@ ") || strings.Contains(host, "..") {
		return errors.New("must be a bare hostname like app.example.com (no scheme, port, or path)")
	}
	if !strings.Contains(host, ".") {
		return errors.New("must be a fully-qualified domain name (e.g. app.example.com)")
	}
	for _, r := range host {
		if !isHostChar(r) {
			return errors.New("may contain only lowercase letters, digits, dots, and hyphens")
		}
	}
	return nil
}

// isHostChar reports whether r is allowed in a published hostname.
func isHostChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-'
}

// boolToInt maps a bool to the 0/1 SQLite stores for it.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Unpublish stops forwarding a session's published port and forgets it.
func (m *Manager) Unpublish(ctx context.Context, ref string, guestPort int) error {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return err
	}
	pp, gerr := m.q.GetPublishedPortBySessionPort(ctx, sqliteq.GetPublishedPortBySessionPortParams{
		SessionID: row.ID, GuestPort: int64(guestPort),
	})
	if errors.Is(gerr, sql.ErrNoRows) {
		return errs.Newf(errs.CategoryNotFound, "port %d is not published for session %q", guestPort, row.Name)
	}
	if gerr != nil {
		return fmt.Errorf("get published port: %w", gerr)
	}
	if _, derr := m.q.DeletePublishedPort(ctx, pp.ID); derr != nil {
		return fmt.Errorf("delete published port: %w", derr)
	}
	if m.broker != nil {
		m.broker.Close(pp.ID)
	}
	return nil
}

// ListPorts returns the session's published ports.
func (m *Manager) ListPorts(ctx context.Context, ref string) ([]PublishedPort, error) {
	row, err := m.lookup(ctx, ref)
	if err != nil {
		return nil, err
	}
	rows, err := m.q.ListPublishedPortsBySession(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("list published ports: %w", err)
	}
	out := make([]PublishedPort, len(rows))
	for i, r := range rows {
		out[i] = publishedFromRow(r)
	}
	return out, nil
}

// ReconcilePorts re-opens the host-side forwarders for every published port at
// daemon boot, reusing each port's stored tunnel port. Best-effort: a port that
// cannot be reopened (e.g. its tunnel port is taken) is logged, not fatal.
func (m *Manager) ReconcilePorts(ctx context.Context) error {
	if m.broker == nil {
		return nil
	}
	rows, err := m.q.ListPublishedPorts(ctx)
	if err != nil {
		return fmt.Errorf("list published ports: %w", err)
	}
	for _, r := range rows {
		pp := publishedFromRow(r)
		if _, oerr := m.broker.Open(pp); oerr != nil {
			m.logger.Warn("reopen published port",
				slog.String("session_id", pp.SessionID),
				slog.Int("guest_port", pp.GuestPort),
				slog.String("err", oerr.Error()))
		}
	}
	return nil
}

// closeSessionPorts closes the broker forwarders for a session's published
// ports (used on delete, before the cascade removes the rows).
func (m *Manager) closeSessionPorts(ctx context.Context, sessionID string) {
	if m.broker == nil {
		return
	}
	rows, err := m.q.ListPublishedPortsBySession(ctx, sessionID)
	if err != nil {
		m.logger.Warn("list published ports on delete", slog.String("session_id", sessionID), slog.String("err", err.Error()))
		return
	}
	for _, r := range rows {
		m.broker.Close(r.ID)
	}
}

// startLock returns the per-session mutex serialising Start for that id.
func (m *Manager) startLock(id string) *sync.Mutex {
	v, _ := m.startLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// ReconcileOnBoot marks every "running" session as "stopped" at daemon start:
// their VMs died with the previous daemon process. The forks persist, so the
// operator can start them again. Mirrors the job supervisor's boot reconcile.
func (m *Manager) ReconcileOnBoot(ctx context.Context) error {
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("reconcile sessions: %w", err)
	}
	for _, r := range rows {
		if State(r.State) != StateRunning {
			continue
		}
		if err := m.setState(ctx, r.ID, StateStopped); err != nil {
			return err
		}
		m.logger.Info("reset orphaned running session to stopped", slog.String("session_id", r.ID))
	}
	return nil
}

// StartDeployedOnBoot boots every app session (run_app) after a daemon restart,
// so a deployed app comes back on its own rather than waiting for an inbound
// request to wake it. Best-effort: a session that fails to boot is logged and
// skipped. Call after ReconcileOnBoot has reset stale running rows to stopped.
func (m *Manager) StartDeployedOnBoot(ctx context.Context) {
	if m.runtime == nil {
		return
	}
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		m.logger.Warn("start deployed sessions on boot", slog.String("err", err.Error()))
		return
	}
	for _, r := range rows {
		if r.RunApp == 0 {
			continue
		}
		if _, serr := m.Start(ctx, r.ID); serr != nil {
			m.logger.Warn("auto-start deployed app session on boot",
				slog.String("session_id", r.ID), slog.String("name", r.Name), slog.String("err", serr.Error()))
			continue
		}
		m.logger.Info("auto-started deployed app session on boot", slog.String("name", r.Name))
	}
}

// lookup resolves ref (id or name) to a row, returning ErrNotFound if missing.
func (m *Manager) lookup(ctx context.Context, ref string) (sqliteq.Session, error) {
	row, err := m.q.GetSessionByRef(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return sqliteq.Session{}, ErrNotFound
	}
	if err != nil {
		return sqliteq.Session{}, fmt.Errorf("get session: %w", err)
	}
	return row, nil
}

func (m *Manager) setState(ctx context.Context, id string, state State) error {
	if err := m.q.UpdateSessionState(ctx, sqliteq.UpdateSessionStateParams{
		State:     string(state),
		UpdatedAt: time.Now().Unix(),
		ID:        id,
	}); err != nil {
		return fmt.Errorf("update session state: %w", err)
	}
	return nil
}

func (m *Manager) touch(ctx context.Context, id string) {
	now := time.Now().Unix()
	if err := m.q.TouchSession(ctx, sqliteq.TouchSessionParams{
		LastUsedAt: &now,
		UpdatedAt:  now,
		ID:         id,
	}); err != nil {
		m.logger.Debug("touch session", slog.String("session_id", id), slog.String("err", err.Error()))
	}
}

func (m *Manager) putHandle(id string, h runtime.SessionHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handles[id] = h
}

func (m *Manager) getHandle(id string) runtime.SessionHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handles[id]
}

func (m *Manager) takeHandle(id string) runtime.SessionHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.handles[id]
	delete(m.handles, id)
	return h
}

// ReapIdle stops (hibernates) every running session with no work in flight that
// has been idle longer than the configured timeout. "Work in flight" means an
// active host operation (exec/shell/ssh) or a busy guest (load average), so a
// running agent or task is never stopped mid-work, even with no user attached.
// It is a no-op when the idle timeout is disabled.
func (m *Manager) ReapIdle(ctx context.Context) (int, error) {
	if m.opt().IdleTimeout <= 0 {
		return 0, nil
	}
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap idle sessions: %w", err)
	}
	stopped := 0
	for _, r := range rows {
		if State(r.State) != StateRunning || m.busyCount(r.ID) > 0 {
			continue
		}
		handle := m.getHandle(r.ID)
		if handle == nil {
			continue
		}
		if load, lerr := handle.Load(ctx); lerr == nil && load >= idleLoadThreshold {
			m.touch(ctx, r.ID) // working with no host op attached; reset the idle clock
			continue
		}
		if time.Since(lastActivity(r)) < m.opt().IdleTimeout {
			continue
		}
		if _, serr := m.Stop(ctx, r.ID); serr != nil {
			m.logger.Warn("auto-stop idle session", slog.String("session_id", r.ID), slog.String("err", serr.Error()))
			continue
		}
		m.logger.Info("auto-stopped idle session", slog.String("session_id", r.ID), slog.String("name", r.Name))
		stopped++
	}
	return stopped, nil
}

// checkCaps refuses a new session when a configured count or disk cap is hit,
// reporting what is using the space rather than deleting anything.
func (m *Manager) checkCaps(ctx context.Context) error {
	if m.opt().MaxCount <= 0 && m.opt().MaxDiskBytes <= 0 {
		return nil
	}
	rows, err := m.q.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("check session caps: %w", err)
	}
	if m.opt().MaxCount > 0 && len(rows) >= m.opt().MaxCount {
		return errs.Newf(errs.CategoryFailedPrecondition,
			"session count cap reached (%d/%d). Delete one to make room:\n%s",
			len(rows), m.opt().MaxCount, usageReport(rows))
	}
	if m.opt().MaxDiskBytes > 0 {
		var total int64
		for _, r := range rows {
			total += forkBytes(r.ForkPath)
		}
		if total >= m.opt().MaxDiskBytes {
			return errs.Newf(errs.CategoryFailedPrecondition,
				"session disk cap reached (%d/%d GiB). Delete one to make room:\n%s",
				total>>30, m.opt().MaxDiskBytes>>30, usageReport(rows))
		}
	}
	return nil
}

// usageReport lists each session's disk use and last-used time for a cap error.
func usageReport(rows []sqliteq.Session) string {
	var b strings.Builder
	for _, r := range rows {
		last := "never"
		if r.LastUsedAt != nil {
			last = time.Unix(*r.LastUsedAt, 0).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "  %s  %s  %d MiB  last used %s\n",
			r.Name, r.State, forkBytes(r.ForkPath)>>20, last)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Manager) markBusy(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy[id]++
}

func (m *Manager) unmarkBusy(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[id] <= 1 {
		delete(m.busy, id)
		return
	}
	m.busy[id]--
}

func (m *Manager) busyCount(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[id]
}

// lastActivity is when a session was last used, falling back to when it last
// changed state (its start) for one that has never had an operation.
func lastActivity(r sqliteq.Session) time.Time {
	if r.LastUsedAt != nil {
		return time.Unix(*r.LastUsedAt, 0)
	}
	return time.Unix(r.UpdatedAt, 0)
}

// forkBytes is the on-disk size of a session fork, 0 if it cannot be stat-ed.
func forkBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// busyConn keeps a session marked busy until the SSH connection it wraps closes.
type busyConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *busyConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

func sessionFromRow(r sqliteq.Session) Session {
	s := Session{
		ID:           r.ID,
		Name:         r.Name,
		Image:        r.Image,
		State:        State(r.State),
		CreatedAt:    time.Unix(r.CreatedAt, 0),
		UpdatedAt:    time.Unix(r.UpdatedAt, 0),
		LastUsedAt:   timePtrFromUnix(r.LastUsedAt),
		DiskBytes:    forkBytes(r.ForkPath),
		EgressPolicy: r.EgressPolicy,
		RunApp:       r.RunApp != 0,
		Gateway:      r.Gateway,
		HasRollback:  r.PrevForkID != nil,
	}
	if r.VolumeID != nil {
		s.VolumeID = *r.VolumeID
	}
	return s
}

func publishedFromRow(r sqliteq.PublishedPort) PublishedPort {
	host := ""
	if r.Host != nil {
		host = *r.Host
	}
	return PublishedPort{
		ID:         r.ID,
		SessionID:  r.SessionID,
		GuestPort:  int(r.GuestPort),
		Name:       r.Name,
		TunnelPort: int(r.TunnelPort),
		Public:     r.Public != 0,
		Host:       host,
		CreatedAt:  time.Unix(r.CreatedAt, 0),
	}
}

func timePtrFromUnix(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := time.Unix(*v, 0)
	return &t
}
