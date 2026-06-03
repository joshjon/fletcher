# Fletcher

> Private agent compute on hardware you own.

Fletcher is a single Go binary you install on one Linux box. From a native
client (CLI today, mobile/desktop apps later) you spin up isolated jobs and
run agents inside them — coding assistants, day-to-day tasks, recurring
monitoring — with nothing leaving your network and no cloud account in the
loop.

The pitch in one line: the model gateway, the credentials, the audit log,
the snapshots — everything runs on metal you control.

## Status

Pre-v0.1.0. The build pipeline, daemon lifecycle, job/secret/approval/peer
surfaces, and a working OpenAI-compatible model gateway are in place. The
Firecracker runtime driver is stubbed pending verification on a Linux/KVM
host; runc + btrfs Linux drivers are wired but unverified outside Linux.

## Quickstart

Requires Linux on amd64 or arm64.

```sh
# 1. Install (replace TAG when releases land)
curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh

# 2. Enable + start the daemon
sudo systemctl enable --now fletcher

# 3. Talk to it
fletcher health
fletcher secret set anthropic_api_key sk-ant-...
fletcher job create --name hello --command "echo hi" --image mock
fletcher job list

# 4. Add a WireGuard peer for your phone / laptop
fletcher peer add laptop --address 10.99.0.2/32 --endpoint your.dyndns.org:51820
fletcher peer server-config --listen-port 51820 > /etc/wireguard/fletcher.conf
sudo wg-quick up fletcher
```

The CLI talks to the daemon over a local Unix socket. Subcommand help is
the source of truth: `fletcher --help`, `fletcher job --help`, etc.

## Documentation

- [`DESIGN.md`](./DESIGN.md) — positioning, architecture, the thinking
  behind the trust boundary and the job model. Read this first.
- [`STANDARDS.md`](./STANDARDS.md) — repo conventions: layout, lint, test,
  error handling, logging, dependencies, release process.

## Building from source

```sh
git clone https://github.com/joshjon/fletcher.git
cd fletcher
make build          # local platform binary at ./bin/fletcher
make build-linux    # cross-compile amd64 + arm64 Linux artefacts
make check          # lint + tests + generated-file drift check
```

Go 1.24+ is required (we use the `tool` directive). All other build-time
tools are pinned in `go.mod` and reachable via `go tool <name>`.
