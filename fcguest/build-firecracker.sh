#!/bin/sh
# build-firecracker.sh — obtain the microVM monitor.
#
# Firecracker is the most privileged binary koto runs: it opens /dev/kvm and IS
# the boundary the trust model rests on. This fetches the pinned upstream
# release and VERIFIES IT against a checksum recorded here. It previously did
# neither — plain `curl` then `chmod +x`, with no SHA256 and no signature. TLS
# proves you reached GitHub; it does not prove you got the right bytes.
#
# FC_FROM_SOURCE=1 builds it instead, from a pinned commit inside upstream's
# own build container. That is opt-in rather than the default deliberately:
#
#   - The checksum above already closes the INTEGRITY gap, which is the thing
#     that was actually broken. Building from source does not improve on a
#     verified artifact for that.
#   - What it does buy is SOVEREIGNTY: source you can read, the ability to
#     carry a patch, and independence from a release artifact staying up.
#   - It does not shrink the trust set. You swap "trust one 3.4MB signed
#     binary" for "trust a multi-GB CI image plus the source", and a
#     compromised toolchain can inject into output that reads as clean.
#   - Practically: multi-GB image, ~10-20 minute build, and a second pin to
#     keep current. Making it the default would let an upstream toolchain
#     break block every install.
#
# The container is needed because a plain Rust image cannot do it: Firecracker
# links libseccomp and runs bindgen, so a musl-static build (mandatory — see
# the ldd check below) needs musl-built C deps. Debian ships no musl
# libseccomp; on Alpine bindgen cannot dlopen libclang, musl having no dynamic
# loading in a static build script. fcuvm has both.
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

# --- source build (FC_FROM_SOURCE=1) ---------------------------------------
# The COMMIT is the real pin: a tag can be moved, a commit cannot, so the build
# verifies HEAD and refuses on a mismatch. Dereference the ANNOTATED tag when
# refreshing it — ls-remote on the bare ref gives the tag OBJECT, not the
# commit, and pinning that fails the check every time:
#   git ls-remote "$FC_GIT" "refs/tags/vX.Y.Z^{}"
FC_COMMIT="${FC_COMMIT:-2038188f145fb81b8d098147a10e9d9f392fd22f}"
FC_GIT="${FC_GIT:-https://github.com/firecracker-microvm/firecracker}"
# Upstream's build container, pinned by DIGEST. The tag tracks the Firecracker
# release: tools/devtool in v1.16.1 pins DEVCTR_IMAGE_TAG=v90, so bump both
# together. We invoke cargo in it directly rather than running devtool, which
# is itself a docker wrapper and would nest engines for no gain.
DEVCTR="${DEVCTR:-public.ecr.aws/firecracker/fcuvm@sha256:a716905776133b78c72c1992a79d346a1088ce2981e206b960b1912522423042}"
FC_TARGET=x86_64-unknown-linux-musl

if [ -x "$OUT/firecracker" ] && [ "${FC_FORCE:-}" != "1" ]; then
  "$OUT/firecracker" --version | head -1
  exit 0
fi

if [ "${FC_FROM_SOURCE:-}" = "1" ]; then
  CONTAINER="${CONTAINER:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}"
  [ -n "$CONTAINER" ] || { echo "podman or docker is required for FC_FROM_SOURCE"; exit 1; }
  # Rootless engines map container-root to us, so the artifact lands owned by
  # the invoking user and the correct chown target is 0:0. Chowning to our real
  # uid there would map it into the SUBUID range and leave a file we cannot
  # read — the mistake build-rootfs.sh made first.
  if [ "$("$CONTAINER" info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" = "true" ]; then
    CHOWN_TO="0:0"
  else
    CHOWN_TO="$(id -u):$(id -g)"
  fi
  echo "==> building firecracker $FC_VERSION from source  [$(basename "$CONTAINER"), fcuvm]"
  mkdir -p "$HERE/.cargocache"
  "$CONTAINER" run --rm --security-opt label=disable \
    -v "$OUT:/out" -v "$HERE/.cargocache:/cargo" \
    -e CARGO_HOME=/cargo -e CARGO_TARGET_DIR=/target \
    -e FC_VERSION="$FC_VERSION" -e FC_COMMIT="$FC_COMMIT" -e FC_GIT="$FC_GIT" \
    -e FC_TARGET="$FC_TARGET" -e CHOWN_TO="$CHOWN_TO" \
    "$DEVCTR" sh -eu -c '
      git clone --depth 1 --branch "$FC_VERSION" "$FC_GIT" /src 2>/dev/null
      cd /src
      HEAD=$(git rev-parse HEAD)
      if [ "$HEAD" != "$FC_COMMIT" ]; then
        echo "!! $FC_VERSION resolves to $HEAD, expected $FC_COMMIT" >&2
        echo "!! the tag moved, or this is not the repo we think it is" >&2
        exit 1
      fi
      # CARGO_TARGET_DIR is set explicitly because the upstream .cargo/config
      # redirects output to build/cargo_target, so the conventional
      # target/<triple>/release path does not exist and the install has
      # nothing to copy.
      cargo build --release --target "$FC_TARGET" --bin firecracker
      install -m 755 "/target/$FC_TARGET/release/firecracker" /out/firecracker
      chown "$CHOWN_TO" /out/firecracker
    '
  if command -v ldd >/dev/null 2>&1 && ldd "$OUT/firecracker" 2>&1 | grep -q "=>"; then
    echo "!! built firecracker is dynamically linked; the jail chroot has no libc" >&2
    exit 1
  fi
  "$OUT/firecracker" --version | head -1
  ls -lh "$OUT/firecracker"
  echo "firecracker ready (built from $FC_COMMIT)"
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
