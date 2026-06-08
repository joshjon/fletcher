# Pair a device

Pairing grants a device control over Fletcher: it hands out a WireGuard tunnel
config and a per-device API token. Once paired, that device can drive the daemon
over the tunnel from anywhere.

::: warning A paired device has full daemon access
Pairing is not "letting a device onto my LAN" - it is "granting that device
control over Fletcher": submitting jobs, managing secrets and settings, using
the model gateway. Pair only devices you intend to use Fletcher with. See
[Security](/guide/security) for the full picture.
:::

## Pair

With the daemon up (Mode A) or reachable over your VPN (Mode B), run:

```sh
fletcher peer pair phone
```

This prints, in the order you use them (each secret shown **exactly once**):

1. A summary line - `paired phone, address: 10.99.0.2/32, endpoint: ...`
2. **The WireGuard tunnel** - the full `wg-quick` config text and a QR code
   encoding it. Bring this up first.
3. **The API token** - a ready-to-paste `fletcher login <token>` line, plus the
   equivalent `--remote` / `--token` flags. Use it *after* the tunnel is up.

The daemon picks the next free `10.99.0.x/32` address for each peer
automatically (253 peers per default subnet - not a limit you'll hit).

## Connect a phone

1. Install the official **WireGuard** app.
2. Tap **Add tunnel -> Create from QR code** and scan the code.
3. Toggle the tunnel on. The phone is connected.

To confirm the tunnel, look at the WireGuard app itself: tap the tunnel and check
**Latest handshake** and the data counters.

::: tip `wg show` on the server prints nothing
Fletcher runs userspace WireGuard, so there's no kernel device for `wg show` to
read. Confirm the interface with `ip addr show fletcher0`, and watch peers being
added with `fletcher daemon logs | grep -i wireguard`.
:::

A phone can join the tunnel, but there's no client to drive the daemon from iOS
yet - that's the native app, still to come. Today the tunnel works from the
phone; control is laptop-only.

## Connect a laptop

Same command, different name:

```sh
fletcher peer pair laptop
```

Copy the `[Interface]` / `[Peer]` block into `/etc/wireguard/fletcher.conf` on
the laptop, then:

```sh
sudo wg-quick up fletcher
```

From there you can drive the daemon remotely - install the CLI, run
`fletcher login <token>`, and go. See [Remote control](/guide/remote).

## Revoke a device

If a device is lost or you're done with it:

```sh
fletcher peer list                 # find the id
fletcher peer delete peer_01h...   # revoke tunnel access and token
```

The running tunnel syncs immediately, so the device can't connect any more -
regardless of what's stored on it.

## Next

Your device is paired. Now [run your first agent](/guide/first-agent).
