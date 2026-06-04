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
4. Confirm the handshake on the server:

   ```sh
   sudo wg show
   ```

   Success is a `latest handshake: <N> seconds ago` line and non-zero
   `transfer:` for the peer.
5. Confirm traffic flows: from the phone, `ping 10.99.0.1` (the
   server's default tunnel address).

If `wg show` lists the peer but shows **no handshake**, that is a
router/firewall problem on the WireGuard UDP port, not Fletcher -
`doctor` having confirmed the forward narrows it to the router actually
honouring it or an upstream ISP block.

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

Prerequisites: `runc` on `PATH` and a btrfs filesystem for the snapshot
root. If you do not have a spare btrfs partition, a loopback image is the
no-hardware dev path:

```sh
sudo truncate -s 5G /var/lib/fletcher-snap.img
sudo mkfs.btrfs /var/lib/fletcher-snap.img
sudo mkdir -p /var/lib/fletcher/snapshots
sudo mount -o loop /var/lib/fletcher-snap.img /var/lib/fletcher/snapshots
```

Point the daemon at the real drivers via a systemd drop-in:

```sh
sudo systemctl edit fletcher
#   [Service]
#   Environment=FLETCHER_RUNTIME=runc
#   Environment=FLETCHER_SNAPSHOT=btrfs
#   Environment=FLETCHER_BTRFS_ROOT=/var/lib/fletcher/snapshots
sudo systemctl restart fletcher
```

Confirm the switch took: the startup log should now read
`drivers selected runtime=runc snapshot=btrfs`, and `fletcher doctor`
should still be clean.

**Current limitation - read before running a job.** The btrfs driver runs
a job against a rootfs template at `<btrfs-root>/images/<image>`. There is
not yet any tooling to flatten the `fletcher-base` OCI image (from
`make image`) into that location, so a job with no matching template gets
an empty subvolume and runc has nothing to `exec`. Until the image-flatten
step lands (tracked in `ROADMAP.md`), an end-to-end real-agent job is not
push-button. For a manual smoke test you can populate a rootfs yourself,
e.g. export a container filesystem into a freshly created subvolume:

```sh
# Build the base image first (needs Docker): make image
sudo btrfs subvolume create /var/lib/fletcher/snapshots/images/fletcher-base
CID=$(docker create fletcher-base:dev)
docker export "$CID" | sudo tar -x -C /var/lib/fletcher/snapshots/images/fletcher-base
docker rm "$CID"
# Then a job with --image fletcher-base has a real rootfs to run in.
fletcher job create --name runc-smoke --command "echo hi from runc" --image fletcher-base
sleep 1
fletcher job get <job-id>   # expect status: succeeded
```

To revert to the mock drivers, remove the drop-in
(`sudo systemctl revert fletcher`), restart, and unmount the loopback if
you created one.

### Power-user CLI

`fletcher peer add` (the pre-Phase-15 CLI surface) still exists for
operators who want to override tunnel IP, endpoint, or allowed-IPs
manually. The `pair` command is the documented path; `add` is the
escape hatch for atypical cases.
