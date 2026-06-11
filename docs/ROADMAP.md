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
| 9 | Networking | PARTIAL | UPnP + NAT-PMP, with lease refresh + release-on-shutdown (PCP pending); **no DDNS**; same-LAN/hairpin pending |
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


- **NAT-PMP (phase 9). DONE.** `internal/network/portmap` now tries NAT-PMP
  (gateway from `/proc/net/route`, RFC 6886) before UPnP, behind a `Mapper`
  that refreshes every mapping on a timer and releases them on shutdown. This
  fixed a real router that silently dropped UPnP *TCP* mappings (so the pairing
  port never forwarded) while honoring NAT-PMP, and the missing refresh that
  let the WireGuard UDP forward lapse after its 1-hour lease. `doctor` now
  probes both UDP and TCP and reports the method.
- **PCP (phase 9).** RFC 6887, the NAT-PMP successor on the same gateway port.
  Still a follow-up; NAT-PMP covers the routers seen so far.
- **Same-LAN / hairpin (phase 9).** When a client is on the same LAN as the
  box, dialing the public endpoint needs router hairpinning, which many routers
  lack. Planned fix for Mode A: advertise the box's LAN IP (with a cert SAN that
  covers it) and have the client prefer it when local. Pending for Mode A;
  **sidestepped entirely in Mode B**, where Tailscale connects same-LAN peers
  directly without the public endpoint or hairpinning.
