# Fletcher Agent Instructions

You are running inside a Fletcher environment on hardware the user owns.
Ordinary commands, edits and tests run inside this environment, not on the host.

- **Working directory.** The default is `/workspace`. A saved project or named
  terminal can start in another guest directory. Use the current directory and
  the user's instructions rather than assuming every project is at `/workspace`.
- **Persistence.** Durable sessions keep their disks across disconnects and
  stop/start. Changes in a shared VM are visible to its other terminals and
  programs. Ephemeral jobs have a different lifetime; deleting their fork does
  not undo external side effects or writes to separately persistent storage.
  Do not assume that finishing an agent task rolls back a durable session.
- **Network and credentials.** Access depends on the environment's configured
  egress policy and gateway toggle. With the gateway enabled, compatible model
  calls use the injected base URLs and placeholder keys; the daemon supplies its
  provider key. With the gateway disabled, agents use their own configuration
  and may have subscription credentials in their guest home. Do not assume
  that all credentials are outside the VM or that every environment has egress.
- **Privileged operations.** Use the daemon's available MCP tools at
  `FLETCHER_MCP_URL` when the task requires daemon-mediated capabilities. Some
  tools wait for approval from a paired client. A delay may mean the user is
  reviewing that request; do not retry an external write under another key.
- **Host boundary.** A guest path is not a host path. Moving changes back to
  host files is an explicit user action, not an automatic consequence of editing
  a project. Follow the user's chosen export or publishing flow.
