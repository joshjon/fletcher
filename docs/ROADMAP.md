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
`fletcher daemon`, no systemctl), **Milestone 4** (a paired client drives the
daemon over the tunnel, gated by a per-peer token), and **Milestone 5**
(Firecracker microVMs - the default isolation tier, see below). All planned
milestones M1-M5 are complete.

Everything needed to run a real agent in a microVM is in place: a base image is
published to `ghcr.io/<owner>/fletcher-base` by CI, so an operator pulls and
imports it (`fletcher image import ghcr.io/<owner>/fletcher-base:debian-13
--format ext4`) rather than building it. Verified: the image boots in a microVM
and `claude`/`node`/`go` run inside it.

Remaining rough edges (deferred / backlog, not planned milestones): a multi-arch
image (arm64 is not built yet - slow under emulation), and trimming the
snapshot-root setup so the few-GB space requirement is provisioned, not manual.

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
| 8 | Real Linux runtime | DONE | runc (rootless) + btrfs (M2a) and Firecracker microVMs + ext4 (M5), both real and runnable, behind the runtime/snapshot interfaces |
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

### Milestone 5 - Firecracker (real microVMs) - DONE

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

- **M5a - VMM provisioning + bundling. DONE.** The `vmm` package embeds the
  `firecracker` binary (v1.16.0) and a guest `vmlinux` (5.10.225), arch-selected
  via build constraints, and extracts them to a cache dir on first run
  (idempotent, atomic). Binaries are gitignored and fetched by `make fetch-vmm`
  (SHA256-verified); a committed about.txt keeps a fresh checkout compiling.
  `doctor` gained a `/dev/kvm` presence + `kvm`-group check for the daemon user.
  Verified: extract test runs `firecracker --version`; doctor flagged the daemon
  user missing from the `kvm` group on the dev box.
- **M5b - rootfs pipeline. DONE.** `internal/snapshot/ext4driver` clones a per-job
  ext4 image from a `<name>.ext4` template (FICLONE reflink on btrfs/xfs, full-copy
  fallback elsewhere), behind `snapshot.Driver`. `image import --format ext4`
  builds the template via `mkfs.ext4 -d` over the existing docker-export flatten;
  `image ls`/`rm` and the `ext4` snapshot setting follow. Verified: imported
  busybox to a mountable ext4 rootfs and confirmed a clone re-mounts.