- **CGNAT / no-cooperating-router (open question, on-thesis boundary).** When
  the ISP uses CGNAT or the router has UPnP+NAT-PMP+PCP all disabled, there is
  no public port to open and automatic mapping cannot help. The only zero-step
  fix is a relay, which DESIGN.md keeps off-thesis ("cannot be fixed without
  hosting a relay"). The on-thesis option is leaning on a user-provided relay
  (e.g. the operator's own Tailscale/Headscale) as an opt-in transport.
  **Now available via Mode B** (see the Mode B entry above): the operator runs
  Tailscale, the daemon exposes its API on the tailnet (`--remote-api-listen`),
  and the iOS app / CLI reach it over the user's own tailnet - whose relays
  punch through CGNAT - with nothing Fletcher-hosted.
- **Boot-time endpoint resilience (phase 9). Found on hardware 2026-06-10.
  Bounded retry DONE; late-WAN recovery still open.**
  The daemon resolves its public endpoint (and brings up the WireGuard tunnel +
  pairing listener) once at startup in `bringUpNetwork`. After a host reboot the
  daemon can start before the WAN/router is reachable, so NAT-PMP/UPnP discovery
  fails, the endpoint is empty, and the tunnel + pairing listener never come up
  until a manual `systemctl restart fletcher`.
  - (a) `Wants=network-online.target` / `After=network-online.target` was
    already in `init/fletcher.service`. It guarantees the local link, not WAN
    reachability, so it is necessary but not sufficient - the race is the few
    seconds (longer after a power-cut that reboots the router too) between the
    link coming up and NAT-PMP/UPnP answering.
  - (b) DONE: `deriveEndpoint` now retries derivation for a bounded window
    (~60s, exponential backoff) before giving up, but only when the derived
    endpoint is load-bearing (no operator `--public-endpoint`, on Linux). This
    covers the common reboot case where the WAN settles within a minute.
  - **Still open:** a WAN that only comes back *after* the bounded window still
    needs a manual restart. The full fix is an async network supervisor that can
    bring the tunnel + remote-API + pairing listeners up at any later point
    (today those are wired once at boot, so they cannot start post-boot without
    restructuring `buildServices`/the run group). `portmap.Mapper`'s refresh
    loop is the natural place to drive re-derivation. Lower priority than the
    bounded retry now that the common case is handled.

- **Mode B / BYO-VPN transport for the iOS app (phase 9). DONE 2026-06-10
  (daemon listener + provisioning; iOS optional-tunnel transport shipped in
  fletcher-ios deb3c97).**
  Resolves three open items above at once - the Tailscale-coexistence question
  (iOS allows one active VPN tunnel, so the app's embedded WireGuard tunnel
  cannot run alongside Tailscale), the CGNAT / no-cooperating-router boundary,
  and same-LAN / hairpin - by making the app's tunnel optional. In Mode B the
  app brings up no VPN and acts as a thin RPC client over a transport the
  operator already runs (Tailscale/Headscale/ZeroTier/plain WireGuard).
  On-thesis: the relay is the user's own tailnet, Fletcher hosts nothing.
  - **Daemon API bind scope (decision: configured VPN IP). DONE.**
    `--remote-api-listen` / `FLETCHER_REMOTE_API_LISTEN` binds the token-gated
    remote API to an operator-specified address; they set it to their Tailscale
    IP (`100.x.y.z:11700`), so the API is reachable only over that VPN, never the
    LAN. Default stays tunnel-only - Mode A is unchanged. `remoteAPIListenActor`
    retries the bind indefinitely (the VPN can come up after the daemon) and is
    independent of the Fletcher tunnel, so it serves even when no tunnel exists.
  - **App provisioning (decision: reuse the login blob). DONE.**
    `fletcher peer pair <name> --byo-vpn` mints a peer + token and renders the
    existing `{remote, token}` login blob (and a QR) with the daemon's
    `remote_api_listen` address pre-filled (surfaced via the new
    `PairPeerResponse.remote_api_endpoint`). No new pairing protocol and no cert
    pinning - plain `http` over the VPN's own encryption, the same trust model as
    today's over-the-tunnel calls. The blob is decode-compatible with
    `fletcher login`, and mutually exclusive with the versioned WireGuard pair
    blob, so the iOS scanner can try both unambiguously. `remote_api_listen` is
    also a settings key.
  - **Mode selection (decision: explicit at setup).** The user picks "set up
    Fletcher's tunnel" (Mode A, WireGuard pairing QR) or "I already reach my box
    over a VPN" (Mode B, scan the `{remote, token}` QR). One box, one mode;
    switching is a re-pair. No auto-detect.
  - **Split.** Daemon = let the API be reached over a VPN it did not create
    (bind knob + retry; token auth unchanged). iOS = let the app talk to the box
    without creating its own VPN (optional `NEPacketTunnelProvider`, a transport
    abstraction so Mode A/B share the `RPCClient`/`SessionsService`/token code
    and differ only at "how do I reach the box"). Build daemon-first (small,
    locally testable), iOS after. The iOS ROADMAP backlog cross-references this.

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
  (M1-M4). It is the operator's final validation, documented in docs/site/guide/first-agent.md.

### Milestone 6 - Durable sessions (interactive, persistent workspaces) - DONE

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
- **Phase 3 - brokered SSH (IDE attach) - DONE.** `fletcher session ssh <ref>`
  gives SSH and IDE Remote-SSH access, brokered over vsock so the VM stays
  unroutable (the preview-proxy pattern generalised from HTTP to a raw byte
  stream). The image bakes openssh-server; the guest generates host keys on
  first boot and runs sshd on loopback with a vsock relay (SSHPort) the daemon
  splices into. A new bidi RPC (ProxySession) carries opaque bytes via
  SessionHandle.DialSSH. `session ssh` mints a managed ed25519 keypair, installs
  its public key in the VM, and writes an Include-d SSH config Host block whose
  ProxyCommand is the hidden `session ssh-proxy` (which wakes a stopped session
  first). Verified on a real microVM: login, exit codes, scp, and wake-on-connect
  with the disk surviving stop/start.
- **Phase 4 - hibernate (snapshot/restore) - DONE.** Stop now hibernates a
  session (Firecracker pause + memory snapshot + VMM exit, freeing host RAM)
  instead of a clean shutdown; Start restores from the snapshot for an instant
  wake with the live process tree intact, falling back to a cold boot from the
  fork when no valid snapshot exists. A snapshot is fingerprinted to the VMM +
  kernel so a Fletcher upgrade invalidates it, and is consumed on restore so a
  crash falls back to a clean disk boot rather than a stale memory image. This is
  the Layer-2 instant-wake UX on Layer 1, never load-bearing for durability.
  Verified on a real microVM: stop frees the VMM and writes a 512MiB snapshot;
  start restores in ~30ms with the same boot id, a background process still
  alive, and sshd still serving - across cycles and via SSH wake-on-connect.
- **Phase 5 - storage caps + work-based idle auto-stop - DONE.** RAM is freed
  automatically, disk never is (the deliberate asymmetry above). A reaper actor
  hibernates running sessions with no work in flight - an active host op
  (exec/shell/ssh, tracked by a busy counter) or a busy guest (its 1-minute load
  average via a new stat control message) both count as work, so a running agent
  with no user attached is never stopped mid-task. The `session_idle_timeout`
  setting drives it (0 disables). `session_max_count` and `session_max_disk_gb`
  caps refuse new sessions over the limit with a report of what is using the
  space (never auto-deleting); `session list`/`get` show each session's disk use
  and last-used time. Verified on real microVMs: an idle session auto-stops while
  a busy one keeps running and has its idle clock reset; the count cap refuses
  with a usage report; list shows disk + last-used.

### Milestone 7 - SwiftUI iOS client (the hero) - PLANNED

**Goal.** The native first-party client the product is actually for (DESIGN.md
§1, §7, §8): from an iPhone, pair to your own box, spin up an isolated VM, drop
into a terminal running Claude Code inside it, and supervise + approve unattended
agents - all over the WireGuard tunnel, nothing leaving your network. This
promotes the former "Native client app" backlog line to a committed milestone:
durable Claude-Code sessions driven from a phone is *the* wedge (DESIGN.md §8,
"a beautiful iOS/Mac app is the hardest thing for a bot-shaped competitor to
copy"), not a someday GUI. A first-cut HTML UI mockup of these screens (approved
as the visual direction) lives in `design/ios-mockup/`.

**Daemon-support audit (2026-06-11) - gaps the iOS milestones need.** The iOS
app (separate repo) has shipped its M1-M5; its M6-M11 are planned. All nine
Connect services are on the remote mux the app uses (`newRemoteServer(...,
connectSrv.Handler)`), so every service is reachable with a peer token -
**including `AdminService`, contrary to the "local-socket only" note below,
which is now stale.** So the gaps are missing fields and RPCs, not missing
services:

- **M3 (shipped) - image picker.** `ImageService` has only `Import`, no
  `ListImages`, so the app's image field is free-text. *Small: a `ListImages`
  RPC over the imported templates.*
- **M4 (shipped) - editable trust dials. DONE 2026-06-11.** `UpdateSession`
  changes a session's `egress_policy` and/or `gateway` (empty leaves a field
  unchanged). Both are baked into the fork at VM boot, so the response's
  `restart_required` is true for a running session (the change applies on its
  next start); a stopped session applies it immediately when started. The app's
  chips can become tappable, then prompt for a restart when needed.
- **M5 (shipped) - TLS status chip. DONE 2026-06-11.** `PublishedPort` now
  carries `tls_status` (pending/valid/renewing/expired) and `tls_expires_at`,
  populated in `ListPorts` for public ports from certmagic's managed-cert store
  (read-only, no issuance). "failed" is not reliably detectable from certmagic
  so it is folded into "pending". Shared with M6.
- **M6 (the named blocker) - deploys.** The daemon persists only `run_app`
  (bool); it does not track entrypoint, exposed port, restart count, or app
  health, and there is no logs RPC or restart/redeploy RPC. Per the iOS M6
  deliverables it needs: deploy detail (`entrypoint`, `exposed_port` from the
  image EXPOSE, `restart_count` - new supervisor tracking) on `Session` or
  `GetSession`; `tls_status` (shared with M5); an **app-logs RPC** (tail/stream
  `/var/log/fletcher-app.log`, none exists); and **Restart + Redeploy** RPCs
  (only Start/Stop/Delete exist). Public hostname is already available via
  `ListPorts` -> `PublishedPort.host`.
  - **Shipped 2026-06-11:** `RestartSession`; unary `GetSessionLogs` (tail);
    deploy detail (`entrypoint`/`exposed_port` persisted in `TemplateMeta`,
    surfaced on `GetSession` as `DeployInfo`); `PublishedPort.tls_status` +
    `tls_expires_at`; `RedeploySession` (re-fork from the current template +
    restart, with a best-effort registry re-pull first - works for registry and
    local-tagged images; the daemon does not rebuild a Dockerfile).
  - **Guest-side batch DONE 2026-06-11 (needs the guest rebuilt + images
    re-imported to take effect):** `restart_count` - the in-guest supervisor
    counts restarts and reports them in `Stat`; surfaced on `DeployInfo` via the
    runtime `AppRestarts`. Live-follow log stream `StreamSessionLogs` - guest
    exec now runs under a context cancelled when the host disconnects (so
    `tail -F` is killed instead of leaking), and the daemon streams it through a
    server-streaming RPC (`session logs --follow` on the CLI). **M6 complete.**
- **M7 (next) - live settings.** `SettingsService` Set/Delete/List is complete;
  `AdminService.Health` is remote with rich fields (public_endpoint, runtime,
  base-image flags, pairing_endpoint), so the doctor-warnings row is derivable.
  *Buildable now.* **Added 2026-06-11:** each `Setting` now carries
  `requires_restart`, and `ReloadSettings` live-applies the reloadable ones
  (the 6 session/job create-time defaults and caps - swapped atomically in both
  managers) so the app's "Apply now" is instant; boot-bound settings
  (listeners, drivers, tunnel, certs, `public_web`, `vm_memory_mb`, `log_level`)
  stay flagged restart-required and the user restarts manually. *Tiny optional
  add still: a `default_agent` setting the create form wants.*
- **M8 - approvals + APNs. DONE 2026-06-11.** `ApprovalService` already had
  approve/deny. Added `PushService` (`RegisterPushToken`/`UnregisterPushToken` +
  a `device_tokens` table) and a daemon-side APNs sender: when a pending
  approval is created, the daemon pushes a **content-light** notification (a
  generic alert + the approval id; the app fetches detail over the tunnel) to
  every registered device, dropping tokens APNs reports dead. The sender is
  hand-rolled on the stdlib (ES256 provider JWT + `net/http` HTTP/2) - no new
  dependency - and pushes **directly to Apple** with the operator's own key (on
  thesis; nothing through Fletcher). Operator config via `apns_*` settings (the
  `.p8` key path, key/team IDs, topic, environment); push is off until set.
  Untested on hardware here - needs a real key + device.
- **M9 - scheduled jobs. Schedule-edit DONE 2026-06-11** (`UpdateJobSchedule`
  reschedules a cron definition and recomputes `next_run_at`; the poller picks
  it up on its next tick). Listing/history were already covered.
- **M9 (original note) - scheduled jobs.** `JobService` + `Job{schedule, next_run_at,
  parent_id, trigger}` already cover listing cron jobs and run history
  (client-side filter by trigger/parent_id). *Gap: no `UpdateJob`/`SetSchedule`
  to edit a schedule (Create+Cancel only); a `parent_id`/`trigger` filter on
  `ListJobs` would beat client-side filtering. Low priority.*
- **M10 - inbox.** Superseded 2026-06-11: the `fletcher.report` MCP tool ships
  with daemon Milestone 14 as push-notification content; the inbox feed/tab is
  parked until a cron/jobs usage signal exists (see the mobile-first wave
  below).

