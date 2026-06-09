# Your first agent

This is the payoff. An agent running inside a hardware-isolated microVM on your
box, reaching a model only through the daemon's gateway, with no other network
egress.

## Check your runtime

On a host with KVM, the daemon defaults to the **Firecracker** runtime, so each
job boots its own microVM. Confirm what you have:

```sh
fletcher doctor
```

The doctor checks `/dev/kvm`, the bundled VMM, networking, and your model
providers, then prints an action plan for anything missing. If you don't have
KVM, see [Runtimes & base images](/advanced/runtimes) for the runc and mock
fallbacks.

## 1. Import a base image

An agent needs a base-image rootfs to boot from. Pull the published image and
import it as an ext4 rootfs (no local build needed):

```sh
sudo fletcher image import ghcr.io/joshjon/fletcher-base:debian-13 \
  --format ext4 \
  --btrfs-root /var/lib/fletcher/snapshots \
  --name fletcher-base
```

The rootfs is roughly 3 GB, so make sure the snapshot root has a few GB free. On
btrfs the per-job clones are reflinks. The importer injects the microVM init for
you.

## 2. Give the daemon a model key

The gateway needs a key so it can reach models on the agent's behalf. The key
lives with the daemon and never enters a VM.

```sh
fletcher secret set anthropic_api_key sk-ant-...
```

## 3. Run the agent

```sh
fletcher job create --command "claude -p 'say hi'"
```

That's the whole command. `--image` defaults to the `default_image` setting
(`fletcher-base` out of the box) and `--name` defaults to the command's program
name (here, `claude`), so only `--command` is required. Pass `--image` /
`--name` to override.

The agent runs inside the microVM, reaches Anthropic **only** through the daemon
gateway (the key never enters the VM), and has no other network egress. Browse
what the gateway can route to with `fletcher model list`.

## What just happened

- The daemon cloned a copy-on-write fork of `fletcher-base` and booted it as a
  Firecracker microVM.
- The agent process ran inside that VM with no credentials and no NIC, just a
  vsock channel back to the daemon.
- When the agent called its model, the request went over vsock to the daemon's
  gateway, which attached your key and forwarded it. The fork was torn down when
  the job finished.

Every command has `--help` with more detail.

## Where to go next

- Run work on a schedule, or keep a workspace alive:
  **[Jobs & cron](/guide/jobs)** and **[Durable sessions](/guide/sessions)**.
- Drive all of this from your laptop over the tunnel:
  **[Remote control](/guide/remote)**.
- Keep your base image current: **[Runtimes & base
  images](/advanced/runtimes)**.
