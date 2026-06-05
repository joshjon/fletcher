# Testing Fletcher (developer smoke tests)

> **This doc is for Fletcher developers**, not end users. If you want to
> install and use Fletcher, read [`setup.md`](setup.md) instead.

Automated tests live alongside the code (`make test`); this file covers
the manual end-to-end smoke tests we run against a daemon before
shipping a change.

Two flows: **macOS for development** (no Linux, no networking knowledge
required) and **Linux server for real deployment** (covered in detail in
`setup.md`; the testing-specific bits are below).

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

## Linux server (testing a change)

For the full setup flow that an end user follows (install, networking,
pairing a device, security notes, troubleshooting), see
[`setup.md`](setup.md). This section covers what's testing-specific
for developers iterating on Fletcher itself.

### First deploy to a test server

After cloning Fletcher on the server, one command does everything
`scripts/install.sh` does, against local files:

```sh
make install
```

This creates the `fletcher` system user (if missing), pre-creates the
state directories under `/var/lib/fletcher` and `/etc/fletcher`,
installs the binary at `/usr/local/bin/fletcher` and the systemd unit
at `/etc/systemd/system/fletcher.service`, reloads systemd, and (if
the service is already running) restarts it. The `daemon-reload` and
restart matter when the unit file has changed in your branch.

