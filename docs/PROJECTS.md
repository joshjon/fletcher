# Saved projects

A saved project records a name, an existing development VM, and an absolute
working directory inside that VM. It is a shortcut to durable compute, not a new
execution mode or a directory-level security boundary (DESIGN sections 4 and 5).

## Register and open

Assuming a development session named `dev` already contains `/workspace/app`:

```sh
fletcher project create --name app --session dev --workdir /workspace/app
fletcher project open app
fletcher project open app --terminal build
```

Registration does not boot a VM, copy files, run setup, or contact a model
provider. The default directory is `/workspace`. App deployments are not valid
project targets.

Open starts the recorded VM if needed, verifies that the directory exists, and
attaches a named tmux terminal. Repeating `open app` reattaches that project's
`main` terminal. A different label, such as `build`, opens another terminal in
the same VM. It does not create another VM or fork, rerun setup, or restart a
program that is already running in that terminal.

A terminal label is stable across project renames. Its working directory is an
initial setting: changing a registration does not move an existing terminal's
shell or restart its processes. Use a new label when you want a new terminal in
the updated directory.

## Manage registrations

```sh
fletcher project list
fletcher project get app -o json
fletcher project update app --name api --workdir /workspace/api
fletcher project delete api --yes
```

Deleting a registration leaves its VM, files and processes untouched. Deleting
the underlying session removes its registrations. A missing directory causes
Open to fail; Fletcher does not silently create it or switch to another path.

The lower-level shell command also accepts explicit targets:

```sh
fletcher session shell dev --terminal scratch --workdir /workspace/app
```

Without these flags, existing clients continue to use the VM's default `main`
terminal. Project terminals have separate, project-ID-prefixed names, so they
do not silently attach that legacy terminal or another project's default shell.

## Security and persistence

All terminals and programs in one VM share its filesystem, credentials,
network policy and security boundary. Separate terminal labels are not separate
sandboxes. Use separate VMs for independent isolation, and separate working
trees when concurrent edits should not share a checkout.

Closing a client leaves a tmux terminal running while the VM is running. A VM
stop or reboot interrupts processes; the durable disk survives, but tmux and
agent process state is not promised across a cold boot.

Named terminals require an updated guest and tmux. An older guest rejects the
request without falling back to another terminal. A cold restart with the
updated daemon refreshes the guest init on the existing disk, subject to the
existing best-effort refresh behavior. Hibernation restore is not a cold restart.
Check the daemon's init-refresh warning if the guest remains outdated; do not
redeploy or replace a development disk merely to update terminal support.

## Implementation status

The daemon, persistence schema, Connect API and CLI support saved projects and
named terminals. The native client also implements registration, navigation,
editing, removal, Open and New terminal. Its tmux bridge discovers the attached
pane rather than assuming the server's first pane. Swift tests and Apple builds
remain unexecuted on the Linux implementation host; real-VM and device
verification is still required.

Follow-on work:

- Import a selected host folder as a copy into the guest. Leave the source
  unchanged and return changes through explicit review/export. Reject unsafe
  archive paths and symlink escapes, apply size limits, and expose progress and
  cancellation.
- Reuse prepared images containing tools and non-secret configuration. Do not
  bundle live login credentials. Rootfs images and separately mounted volumes
  have distinct lifetimes; image preparation must make that scope visible.

Host execution and live host-directory mounts are not part of these flows.
