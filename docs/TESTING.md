# Testing Fletcher

How to exercise Fletcher manually. Automated tests live alongside the
code (`make test`); this file is for end-to-end checks against a
running daemon.

Two flows: **macOS for development** (no Linux, no networking knowledge
required) and **Linux server for real deployment** (one-time setup of
your public endpoint, then add devices with one command each).

## macOS (development)

Five minutes, no networking knowledge required. The daemon runs end-to-
end on macOS via mock drivers - no Linux box, no WireGuard, no real VM
isolation. Each step says what it proves so you can skip ones you don't
care about.

**What this proves:** the daemon serves, the CLI talks to it, the job
supervisor runs jobs, secrets encrypt/decrypt, the model gateway routes
to Anthropic, and the model catalog is reachable.

**What this does NOT prove:** real isolation (Firecracker), real CoW
snapshots (btrfs), or WireGuard peer-to-daemon networking. Those are
Linux-only and covered separately.

### Setup

**Build:**

```sh
make build
```

**Start the daemon** (Terminal 1, leave running):

```sh
mkdir -p /tmp/fletcher-data
./bin/fletcher serve \
  --socket   /tmp/fletcher-data/fletcher.sock \
  --database /tmp/fletcher-data/fletcher.db \
  --age-key  /tmp/fletcher-data/age.key
```

You should see `drivers selected runtime=mock snapshot=mock` plus three
`listening` lines (Connect socket, gateway, MCP).

**Open Terminal 2** and point the CLI at the daemon:

```sh
export FLETCHER_SOCKET=/tmp/fletcher-data/fletcher.sock
```

All commands below assume `$FLETCHER_SOCKET` is set.

### 1. Daemon is alive

```sh
./bin/fletcher version
./bin/fletcher health
```

Proves Connect-RPC over the Unix socket works in both directions.

### 2. Job lifecycle - success, failure, cancellation

```sh
# Should succeed.
./bin/fletcher job create --name ok   --command "echo hi" --image mock

# Should fail with exit_code=7.
./bin/fletcher job create --name fail --command "exit 7"  --image mock

# Long-running; we'll cancel it.
./bin/fletcher job create --name slow --command "sleep 30" --image mock

sleep 1
./bin/fletcher job list
```

`ok` should be `succeeded`, `fail` should be `failed`, `slow` should be
`running`. Cancel the long one:

```sh
SLOW_ID=$(./bin/fletcher job list -o json | \
  sed -n '/"name":  *"slow"/{x;p;d;};x;h' | \
  sed -n 's/.*"id":  *"\(job_[^"]*\)".*/\1/p' | tail -1)
./bin/fletcher job cancel "$SLOW_ID"
./bin/fletcher job get "$SLOW_ID"
```

`status: cancelled`, `completed_at` populated. Proves the supervisor's
atomic queued→running claim, exit-code propagation, and cancellation.

### 3. Secrets (age-encrypted at rest)

```sh
./bin/fletcher secret set anthropic_api_key sk-ant-fake
./bin/fletcher secret list
./bin/fletcher secret delete anthropic_api_key
```

Proves the age identity was generated on first boot, the value was
encrypted, and the list endpoint deliberately doesn't return plaintext.

### 4. Model gateway and catalog

The daemon exposes two LLM wire formats on its gateway listener
(default `127.0.0.1:11500`):

- `POST /v1/chat/completions` - OpenAI Chat Completions shape (used by
  Codex, Aider, OpenHands, pi). Translates to Anthropic internally.
- `POST /v1/messages` - Anthropic Messages shape (used by Claude Code).
  Passes through to api.anthropic.com unchanged with the key stamped.

**List what the gateway can route to:**

```sh
./bin/fletcher model list
```

You should see two providers (`anthropic` and `openai-compatible`)
each listing three models. JSON output: `./bin/fletcher model list -o json`.

**Test the gateway without a real key.** You should get a clean 401:

```sh
curl -sS http://127.0.0.1:11500/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4-5","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}'
```

Output: `{"error":{"type":"no_api_key","message":"no secret \"anthropic_api_key\" set; run \`fletcher secret set anthropic_api_key <key>\`"}}`.

