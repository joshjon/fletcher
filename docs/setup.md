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

> **Pre-v0.1.0 note:** until the first release is tagged, the installer
> errors with "could not resolve latest release from GitHub API." If
> you're an early tester who wants to try Fletcher today, see the
> "Building from source" section in the [README](../README.md) - the
> from-source path is documented there for developers.

## First run (smoke test)

Before configuring networking, run the daemon in the foreground to confirm
the binary is healthy on this machine:

```sh
mkdir -p /tmp/fletcher-data
fletcher serve \
  --socket   /tmp/fletcher-data/fletcher.sock \
  --database /tmp/fletcher-data/fletcher.db \
  --age-key  /tmp/fletcher-data/age.key
```

You should see startup logs ending in `model gateway ready` and
`mcp server ready`. From another shell on the same machine:

```sh
fletcher health --socket /tmp/fletcher-data/fletcher.sock
```

`status: ok` confirms the daemon is reachable. `Ctrl-C` the daemon, then
clean up: `rm -rf /tmp/fletcher-data`.

## Networking

Fletcher has two networking modes. **You only pick one.**

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
sudo systemctl enable --now fletcher
sudo journalctl -u fletcher -f
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
   admin UI and enable it, then `sudo systemctl restart fletcher`.
2. You're behind CGNAT (your ISP shares a public IP across customers).
   This isn't fixable from your end — your router doesn't have a public IP
   to forward. See [Mode B](#mode-b-bring-your-own-vpn) below; that's
   what to do.

**If you'd rather skip UPnP** and forward the port manually (e.g. for
security reasons or because UPnP is unreliable on your router): set your
public endpoint explicitly and disable the auto-attempt.

```sh
sudo systemctl edit fletcher
```

Paste:

```
[Service]
Environment=FLETCHER_PUBLIC_ENDPOINT=your-host-or-ip:51820
Environment=FLETCHER_NO_UPNP=true
```

Then forward UDP 51820 manually in your router (look for "Port
Forwarding", "Virtual Server", or "NAT/Gaming"). Protocol: UDP, External
port: 51820, Internal port: 51820, Destination: the LAN IP of this
machine (`ip -4 addr` shows it).

### Mode B: bring your own VPN

If you already use a VPN (Tailscale, Headscale, ZeroTier, plain WireGuard,
or anything else) to reach your home network, **point Fletcher at that
network instead** and skip the built-in WireGuard entirely. This is the
right choice if:

- You're behind CGNAT or a restrictive ISP firewall
- You already run a VPN on your devices and don't want a second tunnel
- You'd rather not expose any port on the public internet at all

Fletcher's services (the Connect-RPC API, the model gateway, the MCP
server) bind to `127.0.0.1` by default. To make them reachable over your
existing VPN, bind them to the VPN interface or to all interfaces:

```sh
sudo systemctl edit fletcher
```

Paste:

```
[Service]
Environment=FLETCHER_GATEWAY_LISTEN=0.0.0.0:11500
Environment=FLETCHER_MCP_LISTEN=0.0.0.0:11600
Environment=FLETCHER_NO_UPNP=true
```

The `FLETCHER_NO_UPNP=true` is important — there's nothing for the
built-in tunnel to do here.

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

This outputs:
- A summary line (`paired phone, address: 10.99.0.2/32, endpoint: ...`)
- The full `wg-quick` configuration text
- A QR code (in the terminal) encoding the same config

On your phone, install the official **WireGuard** app, tap "Add tunnel",
choose "Create from QR code", scan the code. Toggle the tunnel on; the
phone is connected. On the server, `sudo wg show` (Mode A) should list
the peer with a recent handshake within a few seconds.

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
random bots) the port looks identical to a closed port — no version
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

**Anyone with a valid peer key has full daemon access.** They can
submit jobs, read your secrets store via the API, and use the model
gateway. Pairing a peer is not "letting a device on my LAN" — it's
"granting that device control over Fletcher." Pair only devices you
intend to use Fletcher with.

## Troubleshooting

**`fletcher` command not found after install.** Make sure `/usr/local/bin`
is on your `$PATH` (`echo $PATH`). Run with the full path:
`/usr/local/bin/fletcher version`.

**Daemon won't start: "operation not permitted" on TUN.** The systemd
unit grants `CAP_NET_ADMIN` automatically. If you're running the daemon
manually (not via systemd), you'll need to run as root or grant the
capability: `sudo setcap cap_net_admin+ep /usr/local/bin/fletcher`.

**`upnp port-forward unavailable` in logs.** Either UPnP isn't enabled
on your router, or you're behind CGNAT. See the troubleshooting steps in
[Mode A](#mode-a-built-in-wireguard-recommended-for-most-homelabs).

**Peer's WireGuard app shows "no handshake".** First check the server:
`sudo wg show` should list the peer's public key. If yes, the issue is
network reachability (port forward not actually open, or wrong endpoint
in the config). Test from outside your LAN: an online UDP port checker
against `<your-public-ip>:51820` should report "open".

**I want to start over.** Stop the daemon, wipe the state directories:

```sh
sudo systemctl stop fletcher
sudo rm -rf /var/lib/fletcher
sudo systemctl start fletcher
```

This regenerates the age identity, the server WireGuard key, and an
empty peer registry. All previously-paired devices will need to be
re-paired.

## What's next

- Set the daemon's Anthropic API key: `fletcher secret set anthropic_api_key sk-ant-...` (only required if you want the daemon to translate OpenAI-shape calls to Anthropic; for Anthropic-native agents like Claude Code with their own auth, no secret is needed in the daemon)
- Browse the model catalog the daemon exposes: `fletcher model list`
- Submit your first job: `fletcher job create --name hello --image fletcher-base:dev --command "echo hello world"`

Each of these has its own `--help` flag with details. The `--help` output
for `fletcher model list` in particular explains what the endpoints mean
and how SDK-based agents discover them.
