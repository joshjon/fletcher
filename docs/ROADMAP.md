# Fletcher Roadmap and Status

Status tracker for the phases defined in `DESIGN.md` §13. That section is the
plan of record: what each phase means and why. This file tracks *delivery* -
what is actually built, what was deliberately cut and the reason, and what
comes next.

Why a separate file: §13 describes intent and intentionally does not change as
work lands. Without a status layer, "phase 16 shipped" hides the corners that
were cut inside earlier phases (e.g. the settings table that §13 never had a
row for at all). This file is that layer, and it is meant to be edited as
state changes.

Verdict legend:

- **DONE** - implemented and working.
- **PARTIAL** - core path works; named pieces deferred.
- **STUB** - scaffolding/interface exists, returns "not implemented".
- **DEFERRED** - intentionally not built yet, with a reason and a fallback.
- **MISSING** - specified somewhere but not built and not tracked until now.

## Status at a glance

| # | Phase | Verdict | Cut corner (see notes) |
|---|-------|---------|------------------------|
| 0 | Foundations | DONE | - |
| 1 | Skeleton daemon | DONE | - |
| 2 | Job model + storage | DONE | - |
| 3 | Mock runtime + supervisor + resume | DONE | - |
| 4 | Trust-boundary plumbing | DONE | Audit recorder is `Noop`; no storage |
| 5 | Model gateway (basic) | PARTIAL | Anthropic only; non-stream `/v1/chat/completions` |
| 6 | MCP server | PARTIAL | 3 demo tools; egress validation permissive |
| 7 | Approvals | DONE | APNs push deferred (polling `Wait` instead) |
| 8 | Real Linux runtime | PARTIAL | runc + btrfs real; **Firecracker is a stub** |
| 9 | Networking | PARTIAL | UPnP only (no NAT-PMP/PCP); **no DDNS** |
| 10 | v0.1.0 polish | PARTIAL | Release tooling ready; **no tag cut yet** |
| 11 | Base image (`fletcher-base`) | DONE | pi-extension is a skeleton (see phase 14) |
| 12 | Trusted-credential mode | DONE | - |
| 13 | Anthropic-native inbound | DONE | - |
| 14 | Model catalog | PARTIAL | Catalog + CLI real; pi-extension skeleton |
| 15 | Zero-touch networking | DONE | Falls back to manual endpoint when UPnP absent |
| 16 | `fletcher doctor` | DONE | - |
| - | **Runtime settings (`settings` table + CLI)** | **MISSING** | Specified in STANDARDS §98 / DESIGN §13; never built |

## Phases 0-16: what landed, and what was cut

The vertical-slice strategy (DESIGN.md §13) drove the order: build the thinnest
end-to-end job path on mock drivers, then swap in real implementations. That is
why some horizontal concerns (audit storage, settings, a second model provider)
were left as seams: the path that proves the architecture did not need them yet.
The cut corners below are the price of that strategy. Most are deliberate and
documented in code; one (settings) fell through the cracks.

### Deliberate cuts, with fallbacks

- **Audit storage (phase 4).** `internal/audit` defines `Event` + `Recorder`;
  every MCP tool call routes through it (`internal/mcp/server.go`), but the
  daemon wires `audit.Noop{}` (`internal/daemon/daemon.go`). The seam exists so
  a SQLite-backed recorder drops in without touching call sites. *Why cut:* the
  trust boundary is provable with the seam in place; storage is additive.

- **Gateway breadth (phase 5).** One backend (Anthropic). `/v1/messages`
  passthrough streams fine (SSE copied through); `/v1/chat/completions`
  translation rejects streaming (`internal/gateway/anthropic.go`) and there is
  no second provider. *Why cut:* the credential-stamping boundary is the load-
  bearing part; more providers are repetition, not risk.

- **MCP egress hardening (phase 6).** `validateEgressURL` is intentionally
  permissive (`internal/mcp/tools.go`); no SSRF/loopback/metadata guard yet.
  *Why cut:* the comment ties hardening to egress approvals, which are not built.