Recommended order: (1) **M6** (named blocker, the largest), folding in
`tls_status` (also closes M5's chip); (2) cheap upgrades to shipped milestones -
`ListImages` (M3) and the M4 mutate RPC; (3) M8 APNs and M9 schedule-edit when
those milestones are taken up. Each proto change requires re-vendoring the
protos into fletcher-ios and regenerating the Swift stubs, so batch changes per
milestone.

**The substrate is already shipped.** This is a client on top of contracts M4 +
M6 already expose; the app consumes the daemon, it does not extend it:

- **Remote API over WireGuard (M4).** The remote listener serves the same Connect
  handler as the local socket (`newRemoteServer(..., connectSrv.Handler)`), so
  `SessionsService` and `JobsService` are reachable from a paired peer. The phone
  reaches it as a WireGuard peer (M7's pairing delivers that peer config - see the
  pairing phase below); `AdminService` stays local-socket only.
- **Interactive terminal as a streaming RPC (M6).** `ShellSession` is
  bidirectional streaming (`stream ShellSessionRequest` -> `stream
  ShellSessionResponse`): the in-app terminal pumps that PTY stream (stdin /
  stdout / resize frames), no SSH client needed. `ProxySession` (also a bidi
  stream) is there if a raw SSH byte stream is ever wanted instead.
- **Claude actually runs in those sessions** after the session gateway/env fix
  (loopback gateway+MCP forwards plus the `/etc/profile.d` env), so an in-app
  terminal reaches models with no extra wiring.
- **gRPC is the chosen app transport.** DESIGN.md §9: one handler -> gRPC
  (SwiftUI), HTTP/JSON (CLI), gRPC-Web (web). Swift stubs generate from the same
  `.proto` via buf.
- **Approvals are already modeled** as `pending_approval` SQLite rows that survive
  reboot, with APNs as the intended push transport (DESIGN.md §5).

**New work (the app, sequenced by dependency).**

1. **Swift client + embedded WireGuard + unified pairing.** Embed **WireGuardKit**
   in a packet-tunnel Network Extension and drive it from the app via
   `NETunnelProviderManager`, so the tunnel comes up *inside* Fletcher - no
   separate WireGuard app to install or cycle between (the official WireGuard app,
   Tailscale, and Mullvad all ship this way). One-time iOS VPN-consent prompt,
   then one tap or on-demand; the tunnel runs in the extension process, so it
   survives app backgrounding. This requires a **unified pairing payload**: one
   QR/token that carries the WireGuard config (daemon pubkey, public endpoint,
   allowed IPs) *and* the RPC token, with the app generating its own WireGuard
   keypair locally and registering its pubkey during pairing. That collapses
   today's two-step setup (configure WireGuard, then `fletcher login`) into a
   single in-app tap. Daemon-side it is mostly repackaging what `fletcher peer`
   already issues into the pairing blob (today `{remote, token}` only); no new
   networking. A generated Connect-Swift (or grpc-swift) client rides the tunnel.
   Proven by pairing a clean phone and listing / creating / stopping sessions.
   Foundation for everything else.

   > **Update (2026-06-09): the "no new networking" assumption was wrong.**
   > The first real-device pairing test surfaced a bootstrap deadlock. The
   > client-keygen flow (commit `183f405`) exposed `CompletePair` only on the
   > tunnel-side API (`10.99.0.1:11700`, behind a per-peer token), but a native
   > client cannot reach that: the daemon only learns the device's WireGuard
   > public key *at* `CompletePair`, so the tunnel that call would travel over
   > cannot exist yet (WireGuard will not handshake an unknown key), and the
   > per-peer token gating it is the very thing `CompletePair` returns. On a real
   > phone the request leaked to iCloud Private Relay and timed out.
   >
   > **Fix (shipped):** a dedicated **public pairing listener** - a TLS-terminated
   > TCP port (default 51821, settings key `pairing_port`, UPnP-forwarded) that
   > serves *only* `CompletePair`, authenticated by the one-time pairing code
   > rather than a peer token. It uses a self-signed cert whose SHA-256 leaf
   > fingerprint is carried in the pairing blob; the app pins it (the QR is the
   > out-of-band trust anchor), so a bare-IP endpoint with no CA is still
   > MITM-proof. The app now calls `CompletePair` over this endpoint first, then
   > brings up the tunnel (the daemon now knows its key) and uses the tunnel API
   > for everything after. The steady-state invariant holds - clients remain
   > WireGuard-only peers; the public channel exists solely for the short-lived,
   > code-gated bootstrap. New fields: `BeginPairResponse.pairing_endpoint` and
   > `pairing_tls_fingerprint`, plumbed into the pairing blob.
2. **In-app terminal (the hero interaction).** A SwiftUI terminal over
   `ShellSession`: attach to a session, run `claude`, detach and reattach.
   Backgrounding the app never kills the session - durability is server-side
   (M6), which is the whole point. This is the demo.
3. **Approvals + APNs push.** Promote APNs from the backlog: the daemon pushes a
   `pending_approval` to the device; the app approves / denies, resolving the row.
   Polling is the fallback until this lands.
4. **Results inbox (DESIGN.md §7, the second surface).** A feed of job-output
   cards / dashboards ("today's prices", "build finished - preview"). Half of why
   monitoring use-cases are sticky and what makes a non-technical user open the
   app daily. *Open design question, to settle at implementation:* how a job
   result becomes a structured card. Proposed direction - the §4 "sink" plus a
   `fletcher.report` MCP tool the agent/job calls for rich cards
   (`{title, summary, status, metric?, link?}`), with a generic
   name / exit-code / output-tail fallback so any job gets a card with zero
   config. Matches "agent-authored-then-automated" (write the report call once,
   the cron'd program reuses it). Needs testing before it is settled.

**Dependencies and risks to verify before betting on.**

- **Embedded WireGuard on iOS** via WireGuardKit in a packet-tunnel Network
  Extension (the Network Extensions capability + a paid Apple developer account; a
  separate extension target). The daemon already speaks `wireguard-go`, so only
  the client and the pairing payload change. App Store review scrutinises VPN
  apps, but the tunnel is to the user's own box (not a commercial VPN service), so
  it is routine: needs a privacy policy and clear disclosure, and is a non-issue
  for TestFlight / personal distribution. The extension's memory cap is
  comfortable for WireGuardKit.
- **gRPC bidi streaming from iOS over the tunnel** - confirm Connect-Swift carries
  `ShellSession` cleanly; the gRPC-Web surface is the fallback if not.
- **iOS backgrounding** truncates long-lived streams - acceptable by design: the
  session is durable server-side, so the app detaches and reattaches rather than
  holding the stream open.
- **APNs** needs an Apple developer account + push setup; the push goes through
  Apple's push service (the accepted transport in §5), not infrastructure we
  operate, so it stays on-thesis.
- **A remote status/health RPC** may be wanted: `AdminService.Health` is
  local-socket only today, so the app has no daemon-status call. A small addition
  if the control panel needs one.

**Deferred within this milestone.** The **Mac app's UI** - but the architecture is
built for it from day one. The client, models, view-models, pairing, and
WireGuardKit config building live in a **shared Swift package** behind a
multiplatform target, so adding Mac is a UI-only port, not a rewrite; only the
Mac-specific UI (windowing, menu bar, sidebar, pointer + hardware-keyboard
terminal UX) and its NetworkExtension packaging are deferred. Rationale: iOS is
the hero and owns the hard problems (touch terminal, small screen, app
backgrounding); a Mac already has a working path today (the `fletcher` CLI plus
brokered SSH / IDE Remote-SSH from M6), so a Mac app is a convenience on an
already-working flow, not an unblock the way the iOS app is; and carrying two UIs
through early churn would halve iteration speed. Ship iOS, port the Mac UI once
the iOS design has stabilised. The **web client** (the gRPC-Web surface already
exists). Multi-account / multi-box switching (one box, one user is the thesis;
DESIGN.md out-of-scope).

### Milestone 8 - Published ports + public web sessions - IN PROGRESS

**Goal.** Let a durable session expose a port it is serving - first to the
operator's own paired devices over the tunnel, then (opt-in) to the public
internet under a domain the operator controls. The motivating use case: a
developer hosts a small web app in a session for friends or family to reach in a
browser, on a custom domain, with a real TLS cert - all served from the
operator's own box, over their own connection, with the app sandboxed in a
no-NIC microVM.

**On-thesis check (recorded so drift is visible).**

- *Developer hosts nothing.* The operator's box serves the traffic over the
  operator's home connection, public IP, and domain (registered at the operator's
  own registrar). Fletcher routes nothing through any service we run. The
  off-thesis version of this idea - tunnelling through ngrok / Cloudflare Tunnel -
  is explicitly not built; an operator may BYO such a tunnel, but it is not a
  dependency.
- *The daemon stays the gate (DESIGN §5).* Public traffic hits a daemon listener
  and is **reverse-proxied** into the VM over the existing vsock forward. The VM
  keeps **no NIC**; nobody gets a route into VM-land, only a brokered response.
  This is the preview-URL pattern (DESIGN §5, "preview URLs are the daemon
  reverse-proxying into a VM") widened from "authenticated tunnel peer" to
  "anyone", and it realises the M6 "Preview ports" sketch that was designed but
  never built.
- *One primitive, many hats (DESIGN §4).* A published port is a property of a
  `long_running` session whose **sink is a public URL** - §4 already lists a
  preview URL as a valid sink. No fourth trigger, no new subsystem.
- *Blast radius is a feature, not a worry.* A public web app that is compromised
  is trapped in a no-NIC microVM with its egress policy (B3) still in force -
  instant rollback, no route to the LAN. This is a *better* story than exposing a
  port on a normal homelab box, and it leans on the structural moat (DESIGN §8).

**The one real new surface.** Today the only thing the box exposes publicly is the
WireGuard UDP port, which is silent (drops unauthenticated packets, no response).
Phase 2 adds a public HTTP(S) listener that *responds to anyone* - genuine new
attack surface. Mitigations baked in: the public listener serves **only published
session ports**, never the daemon API / gateway / MCP / egress sockets; it is
**off by default** (a global enable setting plus per-port opt-in); the LAN/metadata
guard stays on; and every public hostname served is audit-logged.

**Decisions (settled with the operator before building).**

- *Sequencing:* Layer A (tunnel-reachable preview ports) ships and is verified
  first, as Phase 1, with zero new public surface; the public listener + TLS +
  UPnP land on top as Phase 2.
- *Routing:* **explicit hostname -> session:port** (e.g.
  `session publish <ref> 3000 --public --host app.example.com`), not wildcard
  subdomains. This keeps TLS on HTTP-01 / TLS-ALPN-01 (the operator just points a
  DNS A record at the box) and keeps a DNS-provider API token *out* of the daemon -
  the more on-thesis posture. Wildcard subdomains (needing DNS-01 + a registrar
  token) are a deferred follow-up if a usage signal asks for them.
- *Verification:* the operator has a real public IP (not CGNAT) and a controlled
  domain, so Phase 2 is verified end to end this round (forward, ACME cert,
  browser from outside the network).

**Substrate already in place (confirmed 2026-06-08).** Egress policy B3
(`none`/`allowlist`/`open`, fully plumbed); `portmap.Map()` already maps arbitrary
**TCP** ports (only WireGuard UDP uses it today); public-endpoint resolution; the
idle reaper's busy-counter; and the `dialGuest(vsock, port)` primitive. Net-new:
a generic "dial guest loopback port N" (today `DialSSH` hardcodes the SSH vsock
port - parameterising it is a small host + guest change), the published-ports data
model, the reverse-proxy/forward broker, `certmagic` for TLS (reserved in
DESIGN §9, not yet imported), and **wake-on-connect** (the SSH path does *not*
auto-wake a stopped session today - this is built fresh and also improves SSH).

**Phases.**

- **Phase 1 - preview ports over the tunnel (Layer A) - DONE (verified on
  hardware 2026-06-08).** Generic guest port dial: a dedicated guest vsock relay
  (`guestproto.PortForwardPort`, the SSH relay generalised - the host writes a
  2-byte target-port header, the guest splices to that loopback port) +
  `SessionHandle.DialPort` + `Manager.DialPort` (busy-marked for the connection
  lifetime, wakes a stopped session first via a now per-session-serialised
  `Start`). A `published_ports` table (migration 0012) + sqlc and `session
  publish` / `unpublish` / `ports` verbs. A `session.Broker` that, per published
  port, runs a raw TCP forwarder on the tunnel IP splicing to the guest port via
  `DialPort`, re-opened on daemon boot (`ReconcilePorts`) and closed on shutdown.
  Unit-tested (publish lifecycle, dup conflict, wake-on-connect, delete-closes-
  ports); `make check` green. *Drive-by fix:* the darwin firecracker stub had
  drifted (missing the egress B2 `Forward.Egress` / `Options.EgressOpenSocket`
  fields), silently breaking `make cross-check`; restored. *Verified on hardware:*
  a paired client reached a dev server in a real microVM at the tunnel address.
- **Phase 2 - public exposure (Layer B, opt-in) - DONE (verified on hardware
  2026-06-08).** `session publish --public --host app.example.com`
  serves a port on the internet over HTTPS. A single public listener (443 + 80 for
  ACME HTTP-01 + an HTTPS redirect) bound to all interfaces, UPnP-forwarded via
  `portmap` (TCP), gated by a global `public_web` setting (off by default; a
  `--public` publish while off is refused with a clear message). `certmagic`
  (`internal/session/public.go`) terminates TLS with **on-demand** issuance whose
  decision function only allows a hostname that maps to a published public port -
  so the internet-facing listener can never be coaxed into minting certs for
  arbitrary names. HTTP-01 + TLS-ALPN-01 (no DNS-provider token). Routing is by
  `Host` header -> the published port's `(session, guest_port)`, reverse-proxied
  into the VM over the existing vsock `DialPort` path (so wake-on-connect +
  busy-marking come free); an unknown host gets a 404. The listener serves only
  published public ports - the daemon API/gateway/MCP are never on it - and every
  request is audit-logged. `acme_staging` / `acme_email` settings; systemd unit
  gains `CAP_NET_BIND_SERVICE` (binding 443/80 is best-effort - if the cap is
  missing the daemon still runs, just without public serving). A public port is
  also tunnel-reachable (defense in depth); the tunnel forwarder is best-effort
  for public ports so they serve even when the tunnel is down. `make check` green.
  *Verified on hardware:* a browser outside the network reached a session's web
  server at `https://<operator-domain>` with a Let's Encrypt cert (staging first,
  then production). Two follow-ups surfaced while verifying: (1) the in-VM server
  was a hand-backgrounded process that does not survive a session restart (a
  daemon restart cold-boots the session), so it had to be relaunched - the M9
  entrypoint/auto-start work fixes this for real deploys; (2) switching
  `acme_staging` to false needs a daemon restart, and certmagic may keep serving
  the cached staging cert until it is cleared from the cert store.

**Limitations recorded up front.** CGNAT makes public inbound impossible and
cannot be fixed without hosting a relay (off-thesis) - documented, not hidden,
same population that can already use the WireGuard endpoint. A dynamic public IP
makes **DDNS** load-bearing for a stable A record; DDNS is a current backlog gap,
so until it lands the operator re-points DNS on IP change. Phase 2 promotes DDNS
from the backlog as a fast-follow if the dynamic-IP case bites.

**Backlog - behind-a-proxy (Cloudflare/CDN) mode (opt-in).** The default public
path is DNS-only with Fletcher owning the Let's Encrypt cert (direct, no third
party in the path, no DNS token). Some operators want to front the box with
Cloudflare (proxied) to hide the home IP and get DDoS protection - a legitimate
choice for public hosting, like the bring-your-own-VPN stance, but it means the
CDN terminates TLS, so Fletcher's ACME challenge can't complete and Fletcher must
stop owning the cert. Support would be an opt-in per-port mode (e.g. `publish
--behind-proxy` / `deploy --behind-proxy`): serve the port as **plain HTTP** on
:80 with no ACME and no HTTPS redirect, letting the CDN do public TLS (covers
Cloudflare "Flexible" and any fronting reverse proxy). An origin-HTTPS variant for
Cloudflare "Full" (a self-signed / Cloudflare-Origin-CA cert, no ACME) is a
further follow-on. Explicitly *not* doing DNS-01 for a real cert while proxied -
that needs a stored DNS-provider token, the thing the HTTP-01/TLS-ALPN-01 choice
deliberately avoids. Not scheduled; raised while testing a Cloudflare-proxied
domain (2026-06-08).

