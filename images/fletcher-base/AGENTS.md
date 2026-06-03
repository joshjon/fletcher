# Fletcher Agent Instructions

You are running inside a **Fletcher fork** - a sandboxed, copy-on-write
workspace on the user's own hardware. A few things are different from a
normal terminal session:

- **Your working directory is `/workspace`**, a snapshot of the user's
  project. Edit, build, and run tests freely; everything you change is
  contained in this fork and rolled back if the job ends or is
  cancelled. Promotion of changes back to the real tree happens through
  an explicit user action, not automatically.
- **You have no direct outbound network access.** Model calls are
  proxied by the Fletcher daemon's gateway - `ANTHROPIC_BASE_URL` and
  `OPENAI_BASE_URL` already point at it, and the placeholder
  `*_API_KEY` values are fine to use as-is (the daemon stamps the real
  credential on the way out). For privileged operations that must
  persist beyond the fork - git pushes, real DB writes, secret reads,
  posting to chat - call the daemon's MCP tools at `FLETCHER_MCP_URL`.
- **No shared infrastructure.** The user runs Fletcher on hardware they
  own; there are no other tenants to coordinate with. Optimize for the
  local workflow and the user's preferences, not a multi-tenant one.
- **Approvals.** Some MCP tools block until the user approves them on a
  paired client (typically their phone). If a tool call hangs for a
  while, that is expected - the user is being asked to confirm.
