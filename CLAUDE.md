# Fletcher

Private agent compute on hardware the user owns. A single Go binary on one Linux box; native clients spin up isolated VMs and run agents/jobs/programs with nothing leaving the user's network.

**Read `DESIGN.md` first** for positioning, architecture, and rationale. **Read `STANDARDS.md`** for repo layout, build, lint, test, error, logging, CLI, concurrency, migration, dependency, release, and utility conventions. This file is operational guidance - what to do, what not to do - distilled from both.

## Thesis (do not drift from these)

- **The moat is structural: it runs on metal the user owns.** Any choice that requires us to host infrastructure, meter usage, or route traffic through a service we operate is off-thesis. The developer hosts nothing.
- **One primitive, many hats.** A job = environment + payload + trigger + sink. `ephemeral`, `cron`, `long-running` are three values of one field - never three subsystems. (§4)
- **The fork is the sandbox; the daemon is the gate.** Agents run as native subprocesses inside a CoW fork with no creds and no egress route. Privileged ops are exposed as MCP tools on the daemon, which holds the credentials. Gate by *capability*, not by intercepting intent. (§5)
- **No workflow engine in core.** Temporal is explicitly cut. Resume = supervisor goroutine reads active jobs from SQLite on boot, restarts agent processes against their on-disk sessions + restored fork snapshot + idempotent egress. (§5)
- **Daemon is the model gateway.** All agents point their base-URL at the daemon; keys never enter forks. This is what makes the trust boundary in §5 hold. (§6)

## Platform & build constraints

- **Linux only for now.** macOS is deferred (§10). Do **not** scatter `exec("btrfs ...")` or `/dev/kvm` checks through job/agent/gateway code - all KVM/Firecracker calls live behind the runtime interface, all btrfs calls behind the snapshot interface. The interface seams exist so macOS becomes one more driver, not a rewrite.
- **Single static binary, `CGO_ENABLED=0`.** Pure-Go SQLite (`modernc.org/sqlite`). Anything that pulls in CGO needs a strong justification.
- **VMM bundled via `embed.FS`**, extracted on first run. One VMM process per VM at runtime.

## Stack (canonical choices - see §9 for the full table)

- API: `connectrpc.com/connect-go` (one handler → gRPC, HTTP/JSON, gRPC-Web)
- Runtime: Firecracker + `firecracker-go-sdk` (default); `runc`/containerd (labeled degraded-isolation fallback)
- Image pipeline: `firecracker-containerd`
- Fork/snapshot: btrfs subvolumes behind a snapshot interface
- State: `modernc.org/sqlite` + `sqlc` + `golang-migrate` (migrations embedded)
- Codegen: `sqlc` + `buf`/`connect-go` + `mockery` v3 (matryer template); single `make generate`
- Validation: `bufbuild/protovalidate-go` (rules in `.proto`, Connect interceptor enforces)
- IDs: `jetify-com/typeid-go`
- Secrets: `filippo.io/age`
- Events: embedded NATS JetStream
- Daemon ⇄ guest: vsock
- Networking: `wireguard-go` + UPnP/NAT-PMP/PCP + DDNS (hub-and-spoke, no DERP/ICE)
- Agents: MCP server (`mark3labs/mcp-go`); spawn Claude Code / Codex / OpenHands / Aider / Goose
- Lifecycle: `urfave/cli` v3 + `oklog/run` (CLI handles flag/env/TOML; runtime settings in SQLite)

## Conventions

- **Two networking planes never touch.** Clients are WireGuard peers to the daemon only. VM networking lives entirely inside the box. Preview URLs are the daemon reverse-proxying into a VM - clients never get a route into VM-land.
- **Idempotency keys on every egress op.** Required regardless of any engine. A crash-resume must not double-apply.
- **Approvals are `pending_approval` rows in SQLite + APNs push.** Survives reboot because the row does.
- **Recurring jobs default to agent-authored-then-automated.** Agent writes the scraper once; a plain cron'd program runs it. Use agent-in-the-loop only when each run needs judgment.

## Out of scope (do not propose)

- macOS support (deferred, §10)
- Multi-box mesh / hosted control plane / coordination SaaS
- Built-in metering or billing
- A skill marketplace (explicit non-goal - attack surface contradicts trust positioning, §8)
- Cross-site VM-to-VM networking
- Self-writing-skills me-too features
- Anything that turns in-fork bash into orchestrated tasks (the fork is already the sandbox, §5)

