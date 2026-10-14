# Public apps behind an existing reverse proxy

Fletcher can serve explicitly published apps in two modes:

- `edge` (default): Fletcher binds ports 80/443 and manages TLS with ACME.
- `behind_proxy`: Fletcher serves plain HTTP on a loopback listener. Your
  same-host reverse proxy owns public listeners, TLS, DNS and access control.
  Fletcher does not start ACME, redirect requests, or automatically map ports
  80/443 on the router in this mode. Other networking, including pairing and
  WireGuard port mappings, is unchanged.

Both modes require `public_web = true` and an explicit public publish for each
app. Neither listener exposes the management API, model gateway or MCP. Traffic
is still brokered into guests over vsock; guests do not acquire a network route
(DESIGN sections 3, 7 and 8).

## Configure

For a proxy running on the same host:

```sh
fletcher settings set public_web_mode behind_proxy
fletcher settings set public_web_listen 127.0.0.1:11800
fletcher settings set public_web_trusted_proxies '127.0.0.1/32'
fletcher settings set public_web true
fletcher daemon restart

fletcher session publish <session-ref> 8080 --public --host app.example.com
```

These settings require a daemon restart. `public_web_listen` defaults to
`127.0.0.1:11800`. Only literal loopback IP addresses with a nonzero port are
accepted; wildcard, LAN, public and hostname binds are rejected. IPv6 loopback
is supported, e.g. `[::1]:11800` with a trusted source of `::1/128`.

This initial mode does not support a proxy on another host or in a separate
container network namespace. Do not expose the origin with a wildcard bind or
add an unprotected port forward as a workaround.

`public_web_trusted_proxies` is a space-separated list of source CIDRs, empty by
default (no trusted sources). Trust only the smallest source range needed.
Loopback is a network boundary, not process authentication: another local
process using a trusted source can supply the same metadata as the proxy.

To return to self-contained TLS, stop or reconfigure the existing proxy so
Fletcher can bind 80/443, set `public_web_mode edge`, then restart Fletcher.
The behind-proxy bind and trusted-source settings have no effect in edge mode.

## Proxy contract

- Preserve the original `Host` header. It is the only routing authority.
  Unknown/unpublished hosts return 404, even if forwarded headers name an app.
- Support HTTP/1.1 upgrades for WebSockets.
- **Overwrite** `X-Forwarded-Proto` with `http` or `https`, and
  `X-Forwarded-For` with a single client IP. Do not append a proxy chain or pass
  these headers through from visitors. Fletcher accepts them only from an
  explicitly trusted immediate peer; malformed, duplicate or chained values
  are ignored in favor of the immediate connection's metadata.
- Fletcher reconstructs `X-Forwarded-Host` from `Host`. Incoming `Forwarded`,
  `X-Real-IP` and other `X-Forwarded-*` fields are discarded, not passed through
  as alternative identity assertions. Edge mode ignores all incoming forwarded
  metadata.
- Configure upstream timeouts to allow guest wake-up. Public requests can wake
  stopped sessions through the existing port dialer.

Trusted metadata does not grant administrative access. Apps still need their own
authentication or a proxy access gate. Publishing does not create a Fletcher
browser login. The operator is responsible for any additional identity headers
or access controls supplied by their proxy.

## Caddy example

Replace the fictional hostnames with names you control. Publish each hostname
against the intended session/guest port, then configure Caddy:

```caddyfile
one.example.com, two.example.com {
    reverse_proxy 127.0.0.1:11800 {
        header_up Host {http.request.hostport}
        header_up X-Forwarded-Proto {scheme}
        header_up X-Forwarded-For {remote_host}
    }
}
```

Caddy handles WebSocket upgrades and public TLS. Point DNS at the proxy's public
endpoint and configure your firewall/router for that proxy, not the loopback
origin. If another proxy precedes Caddy, this example reports the immediate peer
as the client; a multi-hop client-IP trust policy is your responsibility.

## nginx example

Put the `map` in nginx's `http` context. Supply certificates covering the two
names through your existing certificate-management process:

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    '' close;
}

