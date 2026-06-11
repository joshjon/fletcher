// Package settings stores runtime-mutable operational settings in the daemon's
// SQLite database. They are edited via `fletcher settings` and applied over the
// flag/env config at daemon startup; changes take effect on the next restart.
// Bootstrap config (database, socket, age key) is not managed here - it is
// needed to open the database these settings live in. See STANDARDS.md
// sections 95/98.
package settings

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joshjon/fletcher/internal/errs"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// Known setting keys. The same names are used as the stored keys and by the
// daemon when applying settings over its config.
const (
	KeyRuntime         = "runtime"
	KeySnapshot        = "snapshot"
	KeyBtrfsRoot       = "btrfs_root"
	KeyPublicEndpoint  = "public_endpoint"
	KeyWireGuardPort   = "wireguard_port"
	KeyPairingPort     = "pairing_port"
	KeyLogLevel        = "log_level"
	KeyCredentialsDir  = "credentials_dir"
	KeyNoUPnP          = "no_upnp"
	KeyGatewayListen   = "gateway_listen"
	KeyMCPListen       = "mcp_listen"
	KeyRemoteAPIListen = "remote_api_listen"

	KeySessionIdleTimeout = "session_idle_timeout"
	KeySessionMaxCount    = "session_max_count"
	KeySessionMaxDiskGB   = "session_max_disk_gb"

	KeyDefaultImage = "default_image"
	KeyDefaultAgent = "default_agent"

	KeyDefaultEgressPolicy = "default_egress_policy"

	KeyVMMemoryMB = "vm_memory_mb"

	KeyDefaultGateway = "default_gateway"

	KeyPublicWeb   = "public_web"
	KeyACMEStaging = "acme_staging"
	KeyACMEEmail   = "acme_email"

	KeyAPNSKeyPath     = "apns_key_path"
	KeyAPNSKeyID       = "apns_key_id"
	KeyAPNSTeamID      = "apns_team_id"
	KeyAPNSTopic       = "apns_topic"
	KeyAPNSEnvironment = "apns_environment"
)

// definition describes a settable key: its help text and an optional validator.
type definition struct {
	key         string
	description string
	validate    func(string) error
}

var registry = []definition{
	{KeyRuntime, "runtime driver: mock | runc | firecracker", oneOf("mock", "runc", "firecracker")},
	{KeySnapshot, "snapshot driver: mock | btrfs | ext4", oneOf("mock", "btrfs", "ext4")},
	{KeyBtrfsRoot, "btrfs snapshot root directory", nil},
	{KeyPublicEndpoint, "host:port peers dial to reach this daemon", nil},
	{KeyWireGuardPort, "WireGuard UDP listen port, 1-65535", portNumber},
	{KeyPairingPort, "public TCP port the iOS app dials to complete pairing (TLS, self-signed cert pinned via QR), 1-65535", portNumber},
	{KeyLogLevel, "log level: debug | info | warn | error", oneOf("debug", "info", "warn", "error")},
	{KeyCredentialsDir, "host directory for trusted-credential mounts", nil},
	{KeyNoUPnP, "disable the automatic UPnP port-forward: true | false", oneOf("true", "false")},
	{KeyGatewayListen, "model gateway listen address, host:port", hostPort},
	{KeyMCPListen, "MCP server listen address, host:port", hostPort},
	{KeyRemoteAPIListen, "Mode B: extra host:port to expose the token-gated API on, beyond the WireGuard tunnel (e.g. your Tailscale IP)", hostPort},
	{KeySessionIdleTimeout, "auto-stop a session idle (no work in flight) this long, e.g. 30m; 0 disables", durationOrZero},
	{KeySessionMaxCount, "maximum number of sessions; 0 disables the cap", nonNegInt},
	{KeySessionMaxDiskGB, "maximum total session disk in GB; 0 disables the cap", nonNegInt},
	{KeyDefaultImage, "base image used by `job`/`session create` when --image is omitted; empty makes --image required", nil},
	{KeyDefaultAgent, "agent the app's create form suggests by default: pi | claude | codex (a hint for clients; the agent itself is baked into the image)", nil},
	{KeyDefaultEgressPolicy, "default fork egress policy when --egress is omitted: none | allowlist | open", oneOf("none", "allowlist", "open")},
	{KeyVMMemoryMB, "per-VM guest memory in MB for job/session microVMs (default 2048); an interactive agent needs well over 512", nonNegInt},
	{KeyDefaultGateway, "default model-gateway wiring when --gateway is omitted: on (inject gateway env) | off (agent uses its own auth)", oneOf("on", "off")},
	{KeyPublicWeb, "expose `session publish --public` ports on the public internet over HTTPS (binds 443/80): true | false", oneOf("true", "false")},
	{KeyACMEStaging, "use Let's Encrypt's staging CA for public TLS certs (untrusted, but no rate limits - for testing): true | false", oneOf("true", "false")},
	{KeyACMEEmail, "contact email for the ACME account used to issue public TLS certs (optional)", nil},
	{KeyAPNSKeyPath, "path to the APNs auth key (.p8) on the box, for pushing approval notifications to the iOS app (empty disables push)", nil},
	{KeyAPNSKeyID, "the APNs auth key's ID (Apple Developer)", nil},
	{KeyAPNSTeamID, "the Apple Developer team ID", nil},
	{KeyAPNSTopic, "the iOS app's bundle ID (APNs topic)", nil},
	{KeyAPNSEnvironment, "APNs environment: production | sandbox", oneOf("production", "sandbox")},
}

