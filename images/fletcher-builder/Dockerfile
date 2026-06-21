# Fletcher builder image - a sandboxed, daemonless Docker-image builder.
#
# M19 (session-native build + deploy): an ephemeral build fork boots from this
# image, builds a project's Dockerfile with buildah (no Docker daemon, no host
# Docker), and exports a flattened rootfs the daemon turns into a deployable
# template. Kept separate from fletcher-base so the agent/session image is not
# bloated with a build toolchain.
FROM debian:13-slim
ARG DEBIAN_FRONTEND=noninteractive

# buildah builds the image; crun runs the Dockerfile's RUN steps; netavark +
# aardvark-dns are the container-network helpers buildah requires to exist even
# to create a build container (the rest are runtime deps, ca-certs for registry
# TLS). RUN steps use --network host (see the build script) so they share the
# fork's network and reach the daemon egress proxy; the fork has no NIC of its
# own, so a netavark bridge would have nowhere to route.
RUN apt-get update && apt-get install -y --no-install-recommends \
        buildah crun netavark aardvark-dns fuse-overlayfs ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Storage driver "vfs": no overlayfs / loop devices, so the build works in a
# microVM guest kernel without needing overlay support - it just copies layers
# (slower, but dependency-free and reliable). buildah runs as root here (the
# guest's PID 1 is root), so no user-namespace plumbing is needed. runroot and
# graphroot must be set explicitly - buildah errors with "runroot must be set"
# if storage.conf names a driver but leaves these empty.
RUN mkdir -p /etc/containers \
    && printf '[storage]\ndriver = "vfs"\nrunroot = "/run/containers/storage"\ngraphroot = "/var/lib/containers/storage"\n' \
        > /etc/containers/storage.conf

# Resolve unqualified base images (e.g. `FROM alpine`) against Docker Hub, and
# allow the common registries. containers-common ships a policy.json that trusts
# anything; that is fine for a local build box.
RUN printf 'unqualified-search-registries = ["docker.io"]\n' > /etc/containers/registries.conf

# A tiny baked-in context so the build toolchain can be verified in a fork
# (`buildah --storage-driver vfs bud -t probe /opt/build-probe`) without needing
# any input wired in yet.
RUN mkdir -p /opt/build-probe \
    && printf 'FROM alpine:3.19\nRUN echo "fletcher builder ok" > /built.txt\nCMD ["cat","/built.txt"]\n' \
        > /opt/build-probe/Dockerfile

WORKDIR /workspace
