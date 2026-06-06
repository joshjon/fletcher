# Setting up Fletcher

This guide walks you through installing Fletcher on a Linux server, getting
the daemon running, choosing a networking setup, and pairing your first
device. It's written for someone who manages their own Linux box (a homelab
server, a small VPS, a Raspberry Pi) but doesn't necessarily live in
networking deep-dive territory.

If you're trying to develop Fletcher itself or run a smoke test on macOS,
see `docs/TESTING.md` instead.

## What you'll need

- A Linux machine you control (Debian/Ubuntu, Fedora, Arch, or similar)
- Root access (Fletcher runs as an unprivileged user but the install
  steps need root)
- One outgoing network choice: see [Networking](#networking) below

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh
```

The installer downloads the latest release tarball, verifies its SHA256,
drops the binary at `/usr/local/bin/fletcher`, installs the systemd unit,
creates the unprivileged `fletcher` system user, and pre-creates the
state directories. The same command works for first install and upgrade
- the script detects whether the service is already running and
restarts it on upgrade automatically.

You should now have `fletcher version` working.

> **Pre-v0.1.0 note:** until the first release is tagged, the installer's
> default download path errors with "could not resolve latest release from
> GitHub API." To try Fletcher today, either build from source (see the
> [README](../README.md)), or install a locally-built release with the
> installer's `FLETCHER_LOCAL_TARBALL` mode - both paths are written up in
> [`TESTING.md`](TESTING.md).

## Networking

Fletcher has two networking modes. **You only pick one.** This is also where you
start the daemon for the first time - it sets up networking when it starts, so
pick your mode first, then start it (each mode below ends with the start
command).

### Mode A: built-in WireGuard (recommended for most homelabs)

The daemon embeds WireGuard directly: when it starts, it brings up its own
WG interface, asks your router to forward the WireGuard port via UPnP, and
makes itself reachable from your phone or laptop anywhere on the internet.
No `wg-quick`, no `/etc/wireguard/` config files.

**This works for you if:**
- Your home internet connection has a real public IP (not CGNAT)
- Your router supports UPnP (most consumer routers do)

**Try it:**

```sh
fletcher daemon enable      # start now and on every boot (prompts for sudo)
fletcher daemon logs -f     # follow the startup logs
```

In the logs you're looking for:

```
INFO  upnp port-forward installed  external_ip=...  external_port=51820
INFO  public endpoint derived from upnp  endpoint=<ip>:51820
INFO  wireguard tunnel up  interface=fletcher0  address=10.99.0.1/24
```

If you see all three, you're done with setup. Skip to
[Pair your first device](#pair-your-first-device).

**If UPnP fails**, the log says so. Two reasons it commonly fails:

1. Your router has UPnP disabled. Look for "UPnP" or "IGD" in the router
   admin UI and enable it, then `fletcher daemon restart`.
2. You're behind CGNAT (your ISP shares one public IP across many customers,
   so your router has no public IP to forward). Two ways forward:
   - **Ask your ISP to take you off CGNAT.** Many will, often for free - some
     let you toggle it yourself in their account/admin portal, otherwise a
     quick call or support ticket requesting "a public IP" or "to be removed
     from CGNAT" usually does it. Once they switch it, `fletcher daemon
     restart` and Mode A should work.
   - **Or use [Mode B](#mode-b-bring-your-own-vpn)** - reach the daemon over a
     VPN you already run (Tailscale, etc.), no public IP needed.

**If you'd rather skip UPnP** and forward the port manually (e.g. for
security reasons or because UPnP is unreliable on your router): set your
public endpoint explicitly and turn off the auto-attempt.

```sh
fletcher settings set public_endpoint your-host-or-ip:51820
fletcher settings set no_upnp true
```

Then forward UDP 51820 manually in your router (look for "Port Forwarding",
"Virtual Server", or "NAT/Gaming"). Protocol: UDP, External port: 51820,
Internal port: 51820, Destination: the LAN IP of this machine (`ip -4 addr`
shows it). Then start the daemon: `fletcher daemon enable`.

### Mode B: bring your own VPN

If you already use a VPN (Tailscale, Headscale, ZeroTier, plain WireGuard,
or anything else) to reach your home network, **point Fletcher at that
network instead** and skip the built-in WireGuard entirely. This is the
right choice if:

- You're behind CGNAT or a restrictive ISP firewall
- You already run a VPN on your devices and don't want a second tunnel
- You'd rather not expose any port on the public internet at all

Fletcher's services (the model gateway and the MCP server) bind to
`127.0.0.1` by default. To make them reachable over your existing VPN, bind
them to the VPN interface or to all interfaces, and turn off the built-in
tunnel (there's nothing for it to do here):

```sh
fletcher settings set gateway_listen 0.0.0.0:11500
fletcher settings set mcp_listen 0.0.0.0:11600
fletcher settings set no_upnp true
```

Then start the daemon: `fletcher daemon enable`.

For example, **with Tailscale** on the server and on the device you want
to connect from:

1. Install Tailscale on the server: `curl -fsSL https://tailscale.com/install.sh | sh`, then `sudo tailscale up`
2. Sign in. Note the server's Tailscale IP (e.g. `100.x.y.z`)
3. On your phone/laptop: install Tailscale, sign in to the same tailnet
4. From your device: `fletcher health --socket <unix socket>` won't work
   off-machine (Unix sockets are local), but the gateway and MCP servers
   are now reachable at the server's Tailscale IP on ports 11500 and
   11600. Point any tool at those.

**Trade-off to be aware of:** Tailscale's coordination server sees
metadata about your tailnet (which devices, when they connect). For most
homelab users that's an acceptable trade. If "no third-party service in
the loop" is non-negotiable for you, stick with Mode A and accept that
CGNAT means LAN-only access.

## Pair your first device

Once Mode A is up (or Mode B's VPN can reach the server), pair a device:

```sh
fletcher peer pair phone
```

This outputs (each secret shown exactly once), in the order you use them:
- A summary line (`paired phone, address: 10.99.0.2/32, endpoint: ...`)
- **Step 1 - the WireGuard tunnel:** the full `wg-quick` configuration text and
  a QR code encoding it. Bring this up first.
- **Step 2 - the API token:** a ready-to-paste `fletcher login <token>` line (the
  easy path), and the equivalent `fletcher --remote <host:port> --token <token>`
  flags. Use it *after* the tunnel is up - `fletcher login` reaches the daemon to
  verify the token, so it needs the tunnel connected first (see "Driving the
  daemon from another device" below).

On your phone, install the official **WireGuard** app, tap "Add tunnel",
choose "Create from QR code", scan the code. Toggle the tunnel on; the
phone is connected.

To confirm the tunnel, look at the **WireGuard app** itself: tap the tunnel
and check "Latest handshake" and the data counters. (`wg show` on the server
prints nothing - Fletcher runs userspace WireGuard with no kernel device for
that tool to read; `ip addr show fletcher0` confirms the interface is up, and
`fletcher daemon logs | grep -i wireguard` shows the peer being added.)

For a laptop, the same `fletcher peer pair laptop` command outputs the
config. Copy the `[Interface]` / `[Peer]` block into
`/etc/wireguard/fletcher.conf` on the laptop, then `sudo wg-quick up
fletcher`.

The daemon picks the next free `10.99.0.x/32` address for each peer
automatically. There's no limit you'll hit (253 peers per default subnet).

## Security: what you're exposing

When Mode A is active, UDP 51820 is open to the public internet on your
home connection. Some honest framing:

**WireGuard is unusually safe to expose.** The protocol refuses to
acknowledge that it's running unless a packet completes a cryptographic
handshake with a key it already knows. To port scanners (nmap, Shodan,
random bots) the port looks identical to a closed port - no version
string, no fingerprint, no reply. That's the opposite of SSH or any HTTP
service, which respond to every probe.

**The real security boundary is the peer-pair step.** When you pair a
device, the QR code / wg-quick config contains a private key that
authorises the device to reach Fletcher. That key is **shown exactly
once** and is never stored on the server again. Treat it the same way
you'd treat a password: don't paste it into chat, don't log it.

**If a device is lost or compromised:** revoke the peer immediately.

```sh
fletcher peer list                       # find the id
fletcher peer delete peer_01h...         # revoke
```

The next time the running tunnel syncs (which happens automatically on
this command), the lost device can't connect any more, regardless of
what's on it.

**A paired device has full daemon access.** Driving the daemon over the
tunnel takes two things, both handed out at pair time: tunnel reachability
(the WireGuard key) *and* a per-peer API token (sent as a bearer token; the
daemon stores only its hash). That is defense in depth - a leaked WireGuard
key alone reaches the API port but gets `401` without the token - but a fully
paired device gets both, and with them can submit jobs, manage secrets and
settings, and use the model gateway. Pairing a peer is not "letting a device
on my LAN"; it is "granting that device control over Fletcher." Pair only
devices you intend to use Fletcher with. Deleting the peer
(`fletcher peer delete <id>`) revokes both the network access and the token.

## Troubleshooting

**First step: run `fletcher doctor`.** This runs a battery of checks
against the daemon, the host networking stack, and the upstream
providers, then prints a prioritised action plan for anything that
needs attention. Each check explains what failed and gives concrete
commands to fix it. Re-run after each change to confirm progress.

```sh
fletcher doctor
```

The doctor output is also available as JSON for scripting:
`fletcher doctor -o json`.

Common things the doctor catches:

- Daemon not running or not reachable on the Unix socket
- Job runtime not ready: the mock runtime (no real isolation), or no base
  image imported yet (jobs and sessions can't boot until one is)
- `/dev/net/tun` missing (kernel TUN module not loaded)
- Multiple default routes on the same subnet (causes asymmetric paths)
- Public IP in the CGNAT range (need a VPN like Tailscale; see Mode B)
- UPnP not responding (enable it on the router, or use the manual path)
- Upstream model providers unreachable (DNS / outbound firewall)

If the doctor's plan doesn't resolve your issue, the more specific
hints below may help.

**`fletcher` command not found after install.** Make sure `/usr/local/bin`
is on your `$PATH` (`echo $PATH`). Run with the full path:
`/usr/local/bin/fletcher version`.

**Daemon won't start: "operation not permitted" on TUN.** The systemd unit
grants `CAP_NET_ADMIN` for the tunnel automatically, so this usually means the
daemon was started outside systemd. Start it with `fletcher daemon start` (or
`enable`), which run it under the unit.

**`upnp port-forward unavailable` in logs.** Either UPnP isn't enabled
on your router, or you're behind CGNAT. `fletcher doctor` distinguishes
the two cases and points at the right fix.

**Peer's WireGuard app shows "no handshake".** First confirm the daemon
knows the peer: `fletcher peer list` and `fletcher daemon logs | grep -i
wireguard` (the daemon runs userspace WireGuard, so `wg show` prints
nothing). If the peer is registered, the issue is network reachability
(port forward not actually open, or wrong endpoint in the config). Test
from outside your LAN: an online UDP port checker against
`<your-public-ip>:51820` should report "open".

**I want to start over.** Stop the daemon, wipe the state directories:

```sh
fletcher daemon stop
sudo rm -rf /var/lib/fletcher
fletcher daemon start
```

This regenerates the age identity, the server WireGuard key, and an
empty peer registry. All previously-paired devices will need to be
re-paired.

## Configuring Fletcher

Operational settings live in the daemon's database and are edited with
`fletcher settings` - no editing systemd unit files. List every setting,
its current value (or `(default)`), and help with:

```sh
fletcher settings list
```

Set one and apply it with a restart (settings take effect on the next start):

```sh
fletcher settings set log_level debug
fletcher daemon restart
fletcher settings unset log_level        # revert to the flag/env default
```

Settable keys include `runtime`, `snapshot`, `btrfs_root`, `public_endpoint`,
`wireguard_port`, `no_upnp`, `gateway_listen`, `mcp_listen`, `log_level`,
`credentials_dir`, `default_image` (the base image `job`/`session create` use
when `--image` is omitted; `fletcher-base` out of the box, set empty to make
`--image` required), and the session limits `session_idle_timeout`,
`session_max_count`, and `session_max_disk_gb` (see [Durable
sessions](#durable-sessions-persistent-workspaces-you-can-ssh-into)). Only
bootstrap config - where the database, socket, and age key live - stays in the
flag/env layer, because it's needed to open the database these settings live in.

## Managing the daemon

You don't need `systemctl`. `fletcher daemon` is a thin wrapper over the
service:

```sh
fletcher daemon status
fletcher daemon restart
fletcher daemon logs            # recent logs; -f to follow
fletcher daemon enable          # start now and on every boot
fletcher daemon stop
```

systemd is still the supervisor underneath (boot persistence, crash-restart,
the unit sandbox); these are just friendlier verbs.

## Driving the daemon from another device

Once a device is paired, you can drive the daemon over the WireGuard tunnel -
submit jobs, run agents, manage settings - as if you were on the server. The
work still happens on the server (a job spins up its microVM there); your laptop
is just a thin client talking to the daemon over the tunnel.

You need three things on the device:

1. **It's paired.** Run `fletcher peer pair laptop` on the server (see
   [Pair your first device](#pair-your-first-device)); the output includes the
   WireGuard config and a ready-to-paste `fletcher login <token>` line.
2. **The tunnel is up.** Import that WireGuard config on the device and connect,
   so it can reach the daemon's tunnel address `10.99.0.1`.
3. **The `fletcher` CLI.** It's the same binary; the client half runs anywhere.
   On macOS install the client with the install script (`curl -fsSL
   https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh |
   sudo sh` - it detects macOS and installs the client only, no daemon), or build
   from source (`go install ./cmd/fletcher`, or `make install`). A Mac build is a
   pure client: the daemon is Linux-only, and `fletcher --help` groups commands
   into "Client" and "Daemon (Linux host)" so it's clear which apply.

**Log in once, then just use `fletcher`.** Paste the `fletcher login <token>`
line from the pair output on the device:

```sh
fletcher login <token>          # verifies, then saves to ~/.config/fletcher/config.toml (0600)
fletcher health                 # -> status: ok  (now targets the remote daemon)
fletcher session list
fletcher logout                 # forget the remote; revert to the local socket
```

`login` makes the remote the default target for every command, so you don't
repeat the address and token. For one-off commands or CI you can still pass them
explicitly (or via the `FLETCHER_REMOTE` / `FLETCHER_TOKEN` env vars), which
override the stored login:

```sh
fletcher --remote 10.99.0.1:11700 --token <token> health     # -> status: ok
fletcher --remote 10.99.0.1:11700 --token <token> job list
```

> **`login` worked but commands return `401 Unauthorized`?** A leftover
> `FLETCHER_REMOTE` or `FLETCHER_TOKEN` in your shell overrides the stored login
> (`login` ignores them, so it still succeeds). Check `echo $FLETCHER_REMOTE
> $FLETCHER_TOKEN` and `unset` whichever is set (and remove it from your shell
> profile). The CLI also prints a warning when it targets a remote with no token.

**Spin up a microVM remotely.** Once you've run `fletcher login`, commands need
no flags. A `job create` runs its command inside a Firecracker microVM on the
server (assuming the Firecracker runtime and an imported base image - see
[Running real agents](#running-real-agents-in-a-microvm)):

```sh
fletcher job create --name remote-vm --image fletcher-base \
  --command "echo KERNEL=\$(uname -r); cat /proc/1/comm; exit 3"
fletcher job get <job-id>
```

The `exit 3` makes the job fail so its captured output lands in the job's error
field, which `job get` shows remotely - you'll see the guest kernel version and
`fletcher-init` as PID 1, confirming it ran inside a microVM on the server.

> **Tip:** before troubleshooting WireGuard on the device, sanity-check the
> remote API from the server itself - `10.99.0.1` is a local interface there, so
> `fletcher --remote 10.99.0.1:11700 --token <token> health` works locally. If it
> does, any failure from the device is tunnel reachability, not the API.

The network API binds to the tunnel interface only and requires the token; the
local Unix socket needs neither (it's gated by file permissions). Revoke a
device any time with `fletcher peer delete <id>` - that drops both its tunnel
access and its token.

**From a phone:** the phone can join the tunnel (it's a WireGuard peer), but
there's no client to drive the daemon from iOS yet - that's the native app,
still to come. So today the tunnel works from the phone, but control is
laptop-only.

## Running real agents in a microVM

On a host with KVM, the daemon defaults to the **Firecracker** runtime: each job
boots a hardware-isolated microVM from its own kernel and a copy-on-write ext4
rootfs, reaching models only through the daemon's gateway and with no network
egress at all (the VM has no NIC - just a vsock channel to the daemon). On a host
without `/dev/kvm` the daemon falls back to the mock runtime; **runc** is
available as an explicit, shared-kernel fallback (`fletcher settings set runtime
runc`). Confirm what you have with `fletcher doctor` (it checks `/dev/kvm` and the
bundled VMM).

Running an agent needs a base-image rootfs. For the Firecracker default:

1. Import a base image as an ext4 rootfs. Pull the published image (no local
   build needed):
   `sudo fletcher image import ghcr.io/joshjon/fletcher-base:debian-13 --format
   ext4 --btrfs-root /var/lib/fletcher/snapshots --name fletcher-base`. (Or build
   it yourself with `make image` and import `fletcher-base:dev`.) The agent rootfs
   is roughly 3 GB, so make sure the snapshot root has a few GB free; on btrfs the
   per-job clones are reflinks. The importer injects the microVM init for you.
2. Give the daemon an Anthropic key so the gateway can reach models:
   `fletcher secret set anthropic_api_key sk-ant-...`.
3. Run an agent as a job: `fletcher job create --command "claude -p 'say hi'"`.
   `--image` defaults to the `default_image` setting (`fletcher-base` out of the
   box) and `--name` defaults to the command's program name (here, `claude`), so
   only `--command` is required; pass `--image` / `--name` to override.

The agent runs inside the microVM, reaches Anthropic only through the daemon
gateway (the key never enters the VM), and has no other network egress. Browse
what the gateway can route to with `fletcher model list`. Every command has
`--help` with more detail.

The imported template is pinned: it does not change underneath you. When the
registry has a newer build of the default image (e.g. a rebuilt rootfs with
package updates), the daemon notices on its next start and `fletcher doctor`
shows a "newer version is available" note. Re-pull and re-import with
`sudo fletcher image update` when you want it; existing jobs and sessions keep
their already-cloned forks, and new ones pick up the updated template. (`image
update` re-imports the `default_image`; pass a template name to update a
different one.)

> For the runc fallback instead, set `runtime runc` / `snapshot btrfs`, provision
> a btrfs snapshot root, and import with `--format subvolume` (the default). The
> trust properties are the same; the isolation is a shared-kernel container
> rather than a VM.

### Recurring jobs (cron)

A job can run on a schedule instead of once. Give it `--trigger cron` and a cron
expression:

```sh
fletcher job create --trigger cron --schedule "*/30 * * * *" \
  --name hourly-scrape --image fletcher-base --command "scrape.sh"
```

The cron job is a *definition*: it shows up with status `scheduled` and a
`next_run_at`. Each time the schedule fires, Fletcher creates a normal run of it
(a child job you'll see in `job list`, linked back by its parent), so every run
has its own status, output, and exit code. The schedule is a standard 5-field
expression (`min hour day-of-month month day-of-week`) or a macro (`@hourly`,
`@daily`, `@weekly`, ...). A run that is still going when the next window arrives
is not double-started, and a window missed while the daemon was down fires once
on the next start (no backfill). Stop a cron job with `fletcher job cancel <id>`.

This is the agent-authored-then-automated pattern: have an agent write the
scraper once (an interactive session or a one-off job), then schedule the plain
program to run it on a cron.

## Durable sessions: persistent workspaces you can SSH into

A *job* (above) runs one command in a fresh microVM and tears it down. A
**session** is the other half: a persistent microVM you create once and keep.
Its disk - the fork - survives across stops and restarts, so a `git clone`, your
edits, and an agent's on-disk history are all still there when you come back.
Sessions are how you use Fletcher interactively: open a shell, attach an IDE over
SSH, or leave an agent running.

Sessions need the **Firecracker** runtime (they don't run on the runc fallback)
and an imported base image - the same `fletcher-base` you imported in [Running
real agents](#running-real-agents-in-a-microvm).

> **Brokered SSH needs an image with an SSH server.** `fletcher session ssh`
> runs `sshd` inside the VM. The image built by `make image` includes it; the
> currently-published `ghcr.io/joshjon/fletcher-base:debian-13` does **not** yet,
> so for the SSH step build the image locally (`make image`, then import
> `fletcher-base:dev`). `create`/`exec`/`shell` work with either image.

### Create and use a session

```sh
fletcher session create --name dev --image fletcher-base   # boots the VM
fletcher session list                                      # state, disk, last-used
fletcher session exec dev 'echo hello > /workspace/notes; cat /workspace/notes'
```

`exec` runs a one-off command and returns its output (and exit code). For an
interactive terminal straight into the VM:

```sh
fletcher session shell dev      # a real PTY inside the VM; type `exit` to leave
```

### SSH and IDE attach

`fletcher session ssh` sets up everything for a normal `ssh` - and for an IDE's
Remote-SSH - in one step:

```sh
fletcher session ssh dev        # mints a key, installs it, writes your SSH config
ssh fletcher-dev                # just works
```

It generates a managed keypair, installs the public key in the VM, and adds a
`Host fletcher-dev` block (Included from your `~/.ssh/config`) whose
`ProxyCommand` tunnels through the daemon. The VM has no network route of its
own; the daemon brokers the SSH connection over vsock - the same trust boundary
as everything else, generalised from HTTP to SSH. Point VS Code or JetBrains
Remote-SSH at the host `fletcher-dev` and it connects; `scp`/`sftp` and
port-forwarding work too. Connecting to a *stopped* session wakes it first.

From a logged-in laptop, `fletcher session ssh dev` works over the tunnel just
the same - the generated `ProxyCommand` uses your stored login, so `ssh
fletcher-dev` keeps working. (If you drive the daemon with explicit
`--remote/--token` instead of `fletcher login`, the `ProxyCommand` carries those
through.)

### Stop, start, and disk persistence

```sh
fletcher session stop dev       # frees the VM's RAM; keeps its disk
fletcher session start dev      # back exactly where you left off
```

Stop **hibernates** the VM: it snapshots memory to disk and exits the VM process,
so a stopped session costs only disk, not RAM. Start restores it in well under a
second with its processes still running - the same boot, the same shell history,
an agent still mid-run. If a snapshot can't be restored (e.g. after a Fletcher
upgrade changes the VMM), Fletcher cold-boots from the disk instead: you never
lose the workspace, only the in-memory state.

### Automatic cleanup: free RAM, never free disk

Fletcher reclaims RAM on its own but never deletes your disk:

- **Idle sessions auto-stop.** A running session with no work in flight is
  hibernated after `session_idle_timeout` (default 30m; set to `0` to disable).
  "Work in flight" means an active exec/shell/SSH *or* a busy VM - a running
  agent or build with nobody attached is **not** stopped mid-task.
- **Storage is capped, not pruned.** `session_max_count` (default 10) and
  `session_max_disk_gb` (default 50) bound how many sessions exist and how much
  disk they use. Hitting a cap **refuses a new session** with a list of what's
  using the space - it never deletes anything.

```sh
fletcher settings set session_idle_timeout 1h
fletcher settings set session_max_count 20
fletcher daemon restart                         # session settings apply on restart
```

### Delete

```sh
fletcher session delete dev     # stops the VM and destroys its fork (disk)
```

Deleting is the only thing that frees a session's disk, and it's irreversible -
prune intentionally.