server {
    listen 443 ssl;
    server_name one.example.com two.example.com;
    ssl_certificate /etc/nginx/tls/apps.fullchain.pem;
    ssl_certificate_key /etc/nginx/tls/apps.key;

    location / {
        proxy_pass http://127.0.0.1:11800;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 300s;
    }
}
```

Use your existing HTTP-to-HTTPS redirect and unknown-host policy. Do not use
`$proxy_add_x_forwarded_for`: the Fletcher contract expects one overwritten IP,
not a visitor-controlled chain. Add access control here if the app lacks it.

## Status and troubleshooting

`PublishPort` and `ListPorts` return `tls_status = "external"` with
`tls_expires_at = 0` in behind-proxy mode. This means **operator-managed, not
verified by Fletcher**, not that a certificate is valid or pending issuance.
The CLI and native clients display this distinction and do not advise pointing
DNS directly at the daemon. Client `869947d` adds shared iOS/macOS status,
publish/deploy guidance and hosting settings against daemon contract `7daa28a`.
Device verification remains open; this is an M22 contract pair, not a claim that
the separate M23 host-management batch is integrated.

- 404: check the preserved Host and the session's explicit public publish.
- 502: inspect daemon logs and the guest app/port; the upstream could not be
  reached. Do not switch to another exposure mode automatically.
- Connection refused: check the configured loopback address and daemon logs.
  App listener bind failures are logged without taking down host management.
- Wrong scheme/client IP: check the immediate proxy source CIDR and that the
  proxy overwrites, rather than appends, the two accepted metadata headers.
- Certificate/browser errors: inspect the operator's proxy. Fletcher neither
  issues nor inspects its certificates in this mode.

## Verification status

Automated tests cover hostname routing for two guests, unknown-host rejection,
republishing a hostname without reusing the old guest connection, HTTP upgrade
relay, forwarded-header spoofing, edge routing/redirect/certificate authorization,
app-listener isolation, configuration validation and external-TLS API/CLI output.
These tests use local HTTP fixtures, not Firecracker or a real ACME issuer.

A real nginx 1.29.1 TLS-proxy test also passes in an isolated Linux arm64
container. It routes two fixture apps, relays a WebSocket handshake and masked
text frame, rejects unknown hosts, overwrites spoofed scheme/client-IP headers,
and keeps management-looking paths on the selected app. Requests succeed after
restarting the Fletcher app listener. This is a listener restart, not daemon or
VM lifecycle verification. The test trusts its temporary certificate explicitly;
it does not disable certificate verification or use a public ACME service.

Run the disposable Linux proxy smoke test from the repository root:

```sh
scripts/test-public-proxy.sh
```

The script pins the nginx image digest, cross-compiles a static test binary and
runs it as an unprivileged user with no network access, host ports or host
credentials. Docker may pull the test image before the isolated run. No live
daemon, router mapping or guest disk is touched. The cross-compiled smoke binary
has no race instrumentation; ordinary Go unit tests run with `-race`. On a Linux
host with nginx and a native Go toolchain, run the same test with the race detector:

```sh
FLETCHER_TEST_NGINX="$(command -v nginx)" \
  go test -race -tags=integration ./internal/session -run '^TestPublicAppsBehindNGINX$' -v
```

Native client `869947d` passes 37 core tests and 21 UI tests, with unsigned iOS
arm64 and macOS arm64 builds on Xcode 26.3. A universal Mac build is blocked by
the existing arm64-only WireGuard libraries; no Intel Mac claim is made here.
The client repo's `docs/public-hosting.md` records the commands and device checks.

Still required before closing M22:

- [ ] Linux: run two real apps through a same-host TLS proxy, including WebSockets.
- [ ] Linux: hibernate/wake and daemon restart preserve hostname routing.
- [ ] Linux: verify behind-proxy mode creates no ACME state or 80/443 mappings.
- [ ] Edge: obtain/serve a real certificate and verify existing behavior.
- [x] Proxy boundary: nginx replaces spoofed forwarded headers; unknown hosts
      cannot select an app and management paths never select daemon services.
- [x] Native implementation: external-TLS rendering and hosting settings are in
      client `869947d`; iOS and macOS arm64 builds pass.
- [ ] Native devices: open both real apps from paired phone/Mac clients and check
      accessibility, mode changes and older edge-only daemon behavior.

Record daemon/client revisions, guest/image versions, architecture and results
when performing these checks. Mock/HTTP tests alone do not establish those
hardware and device guarantees.
