# Building from source

For developers, early testers, or anyone running on an architecture the release
tarballs don't cover.

## Requirements

Go 1.26+ is required (Fletcher uses the `tool` directive). All other build-time
tools are pinned in `go.mod` and reachable via `go tool <name>`.

## Clone and build

```sh
git clone https://github.com/joshjon/fletcher.git
cd fletcher

make build          # local platform binary at ./bin/fletcher
make build-linux    # cross-compile amd64 + arm64 Linux artefacts
make check          # lint + tests + generated-file drift check
```

## Install on a Linux server

This mirrors what the install script does, using your local build. It creates the
user, installs the binary and unit, and reloads + restarts the service if it's
running:

```sh
make install
```

It's the same command for first install and upgrade, so iterating on a deployed
server is:

```sh
git pull
make install
```

## Build the base image

To build the agent base image locally (rather than pulling the published one):

```sh
make image      # builds fletcher-base:dev
```

Then import it, as in [Runtimes & base
images](/advanced/runtimes#building-the-image-yourself). The locally-built image
includes an SSH server, which the published image does not yet.
