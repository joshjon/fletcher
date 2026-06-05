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

What works (verified on hardware 2026-06-05): **the full private-agent-compute
loop - Milestone 2 is done.** `fletcher image import` flattens `fletcher-base`
into a btrfs template; the daemon CoW-snapshots it per job and runs the command
in a rootless runc fork; the fork reaches the daemon gateway/MCP over a unix
socket (and nothing else - zero egress); and a `claude -p` job completes a real
Anthropic model call through that gateway, exit 0. The agent runs isolated and
unprivileged, the API key never enters the fork, and egress goes only through
the daemon. That is the thesis working end to end.

Correction to the earlier record: Milestone 1's "verified on hardware" claim was
wrong. The daemon was silently on the **mock** runtime the whole time - the
operator's `runc`/`btrfs` systemd drop-in had its `Environment=` lines pasted as
comments, so they never took effect. `runc-smoke`'s `echo` succeeded on mock
(a host subprocess), not in a fork. The runc + btrfs path was only genuinely
exercised during M2a (above). This is also why config-via-systemd-drop-in is
error-prone and is the case for Milestone 3.

Also done since: **Milestone 3** (config + lifecycle via `fletcher settings` /
`fletcher daemon`, no systemctl) and **Milestone 4** (a paired client drives the
daemon over the tunnel, gated by a per-peer token). The runc-first plan (M1-M4)
is complete.

What is **not** possible yet:

- Firecracker microVMs - the intended default isolation tier (Milestone 5, the
  only remaining milestone; a multi-session effort, see below).

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
| 8 | Real Linux runtime | PARTIAL | runc (rootless) + btrfs real, runs agents (M2a); **Firecracker is a stub** |
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

### Milestone 1 - Real isolated execution on runc - SHIPPED (`aaeeab8`, verified on hardware 2026-06-04)

**Goal.** A job runs in a real container fork (runc) on a real copy-on-write
snapshot (btrfs), instead of the mock driver's bare subprocess.

**Delivered.**

- `fletcher image import <docker-ref>` flattens a built OCI image (e.g.
  `fletcher-base:dev` from `make image`) into a btrfs subvolume at
  `<btrfs-root>/images/<name>`, so `fletcher job create --image <name>` has a
  real rootfs. Plus `fletcher image ls` / `rm`. The `docker export` bridge is
  the chosen self-built pipeline (DESIGN.md §11), not an interim hack; Milestone 5
  extends it to also emit an ext4 rootfs image for Firecracker.
- The systemd unit grants the daemon `CAP_SYS_ADMIN` (btrfs subvolume
  create/snapshot, runc namespaces) alongside `CAP_NET_ADMIN`.
- `docs/TESTING.md` Test 2 documents the flow and the snapshot-root ownership
  the daemon needs.

**Known debt carried forward:** `CAP_SYS_ADMIN` is broad (hardening to
rootless-runc + user namespaces is in Backlog).

### Milestone 2 - Run a real agent in the fork + prove the gateway - DONE (verified 2026-06-05)

**Goal (met).** An actual agent runs inside the fork against the daemon gateway,
with credentials never entering the fork. This is the product: private agent
compute. A `claude -p` job completed a real Anthropic call through the gateway
from an isolated rootless fork with zero egress.

**Decisions made:**

- *Fork networking:* originally chosen as veth + restricted route; after the
  rootless discovery (the daemon is unprivileged) the operator chose the simpler,
  equally-isolating **unix-socket forwarder** instead - the fork has only
  loopback and reaches the daemon through bind-mounted sockets. Same property
  (reach only the daemon, no egress), far simpler under rootless.
- *Agent auth:* support both - subscription via credential mount (Phase 12) and
  API key via the gateway (verified) - chosen per job.

**Seams found while scoping:**

- Jobs ran as root with `HOME=/root`, but the fletcher-base image is built
  around its `fletcher` user (uid 1000, `HOME=/home/fletcher`); the agent
  launchers resolve their versioned install relative to `$HOME`, so root breaks
  them. (This is what made the `command -v claude` probe return 127.)
- The fork has its own empty network namespace and the daemon injects
  `ANTHROPIC_BASE_URL=http://127.0.0.1:11500` - which inside the fork is its own
  down loopback. So an agent currently has no path to the gateway at all. This
  is the §6 trust boundary made real for runc, and the core of M2.

**Plan (built in testable steps):**