### Milestone 9 - Dockerfile app deployment ("self-hosted Fly") - CODE COMPLETE (awaiting hardware verification)

**Goal.** Point Fletcher at a Dockerfile (or a built image) and have it run that
app as a long-running VM and expose it on the public internet under your domain -
the workflow you'd get from Fly/Render, but on metal you own, with nothing
hosted or metered. Raised by the operator (2026-06-08): "I deploy personal web
servers from a Dockerfile; I want Fletcher to accept one and expose it."

**On-thesis.** Your box serves your app over your connection and domain; we host
nothing. It is the §4 job model wearing the deploy hat: **environment** (the
built image) + **payload** (the image's own entrypoint, a long-running server) +
**trigger** (`long_running`) + **sink** (a public URL). No new subsystem.

**Already built (consumed, not re-done).** The image pipeline is already
Dockerfile-based: `fletcher image import <docker-ref>` does `docker build` ->
flatten to an ext4/btrfs rootfs -> CoW-clone per VM (DESIGN §11). And M8 Phase 2
is the public-exposure half. So M9 sits on top of both.

**Positioning vs exe.dev (the reference for this flow).** exe.dev is a hosted
service whose VMs pull an image from a registry at boot (`new --image
<registry-ref>`, public / private / a registry you run on one of their VMs).
Fletcher's structural wins: it runs on metal the operator owns (nothing hosted,
metered, or visible to us), and because it *builds and flattens locally* it needs
**no registry at all** for the common case - the code never leaves the LAN to be
deployed back to it. exe.dev is currently smoother on pure convenience: it runs
the image's app automatically and there is no box to operate. M9 closes that
convenience gap (run the app automatically, one command) while keeping the
ownership win. Honest framing: better where it counts (ownership/privacy),
behind on turnkey ergonomics until M9 lands.

