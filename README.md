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

Pre-v0.1.0, but the core loop works end to end on a real Linux box: a job
runs a real agent (Claude Code) inside an isolated **rootless runc + btrfs
fork**, reaching models *only* through the daemon's gateway (the API key never
enters the fork, and the fork has no other network egress). You configure and
manage it entirely with `fletcher` verbs - `fletcher settings`,
`fletcher daemon` - with no systemctl, and you can drive the daemon from a
paired device over the tunnel with a per-peer token. The **Firecracker**
microVM runtime (the intended default isolation tier) is the remaining piece
and is still stubbed.

See [`docs/ROADMAP.md`](docs/ROADMAP.md) for exactly what is built, verified,
and pending.

## Quickstart

Requires Linux on amd64 or arm64.

```sh
# 1. Install (downloads the latest release, sets up systemd):
curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh

# 2. Enable + start the daemon:
sudo systemctl enable --now fletcher

# 3. Talk to it:
fletcher health
fletcher job create --name hello --command "echo hi" --image mock
fletcher job list

# 4. Pair your phone (scan the QR with the WireGuard app):
fletcher peer pair phone
```

That runs against the default mock runtime (a plain host subprocess - good
for a smoke test). Running a *real agent in an isolated fork* needs the
runc + btrfs runtime and a base image; the full walkthrough - including
`fletcher settings`, `fletcher daemon`, running Claude Code in a fork, and
driving the daemon from a paired device - is in
[`docs/setup.md`](docs/setup.md).

The daemon brings up its own WireGuard interface and asks your router to
forward the listening port via UPnP - on most home connections that's
the whole setup. Troubleshooting and the "bring-your-own-VPN" alternative
(Tailscale, etc.) are in [`docs/setup.md`](docs/setup.md) too.

The CLI talks to the daemon over a local Unix socket (or a remote daemon
with `--remote host:port --token …`). Subcommand help is the source of
truth: `fletcher --help`, `fletcher job --help`, etc.

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
