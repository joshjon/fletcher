# Installation

Fletcher installs with a single command on any Linux machine you control
(Debian/Ubuntu, Fedora, Arch, or similar), on amd64 or arm64.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh
```

The installer:

- downloads the latest release tarball and verifies its SHA256,
- drops the binary at `/usr/local/bin/fletcher`,
- installs the systemd unit,
- creates the unprivileged `fletcher` system user,
- pre-creates the state directories.

The same command works for first install and upgrade - the script detects
whether the service is already running and restarts it on upgrade
automatically.

Confirm it landed:

```sh
fletcher version
```

::: warning Pre-release
Until the first release is tagged, the installer's default download path can't
resolve a release from GitHub and will error. To try Fletcher today, [build from
source](/advanced/building) - the same `make install` path sets up the service
from your local build.
:::

::: tip Installing the client on macOS
The daemon is Linux-only, but the `fletcher` CLI runs anywhere. The same install
command on macOS detects the platform and installs the **client only** - no
daemon. Use it to drive a remote daemon from your laptop; see
[Remote control](/guide/remote).
:::

## Building from source

If you're on an architecture the release tarballs don't cover, or you want to
track `main`, build it yourself. See [Building from source](/advanced/building).

## What's next

The daemon isn't running yet - you start it as part of choosing how you'll reach
it. Continue to [Networking & first run](/guide/networking).