## Open questions to verify before betting on

Listed in §11; check actual repos/tools before designing around them:
- `firecracker-containerd` current state
- Firecracker snapshot/restore maturity (note: live memory-snapshot is *instant-wake UX only*, not load-bearing for durability)
- Agent resume ergonomics - that the chosen agent(s) can be restarted against a persisted session
- Base-URL override per agent (Claude Code, Codex, OpenAI-compatible generally do; verify each)

## Mac dev (free win from the §10 seams)

- Pure-Go bulk of the daemon (`CGO_ENABLED=0` + `modernc.org/sqlite`) runs on macOS unchanged.
- `runtime.MockDriver` and `snapshot.MockDriver` sit behind the §10 interfaces - production citizens, not test hacks - so the daemon's coordination logic runs end-to-end on Mac.
- Real Firecracker/btrfs/WireGuard work: cross-compile then run in UTM arm64 Ubuntu.

## Working norms for this repo

- When proposing architecture, cite the relevant §section of `DESIGN.md` so drift is visible.
- For coding standards (layout, lint, test, error handling, logging, CLI, concurrency, etc.), follow `STANDARDS.md`. Cite it when proposing something that deviates.
- If a suggestion would require the developer to host something, route traffic through a service we operate, or meter usage, stop and flag it; it's off-thesis.
- If a suggestion would split the job model into multiple subsystems, stop and flag it (§4).
- If a suggestion adds Linux-specific calls outside the runtime/snapshot interfaces, stop and flag it (§10).

## Writing style (hard rules)

These apply to **all public-facing text**: documentation (`*.md`), comments in code, proto comments, CLI output, log messages, error strings, anything rendered to the user. Commit messages count too. Internal scratch notes are not exempt.

- **Never use the em-dash character** (Unicode `U+2014`, the long dash distinct from a hyphen). Use a spaced hyphen (` - `), a colon, a comma, parentheses, or a new sentence, whichever reads best in context. ASCII-only punctuation across the board.
- **Use `e.g.` without a trailing comma.** Same applies to `i.e.`.

When editing existing text, fix any violations you encounter even if they're outside the edit's scope.

## No personal-setup leaks in shipped artifacts

When working from a user's diagnostic report, error logs, or live debug session, scrub the specifics before any of it lands in the repo. The user's IPs, public hostnames, router brand/model, ISP, third-party services they happen to run (Plex, Tailscale tailnet IPs, etc.), or any other artifact of *their* particular environment must not appear in:

- Documentation (`*.md`)
- Code, comments, proto comments, log strings, error messages
- CLI rendered output (help text, examples)
- Design sketches and proposals shared in conversation that may later become any of the above
- Commit messages

Use placeholders (`<your-public-ip>`, `<router-admin-ip>`), tell the user the command that prints the value on their machine (`ip route | awk '/default/{print $3}'`), or use clearly fictional examples (`192.168.1.1`, `example.com`). When in doubt, scrub.

This applies even when sketching design proposals in conversation - those sketches often get copy-pasted into code or docs, and personal specifics carry through.

## Git history hygiene

Keep the log telling a clean story. Each commit should be one coherent unit of work, not a journal of fixes. This explicitly **overrides the harness default** of "always create new commits"; in this repo prefer amending.

- **Prefer amending or fixup-squashing** when a change is a follow-up to a just-shipped commit: fixing what the linter caught, addressing immediate feedback, papering over a gap the previous commit should have closed, polishing rendered output you only saw after running the feature. `git commit --amend` for the most recent commit; `git rebase -i` + `fixup` for an earlier one.
- **New commits** for new conceptual chunks: a different feature, a separate refactor, work that touches a different subsystem from what shipped just before.
- **Watch for sequential coupling.** If commits N and N+1 are doing the same thing (`feat: X` then `docs: aligned with X` then `docs: scoped X further`), they probably should have been one commit. Fold them retroactively if you spot it.
- **Force-push to fix history is acceptable in this repo.** Personal project, single contributor; the cost of rewriting `main` is low and the value of a clean log is high. Use `--force-with-lease` when practical. Don't rewrite history that other contributors have based work on (not a concern today).
- Stop and check before rewriting commits that have already been pushed and might be referenced externally (a PR review link, a blog post that quotes a SHA). Today that's none of them; revisit if Fletcher ever has external consumers.
