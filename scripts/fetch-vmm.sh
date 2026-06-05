#!/usr/bin/env sh
# Download the Firecracker VMM binary and a guest kernel for each supported
# architecture into the embed asset tree. The files are gitignored; the build
# embeds whichever arch it targets and extracts them on first run.
#
# Usage: scripts/fetch-vmm.sh <fc-version> <kernel-name> <kernel-ci> <dest-root>
#   e.g. scripts/fetch-vmm.sh v1.16.0 vmlinux-5.10.225 v1.11 \
#          internal/runtime/firecrackerdriver/vmm/assets
#
# Re-run after bumping the pinned versions (see the Makefile).
set -eu

FC_VERSION="${1:?firecracker version, e.g. v1.16.0}"
KERNEL="${2:?guest kernel name, e.g. vmlinux-5.10.225}"
KERNEL_CI="${3:?kernel CI channel, e.g. v1.11}"
DEST_ROOT="${4:?destination asset root}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fetch_arch() {
  goarch="$1" # amd64 | arm64   (directory name, matches runtime.GOARCH)
  fcarch="$2" # x86_64 | aarch64 (firecracker / kernel artifact naming)
  dest="$DEST_ROOT/$goarch"
  mkdir -p "$dest"

  echo ">> firecracker $FC_VERSION ($fcarch)"
  base="https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION"
  curl -fsSL -o "$TMP/fc.tgz" "$base/firecracker-$FC_VERSION-$fcarch.tgz"
  curl -fsSL -o "$TMP/fc.sha" "$base/firecracker-$FC_VERSION-$fcarch.tgz.sha256.txt"
  ( cd "$TMP" && awk '{print $1"  fc.tgz"}' fc.sha | sha256sum -c - >/dev/null )
  tar -xzf "$TMP/fc.tgz" -C "$TMP"
  install -m 0755 "$TMP/release-$FC_VERSION-$fcarch/firecracker-$FC_VERSION-$fcarch" "$dest/firecracker"
  rm -f "$TMP/fc.tgz" "$TMP/fc.sha"
  rm -rf "$TMP/release-$FC_VERSION-$fcarch"

  echo ">> guest kernel $KERNEL ($fcarch)"
  curl -fsSL -o "$dest/vmlinux" \
    "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/$KERNEL_CI/$fcarch/$KERNEL"

  echo "   -> $dest/firecracker, $dest/vmlinux"
}

fetch_arch amd64 x86_64
fetch_arch arm64 aarch64
echo "done."
