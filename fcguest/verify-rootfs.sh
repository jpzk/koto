#!/bin/sh
# Verify the pi + npm contract against the image that will be exported into the
# Firecracker rootfs. Keep expected values independent of Docker build args so
# an accidental pin change fails verification instead of changing the oracle.
set -eu

CONTAINER=${CONTAINER:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}
IMAGE=${1:-koto-fcrootfs}
[ -n "$CONTAINER" ] || { echo "podman or docker is required to verify the rootfs"; exit 1; }

PI_VERSION=0.86.0
PI_LOCK_SHA256=510555c39ab4734446f6984762c9de67fd1fcd3400d3165a6da04251bad85e6b
PI_UNDICI=8.10.2
PI_BRACE=5.0.12
NPM_VERSION=11.21.1
NPM_TAR=7.5.22

"$CONTAINER" run --rm --entrypoint /bin/sh \
  -e EXPECTED_PI_VERSION="$PI_VERSION" \
  -e EXPECTED_PI_LOCK_SHA256="$PI_LOCK_SHA256" \
  -e EXPECTED_PI_UNDICI="$PI_UNDICI" \
  -e EXPECTED_PI_BRACE="$PI_BRACE" \
  -e EXPECTED_NPM_VERSION="$NPM_VERSION" \
  -e EXPECTED_NPM_TAR="$NPM_TAR" \
  "$IMAGE" -eu -c '
    test "$(pi --version)" = "$EXPECTED_PI_VERSION"
    test "$(jq -r .version /opt/pi/package.json)" = "$EXPECTED_PI_VERSION"
    a=/opt/pi/node_modules/@earendil-works/pi-coding-agent
    test "$(jq -r .version "$a/package.json")" = "$EXPECTED_PI_VERSION"
    test "$(readlink -f /usr/local/bin/pi)" = "$a/dist/bundle/cli.js"
    test "$(cut -d " " -f 1 /opt/pi/.artifact.sha256)" = "$EXPECTED_PI_LOCK_SHA256"
    test "$(cut -d " " -f 3 /opt/pi/.artifact.sha256)" = package-lock.json
    echo "$EXPECTED_PI_LOCK_SHA256  /opt/pi/package-lock.json" | sha256sum -c - >/dev/null
    for f in $(find /opt/pi -path "*/node_modules/undici/package.json"); do
      test "$(jq -r .version "$f")" = "$EXPECTED_PI_UNDICI"
    done
    for f in $(find /opt/pi -path "*/node_modules/brace-expansion/package.json"); do
      test "$(jq -r .version "$f")" = "$EXPECTED_PI_BRACE"
    done
    test "$(npm --version)" = "$EXPECTED_NPM_VERSION"
    test "$(npm root -g)" = /usr/local/lib/node_modules
    n="$(dirname "$(dirname "$(readlink -f "$(command -v npm)")")")"
    test "$(jq -r .version "$n/node_modules/tar/package.json")" = "$EXPECTED_NPM_TAR"
    test "$PI_OFFLINE" = 1
    test "$PI_TELEMETRY" = 0
    test "$PI_SKIP_VERSION_CHECK" = 1
    m=/usr/local/share/koto/pi/models.minimax.json
    test "$(jq -r .providers.minimax.baseUrl "$m")" = http://127.0.0.1:18888/anthropic
    test "$(jq -r .providers.minimax.apiKey "$m")" = proxied
    test "$(jq -r ".providers|keys[]" "$m")" = minimax
  '

echo "verified pi $PI_VERSION (lock $PI_LOCK_SHA256, undici $PI_UNDICI, brace-expansion $PI_BRACE), npm $NPM_VERSION (tar $NPM_TAR), minimax pi template and offline runtime environment"
