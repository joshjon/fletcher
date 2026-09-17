# Fletcher

> Your Linux host, managed from your iPhone or Mac.

Fletcher turns a Linux host you control into a personal cloud. Create persistent
VMs, deploy container images, and manage their terminals, storage, logs and
published ports through native iOS and macOS clients. A single Go daemon runs on
the host, with a CLI for local use and automation.

Use it for development environments, self-hosted apps or background tasks.
Coding agents are one possible use inside a VM, not a requirement. Creating an
environment or deploying an app needs no agent or model-provider account.

Compute and storage stay on your host. External services, including cloud models
if you choose to use them, receive the requests you send to them. Self-hosting
is not a promise that no data ever leaves your network.

## Status

Pre-v0.1.0. The daemon supports durable Firecracker sessions, image deployment,
hibernation, SSH access and port publishing. Native iOS and macOS clients are
developed in a separate repository.

See [`docs/ROADMAP.md`](docs/ROADMAP.md) for implementation, verification and
release status. Until a release is published, [build from source](#building-from-source).

## Quickstart

Requires Linux on amd64 or arm64, with KVM for VMs and app deployments.

```sh
# 1. Install from a published release (or build from source below):
curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh

# 2. Enable + start the daemon:
sudo systemctl enable --now fletcher

# 3. Check it's healthy:
fletcher health
fletcher doctor          # checks /dev/kvm, the bundled VMM, networking, ...

# 4. Pair your phone (scan the QR in the native Fletcher app):
fletcher peer pair --mobile phone
```

Deploy a container image as an app, accessible from your paired devices:

```sh
fletcher deploy nginx:alpine --name web --gateway off
```

Or create a persistent Linux environment and open a terminal:

```sh
fletcher image pull ghcr.io/joshjon/fletcher-base:debian-13 --name fletcher-base
fletcher session create --name dev --image fletcher-base --gateway off
fletcher session shell dev
```

The daemon pulls registry images, so these commands also work from a remote
client without local Docker. No model gateway configuration is needed.
See [durable sessions](docs/site/guide/sessions.md) and
[deploying apps](docs/site/guide/deploy.md) for the full walkthroughs.

Without KVM, the mock runtime runs jobs as unisolated host processes. It is a
development aid, not a substitute for VMs.

The daemon brings up its own WireGuard interface and asks your router to
forward the listening port via UPnP - on most home connections that's
the whole setup. Troubleshooting and the "bring-your-own-VPN" alternative
(Tailscale, etc.) are in the [networking guide](docs/site/guide/networking.md) too.

The CLI talks to the daemon over a local Unix socket, or to a remote daemon
over the tunnel: run `fletcher login <token>` once (the token is printed by
`fletcher peer pair`) and subsequent commands target it by default, or pass
`--remote host:port --token <token>` / `FLETCHER_REMOTE` + `FLETCHER_TOKEN` per
command. Subcommand help is the source of truth: `fletcher --help`,
`fletcher session --help`, `fletcher deploy --help`, etc.

To publish an app publicly, enable public web and deploy with
`--host app.example.com`. See [public web](docs/site/advanced/public-web.md)
for DNS and HTTPS requirements. [Jobs](docs/site/guide/jobs.md) and
[agents](docs/site/guide/first-agent.md) are optional uses of the same host.

## Documentation

- [User guide](docs/site/guide/introduction.md) - installation, VM creation,
  app deployment, remote access, configuration and security. Start here if
  you're running Fletcher.
- [Native host management](docs/HOST-MANAGEMENT.md) - devices, images,
  restart, logs, diagnostics, storage and configuration from the apps.
- [`docs/ROADMAP.md`](docs/ROADMAP.md) - delivery status: what is built,
  verified, deliberately cut, and planned.
- [`DESIGN.md`](./DESIGN.md) - positioning, architecture, the thinking
  behind the trust boundary and the job model. Read this first if
  you're working on Fletcher.
- [`STANDARDS.md`](./STANDARDS.md) - repo conventions: layout, lint,
  test, error handling, logging, dependencies, release process.
- [`docs/TESTING.md`](docs/TESTING.md) - developer smoke tests against
  a running daemon.

## Building from source

For developers, early testers, or anyone running on an arch the release
tarballs don't cover. Go 1.26+ is required (we use the `tool`
directive); all other build-time tools are pinned in `go.mod` and
reachable via `go tool <name>`.

```sh
git clone https://github.com/joshjon/fletcher.git
cd fletcher

# Local builds:
make build          # local platform binary at ./bin/fletcher
make build-linux    # cross-compile amd64 + arm64 Linux artefacts
make check          # lint + tests + generated-file drift check

# Install on a Linux host (mirrors what scripts/install.sh does
# using your local build):
make install        # create user, install binary + unit, reload + restart-if-running
```

Iterate on a deployed host with:

```sh
git pull
make install        # same command for first install and upgrade
```
