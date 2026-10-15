# Linux host sessions

## Status and scope

The first execution location is the Linux machine running the Fletcher daemon,
not the Mac or iPhone running the client (DESIGN section 10.1). This work takes
priority over folder import and prepared-image reuse.

The account executor, persistent session lifecycle, account-discovery API and
explicit CLI creation are implemented. The native create flow and existing
session/project workspaces also support the host contract. Apple build/runtime
checks and real cross-account systemd verification remain pending below.
Host execution is disabled by default. The optional systemd templates are not installed or
enabled by the default installer, and existing VM sessions are unchanged.

VMs remain the isolated default. Host mode must be selected explicitly, with the
machine and authorized account visible throughout the workspace. It must never
replace a failed VM operation.

## Account authorization and launch

- A local administrator explicitly selects one non-root login account. This is
  not a remote account-switching API. Never choose the daemon service account.
- The same static Fletcher binary runs an optional account-scoped executor,
  started by systemd as that account. The main daemon retains its existing
  service identity and hardening. Neither process calls sudo or switches UID.
- `fletcher-host@.socket` owns a private Unix socket in a root-managed directory.
  Only the daemon account can open it. The executor also checks `SO_PEERCRED`
  against the daemon UID, rather than trusting group membership or request data.
- The executor refuses root, its own account as the daemon identity, missing
  socket activation, and non-Unix listeners. Requests bind a session ID to the
  expected account name, UID and home directory. Protocol or identity changes
  fail closed.
- The service has no Linux capabilities and enables `NoNewPrivileges`. Host
  programs cannot rely on setuid sudo or file capabilities to elevate privilege.
  Standard daemon state, configuration and socket paths are inaccessible in
  the service. Custom paths need corresponding local unit overrides.
- Do not grant the execution account access to daemon keys or state. Its other
  group memberships, reachable services and existing credentials still matter.
  Enabling an account grants the authorized Fletcher control plane the ability
  to run programs with that account's access. This is not per-project trust or
  a boundary between paired devices.

A dedicated account reduces the available files and credentials. An ordinary
login account provides its existing tools and files but grants broader access.
The operator must choose; installation must not infer consent from the account
running an upgrade.

## Terminal and filesystem contract

The runtime foundation supports named interactive terminals, tmux control mode,
PTY resize, and read-only directory listings. It reuses `SessionHandle` for these
operations and has a separate `HostSessionRuntime` entrypoint. There is no VM
image, snapshot driver, virtual disk, guest agent or provider requirement.

Each session has its own tmux socket. Named tabs and project-prefixed names share
that session's server, not the user's existing tmux server. Closing a connection
only detaches its client. Reopening the same terminal preserves its pane. The
native client can continue using pane discovery, output restoration and
`pane_current_path` queries for new-tab and split cwd inheritance.

Shells start in the requested existing directory as the account's login shell.
The launcher constructs a small environment with account identity, home, shell,
terminal and system PATH. It does not inherit daemon credentials, proxy settings,
model-gateway keys or injected environment variables. Login profiles can load the
account's normal tools and settings. Credentials already readable by that account
remain readable by its programs; there is no VM credential boundary.

Directory listings use the executor's filesystem permissions, follow directory
symlinks visibly, return resolved paths, and cap the number of entries. A selected
directory is a starting location, **not a filesystem sandbox**. A shell can read
or modify any location the account can access. Directory browsing itself does not
run shell commands or modify user files.

The first runtime slice rejects noninteractive Exec, SSH/port forwarding,
upload/download and file-mutation RPCs, guest load queries, and app supervision.
The terminal can still run ordinary host tools and edit files. Session-manager
guards reject image commits, redeploy/rollback, volumes, credential seeding, VM
egress settings, gateway injection and deployment flows before any VM or snapshot
operation is attempted. Host sessions are excluded from the VM idle reaper.

**Dependency exception:** optional host terminals require host-installed `tmux`,
matching the protocol already used by guest terminals and the native workspace.
This is a deliberate opt-in exception to STANDARDS.md's dependency guideline
against external runtime binaries. No new Go dependency or CGO requirement is
introduced. Hosts that do not enable this mode do not need this prerequisite.

## Lifetime, cleanup and revocation

- Starting the runtime registers an account-bound terminal namespace. The first
  attachment starts its shell. Starting an existing namespace is idempotent;
  mismatched account or working-directory metadata is rejected.
