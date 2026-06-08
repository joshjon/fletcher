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
in. The image is only used in real anger on Linux (`fletcher image import`
flattens it into an ext4 image for Firecracker or a btrfs subvolume for
runc); on macOS this is just a "did the Dockerfile compile" smoke test. CI
also publishes it to `ghcr.io/<owner>/fletcher-base`, so on Linux you can
pull it instead of building.

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

For first install, let UPnP auto-detect the public endpoint, or set it with
`fletcher settings set public_endpoint <host>:51820` (see
[setup.md § Mode A](setup.md#mode-a-built-in-wireguard-recommended-for-most-homelabs)),
then `fletcher daemon enable`. For testing inside a single LAN, you can skip
both the port forward and the endpoint - peers on the same
network reach the daemon via its LAN IP.

### Deploy-iterate loop

After making changes locally:

```sh
# On the server:
cd ~/git/fletcher
git pull
make install        # same command for upgrade; restarts daemon if running
fletcher daemon logs -f
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
   ip addr show fletcher0                            # UP, with 10.99.0.1/24
   fletcher daemon logs | grep -i wireguard          # "tunnel up" + "peer set updated peers:N"
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

### 2. Real runtime - Firecracker microVMs (the default)

**What this proves:** jobs run inside a real KVM microVM - its own kernel, a
copy-on-write ext4 rootfs, a vsock channel to the daemon, and no NIC - instead
of the mock driver's bare subprocess. This is the default isolation tier.

Prerequisites: a host with `/dev/kvm` (bare metal, or a VM with nested
virtualization). `fletcher doctor` checks `/dev/kvm` and that the VMM is
bundled; both should be green. Nothing else to provision - unlike runc, the
ext4 driver works on any filesystem, so there is no btrfs loopback to set up,
and the daemon creates its own snapshot root.

With no runtime configured, the daemon auto-selects Firecracker on a KVM host.
Confirm:

```sh
fletcher daemon logs | grep "drivers selected"   # runtime=firecracker snapshot=ext4
```

Import a base image as an ext4 rootfs - pull the published one (or `make image`
first and import `fletcher-base:dev`):

```sh
sudo fletcher image import ghcr.io/joshjon/fletcher-base:debian-13 \
  --format ext4 --btrfs-root /var/lib/fletcher/snapshots --name fletcher-base
```

The importer pulls via docker, builds the ext4, and injects the microVM init.
Pass `--btrfs-root` to match the daemon's snapshot root (default
`/var/lib/fletcher/snapshots`); the rootfs is ~3 GB, so leave a few GB free.

Smoke test - a command in a real microVM (exits non-zero so its output lands in
the job's error field):

```sh
fletcher job create --name fc-smoke \
  --command "echo KERNEL=\$(uname -r); cat /proc/1/comm; ls /sys/class/net; exit 9" \
  --image fletcher-base
sleep 6
fletcher daemon logs | grep fc-smoke
```

Expect the guest kernel (`5.10.x`, not the host's), `fletcher-init` as PID 1,
and only `lo` for a NIC - the VM has no other interface, so no egress.

**A real agent + model call.** Give the daemon a key, then have claude generate
(it reaches Anthropic only via the daemon gateway over vsock; the key never
enters the VM):

```sh
fletcher secret set anthropic_api_key sk-ant-...
fletcher job create --name fc-agent --command "claude -p 'reply with: PONG'" --image fletcher-base
sleep 8
fletcher job list   # fc-agent -> succeeded
```

**Driver-level integration tests.** The microVM boot, output streaming, exit
code, the vsock gateway forward, and the no-egress property all have automated
tests behind the `integration` build tag. They need `/dev/kvm` and an ext4
rootfs carrying the guest agent. Copy the template somewhere your user can read,
then run them:

```sh
sudo cp /var/lib/fletcher/snapshots/images/fletcher-base.ext4 /tmp/rootfs.ext4
sudo chown "$USER" /tmp/rootfs.ext4
FLETCHER_TEST_ROOTFS=/tmp/rootfs.ext4 \
  go test -tags integration -run TestFirecracker -v ./internal/runtime/firecrackerdriver/
```

### 3. Real runtime - runc + btrfs (the no-KVM fallback)

**What this proves:** jobs run inside a real OCI container (runc) against
a real copy-on-write snapshot (btrfs), instead of the mock driver's bare
subprocess and plain directory. Use this on a host without `/dev/kvm`, or to
exercise the degraded-isolation path; it is an explicit opt-in
(`fletcher settings set runtime runc`). Unlike Firecracker, runc needs a btrfs
snapshot root - the loopback below.

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

### 4. Remote API (drive the daemon from a paired client)

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

### 5. Durable sessions, published ports, and deployments (M6 / M8 / M9)

**What this proves:** a session is a persistent microVM you can exec/shell into;
a port it serves can be published over the tunnel and (opt-in) on the public
internet over HTTPS; and `deploy` turns a Docker image into a running, published
app in one command - from the box or a remote client.

Prerequisites: the Firecracker runtime (the default) and a base image imported
(test 2). All commands run from the box unless a step says "remote client".

#### 5a. Session + publish a port over the tunnel (M6 + M8 Layer A)

```sh
fletcher session create --name dev                 # boots a persistent microVM
fletcher session exec dev "nohup python3 -m http.server 8000 >/tmp/http.log 2>&1 &"
fletcher session publish dev 8000                  # tunnel-only (no --host)
fletcher session ports dev                         # shows TUNNEL ADDRESS, e.g. 10.99.0.1:41xxx
curl http://10.99.0.1:<tunnel_port>/               # the directory listing
```

Proves the published-port broker forwards the tunnel IP into the VM over vsock.
`fletcher session unpublish dev 8000` stops it.

> Note: a hand-started process like this does **not** survive a session restart
> (a daemon restart cold-boots the session). That is exactly what `deploy --app`
> fixes; this manual flow is for ad-hoc port sharing.

#### 5b. Publish a port publicly over HTTPS (M8 Layer B)

One-time enable (off by default - this opens 80/443 on the box):

```sh
fletcher settings set public_web true
fletcher settings set acme_staging true            # untrusted certs, no LE rate limits - for testing
make install                                       # the unit needs CAP_NET_BIND_SERVICE; restarts the daemon
```

Then publish publicly and follow the printed DNS guidance:

```sh
fletcher session publish dev 8000 --public --host app.example.com
# -> prints: create A record  app.example.com -> <your-public-ip>
fletcher session ports dev                         # DNS column flips to "ok" once it resolves here
```

Add that A record at your DNS provider, then open `https://app.example.com` from
outside the network (staging = browser warning, expected). Switch to a real cert
with `fletcher settings set acme_staging false && fletcher daemon restart`, then
re-hit it. Proves certmagic issues on-demand only for published hosts and the
daemon reverse-proxies into the VM.

#### 5c. Deploy a Docker image (M9) - all variations

`deploy` = import + a `--app` session (runs the image's entrypoint) + publish, in
one command. `nginx:alpine` is a good test image (EXPOSEs 80, runs as root).

```sh
# Public image, tunnel-only (quickest; no DNS needed). Port auto-detected from EXPOSE.
fletcher deploy nginx:alpine --name web
fletcher session ports web && curl http://10.99.0.1:<tunnel_port>/

# Public image, public over HTTPS (needs public_web from 5b + a DNS A record):
fletcher deploy nginx:alpine --name web --host nginx.example.com

# Private registry image (basic auth on the pull):
fletcher deploy ghcr.io/you/app:v1 --registry-auth you:TOKEN --host app.example.com

# Local Dockerfile directory (HOST-ONLY: the build needs the cwd, the import needs root):
sudo fletcher deploy ./myapp --host app.example.com

# From a REMOTE CLIENT (laptop) - a registry ref works over the tunnel, no local docker:
fletcher --remote 10.99.0.1:11700 --token <token> deploy nginx:alpine --name web --host nginx.example.com
```

For a registry ref the **daemon** pulls and flattens the image in-process (no
Docker on the host), which is why it works from a remote client. `fletcher image
pull <ref> [--registry-auth user:token]` does just the import without deploying.

#### 5d. Deploy lifecycle: logs and durability

```sh
fletcher session logs web                          # the app's stdout/stderr
fletcher session get web                           # state + "app: runs the image's entrypoint"

# Durability: the app comes back on its own after a restart (unlike 5a's manual server):
fletcher session stop web && fletcher session start web
fletcher session logs web                           # nginx serving again

fletcher session delete web                         # teardown (also removes published ports)
fletcher image rm nginx                             # optional: drop the imported template
```

Proves app-mode supervision (restart on crash) + boot auto-start of deployed
sessions.

#### Gotchas

- **`--public`/`--host` need `public_web true`** (5b); otherwise publish is
  refused with a clear message.
- **App mode is Firecracker-only** (it relies on the guest init), the default
  runtime.
- **Server-side import needs the images dir daemon-writable.** A pre-existing
  install where `sudo image import` created `<snapshot-root>/images` as root will
  fail a remote deploy with `truncate: ... Permission denied`; fix once with
  `sudo chown fletcher:fletcher /var/lib/fletcher/snapshots/images` (new imports
  set this automatically).
- **Ownership fidelity:** the daemon extracts a server-side import as its own
  user, so rootfs files are daemon-owned - fine for images that run as root; an
  image needing setuid binaries / non-root file ownership should use the
  root-privileged CLI `fletcher image import` instead.

### Power-user CLI

`fletcher peer add` (the pre-Phase-15 CLI surface) still exists for
operators who want to override tunnel IP, endpoint, or allowed-IPs
manually. The `pair` command is the documented path; `add` is the
escape hatch for atypical cases.

## Testing a release the way a user installs it

The highest-fidelity test before tagging: build the actual release artifacts and
install them the way `scripts/install.sh` does. This exercises the real
installer and the release binary - which, unlike `make install`, must embed the
Firecracker VMM + guest agent via the goreleaser hooks.

**1. Build a snapshot.** Needs goreleaser
(`go install github.com/goreleaser/goreleaser/v2@latest`):

```sh
make release-snapshot    # runs the release hooks (fetch-vmm, build-guest-all) -> dist/
```

Confirm the binary actually embedded the VMM (the thing that would otherwise
silently downgrade a release to mock):

```sh
dist/fletcher_linux_amd64_v1/fletcher doctor | grep -A1 "Firecracker VMM"
# -> bundled (firecracker binary + guest kernel)
```

**2. (Optional) start from a clean slate** to test the genuine first-run path -
no pre-existing user, dirs, or storage:

```sh
sudo systemctl disable --now fletcher 2>/dev/null || true
sudo umount /var/lib/fletcher/snapshots 2>/dev/null || true   # only if you made a btrfs loopback
sudo rm -f /usr/local/bin/fletcher /etc/systemd/system/fletcher.service
sudo rm -rf /etc/systemd/system/fletcher.service.d /var/lib/fletcher /etc/fletcher /run/fletcher
sudo systemctl daemon-reload
sudo userdel fletcher 2>/dev/null; sudo groupdel fletcher 2>/dev/null || true
```

**3. Install the snapshot via the real installer.** The `FLETCHER_LOCAL_TARBALL`
mode installs a local tarball instead of downloading a release (skips the GitHub
download + checksum):

```sh
sudo FLETCHER_LOCAL_TARBALL="$PWD/dist/$(cd dist && ls fletcher_*_linux_amd64.tar.gz)" \
  sh scripts/install.sh
```

This creates the `fletcher` user, installs the binary + unit, adds the daemon to
the `kvm` group and you to the `fletcher` group - exactly what `curl | sudo sh`
does once a release is tagged.

**4. From here, follow [`setup.md`](setup.md) as a user would** - `fletcher
daemon enable`, `fletcher doctor`, import the base image, run an agent. Anything
in that flow that needs a step the docs don't mention is a fresh-user-experience
bug to fix before tagging.