**Test with a real Anthropic key** (skip if you don't have one):

```sh
./bin/fletcher secret set anthropic_api_key sk-ant-...real...

# OpenAI shape:
curl -sS http://127.0.0.1:11500/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}],"max_tokens":20}'

# Anthropic native shape (what Claude Code sends):
curl -sS http://127.0.0.1:11500/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4-5","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}'
```

Both should return a real model response. Proves the gateway loads the
secret, stamps it on the upstream call, and that the daemon's role as
"agent points at us, real key never enters the agent's view" works.

### 5. Trusted-credential mode (Phase 12)

The daemon can bind-mount your real `~/.claude/`, `~/.codex/`, or
`~/.config/gemini/` into a job's fork at start time, so agents inside
forks see your existing logins. On macOS with the mock runtime no
actual bind-mount happens - the test value here is just that the
validation and resolution path works.

```sh
# Reject an unknown credential name:
./bin/fletcher job create --name bad-cred --command "echo hi" --image mock --credential nope
# → fletcher: invalid_argument: unknown credential "nope" (allowed: ...)

# If you use Claude Code on this Mac, you already have ~/.claude.
./bin/fletcher job create --name with-cred --command "echo hi" --image mock --credential claude
sleep 1
./bin/fletcher job list
```

If `~/.claude` exists on the host the job succeeds; if not, it fails
with a clear `resolve credentials: ... no such file or directory` -
exactly what the daemon should say.

### 6. Shutdown

`Ctrl-C` in Terminal 1. Expect `daemon stopped`; the socket file
under `/tmp/fletcher-data/` is removed.

### Going further (optional)

**Resume-on-boot.** Create a long job, kill the daemon ungracefully,
restart it, and watch the supervisor reset the orphan row:

```sh
./bin/fletcher job create --name resume --command "sleep 60" --image mock
pkill -KILL -f "fletcher serve"
# Re-run the daemon command from Setup.
# Look for: "resetting orphan running job to queued"
```

**Build the agent base image.** Requires Docker Desktop running:

```sh
make image
```

Builds `fletcher-base:dev` with claude / codex / pi agent CLIs baked
in. The image is only used in real anger on Linux (the snapshot driver
flattens it into a btrfs subvolume); on macOS this is just a "did the
Dockerfile compile" smoke test.

### If something looks off

The daemon log includes `request_id`, procedure, and error category on
every failure. The CLI surfaces errors prefixed with their category
(`not_found: …`, `invalid_argument: …`, `already_exists: …`).

To start clean between runs: `rm -rf /tmp/fletcher-data`.

## Linux server

This is where Fletcher actually does its job: real container isolation
via runc, CoW snapshots via btrfs (once Phase 8 lands the Firecracker
driver, that swaps in too), and your phone/laptop dialing in over
WireGuard.

### One-time setup

1. **Forward UDP 51820 to the Linux box** on your home router. The
   exact UI varies by router model; look for a section called "Port
   Forwarding", "Virtual Server", or "NAT/Gaming". What you need to
   set:
   - Protocol: UDP (not TCP)
   - External port: 51820
   - Internal port: 51820
   - Destination: your Linux box's LAN IP (find it with `ip -4 addr`
     on the server, typically something like 192.168.1.x)

   If port-forwarding isn't accessible (some ISP-provided routers
   lock this down), your home network is behind CGNAT, or you're
   testing inside a single LAN, this step can be skipped — peers
   on the same network can still reach the daemon via its LAN IP.

2. **Pick a public address** peers can dial. Two options:
   - **Static public IP**: use the IP directly (e.g. `203.0.113.5:51820`).
   - **DDNS hostname**: if your ISP rotates your IP, sign up for a free
     dynamic-DNS service (DuckDNS, no-ip) and use its hostname (e.g.
     `myhome.duckdns.org:51820`).

3. **Install Fletcher** and start the daemon with `--public-endpoint`
   set to whatever you picked above. The systemd unit (Phase 10) is
   the long-running form; for a quick first run:

   ```sh
   ./bin/fletcher serve \
     --public-endpoint myhome.duckdns.org:51820
   ```

   That's the only piece of one-time setup. Everything from here is
   one-command operations.

### Add a device

```sh
./bin/fletcher peer pair phone
```

The daemon picks the next free tunnel IP (`10.99.0.2/32` for the first
peer, `10.99.0.3/32` for the next, etc.), generates a keypair, prints
the wg-quick config, and renders a QR code in the terminal. Scan it
with the WireGuard mobile app; the phone is online in seconds.

For a laptop, run the same command (`./bin/fletcher peer pair laptop`)
and copy the printed `[Interface]` / `[Peer]` block into
`/etc/wireguard/fletcher.conf`, then `sudo wg-quick up fletcher`.

If you forgot to set `--public-endpoint`:

```
fletcher: failed_precondition: daemon has no public-endpoint configured;
restart with --public-endpoint <host:port> or set FLETCHER_PUBLIC_ENDPOINT
```

Set it and try again.

### Real container isolation

Switch the runtime driver from the macOS default (`mock`) to `runc` on
the Linux box. Once Phase 8 lands the Firecracker driver, swap to that
for hardware-isolation; `runc` is the degraded-but-functional fallback.

```sh
./bin/fletcher serve \
  --public-endpoint myhome.duckdns.org:51820 \
  --runtime runc \
  --snapshot btrfs \
  --btrfs-root /srv/fletcher/snapshots
```

`--btrfs-root` must already be a btrfs filesystem (a subvolume on /srv
is fine; `mkfs.btrfs` if needed). After that, every job runs inside a
runc container against a fresh CoW snapshot, with WireGuard as the
only path in.

### Power-user CLI

For atypical cases (custom tunnel IPs, split-tunnel allowed-IPs, etc.),
`fletcher peer add` still exists with the same surface it always had.
The pair command is the recommended path; add is for when you need to
override defaults.
