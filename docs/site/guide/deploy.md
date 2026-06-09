# Deploying apps

`fletcher deploy` is the one-command path from a Docker image to a running,
published app on your own box. It builds (or pulls) the image, runs it as a
session that boots the image's own start command, and publishes its port.

```sh
# A public image, served on your domain over HTTPS
# (port taken from the image's EXPOSE):
fletcher deploy nginx:alpine --host app.example.com

# Tunnel-only (no domain needed), reachable from your paired devices:
fletcher deploy nginx:alpine --name web

# A private registry image (basic auth on the pull):
fletcher deploy ghcr.io/you/app:v1 --registry-auth you:TOKEN --host app.example.com

# A local project with a Dockerfile (builds on this box, needs root + docker):
sudo fletcher deploy ./myapp --host app.example.com
```

## The daemon does the pull

For a **registry image, the daemon does the pull and flatten itself**, so
`deploy` (and `fletcher image pull <ref>`) work from a laptop over the tunnel
with **no local Docker**. Your code never leaves your network to be deployed onto
it. Building from a local Dockerfile is the one host-side case, because it needs
the working directory, so it runs on the box.

## A deployment is durable

A deployed app restarts if it crashes and comes back on its own after a reboot.
Manage it like any session:

```sh
fletcher session logs <name>     # the app's output
fletcher session get <name>      # state, port, public URL
fletcher session delete <name>   # stop and remove
```

## Notes

- `--public` / `--host` need `public_web` enabled. See [Public web over
  HTTPS](/advanced/public-web).
- The app runs as the image's user (root unless the image sets one).
- Want a private registry of your own? Run one in a session and `deploy` from it.
  To Fletcher it's just a registry it pulls, no special setup.
