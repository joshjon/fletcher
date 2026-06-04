# Fletcher Roadmap and Status

The single source of truth for where Fletcher actually stands: what is built,
what was deliberately cut and why, the plan from here, and every known gap.
`DESIGN.md` §13 is the plan of record for *what each phase means*; this file
tracks *delivery and the path forward*. If a plan, decision, milestone, or gap
exists, it belongs here - nothing should live only in a conversation.

This file is meant to be edited as state changes. Keep it current.

Verdict legend:

- **DONE** - implemented and working.
- **PARTIAL** - core path works; named pieces deferred.
- **STUB** - scaffolding/interface exists, returns "not implemented".
- **DEFERRED** - intentionally not built yet, with a reason and a fallback.
- **MISSING** - specified somewhere but not built.
- **SHIPPED** / **NEXT** / **PLANNED** - milestone states (see Execution plan).

## Where it stands today

What runs end-to-end **on the mock runtime**: the daemon, job model + supervisor
+ resume, secrets (age), the model gateway (real Anthropic proxy with the key
stamped daemon-side), the MCP server, approvals, WireGuard pairing over the
tunnel, and `fletcher doctor`. The mock runtime executes a job's command as a
plain host subprocess - no isolation, no image - so it proves the plumbing, not
the product.

What is newly runnable (Milestone 1, `fdc90d3`) but **not yet verified on
hardware**: the real **runc + btrfs** path - a job running in a container fork
on a copy-on-write snapshot, with a base-image rootfs imported via
`fletcher image import`.

What is **not** possible yet, and is the difference between "smoke test" and
"the way users will use it":

- A real agent (claude/codex/pi) verified running inside a fork (Milestone 2).
- Changing config or lifecycle without `systemctl` (Milestone 3).
- A remote client reaching the daemon over the tunnel - the API is bound to a
  local Unix socket only (Milestone 4).
- Firecracker microVMs - the intended default isolation tier (Milestone 5).

## Status at a glance (phases 0-16)

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
| 8 | Real Linux runtime | PARTIAL | runc + btrfs real (runnable as of M1); **Firecracker is a stub** |
| 9 | Networking | PARTIAL | UPnP only (no NAT-PMP/PCP); **no DDNS** |
| 10 | v0.1.0 polish | PARTIAL | Release tooling ready; **no tag cut yet** |
| 11 | Base image (`fletcher-base`) | DONE | pi-extension is a skeleton (see phase 14) |
| 12 | Trusted-credential mode | DONE | Bind-mounts blocked by `ProtectHome` until M2 |
| 13 | Anthropic-native inbound | DONE | - |
| 14 | Model catalog | PARTIAL | Catalog + CLI real; pi-extension skeleton |
| 15 | Zero-touch networking | DONE | Falls back to manual endpoint when UPnP absent |
| 16 | `fletcher doctor` | DONE | No runtime-prereq checks (btrfs/runc); see Backlog |
| - | Runtime settings (`settings` table + CLI) | MISSING | Specified in STANDARDS §98 / DESIGN §13; now Milestone 3 |

## Phases 0-16: what landed, and what was cut

The vertical-slice strategy (DESIGN.md §13) drove the order: build the thinnest
end-to-end job path on mock drivers, then swap in real implementations. That is
why some horizontal concerns (audit storage, settings, a second model provider)
were left as seams - the path that proves the architecture did not need them
yet. The cut corners below are the price of that strategy. Most are deliberate
and documented in code; one (settings) fell through the cracks.

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
  Linux + KVM before it is claimed (DESIGN.md §11). runc is the labelled
  degraded-isolation path in the meantime. Now Milestone 5.

- **NAT-PMP / PCP (phase 9).** Only UPnP IGD is implemented
  (`internal/network/portmap`); the `Method` field is shaped for the others.
  *Why cut:* UPnP covers the common home router; the rest are follow-ups.

- **pi-extension (phases 11/14).** `images/fletcher-base/pi-extension/index.ts`
  fetches the catalog on startup but `registerProvider()` is a TODO pending a
  pinned `pi` API version. The `/v1/catalog.json` surface it consumes is done.
  *Why cut:* the published catalog endpoint is the contract; the extension is a
  client that depends on an external project's API stabilising.

### Genuine gaps (were untracked until this file)

- **DDNS (phase 9).** §13 lists DDNS under networking; there is no
  implementation and no deferral note. A dynamic public IP means the operator
  re-sets the endpoint by hand. In Backlog.