// liveKeys are the settings the daemon can re-apply to running components
// without a restart (via ReloadSettings). Everything else is bound at boot
// (listeners, drivers, the tunnel, certs, log level, VM memory) and takes
// effect only on the next start.
var liveKeys = map[string]bool{
	KeyDefaultImage:        true,
	KeyDefaultAgent:        true, // app-facing hint, no daemon component to restart
	KeyDefaultEgressPolicy: true,
	KeyDefaultGateway:      true,
	KeySessionIdleTimeout:  true,
	KeySessionMaxCount:     true,
	KeySessionMaxDiskGB:    true,
}

// RequiresRestart reports whether changing key takes effect only after a daemon
// restart (true), rather than being applied live by ReloadSettings (false).
// Unknown keys default to true - safer to ask for a restart than to silently
// not apply.
func RequiresRestart(key string) bool {
	return !liveKeys[key]
}

// LiveKeys returns the setting keys ReloadSettings applies live, sorted.
func LiveKeys() []string {
	out := make([]string, 0, len(liveKeys))
	for k := range liveKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// View is one setting's full picture for `fletcher settings list`.
type View struct {
	Key         string
	Value       string
	Description string
	Set         bool
	// RequiresRestart is true when the key only takes effect on the next daemon
	// start; false when ReloadSettings can apply it live.
	RequiresRestart bool
}

// Store persists settings over the generated SQLite queries.
type Store struct {
	q sqliteq.Querier
}

// NewStore builds a Store backed by q.
func NewStore(q sqliteq.Querier) *Store {
	return &Store{q: q}
}

// Set validates and upserts a setting. Unknown keys and invalid values are
// rejected so a typo cannot silently persist.
func (s *Store) Set(ctx context.Context, key, value string) error {
	def, ok := lookup(key)
	if !ok {
		return errs.Newf(errs.CategoryInvalidArgument, "unknown setting %q (known: %s)", key, strings.Join(names(), ", "))
	}
	if def.validate != nil {
		if err := def.validate(value); err != nil {
			return errs.Newf(errs.CategoryInvalidArgument, "invalid value for %q: %s", key, err)
		}
	}
	return s.q.UpsertSetting(ctx, sqliteq.UpsertSettingParams{
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now().Unix(),
	})
}

// Delete removes a setting, reporting whether it existed.
func (s *Store) Delete(ctx context.Context, key string) (bool, error) {
	n, err := s.q.DeleteSetting(ctx, key)
	return n > 0, err
}

// Values returns the explicitly-set settings as a key->value map.
func (s *Store) Values(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// Describe returns every known key with its current value (if set) and help.
func (s *Store) Describe(ctx context.Context) ([]View, error) {
	vals, err := s.Values(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(registry))
	for _, d := range registry {
		v, set := vals[d.key]
		out = append(out, View{
			Key:             d.key,
			Value:           v,
			Description:     d.description,
			Set:             set,
			RequiresRestart: RequiresRestart(d.key),
		})
	}
	return out, nil
}

func lookup(key string) (definition, bool) {
	for _, d := range registry {
		if d.key == key {
			return d, true
		}
	}
	return definition{}, false
}

func names() []string {
	out := make([]string, len(registry))
	for i, d := range registry {
		out[i] = d.key
	}
	return out
}

func oneOf(allowed ...string) func(string) error {
	return func(v string) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(allowed, ", "))
	}
}

func portNumber(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("must be a port number 1-65535")
	}
	return nil
}

func hostPort(v string) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("must be host:port, e.g. 0.0.0.0:11500")
	}
	if host == "" {
		return fmt.Errorf("host part is required, e.g. 0.0.0.0:11500")
	}
	return portNumber(port)
}

func durationOrZero(v string) error {
	if v == "0" {
		return nil
	}
	if _, err := time.ParseDuration(v); err != nil {
		return fmt.Errorf("must be a duration like 30m or 2h (0 to disable)")
	}
	return nil
}

func nonNegInt(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fmt.Errorf("must be a non-negative integer")
	}
	return nil
}
