# Fletcher

> Private agent compute on hardware you own.

Fletcher is a single Go binary you install on one Linux box. From a native
client (CLI today, mobile/desktop apps later) you spin up isolated jobs and
run agents inside them - coding assistants, day-to-day tasks, recurring
monitoring - with nothing leaving your network and no cloud account in the
loop.

The pitch in one line: the model gateway, the credentials, the audit log,
the snapshots - everything runs on metal you control.

## Status

Pre-v0.1.0, but the core loop works end to end on a real Linux box. A job runs
inside a **Firecracker microVM** (the default on a KVM host) - or a rootless
**runc** fork as the no-KVM fallback - reaching models *only* through the
daemon's gateway (the API key never enters the VM, and the VM has no network
egress at all - only a vsock channel to the daemon). You configure and manage it
entirely with `fletcher` verbs - `fletcher settings`, `fletcher daemon` - with no
systemctl, and you can drive the daemon from a paired device over the tunnel with
a per-peer token. The remaining gap to a one-command experience is a
shipped/pullable base image (today you build it once with `make image`).

See [`docs/ROADMAP.md`](docs/ROADMAP.md) for exactly what is built, verified,
and pending.

## Quickstart

Requires Linux on amd64 or arm64.

```sh
# 1. Install (downloads the latest release, sets up systemd):
curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh

# 2. Enable + start the daemon:
sudo systemctl enable --now fletcher

# 3. Check it's healthy:
fletcher health
fletcher doctor          # checks /dev/kvm, the bundled VMM, networking, ...

# 4. Pair your phone (scan the QR with the WireGuard app):
fletcher peer pair phone
```

On a KVM host the daemon defaults to the **Firecracker** runtime, so running a
job needs a base image first. Pull the published one and import it:
`sudo fletcher image import ghcr.io/joshjon/fletcher-base:debian-13 --format
ext4 --name <name>`, then `fletcher job create --image <name> --command "..."`.
The full walkthrough - importing the image,
running an agent in a microVM, `fletcher settings` / `fletcher daemon`, and
driving the daemon from a paired device - is in [`docs/setup.md`](docs/setup.md).
(No KVM? The daemon falls back to the mock runtime, where
`fletcher job create --command "echo hi"` runs as a plain subprocess.)

The daemon brings up its own WireGuard interface and asks your router to
forward the listening port via UPnP - on most home connections that's
the whole setup. Troubleshooting and the "bring-your-own-VPN" alternative
(Tailscale, etc.) are in [`docs/setup.md`](docs/setup.md) too.

The CLI talks to the daemon over a local Unix socket, or to a remote daemon
over the tunnel: run `fletcher login <token>` once (the token is printed by
`fletcher peer pair`) and subsequent commands target it by default, or pass
`--remote host:port --token …` / `FLETCHER_REMOTE` + `FLETCHER_TOKEN` per
command. Subcommand help is the source of truth: `fletcher --help`,
`fletcher job --help`, etc.

Beyond one-shot jobs, Fletcher hosts **durable sessions** - persistent microVMs
you shell or SSH into (with hibernate/restore) - and can **deploy a web app** from
a Docker image: `fletcher deploy <image> --host app.example.com` builds or pulls
the image, runs it, and serves it publicly over HTTPS on a domain you own, with
the app sandboxed in a no-egress microVM. The daemon pulls registry images itself,
so `deploy` works from a laptop over the tunnel with no local Docker. Walkthroughs
for both are in [`docs/setup.md`](docs/setup.md).

## Documentation

- [`docs/setup.md`](docs/setup.md) - end-user install, first run,
  running agents, configuration, the remote client, networking modes,
  security notes, troubleshooting. Start here if you're running Fletcher.
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

# Install on a Linux server (mirrors what scripts/install.sh does
# using your local build):
make install        # create user, install binary + unit, reload + restart-if-running
```

Iterate on a deployed server with:

```sh
git pull
make install        # same command for first install and upgrade
```
