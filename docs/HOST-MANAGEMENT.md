# Native host management

The iOS and macOS apps are the primary interface for ordinary Fletcher
administration (DESIGN.md sections 1 and 7). The Host area uses the same
self-hosted, authenticated Connect API as sessions. It does not run SSH commands,
expose a host shell, or introduce a hosted control plane.

## Available controls

- Devices: list peers, identify the current device, issue a WireGuard invitation
  or VPN login, and revoke another device. The app blocks self-revocation.
- Images: list templates, import registry images, update from recorded registry
  sources, choose a default, deploy, and delete unreferenced templates.
- Maintenance: version and readiness, daemon restart, bounded process logs,
  read-only doctor diagnostics, storage accounting and idle build-cache cleanup.
- Configuration: all operational keys exposed by SettingsService, with explicit
  save/reset, live application where supported and restart-required warnings.
- Secrets and persistent-volume management reuse the existing client surfaces.

## Safety and lifecycle

Every paired device is a trusted administrator. There is no viewer role. Network
management calls require a valid peer token; the public pairing listener still
exposes only CompletePair. ServerConfig, which includes the daemon's private
WireGuard key, is now local-socket-only. Peer revocation cancels active API
requests, including streams over a user-provided VPN, as well as rejecting new
requests. Already accepted background work belongs to the host and continues.

WireGuard invitations are one-time and expiring; the new device creates its own
private key. VPN login blobs are bearer credentials that remain valid until
revocation. Share either privately. The apps do not persist invitations.

Registry imports use persisted operation IDs. Repeating a request ID returns the
same operation, not another pull. Only one managed import runs at a time. Client
disconnects do not cancel accepted work; imports have a 30-minute limit. Shutdown
cancels imports and records their result. After an unclean restart, running rows
become interrupted failures, not silently resumed pulls. Inspect the template
before retrying an interrupted operation: publication may have completed before
the result was recorded. Registry credentials are not stored in operation rows.

Image publication uses a sibling temporary file and atomic rename or
create-if-absent link. Boot-file injection is confined to the extracted rootfs,
including symlink handling. A failed import preserves the old template. Updates affect
future forks, not existing session disks. Deletion checks the default image,
sessions and queued/running/scheduled jobs, then delegates removal to the snapshot
driver (DESIGN.md section 10). It does not delete session data or rollback forks.

Restart is available only when this process is the installed systemd service and
its policy restarts nonzero exits. The daemon validates stored configuration,
acknowledges the request, gracefully shuts down and returns a nonzero exit for
systemd to restart it. No sudo, polkit rule or additional privileged service is
needed. A process-generation check prevents an ambiguous retry from restarting
the replacement daemon. Clients wait for a new generation and reconnect. This is
not zero-downtime: connections and running work can be interrupted, and changed
network/storage/runtime settings can still require local recovery.

## Diagnostics and storage

Logs are the latest 256 KiB of structured logs from the current process, not the
system journal or a retained audit log. The UI polls while visible and replaces
the tail after restart. Review logs before sharing them.

Read-only diagnostics reuse the local doctor checks. Public-IP and provider
connectivity probes are opt-in and make outbound requests from the host. Router
mapping probes are deliberately omitted: this RPC never changes router mappings.
Host-side checks cannot prove remote client reachability or repair a broken host.

Storage reports filesystem capacity and available space, plus allocated and
logical sizes by category. Symlinks are not followed. Allocated blocks are not
exclusive physical usage: reflinks can count shared blocks multiple times. Do not
sum categories to estimate reclaimable space. Daemon state can be on a different
filesystem from snapshots. Build-cache cleanup refuses while a build owns the
cache; it never removes session or volume data. Review individual images and
volumes rather than blindly pruning everything.

## CLI

The same remote APIs are available without SSH:

```sh
fletcher host get
fletcher host logs --lines 100
fletcher host restart
fletcher host storage
fletcher host diagnose
fletcher host diagnose --external
fletcher host clear-cache
fletcher image list
fletcher image pull nginx:stable --name web
fletcher image imports
fletcher image update web
fletcher image delete web
```

Pull/update wait for completion by default; `--detach` returns immediately.
Use `-o json` on inspection/import commands and `--yes` to confirm destructive
commands noninteractively. Existing `fletcher --remote ... --token ...` and saved
login configuration work unchanged. `image ls` and `image rm` remain aliases.
An explicit `--btrfs-root` (including its environment variable) selects the legacy
local image path. `fletcher daemon` remains the local systemd facade for recovery.

## Boundaries

- Installation, first pairing, offline start, service enable/disable and a broken
  daemon/network still need a bootstrap or recovery route. There is no remote
  stop button that strands the API needed to start it again.
- The existing registry importer is unprivileged. Images depending on original
  ownership, device nodes or setuid setup can still need a local root-privileged
  import. Local Dockerfile imports remain a host workflow; builds from session
  workspaces already have a native deployment flow.
- Image import requires ext4 snapshots. Remote template deletion is available on
  ext4 and btrfs; unsupported drivers return an explicit error.
- Bootstrap config, arbitrary host OS administration, automatic updates, storage
  migration and repair are not exposed by these controls.

## Verification

Backend lint, race tests, code generation and macOS cross-compilation run through
`make check`. Native code generation and Xcode source registration are committed.
Linux service restart and Apple builds/device behavior need hardware verification:

1. Upgrade the daemon and both native clients. Open Host (iOS tab, macOS sidebar
   or Box menu; also linked from Settings).
2. Pair a second device in each supported transport mode; verify current-device
   labeling, expiration and revocation, including an existing terminal stream.
3. Import a registry image, background/reopen the app and inspect completion.
   Test a failed pull, private registry credentials and an image update. Confirm
   existing session disks are unchanged and referenced images cannot be deleted.
4. Change a live setting and a restart-required setting. Reset both and verify
   their startup defaults. Restart from Host and confirm reconnection, refreshed
   generation and clearing of the pending-restart banner.
5. Review logs and run diagnostics with external probes off, then on if desired.
6. Review storage, clear the idle build cache, confirm a running build blocks
   cleanup, and delete only a deliberately disposable detached volume/template.