- **M2a.1 - run a real agent in the fork - DONE (`22351b7`, verified
  2026-06-05).** The hard part. What it took, found by debugging on the server:
  - Run as the image's user. (Superseded by the rootless mapping below: the job
    runs as container root, which maps to the unprivileged daemon user.)
  - **Rootless runc.** The unprivileged daemon makes runc rootless, which needs
    a user namespace. Chosen mapping (no `/etc/subuid`/`newuidmap`): container
    uid/gid 0 -> the daemon's own euid/egid (single-ID self-map). `image import`
    chowns the template to the daemon user so container root owns the rootfs;
    the driver passes `runc --root <daemon-writable dir>` (default `/run/runc`
    is root-only). `MemoryDenyWriteExecute` and the rest of the sandbox stay on.
  - **Job output capture** (`705d829`): the supervisor discarded stdout/stderr,
    making every failure opaque; it now keeps the tail and stores it in the
    job's error. This is what made the rootless errors debuggable.
  - **Import truncation fix** (`2982976`): `image import` used `cmd.StdoutPipe()`
    + early `Wait`, truncating the tar and dropping the agents' install dirs.
- **M2a.2 - fork reaches the gateway, no egress - DONE (`4320c86`).** Unix-socket
  forwarder: the gateway/MCP also listen on unix sockets; the runc driver
  bind-mounts those sockets plus the daemon's `fletcher` binary into the fork and
  wraps the command with `fletcher fork-run`, which relays the fork's loopback
  calls to the sockets. The fork keeps an empty netns (loopback only), so it
  reaches only the daemon. Verified: fork curls the gateway OK, cannot resolve
  the public internet.
- **M2a.3 - real model call end to end - DONE (`ee4136b`).** A `claude -p` job
  completes a real Anthropic generation through the gateway, exit 0. Took a
  gateway fix: `/v1/messages` now forwards the client's headers (it had dropped
  `anthropic-beta`, so Claude Code's `context_management` requests 400'd). Auth
  used the gateway API-key path; the credential-mount path (needs `ProtectHome`
  relaxed) is still untested. (`MemoryDenyWriteExecute` did not need relaxing.)

### Milestone 3 - Ergonomics: no more systemctl - DONE (verified 2026-06-05)

Folds the previously-separate Phase 17 and Phase 18. The whole config/lifecycle
loop is now `fletcher` verbs - no systemctl.

- **Part A - runtime settings in SQLite (`95161ab`).** Migration 0007 + sqlc
  `settings` table; an `internal/settings` Store with a validated registry of
  known keys; a `SettingsService` RPC and `fletcher settings list|set|unset`.
  The daemon overlays stored settings onto its flag/env config at startup
  (bootstrap config stays env-only); changes apply on restart. Validation
  rejects unknown keys and bad values. *Scoped down from the original spec:*
  applied on restart, no live hot-reload yet (subsystem-bounce is a backlog
  refinement).
- **Part B - `fletcher daemon` facade (`b661091`).** `start|stop|restart|
  enable|disable|status|logs` shelling to systemd; degrades on non-systemd
  hosts. systemd stays the supervisor.

Verified on the server: `fletcher settings set log_level debug` +
`fletcher daemon restart` applies it, with no systemctl; invalid values are
rejected.

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

### Milestone 4 - Remote client access - DONE (`c4cad1f`, verified 2026-06-05)

**Goal (met).** A paired client can drive the daemon over the tunnel.

**Auth model (decided): defense in depth - tunnel transport + per-peer token.**
Tunnel-membership-alone was rejected: a privileged, secrets-holding API needs
per-client identity (the Docker-over-TCP-without-TLS lesson), and a misbinding
or leaked peer key must not mean open admin. Bearer tokens, not mTLS - lighter
for a single-box homelab.

**Delivered.**

- `fletcher peer pair` mints a 256-bit per-peer token (only the SHA-256 is
  stored; migration 0008) and returns it once with the API endpoint.
- A TCP Connect listener bound to the WireGuard server tunnel IP
  (`10.99.0.1:11700`) so only tunnel peers reach it, behind an auth middleware
  that requires a valid token. The local unix socket stays auth-free
  (file-permission gated).
- CLI: persistent `--remote host:port --token` (or `FLETCHER_TOKEN`) routes any
  command to a remote daemon; pairing prints the exact command line.

