#!/bin/sh
# Verify the pi contract against the image that will be exported into the
# Firecracker rootfs. Keep expected values independent of Docker build args so
# an accidental pin change fails verification instead of changing the oracle.
set -eu

CONTAINER=${CONTAINER:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}
IMAGE=${1:-koto-fcrootfs}
[ -n "$CONTAINER" ] || { echo "podman or docker is required to verify the rootfs"; exit 1; }

PI_VERSION=0.84.4
PI_SHA256=c2f3c3e6a1850bd87654cc3ca8811013272397c3d042a4e2a64c43ee1b423972

"$CONTAINER" run --rm --entrypoint /bin/sh \
  -e EXPECTED_PI_VERSION="$PI_VERSION" \
  -e EXPECTED_PI_SHA256="$PI_SHA256" \
  "$IMAGE" -eu -c '
    test "$(pi --version)" = "$EXPECTED_PI_VERSION"
    test "$(jq -r .version /opt/pi/package.json)" = "$EXPECTED_PI_VERSION"
    test "$(readlink -f /usr/local/bin/pi)" = /opt/pi/pi
    test "$(cut -d " " -f 1 /opt/pi/.artifact.sha256)" = "$EXPECTED_PI_SHA256"
    test "$(cut -d " " -f 3 /opt/pi/.artifact.sha256)" = pi-linux-x64.tar.gz
    test "$PI_OFFLINE" = 1
    test "$PI_TELEMETRY" = 0
    test "$PI_SKIP_VERSION_CHECK" = 1
  '

echo "verified pi $PI_VERSION ($PI_SHA256) and offline runtime environment"
