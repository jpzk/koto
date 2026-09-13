#!/bin/sh
# build-kernel.sh — guest vmlinux for Firecracker groups.
#
# Source = the SAME tree Firecracker builds its guest kernels from: the
# Amazon Linux kernel repo (github.com/amazonlinux/linux) at a pinned
# `microvm-kernel-*.amzn2023` tag (this is what resources/rebuild.sh does:
# `git clone amazonlinux/linux` + `git checkout $(get_tag …)`). We add the
# options koto needs on top of FC's guest config: CONFIG_TUN (L3 TAP +
# rootless-podman pasta), FUSE_FS + NF_TABLES (rootless podman storage/net),
# IKCONFIG (verification).
#
# Why NOT kernel.org vanilla: a vanilla kernel can't parse Firecracker's ACPI
# tables (AE_BAD_PARAMETER at boot), which forced an `acpi=off` workaround —
# and acpi=off leaves the guest with no local APIC / no LAPIC timer, so every
# idle microVM busy-polls a full host CPU (~100%). The amzn tree boots WITH
# ACPI, so the LAPIC timer works and idle is ~0% — and we drop acpi=off.
# See docs/kernel-amzn-vs-vanilla.md for the full argument.
#
# Output: fcassets/vmlinux (gitignored). Rebuild via `make kernel`.
# Containerized so the host needs no toolchain. The builder is Fedora, matching
# the rest of koto's images rather than Firecracker's CI (which uses Ubuntu).
# The builder image chooses the COMPILER that produces vmlinux, so this is a
# real pin, not a cosmetic one: a different gcc is a different kernel binary,
# and divergence from upstream's toolchain means a boot bug here may not
# reproduce for them. Pinned by digest, and the boot is verified after a
# rebuild — `cgroup=on` plus a group actually starting is the check that
# matters, since the failure mode is a kernel that builds cleanly and panics.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
# KOTO_FCASSETS_OUT: see fetch-assets.sh. The source cache stays in the clone
# either way — it is a build artifact, not state.
OUT="${KOTO_FCASSETS_OUT:-$HERE/fcassets}"
CACHE="$HERE/.kernelcache"
mkdir -p "$OUT" "$CACHE"

# Pins. microvm-kernel tag from amazonlinux/linux (6.1.186 base, tagged
# 2026-09-07).
#
# This one is a DELIBERATE EXCEPTION to the six-week dependency lag, taken for
# 1.0.0. The lag exists so a broken or compromised release is noticed before we
# adopt it; the trade here runs the other way. The previous pin (6.1.176, tagged
# 2026-07-02) was eleven stable point releases behind 6.1 upstream by the time
# 1.0.0 was cut, and shipping a release with a knowingly stale guest kernel is
# the thing the rule is meant to prevent, not an instance of following it.
#
# Two things make the exception cheap. This is the GUEST kernel, behind the KVM
# boundary: a bug here escalates inside a VM the trust model already allows to
# run root (root=yes is a supported profile), not on the host. And the amzn tree
# is what Amazon Linux 2023 ships to production, so a tag has been exercised at
# a scale no six-week soak here would add to.
#
# NOT 6.18. microvm-kernel-6.18.25-57.115.amzn2023 exists and carries a higher
# version, but it was tagged 2026-05-20 and sits ~26 point releases behind its
# own 6.18 longterm branch — newer number, older and staler code. 6.1 is also
# the series docs/kernel-amzn-vs-vanilla.md reasons about.
KERNEL_TAG="${KERNEL_TAG:-microvm-kernel-6.1.186-50.374.amzn2023}"
# The commit the annotated tag dereferences to (git checkout resolves HEAD to
# this, not the f3ba04a… tag-object sha from `git ls-remote refs/tags/…`).
# The tag resolves to this commit, verified independently against the GitHub API
# rather than taken from the clone — a pin the clone supplies proves nothing.
# annotated tag e4a14d4b1d1131d7952f1005eed2f82c4c6b060b -> commit below.
KERNEL_COMMIT="${KERNEL_COMMIT:-8a40ca92bfa9b706b76287942c89b13884928cb0}"
FC_VERSION="${FC_VERSION:-v1.16.1}"                          # guest-config source tag
# Pinned by DIGEST: the builder image decides the compiler that produces
# vmlinux, so a moving tag means a moving kernel binary.
BUILDER="${BUILDER:-docker.io/library/fedora@sha256:be9d65e2344d805cc11114319c685ecaa96b6d9b4350a0a6460cdb931babbd19}"
FC_CONFIG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config"

echo "==> building guest vmlinux ($KERNEL_TAG + CONFIG_TUN)"

# Either engine, docker first when both are present — same resolution as the
# Makefile and build-rootfs.sh, so one host never builds half its assets with
# one engine and half with the other.
CONTAINER="${CONTAINER:-$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)}"
[ -n "$CONTAINER" ] || { echo "podman or docker is required to build the guest kernel"; exit 1; }
"$CONTAINER" run --rm -i --security-opt label=disable \
  -e KERNEL_TAG="$KERNEL_TAG" \
  -e KERNEL_COMMIT="$KERNEL_COMMIT" \
  -e FC_CONFIG_URL="$FC_CONFIG_URL" \
  -e DEBIAN_FRONTEND=noninteractive \
  -v "$OUT:/out" \
  -v "$CACHE:/cache" \
  "$BUILDER" bash -eu <<'EOF'
