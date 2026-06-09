# Pair a device

Pairing grants a device control over Fletcher. It hands out a WireGuard tunnel
config and a per-device API token. Once paired, that device can drive the daemon
over the tunnel from anywhere.

::: warning A paired device has full daemon access
Pairing is not "letting a device onto my LAN". It is "granting that device
control over Fletcher", which means submitting jobs, managing secrets and
settings, and using the model gateway. Pair only devices you intend to use
Fletcher with. See [Security](/guide/security) for the full picture.
:::

## Pair

With the daemon up (Mode A) or reachable over your VPN (Mode B), run:

```sh
fletcher peer pair phone
```

This prints three things, in the order you use them. Each secret is shown
**exactly once**.

1. A summary line, e.g. `paired phone, address: 10.99.0.2/32, endpoint: ...`.
2. **The WireGuard tunnel.** The full `wg-quick` config text and a QR code
   encoding it. Bring this up first.
3. **The API token.** A ready-to-paste `fletcher login <token>` line, plus the
   equivalent `--remote` / `--token` flags. Use it after the tunnel is up.

The daemon picks the next free `10.99.0.x/32` address for each peer
automatically. That is 253 peers per default subnet, not a limit you'll hit.

## Connect a phone

1. Install the official **WireGuard** app.
2. Tap **Add tunnel -> Create from QR code** and scan the code.
3. Toggle the tunnel on. The phone is connected.

To confirm the tunnel, look at the WireGuard app itself. Tap the tunnel and check
**Latest handshake** and the data counters.

::: tip `wg show` on the server prints nothing
Fletcher runs userspace WireGuard, so there's no kernel device for `wg show` to
read. Confirm the interface with `ip addr show fletcher0`, and watch peers being
added with `fletcher daemon logs | grep -i wireguard`.
:::

The steps above use the generic WireGuard app, which gives the phone a tunnel
but no way to drive the daemon. The native Fletcher iOS app pairs differently -
see below.

## Pair the iOS app (native client)

The native app generates its own WireGuard keypair on the device (the private
half never leaves the secure enclave), so it uses a different command:

```sh
fletcher peer pair --mobile phone
```

This prints (and renders as a QR) a single **pair blob** carrying everything the
app needs: the reserved tunnel address, the server's WireGuard key, the public
endpoint, a one-time pairing code, and the **pairing endpoint** plus its **TLS
fingerprint** (explained below). Scan it in the app.

Pairing is a bootstrap, and the order matters. The app must register its public
key with the daemon *before* it can bring up the tunnel - WireGuard will not
complete a handshake from a key the daemon has never heard of. So the app:

1. Calls `CompletePair` over the **pairing endpoint**: a public TCP port
   (default 51821), separate from the WireGuard port, served over TLS. The
   daemon registers the device's public key and returns the API token.
2. Brings up the WireGuard tunnel. The daemon now knows the key, so the
   handshake succeeds.
3. Drives the daemon over the tunnel at the tunnel-side API endpoint
   (`10.99.0.1:11700`) using the token, exactly like the laptop flow.

The pairing endpoint uses a **self-signed certificate**, because the daemon has
no CA and often only a bare IP. The pair blob carries that certificate's SHA-256
fingerprint, and the app pins it: the QR is the out-of-band trust anchor, so a
man-in-the-middle cannot substitute a certificate. The endpoint serves only
`CompletePair`, gated by the one-time code - nothing else is reachable there.

::: tip Pairing needs the pairing port reachable too
Like the WireGuard port, the pairing TCP port (default 51821) must reach the box
from outside. The daemon forwards it via UPnP at startup; if UPnP is unavailable,
forward it manually alongside the WireGuard port. `fletcher peer pair --mobile`
warns when the daemon has no pairing listener (no public endpoint, or the tunnel
is down).
:::

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

From there you can drive the daemon remotely. Install the CLI, run
`fletcher login <token>`, and go. See [Remote control](/guide/remote).

## Revoke a device

If a device is lost or you're done with it:

```sh
fletcher peer list                 # find the id
fletcher peer delete peer_01h...   # revoke tunnel access and token
```

The running tunnel syncs immediately, so the device can't connect any more,
regardless of what's stored on it.

## Next

Your device is paired. Now [run your first agent](/guide/first-agent).