For first install, set the public endpoint via a drop-in override and
start the service (see [setup.md § Mode A](setup.md#mode-a-built-in-wireguard-recommended-for-most-homelabs)
for the boilerplate). For testing inside a single LAN, you can skip
both the port forward and `--public-endpoint` - peers on the same
network reach the daemon via its LAN IP.

### Deploy-iterate loop

After making changes locally:

```sh
# On the server:
cd ~/git/fletcher
git pull
make install        # same command for upgrade; restarts daemon if running
sudo journalctl -u fletcher -f
```

About 15 seconds from `git pull` to "new binary running." Watch the
logs for the lines you care about (UPnP result, tunnel up, peer pair
output).

### What to verify after a deploy

Most changes only need the macOS smoke test above to gain confidence.
Run `fletcher doctor` first; once it reports `0 issues` the daemon,
host device, networking, and provider reachability are all green, and
the only things left to exercise are the two features that cannot run
on macOS: WireGuard peer pairing and the real runc + btrfs runtime.

The tests below assume `make install` has run and `fletcher doctor` is
clean. Each says what it proves.

### 1. Peer pairing end-to-end (WireGuard)

**What this proves:** the daemon brought its own tunnel up
(`wireguard-go`, no `wg-quick`), the UPnP forward (or your manual one)
lets an outside device reach the WireGuard port, and the handshake
completes. This is the Phase 9 + 15 deliverable and the one thing the
mock path cannot cover.

On the server, pair a device. The endpoint is taken from the daemon's
auto-detected public endpoint, so no flags are needed when `doctor` is
green:

```sh
fletcher peer pair phone
```

This prints a QR code and the equivalent `wg-quick` config. Then:

1. Install the **WireGuard** app on the phone, tap **+**, scan the QR.
2. **Turn wifi off on the phone** (use cellular). This is what proves
   reachability *through the port forward* from outside the LAN, not
   just LAN-local routing.
3. Toggle the tunnel on.
4. Confirm the tunnel on the server. Note `wg show` will print nothing:
   Fletcher runs userspace wireguard-go in-process, so there is no kernel
   WireGuard device and no `/var/run/wireguard` control socket for the
   `wg` tool to read. Check the interface and the daemon log instead:

   ```sh
   ip addr show fletcher0                       # UP, with 10.99.0.1/24
   journalctl -u fletcher | grep -i wireguard   # "tunnel up" + "peer set updated peers:N"
   ```

5. Confirm the handshake from the client. The server cannot show it -
   per-peer handshake/transfer stats live inside the in-process
   wireguard-go device and are not surfaced server-side yet - so the
   WireGuard app itself is the place to look, and it is the easiest
   check on any platform:

   - Tap the tunnel's name to open its detail screen.
   - Look for **Latest handshake: <N> seconds ago** and non-zero
     **Data received / sent**.

   The rendered config sets `PersistentKeepalive = 25`, so the phone
   handshakes within ~30s of enabling the tunnel without any manual
   traffic. If the handshake stays empty, pull to refresh, or open
   `http://10.99.0.1:11500` in the phone's browser - it returns no page
   (nothing is bound there for tunnel clients yet), but the attempt
   pushes a packet through the tunnel and forces the handshake.

   iOS has no built-in `ping`/terminal, so the app's counters are the
   confirmation; if you want an explicit ICMP `ping 10.99.0.1`, install a
   network-utility app from the App Store. On a laptop peer, plain
   `ping 10.99.0.1` works.

If the handshake never lands while `fletcher0` is up and the daemon
logged the peer, that is a router/firewall problem on the WireGuard UDP
port, not Fletcher - `doctor` having confirmed the forward narrows it to
the router actually honouring it or an upstream ISP block.

Scope note: the daemon's Connect API (Unix socket) and gateway
(`127.0.0.1:11500`) are loopback-bound, so a peer cannot reach those
*over* the tunnel yet - that is the future preview-URL reverse proxy.
What this test proves is that the tunnel itself carries traffic.

Tear the test peer down when done:

```sh
fletcher peer list
fletcher peer delete <peer-id>
```

### 2. Real runtime (runc + btrfs)

**What this proves:** jobs run inside a real OCI container (runc) against
a real copy-on-write snapshot (btrfs), instead of the mock driver's bare
subprocess and plain directory.

Prerequisites: `runc` and `docker` on `PATH`, and a btrfs filesystem for
the snapshot root that the daemon (the `fletcher` user) can write to. No
spare btrfs partition? A loopback image is the no-hardware dev path:

```sh
sudo truncate -s 5G /var/lib/fletcher-snap.img
sudo mkfs.btrfs /var/lib/fletcher-snap.img
sudo mkdir -p /var/lib/fletcher/snapshots
sudo mount -o loop /var/lib/fletcher-snap.img /var/lib/fletcher/snapshots
# The daemon creates one fork subvolume per job, so it must own the root:
sudo chown fletcher:fletcher /var/lib/fletcher/snapshots
```

The shipped systemd unit grants the daemon `CAP_SYS_ADMIN` (for btrfs
subvolumes), so `make install` covers the privilege side. The daemon runs
unprivileged, so runc is **rootless**: it maps the container's root to the
daemon's own user, which is why `image import` chowns the template to that
user (next step) - nothing for you to configure.

Point the daemon at the real drivers with `fletcher settings`, then restart:

```sh
fletcher settings set runtime runc
fletcher settings set snapshot btrfs
fletcher settings set btrfs_root /var/lib/fletcher/snapshots
fletcher daemon restart
```

Confirm the switch took: `fletcher daemon logs | grep "drivers selected"`
should read `runtime=runc snapshot=btrfs`, and `fletcher doctor` clean.

Import a base-image rootfs template so jobs have something to run in. A
job's `--image` names a template at `<btrfs-root>/images/<name>`; build the
OCI image once, then flatten it into the btrfs root (the import chowns it to
the daemon user for rootless runc):

```sh
make image                       # builds fletcher-base:dev (needs Docker)
sudo fletcher image import fletcher-base:dev \
  --btrfs-root /var/lib/fletcher/snapshots --name fletcher-base
sudo fletcher image ls --btrfs-root /var/lib/fletcher/snapshots   # -> fletcher-base
```

All three `image` subcommands need `sudo`: the btrfs subvolume and docker
need root, and `/var/lib/fletcher` is mode 0700 owned by the `fletcher`
user. Pass `--btrfs-root` explicitly because `sudo` does not carry
`FLETCHER_BTRFS_ROOT` through.

Smoke test - a command in a real fork:

```sh
fletcher job create --name runc-smoke --command "echo hi from runc" --image fletcher-base
sleep 1
fletcher job list   # runc-smoke -> succeeded
```

**A real agent in the fork.** `claude --version` proves the agent binary
runs as the fork's user; a job's stderr is captured into its error field, so
a failure shows why:

```sh
fletcher job create --name agent --command "claude --version" --image fletcher-base
sleep 2
fletcher job get <job-id>          # status: succeeded
```

**A real model call through the gateway.** Give the daemon a key, then have
the agent generate (it reaches Anthropic only via the daemon gateway):

```sh
fletcher secret set anthropic_api_key sk-ant-...
fletcher job create --name call --command "claude -p 'reply with: PONG'" --image fletcher-base
sleep 8
fletcher job get <job-id>          # status: succeeded (a real generation)
```

**No egress from the fork.** The fork reaches only the daemon, nothing else:

```sh
# reaches the gateway via the in-fork forwarder -> succeeds:
fletcher job create --command "curl -sf http://127.0.0.1:11500/v1/catalog.json -o /dev/null" --image fletcher-base
# the public internet -> fails (cannot even resolve, exit 6):
fletcher job create --command "curl -sf --max-time 6 https://example.com -o /dev/null" --image fletcher-base
```

To revert to the mock drivers: `fletcher settings unset runtime` (and
`snapshot`, `btrfs_root`), `fletcher daemon restart`, and unmount the
loopback if you created one.

### 3. Remote API (drive the daemon from a paired client)

**What this proves:** a paired device drives the daemon over the tunnel, with
the per-peer token enforced and the local socket still open.

Pair a device (or reuse one); the output includes a token and the
`fletcher --remote <addr> --token <token>` line. From the box itself the
tunnel IP is a local interface, so you can test it there:

```sh
ADDR=10.99.0.1:11700
fletcher --remote $ADDR --token <token> health      # -> ok
fletcher --remote $ADDR --token wrong-token health  # -> 401 unauthorized
fletcher --remote $ADDR health                      # -> 401 unauthorized
fletcher health                                     # local socket, no token -> ok
```

### Power-user CLI

`fletcher peer add` (the pre-Phase-15 CLI surface) still exists for
operators who want to override tunnel IP, endpoint, or allowed-IPs
manually. The `pair` command is the documented path; `add` is the
escape hatch for atypical cases.