**Build vs registry (decided).**

- **Local build is the primary path** and the on-thesis differentiator: `docker
  build` on the box -> import -> run. No registry hop; nothing leaves the network.
  A builder is *not* new responsibility - Fletcher already builds (the base image
  is `docker build`; `image import` is `docker create`/`export`). `deploy` just
  exposes that pipeline; we shell to `docker build`, we do not write a builder.
- **Registry pull is the secondary path** for images already built in CI
  (ghcr/Hub/ECR). Works today via the host's `docker login`; add an optional
  `--registry-auth=user:token` convenience (the exe.dev pattern) that does the
  login for the operator.
- **Do not host a registry, and do not recommend the registry-on-a-VM trick.**
  exe.dev needs it (registry-fetch-at-boot); Fletcher does not (local flatten).
  An operator *can* run `registry:2` in a session if they want, but it is never a
  required step.
- **Future (M9 v2):** run the build *inside an ephemeral Fletcher VM*
  (buildkit/buildah, daemonless) so the host needs no Docker and the build itself
  is sandboxed. v1 assumes Docker on the box (status quo for image work).

**Net-new work.**

- **Honor the OCI run config.** Today a session runs a command the operator gives
  it; `fletcher-init` (PID 1 in the VM) does not run the image's own
  `ENTRYPOINT`/`CMD`. Import must capture the OCI config (`ENTRYPOINT`, `CMD`,
  `ENV`, `WORKDIR`, `USER`, `EXPOSE`) into the existing `.meta.json` sidecar, and
  `fletcher-init` needs an "entrypoint mode" that launches it with that config
  mirrored (signals/env/user). `EXPOSE` gives the default port to publish.
- **Keep the app running** (surfaced verifying M8: a hand-started server did not
  survive a daemon restart). A deployed app must restart on crash and come back
  after a daemon/box reboot - the supervisor reconciles deploys on boot and
  re-launches the entrypoint, distinct from the session reaper that hibernates
  idle interactive sessions.
- **A one-shot deploy ergonomic.** `fletcher deploy ./myapp --host app.example.com`
  = build (or pull) -> import -> boot a `long_running` session running the
  entrypoint -> `publish --public`. The `flyctl deploy` equivalent.
- **Logs + status.** A deploy needs `fletcher` to show the app's logs and
  run state (exe.dev's users just SSH in; we have `session shell`/`exec`, but a
  deploy wants a direct `logs`/`status` surface).
- **Redeploy / update.** Ship a new image version without losing the app's data;
  ties into the backlogged "persistent volumes decoupled from session lifecycle"
  (recreate on the new image, reattach the volume).

**Falls out for free.** M8 wake-on-connect gives effective scale-to-zero: a
deployed app with no traffic hibernates and wakes on the first request.

**Caveats to verify before building.** The build step needs Docker on the box
(same requirement the base-image build already has). The app must tolerate being
PID-1-launched by `fletcher-init` (the real work in the OCI-config item).

**Slice 1 - honor the image entrypoint - CODE COMPLETE (awaiting hardware
verification).** The load-bearing unknown (an arbitrary image's app running under
`fletcher-init`) is built:
- `internal/appspec`: a small launch spec (entrypoint/cmd/env/workdir/user) shared
  by the CLI and the guest init.
- `image import` captures the image's run config via `docker image inspect` and
  writes it into the rootfs at `/etc/fletcher/app.json` (ext4 path; best-effort).
- `fletcher-init` gains app mode: on `fletcher.app=1` it runs the captured app in
  the background (image env + workdir, as root for now), logging to
  `/var/log/fletcher-app.log`, while the control server keeps the session
  shell-able.
- `SessionSpec.RunApp` -> the firecracker driver adds `fletcher.app=1` to the
  guest cmdline; persisted as `sessions.run_app` (migration 0014) so a restart or
  wake re-runs the app (not bare); surfaced as `session create --app`.
- Unit-tested (app mode boots and persists across stop/start); `make check` green.

**Slice 2 - keep the app running - CODE COMPLETE.** `fletcher-init` supervises the
app (restarts on exit, 1s crash-loop backoff) and applies the image `USER` when
set/resolvable (name or uid[:gid], else root). The daemon's `StartDeployedOnBoot`
boots every `run_app` session after a restart (in the background), so a deploy
comes back on its own rather than only on the next inbound request.

**Slice 3 - `fletcher deploy` - CODE COMPLETE.** `fletcher deploy <dir|ref>
[--host app.example.com] [--port N]`: builds a directory's Dockerfile (or takes
an image ref), imports it, creates a `--app` session, and publishes the port
(public when `--host` is given, else tunnel-only). Port defaults to the image's
lowest `EXPOSE`. Runs the build/import locally (needs root + docker, like `image
import`) and the session/publish steps over the local socket; prints the DNS
guidance from slice's M8 renderer.

**Slice 4 - observability - CODE COMPLETE.** `fletcher session logs <ref>` shows
the app log (`/var/log/fletcher-app.log`). Run state + app mode already show in
`session get`.

**Slice 5 - remote deploy from a registry (daemon-side import) - CODE COMPLETE.**
The original `deploy` was host-only because import was a CLI+sudo local op - wrong
for a remote-first product (most users drive Fletcher from a laptop). Fixed: a new
`ImageService.Import` RPC has the **daemon** pull and flatten a registry image
**in-process** (pure-Go `go-containerregistry`, no Docker on the host) into its own
snapshot root. So `fletcher deploy ghcr.io/you/app:v1 --host ...` and `fletcher
image pull <ref>` now work **from a remote client** with no local Docker or
filesystem access to the box. Private registries: `--registry-auth user:token`
(basic auth on the pull); a self-hosted registry-in-a-VM (the exe.dev pattern)
works the same way, with zero registry-specific code. Building a local `./dir`
Dockerfile stays host-side (it needs the working directory) and still uses the
root CLI import. The daemon extracts as its unprivileged user, so rootfs files are
daemon-owned - fine for app images that run as root; an image needing setuid
binaries / non-root file ownership should use the root CLI `image import`.
Unit-tested (name + EXPOSE derivation); `make check` green.

