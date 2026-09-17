# Runtimes & base images

Fletcher runs persistent sessions and app deployments in Firecracker microVMs.
Jobs also support other runtimes. The isolation available depends on the host
and the `runtime` setting.

## The three runtimes

| Runtime | Isolation | When it's used |
|---|---|---|
| **Firecracker** | Hardware-isolated microVM, daemon-mediated networking | Default on a host with `/dev/kvm` |
| **runc** | Shared-kernel container (labeled degraded isolation) | Explicit fallback: `fletcher settings set runtime runc` |
| **mock** | None, runs as a plain subprocess | Automatic fallback when there's no `/dev/kvm` |

On a KVM host the daemon defaults to **Firecracker**. Each environment boots a
microVM with its own kernel and a copy-on-write ext4 rootfs. The VM has no NIC
and communicates with the daemon over vsock. Published ports and outbound
requests are brokered by the daemon. Model-gateway use is optional.

Without `/dev/kvm`, the daemon falls back to the **mock** runtime so you can
still exercise the workflow. `fletcher job create --command "echo hi"` runs as a
plain subprocess. **runc** is available as an explicit, shared-kernel fallback
for jobs that need shared-kernel isolation without KVM. Neither fallback
provides persistent VM sessions or app deployments.

Confirm what you have:

```sh
fletcher doctor      # checks /dev/kvm and the bundled VMM
```

## Importing the base image

Both Firecracker and sessions need a base-image rootfs. Pull the published image
and import it (no local build needed):

```sh
sudo fletcher image import ghcr.io/joshjon/fletcher-base:debian-13 \
  --format ext4 \
  --btrfs-root /var/lib/fletcher/snapshots \
  --name fletcher-base
```

The rootfs is roughly 3 GB, so make sure the snapshot root has a few GB free. On
btrfs the per-job clones are reflinks. The importer injects the microVM init for
you.

### Keeping it current

The imported template is **pinned**, so it doesn't change underneath you. When
the registry has a newer build (e.g. a rebuilt rootfs with package updates), the
daemon notices on its next start and `fletcher doctor` shows a "newer version is
available" note. Re-pull and re-import when you want it:

```sh
sudo fletcher image update
```

Existing jobs and sessions keep their already-cloned forks, and new ones pick up
the updated template. `image update` re-imports the `default_image`. Pass a
template name to update a different one.

### Building the image yourself

You can build the base image locally instead of pulling it:

```sh
make image                              # builds fletcher-base:dev
sudo fletcher image import fletcher-base:dev --format ext4 \
  --btrfs-root /var/lib/fletcher/snapshots --name fletcher-base
```

The locally-built image includes an SSH server, which the currently-published
image does not yet. Build locally if you need [`fletcher session
ssh`](/guide/sessions#ssh-and-ide-attach).

## Using the runc fallback

To run on runc instead of Firecracker, set the runtime and snapshot backend,
provision a btrfs snapshot root, and import with `--format subvolume` (the
default):

```sh
fletcher settings set runtime runc
fletcher settings set snapshot btrfs
fletcher daemon restart
```

The trust properties are the same (models reached only through the gateway, no
ambient egress), but the isolation is a shared-kernel container rather than a VM.
Sessions require Firecracker and don't run on runc.
