# Durable sessions

A [job](/guide/jobs) runs one command in a fresh microVM and tears it down. A
**session** is the other half: a persistent microVM you create once and keep.
Its disk, the fork, survives stops and restarts, so a `git clone`, your edits,
and an agent's on-disk history are all still there when you come back.

Sessions are how you use Fletcher interactively. Open a shell, attach an IDE over
SSH, or leave an agent running.

::: info Requirements
Sessions need the **Firecracker** runtime (they don't run on the runc fallback)
and an imported base image, the same `fletcher-base` from [Your first
agent](/guide/first-agent).
:::

## Create and use a session

```sh
fletcher session create --name dev --image fletcher-base   # boots the VM
fletcher session list                                      # state, disk, last-used
fletcher session exec dev 'echo hello > /workspace/notes; cat /workspace/notes'
```

`exec` runs a one-off command and returns its output and exit code. For an
interactive terminal straight into the VM:

```sh
fletcher session shell dev      # a real PTY inside the VM; type `exit` to leave
```

## SSH and IDE attach

`fletcher session ssh` sets up everything for a normal `ssh` (and for an IDE's
Remote-SSH) in one step:

```sh
fletcher session ssh dev        # mints a key, installs it, writes your SSH config
ssh fletcher-dev                # just works
```

It generates a managed keypair, installs the public key in the VM, and adds a
`Host fletcher-dev` block (included from your `~/.ssh/config`) whose
`ProxyCommand` tunnels through the daemon. The VM has no network route of its
own. The daemon brokers the SSH connection over vsock, the same trust boundary
as everything else, generalised from HTTP to SSH.

Point VS Code or JetBrains Remote-SSH at the host `fletcher-dev` and it connects.
`scp`/`sftp` and port-forwarding work too. Connecting to a *stopped* session
wakes it first.

This works over the tunnel from a paired laptop the same way. The generated
`ProxyCommand` uses your stored login. See [Remote control](/guide/remote).

::: warning Brokered SSH needs an image with an SSH server
`fletcher session ssh` runs `sshd` inside the VM. The image built by `make image`
includes it. The currently-published `ghcr.io/joshjon/fletcher-base:debian-13`
does **not** yet. For the SSH step, [build the image
locally](/advanced/runtimes#building-the-image-yourself) and import
`fletcher-base:dev`. `create` / `exec` / `shell` work with either image.
:::

## Stop, start, and disk persistence

```sh
fletcher session stop dev       # frees the VM's RAM; keeps its disk
fletcher session start dev      # back exactly where you left off
```

Stop **hibernates** the VM: it snapshots memory to disk and exits the VM process,
so a stopped session costs only disk, not RAM. Start restores it in well under a
second with its processes still running, including shell history and any agent
that was mid-run. If a snapshot can't be restored (e.g. after a Fletcher upgrade
changes the VMM), Fletcher cold-boots from the disk instead. You never lose the
workspace, only the in-memory state.

## Automatic cleanup: free RAM, never free disk

Fletcher reclaims RAM on its own but never deletes your disk:

- **Idle sessions auto-stop.** A running session with no work in flight is
  hibernated after `session_idle_timeout` (default 30m, set to `0` to disable).
  "Work in flight" means an active exec/shell/SSH or a busy VM. A running agent
  or build with nobody attached is **not** stopped mid-task.
- **Storage is capped, not pruned.** `session_max_count` (default 10) and
  `session_max_disk_gb` (default 50) bound how many sessions exist and how much
  disk they use. Hitting a cap **refuses a new session** with a list of what's
  using the space. It never deletes anything.

```sh
fletcher settings set session_idle_timeout 1h
fletcher settings set session_max_count 20
fletcher daemon restart                         # session settings apply on restart
```

## Delete

```sh
fletcher session delete dev     # stops the VM and destroys its fork (disk)
```

Deleting is the only thing that frees a session's disk, and it's irreversible.
Prune intentionally.

## Next

A session can serve a port, such as a dev server, an app, or an API. See
[Publishing ports](/guide/publishing) to reach it from your devices or the
public web.
