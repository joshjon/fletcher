# Networking & first run

Fletcher has two networking modes. **You pick one.** This is also where you
start the daemon for the first time. It sets up networking on start, so choose
your mode first, then start it.

| | [Mode A: built-in WireGuard](#mode-a-built-in-wireguard) | [Mode B: bring your own VPN](#mode-b-bring-your-own-vpn) |
|---|---|---|
| **Best for** | Most homelabs | CGNAT, or you already run a VPN |
| **Public IP needed** | Yes (a real one, not CGNAT) | No |
| **Setup** | Zero, the daemon does it | Point Fletcher at your existing VPN |
| **Third party in the loop** | None | Your VPN's coordination, if any |

If you have a normal home connection with a real public IP, use **Mode A**. It is
the whole setup on most routers. If you're behind CGNAT or already run something
like Tailscale, use **Mode B**.

## Mode A: built-in WireGuard

The daemon embeds WireGuard directly. When it starts, it brings up its own WG
interface, asks your router to forward the WireGuard port via UPnP, and makes
itself reachable from your phone or laptop anywhere on the internet. There is no
`wg-quick` and no `/etc/wireguard/` config files.

**This works for you if:**

- your home connection has a real public IP (not CGNAT), and
- your router supports UPnP (most consumer routers do).

Start the daemon and watch it come up:

```sh
fletcher daemon enable      # start now and on every boot (prompts for sudo)
fletcher daemon logs -f     # follow the startup logs
```

In the logs you're looking for three lines:

```
INFO  upnp port-forward installed  external_ip=...  external_port=51820
INFO  public endpoint derived from upnp  endpoint=<ip>:51820
INFO  wireguard tunnel up  interface=fletcher0  address=10.99.0.1/24
```

If you see all three, setup is done. Skip to [Pair a device](/guide/pairing).

### If UPnP fails

The log tells you. There are two common causes.

1. **UPnP is disabled on your router.** Look for "UPnP" or "IGD" in the router
   admin UI, enable it, then `fletcher daemon restart`.
2. **You're behind CGNAT.** Your ISP shares one public IP across many customers,
   so your router has no public IP to forward. Either ask your ISP to take you
   off CGNAT (many will, often for free), or switch to
   [Mode B](#mode-b-bring-your-own-vpn).

Prefer to forward the port yourself instead of relying on UPnP? See
[Networking deep dive](/advanced/networking#manual-port-forwarding).

## Mode B: bring your own VPN

If you already use a VPN (Tailscale, Headscale, ZeroTier, plain WireGuard, or
anything else) to reach your home network, point Fletcher at that network and
skip the built-in WireGuard entirely. This is the right choice if:

- you're behind CGNAT or a restrictive ISP firewall,
- you already run a VPN and don't want a second tunnel, or
- you'd rather not expose any port on the public internet at all.

Fletcher's services bind to `127.0.0.1` by default. To reach them over your
existing VPN, bind them to all interfaces and turn off the built-in tunnel:

```sh
fletcher settings set gateway_listen 0.0.0.0:11500
fletcher settings set mcp_listen 0.0.0.0:11600
fletcher settings set no_upnp true
fletcher daemon enable
```

To also drive the daemon itself (and the iOS app) over your VPN, expose the
control API on your VPN address - bound there only, not the whole LAN:

```sh
fletcher settings set remote_api_listen 100.x.y.z:11700   # your box's Tailscale IP
fletcher daemon restart
```

Then pair a client with `fletcher peer pair <name> --byo-vpn` (see
[Pair a device](/guide/pairing#or-pair-the-app-over-your-own-vpn-mode-b)).

For a worked Tailscale example and the trade-offs, see
[Networking deep dive](/advanced/networking#mode-b-bring-your-own-vpn).

## Next

With the daemon up and reachable, [pair your first device](/guide/pairing).
