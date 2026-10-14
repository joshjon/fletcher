#!/usr/bin/env bash
# Exercise the public-app HTTP boundary through real nginx in a disposable Linux container.
# No host ports, Docker socket, credentials, guest disks or network access are exposed.
set -euo pipefail

cd "$(dirname "$0")/.."
image="${FLETCHER_TEST_NGINX_IMAGE:-nginx@sha256:8adbdcb969e2676478ee2c7ad333956f0c8e0e4c5a7463f4611d7a2e7a7ff5dc}"
if ! docker image inspect "$image" >/dev/null 2>&1; then
    docker pull "$image"
fi
arch="$(docker image inspect "$image" --format '{{.Architecture}}')"
case "$arch" in
    amd64|arm64) ;;
    *) echo "unsupported test image architecture: $arch" >&2; exit 1 ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
chmod 755 "$tmp"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -trimpath -tags=integration -o "$tmp/session.test" ./internal/session

docker run --rm --network none --read-only --user 65534:65534 \
    --cap-drop ALL --security-opt no-new-privileges \
    --tmpfs /tmp:rw,nosuid,noexec,size=64m \
    --mount "type=bind,source=$tmp,target=/tests,readonly" \
    --env FLETCHER_TEST_NGINX=/usr/sbin/nginx \
    --entrypoint /tests/session.test "$image" \
    -test.run '^TestPublicAppsBehindNGINX$' -test.v -test.timeout 60s