- Stopping a session cancels its attachments and terminates only its dedicated
  tmux server. It never invokes a global tmux kill, kills by UID, or removes a
  working directory. Detached or independently managed programs can outlive a
  terminal; session Stop is not VM shutdown.
- Stopping the account executor uses systemd `KillMode=control-group` to terminate
  processes still in that service. The executor is bound to the daemon service,
  so it does not intentionally survive a daemon service stop. Process continuity
  across service restart or reboot is not promised.
- Host programs are not sandboxed. A program can delegate work to an existing
  user service or another reachable service. Neither closing a terminal nor
  stopping the executor guarantees termination of that independently managed
  work or reverses its filesystem/network effects.
- Revocation must stop and disable the account socket, stop the account service,
  and remove the daemon's local authorization. Disabling only new connections is
  insufficient while existing processes or streams remain. Revoking one paired
  device is separate from revoking the host account for all devices. Revocation
  does not recover exposed credentials or undo prior account and file changes.
- Removing a project registration preserves files and processes. Removing a host
  session must stop its Fletcher-managed terminals and delete only its metadata,
  never host directories. There is no snapshot or rollback of host-side changes.

## Local setup and revocation

Review the account permissions and complete the systemd checks below before
enabling this on a live host. Use a normal login account to reuse its existing
tools, or deliberately choose a dedicated account. Never enroll an account
silently during installation or upgrade.

1. Install `tmux` using the host's package manager and install the updated Fletcher
   binary. Keep the daemon running as its existing `fletcher` service account.
2. Install `init/fletcher-host@.socket` and `init/fletcher-host@.service` under
   `/etc/systemd/system/`. Review `InaccessiblePaths` if daemon state, keys or
   sockets use nonstandard locations.
3. In a local `fletcher.service` override, set
   `Environment=FLETCHER_HOST_USER=<account>` to the explicitly selected account.
   This boot setting cannot be changed through the remote settings API.
4. Reload systemd, enable `fletcher-host@<account>.socket`, and restart the daemon
   during an agreed maintenance window. The socket starts its account service on
   demand. The executor and daemon must use the same binary/protocol version.
5. Upgrade paired clients before creating host sessions. Older clients do not
   understand the host-specific identity, capabilities or deletion explanation.

After authorization:

```sh
fletcher session create --host --name host-work
fletcher session shell host-work
# Optional initial directory, which must already exist for that account:
fletcher session create --host --name repository --workdir /home/operator/repo
```

The CLI discovers and confirms the authorized username/UID; it does not choose
another account. Omitting `--host` retains the VM default. Projects can reference
host sessions and default to the session's initial directory rather than
`/workspace`. Start, Stop, Restart and Delete keep the recorded execution kind.

To revoke access, stop and disable `fletcher-host@<account>.socket`, stop
`fletcher-host@<account>.service`, remove `FLETCHER_HOST_USER` from the local daemon
override, and restart the daemon. Stop both units, not just the socket. Host files
and independently managed services remain the operator's responsibility.

## Remaining integration and verification

1. Implemented: explicit execution kind and immutable account/directory binding;
   existing rows and omitted create modes remain VM sessions. Lifecycle tests
   cover no snapshot/runtime fallback, account mismatch, stop/delete failures,
   duplicate-create cleanup, persistence and project deletion semantics.
2. Implemented: local `--host-user` authorization, `GetHostSessionAccount`, explicit
   API/CLI host creation and VM-only capability guards. Service-bound lifetime,
   actual cross-account revocation and reboot recovery still need runtime checks.
3. Implemented in the native source: explicit Host creation with account discovery
   and confirmation, visible account/daemon labels, host-specific deletion text,
   browsable project folders, and hidden VM-only controls. Existing tab, split,
   pane-discovery, live-cwd and transport recovery logic is unchanged.
4. Verify root-owned socket activation and distinct daemon/executor UIDs under
   systemd, account permissions, service restart, revocation, process cleanup and
   unavailable/mismatched helpers on a deliberately authorized test account.
5. Run native package tests and both Apple builds, then signed-app terminal,
   reconnection, browsing and configuration checks. Local Swift syntax checks
   do not replace compiler, package-test or signed-runtime verification.

Runtime tests cover framing limits, account and Unix-peer validation, protocol
binding, clean launch environments, directory resolution, control-mode terminal
I/O and cwd queries, cancellation, detach persistence, server-specific shutdown,
and preservation of working-directory files. The tmux tests use private temporary
sockets and directories; they do not enable the account service or exercise a
real cross-account systemd deployment.
