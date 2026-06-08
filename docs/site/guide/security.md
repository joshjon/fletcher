# Security

Fletcher's whole design is a trust boundary you own. This page covers what you
expose when you run it and how access is gated.

## What you're exposing

In [Mode A](/guide/networking#mode-a-built-in-wireguard), UDP 51820 is open to
the public internet on your home connection. Some honest framing:

**WireGuard is unusually safe to expose.** The protocol refuses to acknowledge
that it's running unless a packet completes a cryptographic handshake with a key
it already knows. To port scanners (nmap, Shodan, random bots) the port looks
identical to a closed one - no version string, no fingerprint, no reply. That's
the opposite of SSH or any HTTP service, which respond to every probe.

The only other thing that can face the internet is public web (ports 80/443),
and it's **off until you opt in** - see [Public web over
HTTPS](/advanced/public-web).

## The real boundary is pairing

When you pair a device, the QR code / wg-quick config contains a private key that
authorises the device to reach Fletcher. That key is **shown exactly once** and
is never stored on the server again. Treat it like a password: don't paste it
into chat, don't log it.

**A paired device has full daemon access.** Driving the daemon over the tunnel
takes two things, both handed out at pair time:

- tunnel reachability (the WireGuard key), and
- a per-peer API token (sent as a bearer token; the daemon stores only its hash).

That's defense in depth - a leaked WireGuard key alone reaches the API port but
gets `401` without the token - but a fully paired device gets both, and with them
can submit jobs, manage secrets and settings, and use the model gateway.

So: **pairing a peer is not "letting a device onto my LAN"; it is "granting that
device control over Fletcher."** Pair only devices you intend to use Fletcher
with.

## If a device is lost or compromised

Revoke it immediately:

```sh
fletcher peer list                 # find the id
fletcher peer delete peer_01h...   # revoke
```

The running tunnel syncs on this command, so the lost device can't connect any
more - regardless of what's stored on it. Deleting the peer drops both its
network access and its token.

## How the trust boundary holds inside the box

- **Agents run with no credentials.** A job's microVM has no API keys and no
  network egress - only a vsock channel to the daemon.
- **The daemon is the gate.** Model calls, SSH, and published ports are all
  brokered by the daemon. It holds the keys; the VM never sees them.
- **The two networking planes never touch.** Your devices are WireGuard peers to
  the daemon only. VM networking lives entirely inside the box; clients never get
  a route into VM-land. A preview URL is the daemon reverse-proxying in, not a
  route out.