- **APNs push (phase 7).** Approvals persist and `Wait()` blocks until a
  terminal decision; there is just no push notification. *Why cut:* polling is a
  complete fallback; push needs Apple plumbing out of band.

- **Firecracker (phase 8).** `internal/runtime/firecrackerdriver` returns
  `errNotImplemented`. runc (`runcdriver_linux.go`) and btrfs
  (`btrfsdriver_linux.go`) are real. *Why cut:* the full OCI-to-rootfs + VM
  lifecycle + vsock pipeline is load-bearing and must be verified on real
  Linux + KVM before it is claimed (DESIGN.md §11 open questions). runc is the
  labelled degraded-isolation path in the meantime.

- **NAT-PMP / PCP (phase 9).** Only UPnP IGD is implemented
  (`internal/network/portmap`); the `Method` field is shaped for the others.
  *Why cut:* UPnP covers the common home router; the rest are follow-ups.

- **pi-extension (phases 11/14).** `images/fletcher-base/pi-extension/index.ts`
  fetches the catalog on startup but `registerProvider()` is a TODO pending a
  pinned `pi` API version. The `/v1/catalog.json` surface it consumes is done.
  *Why cut:* the published catalog endpoint is the contract; the extension is a
  client that depends on an external project's API stabilising.

### Genuine gaps (not previously tracked)

- **DDNS (phase 9).** §13 lists DDNS under networking; there is no
  implementation and no deferral note. A dynamic public IP currently means the
  operator re-sets the endpoint by hand. Tracked in Backlog below.

- **No v0.1.0 tag (phase 10).** `.goreleaser.yaml`, `scripts/install.sh`, and
  the systemd unit are all real, but no release has been cut (`git tag` is
  empty). The installer fetches "latest release", which does not exist yet, so
  end-user install is blocked on cutting the tag.

- **Runtime settings table (untracked entirely).** STANDARDS.md §98 and the
  DESIGN.md §13 stack row both specify: *"Runtime-mutable settings live in a
  SQLite `settings` table; edited via `fletcher settings get|set|list`."* None
  of it exists - no migration, no table, no queries, no command. Every knob is
  an env var read once at `serve` startup (`cmd/fletcher/serve.go`), so changing
  one means editing the systemd unit and restarting. This is the root of the
  "why do I keep running `systemctl restart`" friction and is the subject of the
  next phase.

## Next phases (proposed)

