# Networking deep dive

This page goes past the [happy path](/guide/networking) into manual port
forwarding, the bring-your-own-VPN setup, and the CGNAT situation.

## How Mode A works

In [Mode A](/guide/networking#mode-a-built-in-wireguard) the daemon embeds
WireGuard. On start it brings up its own `fletcher0` interface, derives its
public endpoint, and asks your router to forward the WireGuard UDP port via
UPnP. There's no `wg-quick` and no `/etc/wireguard/` config on the server -
userspace WireGuard runs inside the daemon process.

Because it's userspace, `wg show` prints nothing. Use these instead:

```sh
ip addr show fletcher0                       # interface is up
fletcher daemon logs | grep -i wireguard     # peers being added
```

## Manual port forwarding

If you'd rather not rely on UPnP - for security reasons, or because UPnP is
flaky on your router - set your public endpoint explicitly and turn off the
auto-attempt:

```sh
fletcher settings set public_endpoint your-host-or-ip:51820
fletcher settings set no_upnp true
fletcher daemon restart
```

Then forward the port manually in your router. Look for "Port Forwarding",
"Virtual Server", or "NAT/Gaming", and add:

- **Protocol:** UDP
- **External port:** 51820
- **Internal port:** 51820
- **Destination:** the LAN IP of this machine (`ip -4 addr` shows it)

## Mode B: bring your own VPN

If you already use a VPN to reach your home network, point Fletcher at that
network and skip the built-in WireGuard. Fletcher's services bind to `127.0.0.1`
by default; bind them to all interfaces (or your VPN interface) and disable the
built-in tunnel:

```sh
fletcher settings set gateway_listen 0.0.0.0:11500
fletcher settings set mcp_listen 0.0.0.0:11600
fletcher settings set no_upnp true
fletcher daemon enable
```

### Worked example: Tailscale

On the server **and** on the device you want to connect from:

1. Install Tailscale on the server:
   `curl -fsSL https://tailscale.com/install.sh | sh`, then `sudo tailscale up`.
2. Sign in. Note the server's Tailscale IP (e.g. `100.x.y.z`).
3. On your phone/laptop: install Tailscale, sign in to the same tailnet.
4. The gateway and MCP servers are now reachable at the server's Tailscale IP on
   ports 11500 and 11600. Point any tool at those.

(`fletcher health --socket <unix socket>` won't work off-machine - Unix sockets
are local - but the network services are reachable over the tailnet.)

### Trade-off

A coordination-based VPN like Tailscale sees metadata about your network (which
devices, when they connect). For most homelab users that's an acceptable trade.
If "no third-party service in the loop" is non-negotiable, stick with
[Mode A](/guide/networking#mode-a-built-in-wireguard) and accept that CGNAT means
LAN-only access.

## CGNAT

CGNAT (carrier-grade NAT) means your ISP shares one public IP across many
customers, so your router has no public IP to forward - Mode A can't open a port
to you. `fletcher doctor` detects when your public IP is in the CGNAT range. Two
ways forward:

- **Ask your ISP to take you off CGNAT.** Many will, often for free - some let
  you toggle it in their account portal; otherwise a support ticket requesting "a
  public IP" or "to be removed from CGNAT" usually does it. Then `fletcher daemon
  restart`.
- **Use [Mode B](#mode-b-bring-your-own-vpn).** A VPN you already run reaches the
  daemon without any public IP.
