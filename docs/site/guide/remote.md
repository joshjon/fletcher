# Remote control

Once a device is paired, you can drive the daemon over the WireGuard tunnel as if
you were on the server: submit jobs, run agents, manage settings. The work still
happens on the box (a job spins up its microVM there). Your laptop is a thin
client talking to the daemon over the tunnel.

## What you need on the device

1. **It's paired.** Run `fletcher peer pair laptop` on the server (see [Pair a
   device](/guide/pairing)). The output includes the WireGuard config and a
   ready-to-paste `fletcher login <token>` line.
2. **The tunnel is up.** Import that WireGuard config on the device and connect,
   so it can reach the daemon's tunnel address `10.99.0.1`.
3. **The `fletcher` CLI.** It's the same binary, and the client half runs
   anywhere. On macOS, [install the client](/guide/installation) with the install
   script (it detects macOS and installs the client only) or build from source. A
   Mac build is a pure client, since the daemon is Linux-only, and `fletcher
   --help` groups commands into "Client" and "Daemon (Linux host)" so it's clear
   which apply.

## Log in once, then just use `fletcher`

Paste the `fletcher login <token>` line from the pair output:

```sh
fletcher login <token>     # verifies, then saves to ~/.config/fletcher/config.toml (0600)
fletcher health            # -> status: ok  (now targets the remote daemon)
fletcher session list
fletcher logout            # forget the remote; revert to the local socket
```

`login` makes the remote the default target for every command, so you don't
repeat the address and token. For one-off commands or CI you can pass them
explicitly (or via `FLETCHER_REMOTE` / `FLETCHER_TOKEN`), which override the
stored login:

```sh
fletcher --remote 10.99.0.1:11700 --token <token> health     # -> status: ok
fletcher --remote 10.99.0.1:11700 --token <token> job list
```

::: warning `login` worked but commands return `401 Unauthorized`?
A leftover `FLETCHER_REMOTE` or `FLETCHER_TOKEN` in your shell overrides the
stored login (`login` ignores them, so it still succeeds). Check `echo
$FLETCHER_REMOTE $FLETCHER_TOKEN` and `unset` whichever is set (and remove it
from your shell profile). The CLI also warns when it targets a remote with no
token.
:::

## Spin up a microVM remotely

Once you've run `fletcher login`, commands need no flags. A `job create` runs its
command inside a Firecracker microVM on the server (assuming the Firecracker
runtime and an imported base image):

```sh
fletcher job create --name remote-vm --image fletcher-base \
  --command "echo KERNEL=\$(uname -r); cat /proc/1/comm; exit 3"
fletcher job get <job-id>
```

The `exit 3` fails the job so its captured output lands in the error field, which
`job get` shows remotely. You'll see the guest kernel version and `fletcher-init`
as PID 1, confirming it ran inside a microVM on the server.

::: tip Isolate a tunnel problem from an API problem
Before troubleshooting WireGuard on the device, sanity-check the remote API from
the server itself. `10.99.0.1` is a local interface there, so `fletcher --remote
10.99.0.1:11700 --token <token> health` works locally. If it does, any failure
from the device is tunnel reachability, not the API.
:::

## How access is gated

The network API binds to the tunnel interface only and requires the token. The
local Unix socket needs neither, since it's gated by file permissions. Revoke a
device any time with `fletcher peer delete <id>`, which drops both its tunnel
access and its token. See [Security](/guide/security).

## From a phone

A phone can join the tunnel (it's a WireGuard peer), but there's no client to
drive the daemon from iOS yet. That's the native app, still to come. Today the
tunnel works from the phone, but control is laptop-only.
