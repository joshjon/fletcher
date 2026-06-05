# check=skip=SecretsUsedInArgOrEnv
# The *_API_KEY values below are intentional placeholders, not real
# credentials. The daemon stamps the actual key on outbound model calls
# (see DESIGN.md §6); the placeholders just keep agent CLIs happy when
# they sanity-check that an API key env var is set.

# fletcher-base - base image for Fletcher agent forks.
#
# Ships the three first-class agent CLIs (`claude`, `codex`, `pi`) plus
# the daemon contract (well-known env vars, /workspace mount point,
# pre-created credential dirs Phase 12 bind-mounts target). Built via
# `make image` (or pulled from ghcr.io/<owner>/fletcher-base) and flattened
# by `fletcher image import` into a btrfs subvolume (runc) or an ext4 image
# (Firecracker) - the self-built pipeline per DESIGN.md §11.
#
# Reference design: exe.dev's "exeuntu". Reference patterns adopted:
# single AGENTS.md symlinked into each agent's expected config path;
# unprivileged user installs agent CLIs into ~/.local/bin; no baked SSH
# host keys; x-systemd.growfs in /etc/fstab. Off-thesis bits skipped:
# Tailscale (hosted control plane), Chrome/Docker/ffmpeg/imagemagick
# (heavy, push to per-job overlays).

FROM debian:13-slim

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

ARG DEBIAN_FRONTEND=noninteractive

# Base packages: dev essentials, systemd, wireguard-tools.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl wget gnupg \
        git gh jq ripgrep vim less \
        build-essential pkg-config \
        python3 python3-pip python-is-python3 pipx \
        sqlite3 \
        sudo openssh-client \
        iproute2 iputils-ping netcat-openbsd \
        unzip rsync file tree \
        systemd systemd-sysv \
        wireguard-tools \
    && rm -rf /var/lib/apt/lists/*

# Never bake SSH host keys; the VM generates its own on first boot.
RUN rm -f /etc/ssh/ssh_host_*_key*

# Latest Go from go.dev (apt's golang-go lags behind).
RUN ARCH=$(dpkg --print-architecture) && \
    GO_VERSION=$(curl -fsSL 'https://go.dev/dl/?mode=json' | jq -r '.[0].version') && \
    curl -fsSL "https://go.dev/dl/${GO_VERSION}.linux-${ARCH}.tar.gz" | tar -xzC /usr/local && \
    ln -s /usr/local/go/bin/go /usr/local/bin/go && \
    ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# Node.js 22 for npm-distributed agent tooling.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get install -y --no-install-recommends nodejs && \
    rm -rf /var/lib/apt/lists/*

# uv (Python package manager) into /usr/local/bin.
RUN curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh

# Mask systemd units that don't make sense inside a Firecracker microVM.
RUN systemctl mask -- \
        getty.target \
        console-getty.service \
        getty@.service \
        systemd-random-seed.service \
        systemd-modules-load.service \
        systemd-machine-id-commit.service \
        apt-daily.timer apt-daily-upgrade.timer \
        man-db.timer \
        e2scrub_all.timer \
        fstrim.timer \
    && systemctl set-default multi-user.target

# `fletcher` user (UID 1000), NOPASSWD sudo, linger enabled.
RUN useradd --create-home --shell /bin/bash --uid 1000 fletcher && \
    echo 'fletcher ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/fletcher && \
    chmod 0440 /etc/sudoers.d/fletcher && \
    mkdir -p /var/lib/systemd/linger && \
    touch /var/lib/systemd/linger/fletcher

# Rootfs auto-grows on first boot.
RUN echo '/dev/vda / ext4 defaults,x-systemd.growfs 0 1' > /etc/fstab

# Workspace mount point and agent config dirs (Phase 12 bind-mount targets).
RUN mkdir -p /workspace \
            /home/fletcher/.claude \
            /home/fletcher/.codex \
            /home/fletcher/.pi \
            /home/fletcher/.config/gemini && \
    chown -R fletcher:fletcher /workspace /home/fletcher

# Single AGENTS.md, symlinked into each agent's expected location.
COPY --chown=fletcher:fletcher AGENTS.md /home/fletcher/AGENTS.md
RUN ln -s /home/fletcher/AGENTS.md /home/fletcher/.claude/CLAUDE.md && \
    ln -s /home/fletcher/AGENTS.md /home/fletcher/.codex/AGENTS.md && \
    ln -s /home/fletcher/AGENTS.md /home/fletcher/.pi/AGENTS.md

# Install agent CLIs as the unprivileged user (their upstream installers
# expect to write to $HOME/.local/bin).
USER fletcher
ENV HOME=/home/fletcher \
    USER=fletcher \
    PATH=/home/fletcher/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin

RUN mkdir -p /home/fletcher/.local/bin

# claude (Anthropic) - recommended for users on a Claude Max subscription
# (combine with Phase 12 trusted-credential mode).
RUN curl -fsSL https://claude.ai/install.sh | bash && \
    test -x /home/fletcher/.local/bin/claude

# codex (OpenAI) - recommended for users on a ChatGPT Plus/Pro subscription.
RUN curl -fsSL https://chatgpt.com/codex/install.sh | CODEX_NON_INTERACTIVE=1 sh && \
    test -x /home/fletcher/.local/bin/codex

# pi (Earendil) - recommended default for users without a strong agent
# preference. Its extensions system is the integration target for the
# Fletcher pi-extension (Phase 14).
RUN curl -fsSL https://pi.dev/install.sh | sh && \
    test -x /home/fletcher/.local/bin/pi

USER root
RUN ln -sf /home/fletcher/.local/bin/claude /usr/local/bin/claude && \
    ln -sf /home/fletcher/.local/bin/codex /usr/local/bin/codex && \
    ln -sf /home/fletcher/.local/bin/pi /usr/local/bin/pi

# Fletcher pi-extension (Phase 14): fetches the daemon's model catalog on
# pi startup so users see provider/model lists without per-job env-var
# fiddling. Pattern mirrors exe.dev's `exe-dev` pi-extension.
COPY --chown=fletcher:fletcher pi-extension/ /home/fletcher/.pi/agent/extensions/fletcher/

# Daemon contract baked into env defaults. The daemon overrides these
# per-job at runtime with the actual listen addresses; values here are
# sensible fallbacks so an agent invoked directly inside the image
# points somewhere predictable.
ENV ANTHROPIC_BASE_URL="http://daemon-gateway.fletcher.internal/v1" \
    ANTHROPIC_API_KEY="fletcher-gateway" \
    OPENAI_BASE_URL="http://daemon-gateway.fletcher.internal/v1" \
    OPENAI_API_KEY="fletcher-gateway" \
    FLETCHER_MCP_URL="http://daemon-mcp.fletcher.internal" \
    FLETCHER_CATALOG_URL="http://daemon-gateway.fletcher.internal/v1/catalog.json" \
    FLETCHER_WORKSPACE="/workspace" \
    FLETCHER_BASE=1

LABEL org.opencontainers.image.title="fletcher-base" \
      org.opencontainers.image.description="Base image for Fletcher agent forks" \
      org.opencontainers.image.source="https://github.com/joshjon/fletcher" \
      io.fletcher.login-user="fletcher"

WORKDIR /workspace
USER fletcher
CMD ["/bin/bash"]
