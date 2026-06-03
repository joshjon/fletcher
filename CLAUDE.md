# Fletcher

Private agent compute on hardware the user owns. A single Go binary on one Linux box; native clients spin up isolated VMs and run agents/jobs/programs with nothing leaving the user's network.

**Read `DESIGN.md` first.** It is the source of truth for positioning, architecture, and rationale. This file is operational guidance — what to do, what not to do — distilled from it.

## Thesis (do not drift from these)

- **The moat is structural: it runs on metal the user owns.** Any choice that requires us to host infrastructure, meter usage, or route traffic through a service we operate is off-thesis. The developer hosts nothing.
- **One primitive, many hats.** A job = environment + payload + trigger + sink. `ephemeral`, `cron`, `long-running` are three values of one field — never three subsystems. (§4)
- **The fork is the sandbox; the daemon is the gate.** Agents run as native subprocesses inside a CoW fork with no creds and no egress route. Privileged ops are exposed as MCP tools on the daemon, which holds the credentials. Gate by *capability*, not by intercepting intent. (§5)
- **No workflow engine in core.** Temporal is explicitly cut. Resume = supervisor goroutine reads active jobs from SQLite on boot, restarts agent processes against their on-disk sessions + restored fork snapshot + idempotent egress. (§5)
- **Daemon is the model gateway.** All agents point their base-URL at the daemon; keys never enter forks. This is what makes the trust boundary in §5 hold. (§6)

## Platform & build constraints

- **Linux only for now.** macOS is deferred (§10). Do **not** scatter `exec("btrfs ...")` or `/dev/kvm` checks through job/agent/gateway code — all KVM/Firecracker calls live behind the runtime interface, all btrfs calls behind the snapshot interface. The interface seams exist so macOS becomes one more driver, not a rewrite.
- **Single static binary, `CGO_ENABLED=0`.** Pure-Go SQLite (`modernc.org/sqlite`). Anything that pulls in CGO needs a strong justification.
- **VMM bundled via `embed.FS`**, extracted on first run. One VMM process per VM at runtime.

## Stack (canonical choices — see §9 for the full table)

- API: `connectrpc.com/connect-go` (one handler → gRPC, HTTP/JSON, gRPC-Web)
- Runtime: Firecracker + `firecracker-go-sdk` (default); `runc`/containerd (labeled degraded-isolation fallback)
- Image pipeline: `firecracker-containerd`
- Fork/snapshot: btrfs subvolumes behind a snapshot interface
- State: `modernc.org/sqlite` + `sqlc` + `goose`
- Secrets: `filippo.io/age`
- Events: embedded NATS JetStream
- Daemon ⇄ guest: vsock
- Networking: `wireguard-go` + UPnP/NAT-PMP/PCP + DDNS (hub-and-spoke, no DERP/ICE)
- Agents: MCP server (`mark3labs/mcp-go`); spawn Claude Code / Codex / OpenHands / Aider / Goose
- Lifecycle: `knadh/koanf` + `spf13/cobra` + `oklog/run`

## Conventions

- **Two networking planes never touch.** Clients are WireGuard peers to the daemon only. VM networking lives entirely inside the box. Preview URLs are the daemon reverse-proxying into a VM — clients never get a route into VM-land.
- **Idempotency keys on every egress op.** Required regardless of any engine. A crash-resume must not double-apply.
- **Approvals are `pending_approval` rows in SQLite + APNs push.** Survives reboot because the row does.
- **Recurring jobs default to agent-authored-then-automated.** Agent writes the scraper once; a plain cron'd program runs it. Use agent-in-the-loop only when each run needs judgment.

## Out of scope (do not propose)

- macOS support (deferred, §10)
- Multi-box mesh / hosted control plane / coordination SaaS
- Built-in metering or billing
- A skill marketplace (explicit non-goal — attack surface contradicts trust positioning, §8)
- Cross-site VM-to-VM networking
- Self-writing-skills me-too features
- Anything that turns in-fork bash into orchestrated tasks (the fork is already the sandbox, §5)

## Open questions to verify before betting on

Listed in §11; check actual repos/tools before designing around them:
- `firecracker-containerd` current state
- Firecracker snapshot/restore maturity (note: live memory-snapshot is *instant-wake UX only*, not load-bearing for durability)
- Agent resume ergonomics — that the chosen agent(s) can be restarted against a persisted session
- Base-URL override per agent (Claude Code, Codex, OpenAI-compatible generally do; verify each)

## Working norms for this repo

- When proposing architecture, cite the relevant §section of `DESIGN.md` so drift is visible.
- If a suggestion would require the developer to host something, route traffic through a service we operate, or meter usage — stop and flag it; it's off-thesis.
- If a suggestion would split the job model into multiple subsystems, stop and flag it (§4).
- If a suggestion adds Linux-specific calls outside the runtime/snapshot interfaces, stop and flag it (§10).