- **No v0.1.0 tag (phase 10).** `.goreleaser.yaml`, `scripts/install.sh`, and
  the systemd unit are all real, but no release has been cut (`git tag` is
  empty). The installer fetches "latest release", which does not exist yet, so
  `curl | sh` install for anyone else is blocked on cutting the tag.

- **Runtime settings table.** STANDARDS.md §98 and the DESIGN.md §13 stack row
  both specify a SQLite `settings` table edited via `fletcher settings`. None of
  it exists; every knob is an env var read once at `serve` startup
  (`cmd/fletcher/serve.go`), so changing one means editing the systemd unit and
  restarting. This is the root of the `systemctl restart` friction and is now
  Milestone 3.

## Execution plan: milestones (post-phase-16)

Phases 0-16 were the vertical slice that proved the architecture. These
milestones are the path from there to a deployment that works *the way users
will use it*, derived from the first real deployment's friction rather than
speculation (the bar DESIGN.md §13 sets for going past phase 16). They are
sequenced by dependency and risk: prove the core loop on the simpler runtime
first, make it ergonomic, expose it to clients, then upgrade the isolation
tier. Building Firecracker first would mean debugging the VM layer and an
unproven job/fork/agent/gateway loop at once.

### Milestone 1 - Real isolated execution on runc - SHIPPED (`fdc90d3`)

**Goal.** A job runs in a real container fork (runc) on a real copy-on-write
snapshot (btrfs), instead of the mock driver's bare subprocess.

**Delivered.**

- `fletcher image import <docker-ref>` flattens a built OCI image (e.g.
  `fletcher-base:dev` from `make image`) into a btrfs subvolume at
  `<btrfs-root>/images/<name>`, so `fletcher job create --image <name>` has a
  real rootfs. Plus `fletcher image ls` / `rm`. Interim `docker export` bridge
  until the firecracker-containerd OCI pull pipeline lands (DESIGN.md §3).
- The systemd unit grants the daemon `CAP_SYS_ADMIN` (btrfs subvolume
  create/snapshot, runc namespaces) alongside `CAP_NET_ADMIN`.
- `docs/TESTING.md` Test 2 documents the flow and the snapshot-root ownership
  the daemon needs.

**Known debt carried forward:** `CAP_SYS_ADMIN` is broad (hardening to
rootless-runc + user namespaces is in Backlog); first on-hardware verification
still pending.

### Milestone 2 - Run a real agent in the fork + prove the gateway - NEXT

**Goal.** An actual agent (claude/codex/pi) runs inside the fork against the
daemon gateway, with credentials never entering the fork. This is the product:
private agent compute.

**Anticipated scope.**

- The unit's `MemoryDenyWriteExecute=true` likely breaks Node-based agents
  (W^X) and `ProtectHome=true` blocks the Phase 12 trusted-credential bind
  mounts from the operator's home. Both need revisiting for real agents.
- Verify gateway base-URL injection reaches the agent inside the fork and that
  `--credential claude` (or an API key via the gateway) works end-to-end.

### Milestone 3 - Ergonomics: no more systemctl - PLANNED

Folds the previously-separate Phase 17 and Phase 18. Removes the operational
friction the first deployment hit. Part A removes most restarts; Part B covers
the rest.

#### Part A - Runtime settings in SQLite + hot reload

**Problem.** Operational config (public endpoint, WireGuard port, UPnP on/off,
log level, driver selection) is env-only and read once at boot. Changing a value
is `systemctl edit` + `systemctl restart`, which demands systemctl knowledge,
bounces the daemon (dropping in-flight jobs and the tunnel) for a one-line
change, and leaves no record.

**Design.**

- Draw the line at STANDARDS §95 vs §98. *Bootstrap* config (where the DB,
  socket, age key live; listen addresses) stays flag/env/TOML - the daemon needs
  it to start and swapping it live is unsafe. *Operational* knobs move into a
  `settings` table. Only the second set is runtime-mutable.
- Migration `0007_settings`: `STRICT` `settings(key TEXT PRIMARY KEY, value TEXT
  NOT NULL, updated_at INTEGER NOT NULL)`. sqlc `GetSetting` / `ListSettings` /
  `UpsertSetting` / `DeleteSetting`.
- `internal/settings`: a typed accessor over a registry of known keys. Each key
  declares name, type, default, description, and a reload class:
  - **live** - applied immediately (e.g. `log_level`).
  - **subsystem-bounce** - re-runs one oklog/run actor (e.g. `public_endpoint`,
    `wireguard_port` bounce the network actor via `bringUpNetwork` / `tryUPnP`).
  - **on-restart** - persisted but read at next boot; the CLI says so when set.
  Unknown keys rejected; values validated on `set`.
