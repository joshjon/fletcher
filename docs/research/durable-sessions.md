# Prior art: durable, interactive agent sandboxes

> **What this is.** A reference survey of how existing hosted dev-environment /
> sandbox systems implement durable, interactive sessions, captured to inform
> Fletcher's Milestone 6 (see `docs/ROADMAP.md`). It records *patterns,
> trade-offs, and mechanics* and links primary sources. The design choices
> Fletcher actually makes are its own, derived from the single-box, daemon-gated,
> no-route-into-VM-land constraints - this file is research input, not a spec and
> not product positioning. Claims here were cross-checked across sources;
> vendor-published latency figures are best-case and are not repeated as fact.

## 1. Two persistence models

The field splits cleanly into two ways to keep a workspace across a stop:

- **Disk-only.** A persistent disk/volume survives a stop; the running process
  state (RAM) is lost, and you cold-boot from the disk. Simple and durable. Used
  by persistent-disk box providers, by Gitpod (which persists **only**
  `/workspace` - changes to system areas or image-installed tooling outside it
  are discarded on stop, and workspaces auto-stop after ~30 min idle), and by Fly
  Machines (root filesystem ephemeral by default; durable state lives on named
  volumes; the proxy auto-stops idle Machines).
- **Memory-snapshot.** The filesystem **and** full RAM are saved, so processes
  resume exactly where they were. Used by E2B (`pause` saves filesystem + memory)
  and microVM platforms built on Firecracker snapshots.

Takeaway for Fletcher: durability should rest on the disk (the fork), with a
memory snapshot as an optional faster path back to a state the disk already
holds - matching DESIGN §5/§11. Both layers are in M6 scope.

## 2. Hibernation / fast-wake mechanics (Firecracker)

- A Firecracker snapshot comprises a **guest-memory file plus a VM-state file**;
  you must **pause the VM first**, then create the snapshot.
- **Resume is fast because the guest memory is memory-mapped and paged in
  lazily** - you do not read all of RAM up front.
- **Pause cost scales with allocated RAM** (more guest memory = more to write).
- A known optimisation is a **virtio memory-balloon**: inflate it to drop the
  guest page cache *before* snapshotting, so the memory file is smaller. The
  trade-off is the guest re-faulting that cache after wake.
- After restore, in-guest connections (incl. the host<->guest control channel)
  are broken and must be re-established - relevant to Fletcher's vsock channel.
- Snapshots are tied to the Firecracker version and guest kernel, so a platform
  upgrade invalidates them (hence Fletcher keeps cold-boot as the fallback).

## 3. Interactive access is always brokered

No system exposes the sandbox directly to the network; access goes through a
control plane:

- One box provider makes **SSH itself the entire control-plane API** - you
  create and manage boxes over SSH.
- Fly brokers SSH into private VMs by running a **small SSH server inside the VM**
  ("hallpass"), reached over **WireGuard run as an unprivileged userspace
  process** in the client - structurally the same shape as Fletcher's
  wireguard-go daemon brokering into vsock.
- Gitpod brokers all SSH through an intermediary proxy.
- **IDE attach (VS Code Remote-SSH, JetBrains Gateway) rides on that brokered
  SSH** - the editor installs a small server inside the machine over the SSH
  connection. This is why "real SSH," not a bespoke terminal channel, is what
  unlocks IDEs.

Takeaway: Fletcher's brokered-SSH-over-vsock plan (M6) is the well-trodden path;
the daemon is the broker, the VM stays unroutable.

## 4. Agent-in-the-loop persistence

- A coding agent can run as a **long-lived process inside the box** (one provider
  ships a persistent in-box agent).
- Claude Code keeps **session state on disk** and can resume a prior session by
  id (Agent SDK sessions); separately, the community leans on **tmux/screen** to
  keep an agent process alive across a disconnected terminal.
- **Not well documented anywhere:** the exact mechanic for resuming an in-VM
  agent *conversation* - live memory snapshot vs tmux reattach vs re-spawn against
  on-disk session state. This is genuinely Fletcher's to decide; M6 leads with
  re-spawn-against-on-disk-session (durability-correct) and treats the live
  restore as the snapshot path.

## 5. Lifecycle and API shape across systems

- Common nouns/verbs: a named **instance / machine / box / sandbox** with
  `create`, `start`, `stop`, `exec`, `ssh`, `snapshot`, `destroy`.
- Several split **`stop`/`start` (cold)** from **`suspend`/`resume` (memory
  snapshot)** as distinct verbs - a useful precedent for Fletcher's two wake
  layers.
- **Idle auto-stop** (e.g. a ~30 min timeout, or a proxy stopping idle instances)
  is the standard way to reclaim resources from persisted-but-unused environments.
- A durable workspace is modelled as a long-lived named instance; an ephemeral
  task run is a one-shot that is destroyed - the same split Fletcher draws between
  `session` and `job`.

## 6. Open questions this survey could not resolve

(These are reflected in M6's "Remaining open questions.")

- The exact agent-*conversation* resume mechanic (above) - undocumented.
- The wake-time cost of re-faulting a balloon-cleared page cache, and whether the
  agent process itself gets ballooned out.
- On a single box, reclaiming RAM from many idle-but-resumed VMs given the
  memory-file-must-stay-resident constraint - i.e. paging a resumed VM's memory
  out and back without a full stop/start. (Fletcher's sleep = full stop + memory
  file on disk sidesteps this for the *asleep* state.)
- Whether one brokered channel covers every interactive flow (PTY resize, signal
  forwarding, `scp`/`sftp`, port-forward) or IDE/file flows want SSH as a second
  channel. (Fletcher's M6 decision: keep both - a zero-config terminal and SSH.)

## Sources

Persistence and lifecycle:
- https://e2b.dev/docs/sandbox/persistence
- https://www.gitpod.io/docs/configure/workspaces/workspace-lifecycle
- https://fly.io/docs/reference/configuration/
- https://fly.io/docs/machines/machine-states/
- https://fly.io/docs/reference/suspend-resume/
- https://cloud.morph.so/docs/documentation/instances/basic-lifecycle
- https://modal.com/docs/guide/sandboxes
- https://modal.com/docs/guide/sandbox-snapshots
- https://fly.io/docs/blueprints/per-user-dev-environments/

Firecracker snapshots / fast-wake:
- https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md
- https://codesandbox.io/blog/how-we-scale-our-microvm-infrastructure-using-low-latency-memory-decompression
- https://dev.to/adwitiya/how-i-built-sandboxes-that-boot-in-28ms-using-firecracker-snapshots-i0k

Brokered access / SSH / IDE attach:
- https://fly.io/blog/ssh-and-user-mode-ip-wireguard/
- https://www.gitpod.io/docs/configure/user-settings/ssh
- https://wyh.life/article/2022/11/13/how-jetbrains-gateway-works
- https://stanislas.blog/2026/02/netclode-self-hosted-cloud-coding-agent/

Agent session persistence:
- https://code.claude.com/docs/en/agent-sdk/sessions
- https://github.com/timvw/tmux-assistant-resurrect
- https://www.implicator.ai/tmux-keeps-ai-coding-agents-running-for-days-after-you-disconnect/

Box-provider primary/overview:
- https://exe.dev/docs/boxes
- https://exe.dev/docs/all
- https://www.amplifypartners.com/blog-posts/exe-dev-and-the-perfect-little-computer