# Fedora equivalents of the Debian set: build-essential -> gcc/make,
# libelf-dev -> elfutils-libelf-devel, libssl-dev -> openssl-devel,
# xz-utils -> xz. dwarves supplies pahole, which the kernel's BTF generation
# needs. diffutils/findutils/perl are implicit on Ubuntu but not in the
# minimal Fedora image, and the kernel build shells out to all three.
dnf install -y --setopt=install_weak_deps=False --quiet \
  gcc make bc bison flex elfutils-libelf-devel openssl-devel dwarves \
  xz tar curl ca-certificates gzip git zstd diffutils findutils perl \
  >/dev/null

# Source: cached amzn tree tarball, or a shallow clone of the pinned tag.
mkdir -p /tmp/src
CACHED="/cache/${KERNEL_TAG}.tar.zst"
if [ -f "$CACHED" ]; then
  echo "    using cached source $CACHED"
  tar -C /tmp/src --zstd -xf "$CACHED"
else
  echo "    git clone amazonlinux/linux @ $KERNEL_TAG (shallow)"
  git clone --depth 1 --single-branch --branch "$KERNEL_TAG" \
    https://github.com/amazonlinux/linux /tmp/src/linux
  got=$(git -C /tmp/src/linux rev-parse HEAD)
  [ "$got" = "$KERNEL_COMMIT" ] || { echo "!! commit mismatch: $got != $KERNEL_COMMIT"; exit 1; }
  tar -C /tmp/src --zstd -cf "$CACHED" linux
fi
cd /tmp/src/linux

# FC's guest config (amzn-derived, microvm-tuned) + only the options we add.
# NO acpi=off / CMDLINE_DEVICES workarounds: the amzn tree parses FC's ACPI
# tables, so virtio is enumerated via ACPI and the LAPIC timer comes up.
curl -fsSL "$FC_CONFIG_URL" >.config
# Pinned: the kernel SOURCE is verified by commit, and this config decides
# what that source becomes — a tampered one could re-enable vsock loopback
# (see the assertion below). Refresh deliberately when FC_VERSION moves:
#   curl -fsSL "$FC_CONFIG_URL" | sha256sum
echo "${FC_CONFIG_SHA256:-adbc70ab5e89213ba00594b12d25e09bdf8bb1ed3c252d7449326bb14c22963b}  .config" | sha256sum -c - >/dev/null || {
  echo "!! guest kernel .config does not match its pinned sha256"; exit 1; }
# Everything built-in (=y): the guest has no module loader, so netfilter/nft
# bits can't be =m. FC's config has TUN off and only the nftables *core*; we
# add the full nftables NAT stack netavark needs for podman *bridged*
# networking (podman network create + --network), plus TUN/FUSE for L3 + podman.
./scripts/config --file .config \
  -e CONFIG_TUN \
  -e CONFIG_FUSE_FS \
  -e CONFIG_NF_TABLES \
  -e CONFIG_NF_TABLES_INET \
  -e CONFIG_NF_TABLES_IPV4 \
  -e CONFIG_NF_TABLES_IPV6 \
  -e CONFIG_NFT_NAT \
  -e CONFIG_NFT_MASQ \
  -e CONFIG_NFT_CT \
  -e CONFIG_NFT_FIB_INET \
  -e CONFIG_NFT_FIB_IPV4 \
  -e CONFIG_NFT_FIB_IPV6 \
  -e CONFIG_NFT_COMPAT \
  -e CONFIG_NFT_REJECT \
  -e CONFIG_NFT_REJECT_INET \
  -e CONFIG_NFT_COUNTER \
  -e CONFIG_NF_CONNTRACK \
  -e CONFIG_NF_NAT \
  -e CONFIG_BRIDGE_NF_EBTABLES \
  -e CONFIG_IKCONFIG \
  -e CONFIG_IKCONFIG_PROC
make olddefconfig >/dev/null

grep -q '^CONFIG_TUN=y'     .config || { echo "!! CONFIG_TUN missing";     exit 1; }
grep -q '^CONFIG_PVH=y'     .config || { echo "!! CONFIG_PVH missing (FC needs PVH)"; exit 1; }
grep -q '^CONFIG_FUSE_FS=y' .config || { echo "!! CONFIG_FUSE_FS missing (rootless podman)"; exit 1; }
grep -q '^CONFIG_ACPI=y'    .config || { echo "!! CONFIG_ACPI missing (needed to drop acpi=off)"; exit 1; }
# The guest agent RPC on vsock 10000 is unauthenticated and runs exec as
# guest root; it is safe only because nothing INSIDE the guest can reach
# vsock. Loopback vsock would let node drive it and defeat root=no (audit I1).
grep -q '^CONFIG_VSOCKETS_LOOPBACK=y' .config && { echo "!! CONFIG_VSOCKETS_LOOPBACK must stay off"; exit 1; }
grep -q '^CONFIG_VHOST_VSOCK=y'       .config && { echo "!! CONFIG_VHOST_VSOCK must stay off"; exit 1; }
true
grep -q '^CONFIG_NFT_NAT=y' .config || { echo "!! CONFIG_NFT_NAT missing (netavark bridged NAT)"; exit 1; }
grep -q '^CONFIG_NFT_MASQ=y' .config || { echo "!! CONFIG_NFT_MASQ missing (netavark masquerade)"; exit 1; }

make -j"$(nproc)" vmlinux >/dev/null
cp vmlinux /out/vmlinux
echo "    built $(du -h /out/vmlinux | cut -f1) vmlinux"
EOF

ls -lh "$OUT/vmlinux"
echo "vmlinux ready (amzn source, CONFIG_TUN=y, ACPI on)"