- Precedence for runtime-mutable keys: settings-table value if present, else the
  boot config. Boot config still seeds a fresh install.
- Surface: a `SettingsService` Connect RPC + `fletcher settings get|set|list`.
  `list` shows effective value and source.

**Acceptance.** `fletcher settings set public_endpoint ...` brings the tunnel up
on the new endpoint with no restart and no systemctl; `fletcher settings list`
reflects it; survives reboot; invalid values rejected.

#### Part B - `fletcher daemon` lifecycle facade over systemd

**Problem.** A few actions genuinely need the init system: enable-on-boot,
start, stop, restart (binary upgrades, `on-restart` settings), and logs. Users
should not learn `systemctl` / `journalctl`.

**Design.** `cmd/fletcher/daemon.go`: `fletcher daemon start|stop|restart|status|
logs|enable|disable` shelling out to `systemctl` / `journalctl -u fletcher`,
prompting for sudo only when needed; degrade gracefully on non-systemd hosts;
reuse `doctor`'s systemctl remediation strings so the two never drift. systemd
stays the supervisor - this is a thin facade, not a reimplementation of init
(which CLAUDE.md and DESIGN §5 rule out).

**Acceptance.** Someone who has never typed `systemctl` can install, start,
inspect, and tail Fletcher entirely through `fletcher` verbs.

### Milestone 4 - Remote client access - PLANNED

**Goal.** A paired client can drive the daemon over the tunnel. Today the
Connect API is bound to a local Unix socket only (`internal/daemon/daemon.go`
`listenUnix`); the gateway and MCP listeners are loopback. So a paired phone or
laptop cannot call the daemon at all - "connect from a client" is not yet true.

**Scope.**

- Expose the Connect API on a TCP listener bound to the WireGuard interface,
  with authentication (the two networking planes must not leak - CLAUDE.md).
- Once exposed, even the existing `fletcher` CLI pointed at a remote daemon
  works; a native/GUI client app is a separate, larger deliverable (Backlog).

### Milestone 5 - Firecracker (real microVMs) - PLANNED

**Goal.** Swap runc for Firecracker microVMs behind the existing `runtime.Driver`
interface - the intended default isolation tier per the thesis.

**Scope.** The full chunk DESIGN.md §8/§11 describes: OCI image to rootfs via
firecracker-containerd, VM lifecycle via firecracker-go-sdk, the vsock guest
agent, MMDS-injected env. Largest and riskiest milestone; needs a KVM host
(`/dev/kvm` confirmed present on the dev box). Done last, on top of a proven
loop - the runtime interface seam (DESIGN §10) is exactly what lets it drop in
without a rewrite.

## Backlog (not scheduled - awaiting a usage signal)

Per DESIGN.md §13, these wait for real demand rather than being pre-planned.
Listed so they are visible, not lost. Items that became milestones are above.

**Deployment + operability**

- **Cut v0.1.0** - unblocks `curl | sh` install for anyone else (phase 10 gap).
- **Install-time dependency + storage handling** - `scripts/install.sh` should
  check/install `btrfs-progs` and `runc`, detect or guide provisioning a btrfs
  snapshot root, and default Linux installs to the real runtime instead of mock.
  This is the bulk of "users should not run the manual setup dance."
- **`doctor` runtime-prereq checks** - when runc/btrfs drivers are selected,
  verify the `btrfs`/`runc` binaries and a btrfs snapshot root exist, instead of
  surfacing a raw `command not found` at job time.
- **Native client app** - the CLI is the only client today; a GUI/native client
  is a separate deliverable on top of Milestone 4's exposed API.

**Security hardening**

- **`CAP_SYS_ADMIN` hardening** - tighten Milestone 1's broad grant toward
  rootless-runc + user namespaces and unprivileged btrfs.
- **Audit log storage** - swap `audit.Noop` for a SQLite recorder (phase 4 seam).
- **MCP egress hardening + approvals** - SSRF guard, then policy-gated egress
  (phase 6).

**Networking**

- **DDNS** - for operators on a dynamic public IP (phase 9 gap).
- **NAT-PMP / PCP** - port mapping for routers that refuse UPnP (phase 9).
- **Per-peer handshake/transfer visibility** - surface wireguard-go's in-process
  stats (e.g. `fletcher peer status`) since the userspace tunnel is invisible to
  `wg show`.

**Agents + gateway**

- **APNs push** - replace approval polling with push (phase 7).
- **Gateway breadth** - streaming in the translation path; a second provider
  (phase 5).
- **pi-extension `registerProvider`** - once the `pi` extension API is pinned
  (phases 11/14).