These are derived from real usage, not speculation - which is the bar DESIGN.md
§13 sets for going past phase 16 ("anything past them should wait for actual
usage"). The first deployment surfaced two concrete frictions: config changes
require a restart, and a restart requires `systemctl`. Phases 17 and 18 address
exactly those, in that order, because 17 removes most of the need for 18.

### Phase 17: Runtime settings in SQLite + hot reload

**Problem.** Operational config (public endpoint, WireGuard port, UPnP on/off,
log level) is env-only and read once at boot. Changing any value is a
`systemctl edit` + `systemctl restart`, which (a) demands systemctl knowledge,
(b) bounces the daemon - dropping in-flight jobs and the WireGuard tunnel - for
what is often a one-line change, and (c) leaves no record of what was changed.

**Why now.** It is the highest-leverage fix for the observed friction, and it is
not new scope: STANDARDS.md §98 and DESIGN.md §13 already specify it. This phase
closes a known gap rather than inventing one.

**Design.**

- Draw the line at STANDARDS §95 vs §98. *Bootstrap* config - where the DB,
  socket, and age key live, the listen addresses, the driver selection - stays
  flag/env/TOML, because the daemon needs it to start at all and swapping it on
  a live process is unsafe or meaningless. *Operational* knobs move into a
  `settings` table. Only the second set is runtime-mutable.

- Schema: migration `0007_settings` adds a `STRICT` `settings(key TEXT PRIMARY
  KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL)`. sqlc queries
  `GetSetting` / `ListSettings` / `UpsertSetting` / `DeleteSetting`.

- `internal/settings`: a typed accessor over the table backed by a registry of
  known keys. Each registry entry declares name, type, default, a one-line
  description, and a reload class:
  - **live** - applied immediately (e.g. `log_level` adjusts the slog level in
    place).
  - **subsystem-bounce** - re-runs one oklog/run actor (e.g.
    `public_endpoint`, `wireguard_port`, `upnp_enabled` bounce only the network
    actor via the existing `bringUpNetwork` / `tryUPnP` paths).
  - **on-restart** - persisted but only read at next boot; the CLI says so
    explicitly when set.
  Unknown keys are rejected. Values are validated on `set` (a bad endpoint or
  out-of-range port fails fast, before persistence).

- Precedence for runtime-mutable keys: effective value = settings-table value if
  present, else the boot config (flag/env/TOML/default). Boot config still seeds
  a fresh install; the table overrides live without losing the bootstrap path.

- Surface: a `SettingsService` Connect RPC (mirroring the secrets/approvals
  services) and a `fletcher settings get|set|list` command. `list` shows the
  effective value and its source, so `fletcher settings list` doubles as "what
  is this daemon actually running with".

**Acceptance.** Setting `public_endpoint` with `fletcher settings set` brings the
tunnel up on the new endpoint with no restart and no systemctl; `fletcher
settings list` reflects it; the value survives reboot; an invalid value is
rejected with a categorised error.

**Off-thesis check.** None. Entirely local, no hosting, no metering, no new
networking plane.

### Phase 18: `fletcher daemon` lifecycle facade over systemd

**Problem.** Even after phase 17, a few actions genuinely need the init system:
enable-on-boot, start, stop, restart (for `on-restart` settings and binary
upgrades), and viewing logs. End users should not have to learn `systemctl` /
`journalctl` to do them.

**Why now / why thin.** systemd stays the supervisor - it owns boot persistence,
crash-restart, the unit's sandboxing, and the `CAP_NET_ADMIN` grant. Re-creating
that inside `fletcher serve` (daemonize, pidfile, restart-on-crash, boot
integration) would reinvent init and put orchestration in core, which CLAUDE.md
and DESIGN.md §5 rule out. So this phase is a *thin facade*, not a supervisor.

**Design.**

- `cmd/fletcher/daemon.go`: `fletcher daemon start|stop|restart|status|logs|
  enable|disable` shelling out to `systemctl` / `journalctl -u fletcher`,
  prompting for sudo only when the action needs it.
- Detect non-systemd hosts and degrade to printing the manual equivalent rather
  than failing opaquely.
- Reuse `doctor`'s existing systemctl remediation strings so the two never drift.

**Acceptance.** A user who has never typed `systemctl` can install, start,
inspect, and tail Fletcher entirely through `fletcher` verbs.

**Off-thesis check.** None.

## Backlog (not scheduled - awaiting a usage signal)

Per DESIGN.md §13, these wait for real demand rather than being pre-planned.
Listed so they are visible, not lost:

- **Cut v0.1.0** - the one thing blocking `curl | sh` install for anyone else.
- **Image-to-rootfs flatten tooling** - bridge phase 11's `fletcher-base` OCI
  image into the btrfs `images/<name>` template the runc/btrfs path runs against
  (`internal/snapshot/btrfsdriver`). Without it a real-runtime job has no rootfs
  to `exec`, so end-to-end runc + btrfs testing needs a manual rootfs workaround
  (see `docs/TESTING.md`). This is the missing seam between phase 11 and phase 8.
- **Firecracker driver** - the real-isolation runtime; needs a KVM host to build
  against (DESIGN.md §11).
- **Audit log storage** - swap `audit.Noop` for a SQLite recorder.
- **MCP egress hardening + approvals** - SSRF guard, then policy-gated egress.
- **Gateway breadth** - streaming in the translation path; a second provider.
- **DDNS** - for operators on a dynamic public IP.
- **NAT-PMP / PCP** - port mapping for routers that refuse UPnP.
- **pi-extension `registerProvider`** - once the `pi` extension API is pinned.
