# Public web over HTTPS

Fletcher can serve a [published port](/guide/publishing) to the public internet
over HTTPS, on a domain you own - the daemon terminates TLS and reverse-proxies
into the VM, which still has no network route of its own.

This is **opt-in**. It opens ports 80/443 on your box - the only thing besides
the silent WireGuard port that faces the internet - so it stays off until you
turn it on, and it needs a real public IP (not behind CGNAT) with 80/443
reachable.

## 1. Enable public web

```sh
fletcher settings set public_web true
fletcher settings set acme_staging true     # free, untrusted cert first (no rate limits) - for testing
sudo fletcher daemon restart                # on a source build use `make install`; the unit needs a capability
```

Starting with `acme_staging` on means your first attempts use Let's Encrypt's
staging environment, which has no rate limits - so you can iterate on DNS and
firewall without burning your real-certificate quota.

## 2. Publish with a hostname

```sh
fletcher session publish dev 8000 --public --host app.example.com
```

Fletcher prints the exact DNS record to create (`app.example.com A <your-public-ip>`)
- it already knows your public IP. Add that record at your DNS provider.
`fletcher session ports dev` shows when the hostname resolves back to you.

Then open `https://app.example.com`: Fletcher obtains a Let's Encrypt certificate
automatically on the first request.

## 3. Switch to a trusted certificate

Once the staging flow works end to end, switch to real certs:

```sh
fletcher settings set acme_staging false
sudo fletcher daemon restart
```

## What it exposes

The daemon serves **only** the ports you publish, and only issues certificates
for hostnames you've actually published. Ports 80/443 are forwarded via UPnP
where available; otherwise forward them on your router the same way as the
[WireGuard port](/advanced/networking#manual-port-forwarding).

## Deploying an app this way

If you're publishing an app rather than a hand-managed session, [`fletcher
deploy`](/guide/deploy) does the build/pull, run, and publish in one command -
`--public` / `--host` use the same `public_web` switch described here.