**Verified:** remote call with a valid token succeeds; wrong/missing token 401s;
the local socket still works without a token. A native/GUI client app remains a
separate, larger deliverable (Backlog).

### Milestone 5 - Firecracker (real microVMs) - IN PROGRESS

**Goal.** Make Firecracker the real default isolation tier behind the existing
`runtime.Driver` interface (DESIGN §10), with runc staying as the labeled
degraded fallback. Largest and riskiest milestone; needs a KVM host (`/dev/kvm`
confirmed present and read/write accessible on the dev box).

**Architecture decision (resolved).** We use **`firecracker-go-sdk` directly with
a self-built ext4 rootfs**, *not* `firecracker-containerd`. The toolkit would have
meant running a containerd daemon + shim as supervised services and provisioning a
devmapper thin-pool - a second always-on process and a second storage dance, both
off-thesis (single static binary) and against the seamless-setup goal. Its
registry-pull / layer-caching features are unneeded (`fletcher image import`
already covers our flow). The `snapshot.Driver` seam means firecracker-containerd
could still be added later as an alternative driver without a rewrite. Recorded in
DESIGN.md §11 and §9.

**Sub-phases.**

- **M5a - VMM provisioning + bundling.** Acquire the `firecracker` binary and a
  minimal guest `vmlinux`; bundle via `embed.FS`, extract on first run. `doctor`
  gains a `/dev/kvm` presence + `kvm`-group-membership check for the `fletcher`
  user. (UX is intrinsic to the runtime, so it lives in the milestone, not a
  separate one.)
- **M5b - rootfs pipeline.** Extend the image pipeline to flatten an imported OCI
  image into an ext4 rootfs template, and CoW-clone it per job (reflink on btrfs)
  behind `snapshot.Driver`.
- **M5c - VM lifecycle.** `firecrackerdriver.Run` boots a microVM (bundled kernel
  + per-job rootfs + vsock), honours ctx cancellation, returns the exit code.
  Replaces the stub.
- **M5d - guest agent over vsock.** A tiny in-VM agent that receives
  `{command, env, workdir}`, runs it, streams stdout/stderr back, returns the exit
  code - the Firecracker analogue of the runc `fork-run` forwarder.
- **M5e - gateway reachability + no egress.** The guest reaches the daemon gateway
  /MCP only over vsock; the VM has no egress route (preserves §5/§6 - the API key
  never enters the fork). Verified with a no-egress test.
- **M5f - seamless + default.** `doctor`/`settings`/install so selecting Firecracker
  "just works" on a KVM box (auto-extract VMM, auto-provision rootfs storage, clear
  diagnostics); then make Firecracker the default on capable Linux boxes, runc the
  labeled fallback. End-to-end: a real `claude` agent in a microVM through the
  gateway.

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
- **Settings live hot-reload** - M3 settings apply on restart; the original spec
  also wanted live application (slog level in place, bounce the network actor for
  endpoint/port changes) so some changes need no restart at all.

**Security hardening**

- **`CAP_SYS_ADMIN` scope** - the daemon holds it for btrfs subvolume ops and
  the WireGuard tunnel. runc is already rootless (M2a), so this is now only
  about btrfs; unprivileged-btrfs (or a narrower mechanism) would let it go.
- **Audit log storage** - swap `audit.Noop` for a SQLite recorder (phase 4 seam).
- **MCP egress hardening + approvals** - SSRF guard, then policy-gated egress
  (phase 6).

**Agents + image**

- **codex launcher missing in `fletcher-base`** - `command -v codex` fails in the
  fork: the image's `~/.local/bin/codex` symlink targets
  `~/.codex/packages/standalone/current/bin/codex`, which is absent even in a
  clean `docker export` of the image. A codex-install quirk in the Dockerfile,
  separate from the import truncation fix. claude and pi work.

**Tooling**

- **Lint is blind to `_linux.go` on macOS** - `//go:build linux` files (runc,
  btrfs, wireguard drivers) are not compiled on the Mac dev box, so `make lint`
  there never sees their issues. Several only surfaced when linting on the Linux
  server. CI (or a pre-release check) should lint with `GOOS=linux`.
- **M3 commit `95161ab` is missing its generated sqlc files** - they were not
  staged and landed in M4 (`c4cad1f`), so M3 does not build standalone. The tip
  is correct; a history tidy (rebase to move the generated files into M3) would
  fix it. Low priority - generated code, single contributor.

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
