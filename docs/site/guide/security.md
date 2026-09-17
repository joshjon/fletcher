# Security

Fletcher runs environments and apps on a host you control. This page covers what
you expose when you run it and how access is gated. Self-hosting does not mean
that no data leaves your network.

## What you're exposing

In [Mode A](/guide/networking#mode-a-built-in-wireguard), UDP 51820 is open to
the public internet on your home connection. Some honest framing:

**WireGuard is unusually safe to expose.** The protocol refuses to acknowledge
that it's running unless a packet completes a cryptographic handshake with a key
it already knows. To port scanners (nmap, Shodan, random bots) the port looks
identical to a closed one, with no version string, no fingerprint, and no reply.
That's the opposite of SSH or any HTTP service, which respond to every probe.

The only other thing that can face the internet is public web (ports 80/443),
and it's **off until you opt in**. See [Public web over
HTTPS](/advanced/public-web).

## The real boundary is pairing

When you pair a device, the QR code / wg-quick config contains a private key that
authorises the device to reach Fletcher. That key is **shown exactly once** and
is never stored on the server again. Treat it like a password. Don't paste it
into chat, don't log it.

**A paired device has full daemon access.** Driving the daemon over the tunnel
takes two things, both handed out at pair time:

- tunnel reachability (the WireGuard key), and
- a per-peer API token (sent as a bearer token, the daemon stores only its hash).

That's defense in depth. A leaked WireGuard key alone reaches the API port but
gets `401` without the token. A fully paired device gets both, and with them can
create VMs, deploy apps, manage secrets and settings, submit jobs, and use the
model gateway.

So **pairing a peer is not "letting a device onto my LAN". It is "granting that
device control over Fletcher."** Pair only devices you intend to use Fletcher
with.

## If a device is lost or compromised

Revoke it immediately:

```sh
fletcher peer list                 # find the id
fletcher peer delete peer_01h...   # revoke
```

The running tunnel syncs on this command, so the lost device can't connect any
more, regardless of what's stored on it. Deleting the peer drops both its network
access and its token.

## How the trust boundary holds inside the box

- **VM isolation and outbound access are distinct.** MicroVMs communicate with
  the daemon over vsock rather than a guest NIC. The daemon can broker outbound
  requests, so the lack of a NIC does not mean an environment is entirely offline.
- **Credential isolation depends on configuration.** The optional model gateway
  keeps its configured provider API keys outside the guest. Credentials supplied
  to a program or saved by an agent login inside a VM are accessible there.
- **External services receive requests.** Cloud model calls send prompts to the
  selected provider. Image pulls and other external access also use the network.
  Local compute alone is not a guarantee of local-only data processing.
- **The two networking planes never touch.** Your devices are WireGuard peers to
  the daemon only. VM networking lives entirely inside the box, and clients never
  get a route into VM-land. A preview URL is the daemon reverse-proxying in, not a
  route out.
