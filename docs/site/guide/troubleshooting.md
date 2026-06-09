# Troubleshooting

## First step: `fletcher doctor`

```sh
fletcher doctor
```

This runs a battery of checks against the daemon, the host networking stack, and
the upstream providers, then prints a prioritised action plan for anything that
needs attention. Each check explains what failed and gives concrete commands to
fix it. Re-run after each change to confirm progress. JSON output is available
for scripting: `fletcher doctor -o json`.

Common things the doctor catches:

- Daemon not running or not reachable on the Unix socket
- Job runtime not ready: the mock runtime (no real isolation), or no base image
  imported yet (jobs and sessions can't boot until one is)
- `/dev/net/tun` missing (kernel TUN module not loaded)
- Multiple default routes on the same subnet (causes asymmetric paths)
- Public IP in the CGNAT range (need a VPN, see [Mode
  B](/advanced/networking#mode-b-bring-your-own-vpn))
- UPnP not responding (enable it on the router, or use the manual path)
- Upstream model providers unreachable (DNS / outbound firewall)

If the doctor's plan doesn't resolve it, the specific hints below may help.

## `fletcher` command not found after install

Make sure `/usr/local/bin` is on your `$PATH` (`echo $PATH`), or run with the
full path: `/usr/local/bin/fletcher version`.

## Daemon won't start: "operation not permitted" on TUN

The systemd unit grants `CAP_NET_ADMIN` for the tunnel automatically, so this
usually means the daemon was started outside systemd. Start it with `fletcher
daemon start` (or `enable`), which runs it under the unit.

## `upnp port-forward unavailable` in logs

Either UPnP isn't enabled on your router, or you're behind CGNAT. `fletcher
doctor` distinguishes the two cases and points at the right fix. See [Networking
deep dive](/advanced/networking).

## Peer's WireGuard app shows "no handshake"

First confirm the daemon knows the peer:

```sh
fletcher peer list
fletcher daemon logs | grep -i wireguard
```

(The daemon runs userspace WireGuard, so `wg show` prints nothing.) If the peer
is registered, the issue is network reachability. The port forward isn't actually
open, or the endpoint in the config is wrong. Test from outside your LAN: an
online UDP port checker against `<your-public-ip>:51820` should report "open".

## `login` worked but commands return `401`

A leftover `FLETCHER_REMOTE` or `FLETCHER_TOKEN` environment variable overrides
the stored login. See [Remote control](/guide/remote#log-in-once-then-just-use-fletcher).

## I want to start over

See [Managing the daemon -> Starting over](/guide/daemon#starting-over).