**Deferred (not blocking M9's core; revisit on demand):** redeploy/update a new
image version without losing data (ties to the backlogged first-class volumes);
building inside an ephemeral sandboxed VM so the host needs no Docker even for
Dockerfile builds (extends slice 5's no-Docker pull to the build); `logs --follow`
streaming; a richer app-liveness status; persisting registry credentials
(age-encrypted) so `image update` can re-pull a private image without re-supplying
auth. App mode is firecracker-only by design (it needs the guest init), which is
the default runtime.

- *Verify on hardware (operator):* `sudo fletcher deploy <app-image> --host
  <your-domain>` (or a `./dir` with a Dockerfile), then hit the URL; `session
  logs` shows output; `session stop`/`start` and a daemon restart bring the app
  back on its own.

### Mobile-first wave (Milestones 10-14) - PLANNED (decided 2026-06-11)

Decided with the operator 2026-06-11 after walking the hero mobile scenario end
to end: create a dev session from the phone, drive Claude in it, have Claude
publish the result as an image, deploy that image, and watch it live in a
browser - without opening a laptop. The walk surfaced one hard gap (an agent has
no way to publish an image to the daemon - its MCP surface is health, http_get,
http_request, request_approval only), one sharp edge (redeploy cannot change the
image ref and loses runtime data), and one experience gap (the app is a polling
client - no push beyond approvals, no live state). Milestones below are in
priority order; the iOS counterparts live in the fletcher-ios ROADMAP.

Decisions recorded so drift is visible:

- **Inbox split (supersedes the iOS-M10 inbox sketch in the audit above).** The
  `fletcher.report` MCP tool ships as the structured *content* source for push
  notifications (Milestone 14); the dedicated inbox feed/tab is **parked** until
  a cron/jobs usage signal exists. The feed's audience (monitoring +
  non-technical users, DESIGN §7) is not today's user, and reports stay
  queryable via RPC/CLI without it.
- **Persistent volumes promoted from the backlog** (operator call, 2026-06-11):
  redeploy re-forks from the template, so anything the app wrote at runtime dies
  with the old fork. Volumes fix that and also unlock session rebase onto a
  newer base image.
- **Parked iOS-side (operator call):** Live Activities, App Intents / widgets,
  iPad / landscape, Face ID / passcode app lock (explicitly unwanted), the inbox
  tab.
- Release tagging stays manual and operator-driven (deliberately not scheduled).

### Milestone 10 - publish_image: agents publish images to the daemon - DONE (verified on hardware 2026-06-11)

**Goal (met).** An agent inside a session turns its work into an image template
the daemon can deploy - nothing leaves the network, and nothing happens
silently. Verified end to end on real microVMs: a session's disk was committed
(while running) into a template carrying a custom entrypoint, a fresh `--app`
session booted it and the app ran; the MCP path blocked on a pending approval
("publish image X by committing session Y", requester `agent:<session>`),
unblocked on `fletcher approval approve`, and published.

**Shipped.**

- `snapshot.TemplateCommitter` (optional capability on the snapshot seam). The
  ext4 driver commits by reflink-cloning the fork to a temp file and renaming,
  so a failed commit never clobbers an existing template.
- **Offline file injection** instead of writing inside the guest: a custom
  entrypoint becomes `/etc/fletcher/app.json` *in the committed template* via
  e2fsck journal replay + debugfs (both root-free, from e2fsprogs - already a
  dependency via mkfs.ext4). Found on hardware: guest exec runs as the
  unprivileged login user, so the in-guest write was a dead end; offline
  injection also means a *stopped* session can be committed with an entrypoint.
- `session.Manager.CommitImage`: syncs a running guest first (the clone is then
  at worst crash-consistent + journal-replayed), validates names, busy-marks
  the session against the idle reaper, and writes the `TemplateMeta` sidecar
  (inheriting the parent template's entrypoint/port when not overridden).
- `SessionService.CommitSessionImage` RPC + `fletcher session commit` CLI (the
  operator-initiated path, also what the iOS app will call).
- `publish_image` MCP tool: approval-gated (blocks on the pending approval,
  default 10 min, capped at 1 h), commit mode + registry mode (server-side
  pull). *Correction to the sketch:* session identity is **agent-claimed** via
  the new `FLETCHER_SESSION_ID`/`FLETCHER_SESSION_NAME` env the daemon injects
  per session - the vsock forwards splice raw bytes into one shared MCP socket,
  so connection-based identity needs per-session listeners (a follow-up if it
  ever matters). The approval card carries the resolved ground truth (which
  session would be committed, as what name), which is the actual gate.
- Drive-by guest fix: PID 1 boots with an empty environment, so a *relative*
  entrypoint (`cat` vs `/bin/cat`) never resolved in app mode - masked until
  now because Docker entrypoints are usually absolute. The guest now sets the
  standard PATH. **Takes effect on image re-import** (the guest is injected at
  import); the operator's existing `fletcher-base` template carries a
  pre-app-mode guest and should be re-imported anyway.

### Milestone 11 - redeploy to a ref + rollback - DONE (verified on hardware 2026-06-11)

**Goal (met).** The day-2 flow: the operator deployed `app:v1`; the next day
Claude pushes `app:v2` (to the daemon via M10, or to a registry); the operator
redeploys to it from the phone, and can roll back if it breaks.

**Shipped.**

- `RedeploySession` gains an optional `image`: an imported template name
  retargets the session to it; a registry ref is imported under the session's
  current template name first (and unlike the best-effort same-ref refresh, an
  explicit ref that fails to import is an error, never a silent redeploy of
  the old image). CLI: `session redeploy --image <name-or-ref>`.
- **Rollback is session-level, not template-level** (decided here): each
  redeploy *retires* the session's previous fork instead of deleting it
  (reflink-shared, so nearly free; one level kept; migration 0016), and
  `RollbackSession` / `session rollback` swaps back to it and restarts -
  swapping, so rolling forward again is the same call. `Session.has_rollback`
  tells clients when to show the button. Delete reclaims both forks.
- Two real bugs found by the hardware verification, both fixed:
  - **Hibernation restore undid the redeploy.** Stop hibernates; Start after a
    redeploy restored the memory snapshot - resuming the VM on the *old* disk.
    The snapshot fingerprint now includes the rootfs path, so a fork swap
    invalidates it and Start cold-boots the new disk.
  - **Dirty guest pages were lost on stop.** Writes still in the guest page
    cache lived only in the hibernation snapshot; a discarded/stale snapshot
    (daemon upgrade, redeploy, rollback) silently lost them - breaking "disk
    is the source of truth" (DESIGN §5). The manager now syncs the guest
    before every stop (manual, reaper, redeploy, rollback), which is also what
    makes the rollback disk actually hold the pre-redeploy state.

*Verified on hardware:* marker file written in a session disappears on
redeploy (fresh fork) and comes back on rollback; a second rollback swaps
forward; retarget to a local template updates the session's image; retarget to
a registry ref re-imports under the session's template name.

### Milestone 12 - persistent volumes - PLANNED (promoted from backlog)

**Goal.** A first-class volume object that survives session/deploy lifecycle:
its own SQLite row and its own ext4 image / btrfs subvolume (a new lineage on
the snapshot interface - "create/open volume", distinct from "clone template"),
mounted into the VM at a known path. `session delete` detaches rather than
destroys; `session create --volume <ref>` / deploy reattach it to a fresh fork;
redeploy keeps it. Single-writer semantics (one running session per volume),
disk accounting separated from fork accounting, surfaced through
create/list/get/delete RPCs + CLI verbs and the session/deploy create paths.
The full sketch (including the base-image convention questions for what lands
on the volume) is in the backlog entry this milestone absorbs - resolve the
mount-path convention at build time; agent-session-state-on-volume can follow
later.

### Milestone 13 - WatchEvents: a live client - PLANNED

**Goal.** Kill polling as the app's only source of truth. A server-streaming
`WatchEvents` RPC over the existing h2c remote surface: session state changes,
deploy restarts/crash-loops, approval created/resolved, image
imported/published, job state changes. Backed by the embedded NATS bus or an
in-process broadcaster - decided at build time (NATS is already in the stack
for exactly this, DESIGN §9). The iOS list/detail screens and approvals badge
update live; reconnect-with-backoff on stream drop.

### Milestone 14 - notification breadth + fletcher.report - PLANNED

**Goal.** The phone hears about everything that matters, not just approvals.

- **`fletcher.report` MCP tool** (the surviving half of the inbox idea): an
  agent posts `{title, summary, status, link?}`; the daemon stores a `reports`
  row tied to the source session/job and fires a push with that content. The
  hero use: Claude finishes a long task in a durable session and the phone says
  what was done instead of a generic "session idle".
- **Push events:** job finished (with exit status), deploy went live, deploy
  crash-looping (restart_count is already tracked), session went idle after
  work (the reaper's work-based idle signal repurposed as "the agent is done /
  waiting for you").
- **Per-type opt-outs** as settings keys so the iOS Notifications group has
  real dials.
- Reports are queryable (RPC + CLI + shown on session/job detail) so nothing
  depends on a feed UI.

## Toward v1 - hardening (in progress)

With M1-M6 done, the daemon is feature-complete on the thesis (ephemeral jobs +
durable interactive sessions, both verified on real hardware), which is what
makes Milestone 7 (the SwiftUI iOS client - the hero) a client deliverable on a
finished substrate rather than new backend work. This is the short list being
worked through before a v1 is a candidate, distinct from the "await a usage
signal" backlog below; it can run in parallel with M7 since the RPCs M7 needs are
already shipped. Release *tagging* is done manually by the operator and is
intentionally not in this list.

- **Cron scheduler (recurring jobs) - DONE.** A `cron` job is a definition that
  rests in a new `scheduled` status; the supervisor fires it when `next_run_at`
  is due by creating a child ephemeral run (linked by `parent_id`), so the runner
  needs no cron-awareness and every run keeps its own history. Schedules are
  standard 5-field cron or macros (robfig/cron). No double-start while a run is in
  flight, fire-once on a missed window (no backfill). Surfaced as
  `job create --trigger cron --schedule ...`. *Deferred:* pruning old child runs
  (a cron job firing every minute accumulates rows) - add a retention cap when it
  bites.
- **Install ergonomics - PARTLY DONE.** `fletcher doctor` has two Health-driven
  readiness checks that catch gaps at doctor time instead of at job time: a `Job
  runtime` check (the isolation stack: mock runtime warns about no real isolation)
  and a separate `Base image` check (no image imported is a blocker; a newer
  registry build is a follow-up). They are split because the runtime environment
  and the base-image artifact are orthogonal - a custom image on a healthy
  Firecracker stack is the common case, so an image nudge should not read as a
  runtime fault. A missing `runc` binary already fails at daemon start (the runc
  driver validates it), so it surfaces via the daemon check. *Still to do (deferred, low value):* `install.sh` auto-installing
  `btrfs-progs`/`runc` and provisioning a btrfs root for the explicit runc
  fallback - the common Firecracker path needs no extra packages. *Manual release
  action:* republish the `fletcher-base` ghcr image so brokered SSH works from the
  published image (the Dockerfile already adds `sshd`; only a local `make image`
  build carries it until the image is re-pushed).
- **Fix the codex launcher in `fletcher-base`.** `command -v codex` fails in the
  fork (the `~/.local/bin/codex` symlink targets an absent path). The image
  advertises three agents but ships two working (claude, pi).
- **Agent egress: per-job policy + daemon forward-proxy - PLANNED.** Surfaced
  while debugging why interactive Claude Code fails in a session (2026-06-07).
  Two findings drive this:
  - *Interactive Claude Code does a hardcoded connectivity check to
    `api.anthropic.com:443` that ignores `ANTHROPIC_BASE_URL`.* Headless
    `claude -p` routes through the gateway and works; the TUI opens a direct TLS
    connection to `api.anthropic.com` (proven by mapping the host to loopback and
    capturing the ClientHello, SNI `api.anthropic.com`), which the no-egress fork
    cannot resolve or reach, so it renders "Unable to connect to Anthropic
    services". The gateway env fix (`9d549f6`) is correct and unrelated - this is
    a separate gap. Stopgap for the impatient: `claude -p` works today.
  - *The daemon's egress capability is under-wired.* The `http_get` MCP tool
    reaches the public internet behind the SSRF guard (refuses loopback / private
    / link-local / metadata), but it is not registered in the base image's Claude
    config (`mcpServers` is empty) and is GET-only, so agents cannot use it yet.

  Decision (operator, 2026-06-07): make egress a per-job/environment property,
  all mediated by the daemon, so "nothing leaves your network except through your
  own box" still holds (DESIGN.md §5). One mechanism: a daemon HTTP/HTTPS
  forward-proxy (CONNECT), reached from the fork over the existing
  loopback->vsock relay (the fork keeps no NIC); the fork's `HTTP_PROXY` /
  `HTTPS_PROXY` point at it. CONNECT keeps TLS end-to-end, so no MITM/CA is
  needed, and `npm`/`pip`/`cargo`/`go`/`git`/`curl`/`WebFetch` and the Claude TUI
  check all work through it. Policy per job: `none` (today's airtight fork;
  default for private-data work), `tools` (MCP tools only), `allowlist: [...]`
  (named hosts: registries, git, your APIs, Anthropic infra), `open` (any public
  host). Default policy: `allowlist` with a curated set (package registries, git
  hosts, Anthropic infra, common docs). Two invariants on every policy: the
  LAN/metadata guard stays on (a prompt-injected agent can never reach private
  ranges or the router), and every destination is audit-logged. This subsumes the
  interactive-TUI fix (Anthropic hosts are always allowlisted) and unblocks
  coding / research / long-running-service agents while keeping private-data jobs
  airtight.

  Phasing: **Phase A - DONE (2026-06-07).** The daemon's MCP tools are now wired
  into the base image's Claude config (`mcpServers.fletcher` -> `${FLETCHER_MCP_URL}`,
  baked for both the `fletcher` login user and root; user-scope so no trust
  prompt), and a general `http_request` tool (method / body / headers, same SSRF
  guard) sits alongside `http_get`. Verified end to end in a session: `claude mcp
  list` shows the server connected, and `claude -p` fetches a public URL through
  the daemon. So headless / `-p` / job agents have daemon-mediated web access
  today. Note this does *not* unblock the interactive TUI (still gated by the
  `api.anthropic.com` check) - that needs Phase B or the stopgap relay.
  **Phase B (forward-proxy) - B1+B2 DONE (2026-06-07), B3 PLANNED.** B1 landed
  the proxy core (internal/egress: a CONNECT/HTTP proxy with Deny/Open/Allowlist
  policies) and a shared LAN guard (internal/netguard) reused by the MCP egress
  client. B2 wired it to the fork: the egress proxy serves a unix socket reached
  via a third loopback->vsock forward, with HTTP_PROXY/HTTPS_PROXY/NO_PROXY in
  the agent env, enforcing one global default `allowlist` (Anthropic infra, git
  hosts, package registries), LAN-guarded and audit-logged via slog. Verified on
  hardware: allowlisted hosts tunnel through (HTTPS via CONNECT, TLS end-to-end),
  non-allowlisted hosts get 403, and the interactive Claude Code TUI now starts -
  its api.anthropic.com and platform.claude.com startup checks ride the proxy.
  **B3 - DONE (2026-06-08).** Egress policy is now per session/job: an
  `egress_policy` column (sessions + jobs), a `--egress none|allowlist|open` flag
  on `session`/`job create`, and a `default_egress_policy` setting (default
  `allowlist`). The daemon runs two global LAN-guarded proxies (allowlist + open);
  the Firecracker driver, per the fork's policy, points the egress forward at the
  matching proxy socket - or drops the forward and strips HTTP_PROXY for `none`.
  Verified on hardware: a `none` fork cannot resolve or reach anything; an
  `allowlist` fork reaches GitHub but gets 403 on a non-listed host; an `open`
  fork reaches any public host (LAN still blocked). Scope note: enforcement is on
  the Firecracker runtime; runc (degraded fallback) keeps the global allowlist.
  Custom per-job allowlists (beyond the curated default) remain a follow-up.
  Supersedes the "MCP egress policy/approvals" backlog item below.
- **macOS client release - DONE.** One binary, no split CLI (the Consul/Vault
  model): the daemon is Linux-only, the client runs anywhere. Restored the
  darwin cross-build (two non-linux driver stubs had drifted) and guarded it with
  `make cross-check` + a new `ci` workflow. goreleaser builds slim darwin
  binaries (~8 MB vs ~20 MB; the VMM/kernel/guest are linux-embed-only);
  `install.sh` and `make install` are OS-aware (macOS = client only); `--help`
  groups Client vs Daemon (Linux host). Releases publish the linux + darwin
  tarballs to GitHub via a tag-triggered `release` workflow (`git push` a `v*`
  tag), and `install.sh` installs from there on both OSes. *Deferred:* a Homebrew
  tap/cask (needs a tap repo + token; revisit alongside a marketing site); a
  `LICENSE` file; macOS code signing / notarization (curl-installed binaries are
  not quarantined, so not blocking).

- **Default image + update detection - DONE.** `job`/`session create` default
  `--image` to a configurable `default_image` setting (`fletcher-base` out of the
  box), so the common path is just `--command`. Imports now record a sidecar
  (`<name>.meta.json`: source ref, registry digest, format) - the CLI writes it,
  the daemon reads it. The template is pinned (it does not change underneath a
  running box); a background registry check at daemon boot does a lightweight OCI
  manifest query (digest only, no pull) and, if the registry has a newer build
  than the imported digest, logs a non-fatal hint and surfaces a "newer version
  available" warning in `fletcher doctor`. `sudo fletcher image update [name]`
  re-pulls and re-imports on demand (defaults to `default_image`). Deliberately
  *not* auto-updating: reproducibility, supply-chain trust, and in-flight clone
  safety argue for pin-detect-nudge-update-on-command. A local-only image (no
  registry digest) is skipped, not flagged.

- **Per-fork auth: gateway toggle - DONE (2026-06-08); generic mounts PLANNED.**
  Subscription auth (Claude Max, ChatGPT Plus, Gemini Advanced) is supported as a
  *composition of generic primitives*, not a per-vendor `--auth` flag - Fletcher
  never models a vendor's OAuth (DESIGN.md §5 "No per-vendor auth in core").
  `session`/`job create --gateway on|off` (default from the `default_gateway`
  setting) controls whether the model-gateway env (`ANTHROPIC_`/`OPENAI_`
  base-URL + placeholder key) is injected; "off" lets an agent use its own auth
  and reach the provider through the egress proxy. Verified on hardware: a
  gateway-off session has no model-gateway env and routes claude to `/login`;
  gateway-on is unchanged (API-key-via-gateway). *Planned:* generic
  `--mount host:guest[:ro]` so a host credential dir is reused across forks -
  needs virtio-fs on Firecracker (the runc path bind-mounts directly), so
  deferred. Until then, subscription = `--gateway off` + logging in *inside* a
  durable session (the OAuth token persists in the fork across stop/start).

**Deferred for now (revisit on demand, not v1-blocking).** Audit-log storage (the
operator does not want it yet; the `audit.Noop` seam stays). Gateway breadth
(streaming in the OpenAI-compatible translation path; a second provider). APNs
push (polling works). NAT-PMP / PCP and DDNS (UPnP + a manual endpoint cover the
common case). pi-extension `registerProvider` (gated on the external `pi` API
stabilising). These stay in the backlog below.

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
- **Settings live hot-reload** - M3 settings apply on restart; the original spec
  also wanted live application (slog level in place, bounce the network actor for
  endpoint/port changes) so some changes need no restart at all.

**Security hardening**

- **`CAP_SYS_ADMIN` scope** - the daemon holds it for btrfs subvolume ops and
  the WireGuard tunnel. runc is already rootless (M2a), so this is now only
  about btrfs; unprivileged-btrfs (or a narrower mechanism) would let it go.
- **Audit log storage** - swap `audit.Noop` for a SQLite recorder (phase 4 seam).
- **MCP egress policy/approvals** - the SSRF guard landed (the egress client
  refuses non-global addresses at dial time). Still open: policy-gated egress
  (allowlists, approvals) on top of the guard (phase 6). Now folded into the
  broader **Agent egress: per-job policy + daemon forward-proxy** plan under
  "Toward v1 - hardening" above (decided 2026-06-07).

**Agents + image**

- ~~**codex launcher missing in `fletcher-base`**~~ - RESOLVED. Re-verified on
  2026-06-06: `codex --version` (codex-cli 0.137.0) runs in a real microVM fork,
  in `docker run`, and in the published ghcr image; the `~/.local/bin/codex`
  symlink resolves to a present file. The earlier breakage no longer reproduces
  (most likely fixed by the M5 import-truncation fix). All three agents
  (claude, codex, pi) work.

**Sessions + storage**

- **Persistent volumes decoupled from session lifecycle - PROMOTED to
  Milestone 12 (2026-06-11);** the sketch below is its source material - today a
  session is a
  single CoW fork where the base rootfs and `/workspace` are intermingled (M6:
  the durable unit is the fork on disk), so data lives and dies with the session
  and cannot be carried onto a newer base image. The pattern (Docker named
  volumes, k8s PV/PVC, detach/reattach block storage, a Firecracker secondary
  drive) is to make a **volume** a first-class object with its own id and SQLite
  row, its own ext4 image / btrfs subvolume (not a clone of a template), mounted
  into the VM at a known path. Then `session delete` detaches rather than
  destroys, and `session create --volume <id>` reattaches it to a fresh rootfs
  booted from the current `default_image`. This also folds in the
  session-rebase capability (update a running session to a newer image
  non-destructively = recreate on the new image with the same volume attached),
  superseding the earlier "split rootfs + data volume" sketch raised against the
  image-update work. The real work, when a usage signal arrives: a convention for
  what lands on the volume (the agent's `/workspace` *and* its on-disk session
  state, so `claude --resume` survives a rootfs swap and the rootfs is genuinely
  reconstructible - this touches the base image, not just the daemon);
  single-writer semantics (one ext4 volume mounted by at most one running session
  at a time); a second verb on the snapshot interface ("create/open volume" as a
  lineage distinct from "clone template", with detach-on-stop rather than delete);
  and disk accounting that separates rootfs from volume (the `forkBytes`/quota
  logic assumes one fork). Stays on-thesis: storage on metal the user owns,
  nothing hosted or metered, and orthogonal to the job model (a storage
  attachment, not a fourth trigger), so it does not split "one primitive, many
  hats."

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
- **PCP** - the NAT-PMP successor (RFC 6887); NAT-PMP landed, PCP is the
  remaining port-mapping protocol for routers that speak only it (phase 9).
- **Per-peer handshake/transfer visibility** - surface wireguard-go's in-process
  stats (e.g. `fletcher peer status`) since the userspace tunnel is invisible to
  `wg show`.

**Agents + gateway**

- **APNs push** - replace approval polling with push (phase 7).
- **Gateway breadth** - streaming in the translation path; a second provider
  (phase 5).
- **pi-extension `registerProvider`** - once the `pi` extension API is pinned
  (phases 11/14).
