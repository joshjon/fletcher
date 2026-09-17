# Fletcher

Personal compute and app hosting on hardware the user owns. A single Go daemon on one Linux host, managed through native iOS and macOS clients. The core experience is creating VMs and deploying container images. Cloud in a Bottle and exe.dev are broad product references, not integration requirements. Agents are an optional use inside those environments.

**Read `DESIGN.md` first** for positioning, architecture, and rationale. **Read `STANDARDS.md`** for repo layout, build, lint, test, error, logging, CLI, concurrency, migration, dependency, release, and utility conventions. This file is operational guidance - what to do, what not to do - distilled from both.

## Thesis (do not drift from these)

- **VMs and apps first.** The native client is the primary management interface, not an agent companion. VM creation and image deployment must be useful without an agent or model account. Do not turn the roadmap into an agent-harness feature race. (DESIGN sections 1, 2 and 7)
- **Self-hosted compute, not a structural moat.** The user controls compute and storage. No hosted compute, mandatory hosted control plane or metering. Network access and external providers determine what leaves the host. Self-hosting and native clients are shared capabilities, not uniqueness claims. (DESIGN sections 1 and 8)
- **One job model for background execution.** A job = environment + payload + trigger + sink. Keep trigger variants together. This does not make every interactive VM or app an agent task. (DESIGN section 4)
- **The fork is the sandbox, the daemon mediates access.** Programs run natively inside isolated environments. Credential and egress guarantees depend on the configured mode. Do not promise that subscription credentials stay outside a guest where the user logs in. (DESIGN section 5)
- **No workflow engine in core.** Temporal remains excluded. Lifecycle management uses the existing supervisor, persistent state and runtime interfaces. (DESIGN sections 3 and 5)
- **The model gateway is optional.** When enabled, it holds provider API keys and mediates model calls. It is not required for VM or app management, and subscription login is a separate credential mode. (DESIGN section 6)

## Platform & build constraints

- **Linux host runtime only for now.** The macOS host runtime is deferred (DESIGN section 10), not the native macOS client. Do **not** scatter `exec("btrfs ...")` or `/dev/kvm` checks through job/agent/gateway code - all KVM/Firecracker calls live behind the runtime interface, all btrfs calls behind the snapshot interface. The interface seams exist so macOS becomes one more driver, not a rewrite.
- **Single static binary, `CGO_ENABLED=0`.** Pure-Go SQLite (`modernc.org/sqlite`). Anything that pulls in CGO needs a strong justification.
- **VMM bundled via `embed.FS`**, extracted on first run. One VMM process per VM at runtime.

## Stack (canonical choices - see §9 for the full table)

- API: `connectrpc.com/connect-go` (one handler → gRPC, HTTP/JSON, gRPC-Web)
- Runtime: Firecracker + `firecracker-go-sdk` (default); `runc`/containerd (labeled degraded-isolation fallback)
- Image pipeline: self-built (`docker build` -> flattened ext4 rootfs image, CoW-cloned per job behind the snapshot interface); `firecracker-containerd` was evaluated and rejected (§11)
- Fork/snapshot: btrfs subvolumes (runc) / reflinked ext4 images (Firecracker) behind a snapshot interface
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
- **Recurring work can be an ordinary program.** Whether written by a person or an agent, a script can run on a schedule. Use an agent on each run only when the task needs one.

## Out of scope (do not propose)

- macOS host runtime (deferred, DESIGN section 10). The macOS client is in scope.
- Multi-box mesh / hosted control plane / coordination SaaS
- Built-in metering or billing
- A skill marketplace or a first-party agent harness (DESIGN sections 2 and 8)
- Cross-site VM-to-VM networking
- Self-writing-skills me-too features
- Anything that turns in-fork bash into orchestrated tasks (the fork is already the sandbox, §5)

## Open questions to verify before betting on

Listed in §11; check actual repos/tools before designing around them:
- `firecracker-containerd` - RESOLVED (M5): evaluated and rejected for a self-built ext4 pipeline (§11)
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
- Flag any proposal for developer-hosted infrastructure, routed traffic or metering. Core compute and lifecycle management must remain self-hosted. The separately deferred push-delivery exception is recorded in `docs/ROADMAP.md`, not permission to introduce a hosted control plane.
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
