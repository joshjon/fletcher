#!/bin/sh
# Fletcher installer. Fetches the latest release tarball, verifies its
# SHA256 against the published SHA256SUMS, drops the binary at
# /usr/local/bin/fletcher and the systemd unit at
# /etc/systemd/system/fletcher.service.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/joshjon/fletcher/main/scripts/install.sh | sudo sh
#
# Override the release tag with FLETCHER_VERSION=vX.Y.Z, or the install
# prefix with FLETCHER_PREFIX=/opt/fletcher.

set -eu

REPO=${FLETCHER_REPO:-joshjon/fletcher}
VERSION=${FLETCHER_VERSION:-latest}
PREFIX=${FLETCHER_PREFIX:-/usr/local}
UNIT_PATH=${FLETCHER_UNIT_PATH:-/etc/systemd/system/fletcher.service}

log() { printf '\033[36m== %s\033[0m\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
	command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

require curl
require tar
require shasum 2>/dev/null || require sha256sum
require install
# id is part of coreutils on every distro we target; missing means a
# minimal busybox container without it, in which case the user is in an
# unsupported environment anyway.
require id

[ "$(id -u)" -eq 0 ] || die "install.sh must run as root (try: sudo sh install.sh)"

case "$(uname -s)" in
	Linux) ;;
	*) die "fletcher only supports Linux today" ;;
esac

case "$(uname -m)" in
	x86_64|amd64) ARCH=amd64 ;;
	aarch64|arm64) ARCH=arm64 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
esac

if [ "$VERSION" = "latest" ]; then
	log "resolving latest release"
	VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
		| sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -n1)
	[ -n "$VERSION" ] || die "could not resolve latest release from GitHub API"
fi

TARBALL="fletcher_${VERSION}_linux_${ARCH}.tar.gz"
SUMS="SHA256SUMS"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

log "downloading $TARBALL"
curl -fsSL "${BASE_URL}/${TARBALL}" -o "${WORK}/${TARBALL}"
log "downloading checksums"
curl -fsSL "${BASE_URL}/${SUMS}" -o "${WORK}/${SUMS}"

log "verifying checksum"
cd "$WORK"
if command -v sha256sum >/dev/null 2>&1; then
	grep "  ${TARBALL}$" "$SUMS" | sha256sum -c -
else
	# macOS-style fallback (unlikely on Linux but defensive)
	expected=$(grep "  ${TARBALL}$" "$SUMS" | awk '{print $1}')
	actual=$(shasum -a 256 "$TARBALL" | awk '{print $1}')
	[ "$expected" = "$actual" ] || die "checksum mismatch for $TARBALL"
fi
cd - >/dev/null

log "extracting"
tar -xzf "${WORK}/${TARBALL}" -C "${WORK}"

log "installing binary to ${PREFIX}/bin/fletcher"
install -m 0755 "${WORK}/fletcher" "${PREFIX}/bin/fletcher"

if [ -f "${WORK}/init/fletcher.service" ]; then
	log "installing systemd unit to ${UNIT_PATH}"
	install -m 0644 "${WORK}/init/fletcher.service" "$UNIT_PATH"
fi

# Create the system user the unit runs as.
if ! id -u fletcher >/dev/null 2>&1; then
	log "creating fletcher system user"
	useradd --system --home-dir /var/lib/fletcher --shell /usr/sbin/nologin fletcher \
		|| adduser --system --home /var/lib/fletcher --no-create-home --group fletcher
fi

# State / config dirs (systemd would create these on first start, but
# pre-creating them lets the operator drop in an age key before starting).
install -d -m 0700 -o fletcher -g fletcher /var/lib/fletcher /etc/fletcher

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload
	# On upgrade the service is already running - restart so the new
	# binary + unit settings take effect. On first install the service
	# isn't enabled yet; print the enable hint instead.
	if systemctl is-active --quiet fletcher; then
		log "fletcher is running; restarting to pick up the new binary"
		systemctl restart fletcher
	else
		log "installed. start with: sudo systemctl enable --now fletcher"
	fi
else
	log "installed. start with: ${PREFIX}/bin/fletcher serve"
fi
