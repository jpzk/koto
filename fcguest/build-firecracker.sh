#!/bin/sh
# build-firecracker.sh — obtain the microVM monitor.
#
# Firecracker is the most privileged binary koto runs: it opens /dev/kvm and IS
# the boundary the trust model rests on. This fetches the pinned upstream
# release and VERIFIES IT against a checksum recorded here. It previously did
# neither — plain `curl` then `chmod +x`, with no SHA256 and no signature. TLS
# proves you reached GitHub; it does not prove you got the right bytes.
#
# Why not build from source? Attempted, and it needs more than a Rust image.
# Firecracker links libseccomp and runs bindgen, so a musl-static build (which
# is mandatory — see the ldd check below) needs musl-built C dependencies:
# Debian ships no musl libseccomp, and on Alpine bindgen cannot dlopen
# libclang because musl has no dynamic loading in a static build script.
# Upstream solves this with their own `devtool` build container — and pulling
# that image is the same trust position as pulling their release binary, so it
# buys nothing over verifying the checksum. The pin plus checksum is what
# closes the actual gap.
#
# Output: fcassets/firecracker (static musl binary, as upstream ships).
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
# KOTO_FCASSETS_OUT: see the note in build-rootfs.sh.
OUT="${KOTO_FCASSETS_OUT:-$HERE/fcassets}"
mkdir -p "$OUT"

# DELIBERATE EXCEPTION to the project's 6-week dependency-lag rule: track the
# LATEST Firecracker release, not one aged six weeks. The lag rule exists so a
# compromised or broken upstream is noticed before we adopt it, which is the
# right trade for an ordinary library. Firecracker is not one — it IS the
# isolation boundary, so running a six-week-old VMM after a security fix ships
# means deliberately running known-vulnerable boundary code. Promptness beats
# soak time for this one dependency.
#
# To bump: set the version, then take the checksum from upstream's OWN
# published file rather than from whatever you happened to download —
#   curl -fsSL "$FC_REPO_URL/$V/firecracker-$V-x86_64.tgz.sha256.txt"
FC_VERSION="${FC_VERSION:-v1.16.1}"   # latest release as of 2026-08-31
FC_SHA256="${FC_SHA256:-382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6}"
FC_REPO_URL="https://github.com/firecracker-microvm/firecracker/releases/download"
ARCH=$(uname -m)                      # x86_64

if [ -x "$OUT/firecracker" ] && [ "${FC_FORCE:-}" != "1" ]; then
  "$OUT/firecracker" --version | head -1
  exit 0
fi

TGZ="firecracker-$FC_VERSION-$ARCH.tgz"
echo "==> fetching firecracker $FC_VERSION"
curl -fsSL -o "$OUT/$TGZ" "$FC_REPO_URL/$FC_VERSION/$TGZ"

echo "==> verifying checksum"
GOT=$(sha256sum "$OUT/$TGZ" | cut -d' ' -f1)
if [ "$GOT" != "$FC_SHA256" ]; then
  rm -f "$OUT/$TGZ"
  echo "!! checksum mismatch for $TGZ" >&2
  echo "!!   expected $FC_SHA256" >&2
  echo "!!   got      $GOT" >&2
  echo "!! refusing to install an unverified VMM — this binary opens /dev/kvm" >&2
  exit 1
fi

tar -xzf "$OUT/$TGZ" -C "$OUT"
mv "$OUT/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH" "$OUT/firecracker"
chmod 755 "$OUT/firecracker"
rm -rf "$OUT/$TGZ" "$OUT/release-$FC_VERSION-$ARCH"

# Static is not optional: fcjail bind-mounts ONLY this binary into an
# otherwise empty per-VM chroot (fcjail.go), so there is no libc in there to
# link against. A dynamically linked VMM would run fine here and fail at the
# first group boot, from inside the jail, where the error is hard to read.
if command -v ldd >/dev/null 2>&1 && ldd "$OUT/firecracker" 2>&1 | grep -q "=>"; then
  echo "!! firecracker is dynamically linked; the jail chroot has no libc" >&2
  exit 1
fi

"$OUT/firecracker" --version | head -1
ls -lh "$OUT/firecracker"
echo "firecracker ready (sha256 verified)"