- **M5c - VM lifecycle. DONE.** `firecrackerdriver.Run` boots a microVM (bundled
  kernel + per-job rootfs + vsock) via `firecracker-go-sdk` v1.0.0 (CGO-free),
  honours ctx cancellation, returns the exit code. reboot=k + a guest RESTART (not
  an ACPI power-off Firecracker can't service) keeps the full cycle ~1.4s.
- **M5d - guest agent over vsock. DONE.** `cmd/fletcher-guest` runs as the VM init
  (init=/sbin/fletcher-init, injected into the rootfs at import). It dials the host
  over vsock, receives the spec, runs the command via /bin/sh -c streaming
  stdout/stderr back framed (guestproto), reports the exit code, then resets. The
  Firecracker analogue of the runc `fork-run` forwarder.
- **M5e - gateway reachability + no egress. DONE.** The guest brings up loopback and
  relays the gateway/MCP loopback addresses to the host over vsock, where the daemon
  proxies them to the unix sockets; the VM has no NIC, so no egress. Verified: a
  command in the VM reaches a host service through the forward, while ping to the
  internet fails.
- **M5f - seamless + default. DONE.** The daemon auto-selects Firecracker on a KVM
  host with the VMM bundled (else mock; runc stays explicit). The systemd unit
  allows /dev/kvm and install adds the daemon user to the kvm group; `doctor` checks
  both /dev/kvm and the bundled VMM. Verified end to end: the systemd daemon
  auto-selected firecracker+ext4 and a job created via the CLI ran in a real microVM
  (guest kernel 5.10.225 vs host 5.15, fletcher-init as PID 1, loopback-only).

  Not done here (deliberately, needs the operator's key + the heavy fletcher-base
  image): driving a real `claude` agent through the gateway in a microVM. Every
  mechanism it relies on is proven - the VM runs commands, reaches the daemon
  gateway over vsock, has no egress, and the gateway already stamps credentials
  (M1-M4). It is the operator's final validation, documented in setup.md.

### Milestone 6 - Durable sessions (interactive, persistent workspaces) - IN PROGRESS (Phases 1-2 done)

**The gap.** Today every job is ephemeral: a fresh fork runs one command and is
torn down (the supervisor `deleteSnapshot`s on completion). There is no way to
keep a workspace, SSH/shell into a running VM, iterate on a checkout, or hold a
live agent session across commands. The whole interactive/durable half of the
job model (DESIGN §4's `long_running` trigger, §5's on-disk durability and
resume, preview-URL brokered access) is designed but unbuilt. This milestone
builds it - turning Fletcher from an ephemeral task runner into something that
*also* hosts durable, interactive sessions, without splitting the job model.

**Primitive.** A **session is a job with the `long_running` trigger whose fork
persists** - the enum value already exists; the lifecycle differs (DESIGN §4: one
primitive, many hats, not three subsystems). The durable unit is the **fork on
disk** (the ext4 image / btrfs subvolume), not a running process. So `/workspace`,
a `git clone`, edits, and the agent's on-disk session survive across
disconnects, stops, and daemon restarts.

**Persistence model: two sequenced layers, both in scope.** Disk is always the
source of truth (DESIGN §5); a saved snapshot is only ever a faster way back to a
state the disk already holds, so durability never depends on it (DESIGN §11).
Idle auto-stop reclaims the box's RAM/CPU either way - where **"idle" means no
work in flight, not no user input**: a session stays up while its task/agent is
still running and only starts the (configurable) auto-stop countdown once that
finishes, so a long unattended run is never killed mid-task.

- **Layer 1 - cold boot (the foundation, and the permanent fallback).** Sleep =
  stop the VM, keep the fork on disk; nothing in memory is load-bearing. Wake =
  boot a fresh microVM against the persisted fork and resume the agent from its
  on-disk session (e.g. `claude --resume <id>`). This is required regardless: it
  is how a session starts the *first* time (no snapshot exists yet) and the
  fallback whenever a saved snapshot can't be used (see Layer 2). DESIGN §5's
  "resume = restart the agent pointed at its on-disk session," realised.

- **Layer 2 - hibernate (committed next step, built on Layer 1).** This is the
  Firecracker memory snapshot/restore path, and it behaves like laptop hibernate,
  not sleep: on stop we write the VM's memory to a file in the fork and **the VM
  process exits, freeing the host RAM** (a common misconception is that it holds
  RAM resident - it does not; at rest a hibernated session costs only disk). On
  wake we restore that file and the VM resumes **exactly where it was, with the
  live process tree intact** - the instant-reconnect experience IDE attach wants.
  Known work that makes it robust (the real cost, not the snapshot call itself):
  re-establishing the daemon<->VM vsock channel after a restore; **falling back to
  Layer 1 whenever a snapshot is stale** (snapshots are tied to the Firecracker
  version + guest kernel, so a Fletcher upgrade invalidates them); the extra disk
  a sleeping session's memory file costs; and testing both wake paths. Sequenced
  *after* Layer 1 because it sits on top of it - not deferred to "someday."

**Interactive access: brokered, never a direct route into VM-land.** Same trust
boundary as everything else - the client is a WireGuard peer to the daemon; the
daemon brokers into vsock-land (DESIGN §5/§6). Building on the M5d guest agent:

Two access paths, decided, with distinct roles:

- **`fletcher session shell` - the zero-config, always-available terminal (and
  rescue path).** An interactive PTY straight through daemon -> vsock -> guest
  agent (extends the M5d guest agent from "run one command, capture output" to
  PTY + stdin + window-resize + signals). Needs nothing installed in the VM, so it
  works the instant a session exists and is the way back in when SSH will not come
  up. Can only carry a terminal, not files/ports.
- **Brokered SSH - the primary, rich path.** `sshd` in the session VM (the base
  image already plans host keys generated at boot), with the daemon proxying SSH
  over vsock. This is what unlocks the standard toolbox in one move: your own
  `ssh`, IDE Remote-SSH attach, `scp`/`sftp`, and port-forwarding. `fletcher
  session ssh <name>` sets up the keys and your SSH config so it "just works," and
  connecting to a *sleeping* session wakes it first. The daemon's tunnel + token
  auth stays the outer gate (defense in depth); the VM stays unroutable - this is
  the preview-URL reverse-proxy generalised from HTTP to SSH.
- **Preview ports** - reuse the gateway-forward machinery in reverse to expose a
  port the session is serving as a daemon-brokered URL.

**CLI / API shape.** Keep `job` for ephemeral one-shots (unchanged). Add session
verbs that mirror the lifecycle: `session create` (boot a persistent VM),
`session shell` / `session exec`, `session stop` (hibernate) / `session start`
(wake), `session ssh` (set up brokered SSH for an IDE), `session list`,
`session delete` (destroy the fork). Under the hood these are the one job model
with `long_running` + the persistence/transport above.

**Storage and limits: free RAM automatically, never free disk automatically.**
The deliberate asymmetry - RAM is rebuildable, so stop/hibernate reclaims it
freely; a session's disk holds real, unrecoverable work (repos, edits, the
agent's history), so Fletcher never deletes it on its own. Concretely: session
disks are sized more generously than an ephemeral job's (ideally grow-on-demand),
with a configurable cap on session count / total GB (a `fletcher settings` key)
defaulted conservatively; hitting the cap **refuses new sessions with a clear
message listing what's using space** rather than auto-deleting anything;
`session list` shows each session's disk use and last-touched time so pruning is
intentional; and an opt-in auto-clean of long-untouched sleeping sessions (with a
warning and grace period) ships **off by default**.

**Remaining open questions - resolve while building M6** (the whiteboard settled
the model above; these are the implementation details still to decide, some of
which the broader ecosystem does not document, so do not skip past them):

- Agent-conversation resume handoff: both wake paths are in scope (Layer 1
  re-spawns against the on-disk session; Layer 2 restores the live process from
  the snapshot). The detail is keeping the agent's on-disk session current enough
  that a Layer-1 fallback after a stale snapshot loses no real work.
- Idle detection signal: the daemon already tracks the launched process, but how
  to catch a genuinely *stuck* process (a max-lifetime cap, or a long zero-CPU
  watchdog) so it does not pin a VM forever.
- Session representation in the data model: a job row with its trigger flipped to
  `long_running`, or a distinct row referencing the same persistent fork.

This sketch is informed by a prior-art survey of how durable, interactive
sandbox/dev-environment systems handle persistence vs hibernation and brokered
access - the patterns, mechanics, and source links are in
[`docs/research/durable-sessions.md`](research/durable-sessions.md), worth
re-reading at build time. The choices above are Fletcher's own, derived from the
single-box, daemon-gated, no-route-into-VM-land constraints.

**Build status.**

- **Phase 1 - session core (cold boot + exec + lifecycle) - DONE.** A session is
  its own SQLite row referencing a persistent fork, not a job row with the trigger
  flipped (resolves the data-model open question above): a session has no single
  command and a distinct running/stopped lifecycle, so it shares the execution
  engine - the same runtime + snapshot drivers and agent env as jobs (DESIGN §4) -
  without overloading the job table. The firecracker runtime grew a session mode:
  the guest agent (`fletcher-init`) detects `fletcher.session=1` on the kernel
  cmdline and, instead of dialing the host to run one command and rebooting, stays
  up as a vsock control server (the host dials *in* via the Firecracker UDS
  `CONNECT` handshake) serving exec/shutdown. `internal/session.Manager` owns the
  lifecycle and the live VM handles; `ReconcileOnBoot` resets orphaned `running`
  rows to `stopped` after a daemon restart (handles are in-memory). Surfaced as
  `fletcher session create|get|list|start|stop|delete|exec`. Verified end-to-end on
  real microVMs: a file written in one exec survives stop -> start (the VM reboots
  against the same fork) and a full daemon restart; `delete` destroys the fork.
  Layer 1 of the persistence model, realised; hibernate (Layer 2) is Phase 4.
- **Phase 2 - interactive shell (PTY over vsock) - DONE.** `fletcher session
  shell <ref>` opens an interactive login shell in a running session - the
  zero-config terminal and rescue path. It needed a transport change: bidi
  streams require HTTP/2, so the daemon now serves cleartext HTTP/2 (h2c)
  alongside HTTP/1.1 (via `http.Server.Protocols`, negotiated at the connection
  layer so existing unary clients are untouched), and the sessions client speaks
  h2c over the same socket/tunnel. The guest opens a real PTY (creack/pty)
  running a login shell in /workspace; stdin and window resizes flow host->guest
  as new vsock frames, terminal output flows back, and the CLI drives it in raw
  mode with SIGWINCH-driven resizes and exit-code propagation. Verified on a real
  microVM (a genuine /dev/pts PTY).
- **Phases 3-5** - brokered SSH, hibernate, storage caps + idle auto-stop - still
  to build.

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
