#!/bin/sh
set -e
# NOTE: the host user's own podman (below) creates koto-net and the cs_host
# container — that's tier-1 authority, by definition. What cs_host does NOT get
# is the podman *socket*: there is no DooD mount anymore (groups are microVMs
# launched via the firecracker binary, not podman). cs_host therefore has no
# path to the host's podman daemon at all.
podman network exists koto-net || podman network create koto-net >/dev/null
# HERE = project root. The script lives in host/, so go up one level. The
# matching-path bind mount (`-v "$HERE:$HERE"`) requires HERE to resolve to
# the same absolute path inside cs_host_go and on the real host, which is
# only true when HERE is the project root (the path the user is in).
HERE=$(cd "$(dirname "$0")/.." && pwd)
# KOTO_INSTANCE (opt-in): lets a second checkout — e.g. a git worktree —
# run its own daemon alongside this one on the same host. Every other piece
# of state (groups/, groups.json, creds/, run/, .gocache, .gomodcache) is
# already resolved relative to $HERE, so a second worktree gets its own for
# free; the container *name* is the only thing hardcoded enough to collide
# (this script force-removes it on every start). koto-net stays shared —
# containers on the same bridge don't collide on port or address, since each
# has its own network namespace — so no per-instance network is needed.
CS_HOST_NAME="cs_host_go"
[ -n "${KOTO_INSTANCE:-}" ] && CS_HOST_NAME="cs_host_go_${KOTO_INSTANCE}"
mkdir -p "$HERE/groups" "$HERE/creds" "$HERE/.gocache" "$HERE/.gomodcache"
[ -f "$HERE/creds/.credentials.json" ] || [ -n "${ANTHROPIC_API_KEY:-}" ] || { echo "no creds: run \`make login\` first (or export ANTHROPIC_API_KEY)"; exit 1; }
[ -f "$HERE/creds/server.crt" ] || { echo "no daemon TLS cert: run \`make pki-init\` first"; exit 1; }
# creds/ and fcassets/ are sometimes symlinked in from another checkout to
# share one OAuth login + PKI, or the ~800MB firecracker binary+kernel+rootfs
# (e.g. a second worktree running its own instance via KOTO_INSTANCE
# above). The daemon reads both via a $HERE-relative path, resolved through
# the `-v "$HERE:$HERE"` mount below — if the symlink target lives outside
# $HERE, that target isn't mounted anywhere in the container's namespace and
# the open() fails. Bind-mount each symlink's real target at its own
# absolute path too, so the internal symlink traversal finds a live view.
EXTRA_MOUNTS=""
for d in creds fcassets; do
  if [ -L "$HERE/$d" ]; then
    real=$(cd "$HERE/$d" && pwd -P)
    [ "$real" != "$HERE/$d" ] && EXTRA_MOUNTS="$EXTRA_MOUNTS -v $real:$real"
  fi
done
# gRPC listener address. Inside cs_host_go (on the private koto-net) 0.0.0.0
# is reachable only by koto-net peers — the port is NOT host-published. For
# an off-box daemon, set KOTO_BIND to the WireGuard interface IP instead.
KOTO_BIND="${KOTO_BIND:-0.0.0.0}"
KOTO_PORT="${KOTO_PORT:-8443}"
# KOTO_PUBLISH (opt-in): host endpoint to publish the gRPC port to, e.g.
# 127.0.0.1:8443. Needed for a local Android emulator, which reaches the host
# loopback via 10.0.2.2 — set KOTO_PUBLISH=127.0.0.1:8443 so the guest can
# dial 10.0.2.2:8443. Left unset by default to keep the port off the host (mTLS
# +token still gate it, but loopback-only is the safer default).
PUBLISH_ARG=""
[ -n "${KOTO_PUBLISH:-}" ] && PUBLISH_ARG="-p ${KOTO_PUBLISH}:${KOTO_PORT}"
# Firecracker runtime: pass /dev/kvm through when the host has it so the
# daemon can boot microVM groups (config.json "runtime": "firecracker").
# /dev/kvm is 0666 on Fedora; 0660 root:kvm on Ubuntu — hence keep-groups
# below, plus the user being in the kvm group. Hosts without KVM
# still run fine; the fc runtime just fails its preflight with a clear error.
# --group-add keep-groups keeps our supplementary groups across the userns
# mapping (so a host user in the kvm group still has it inside). It is NOT the
# whole story where /dev/kvm is 0660 root:kvm (Ubuntu): the jailed VMM drops to
# a per-VM uid with no groups at all (fcjail.go), so the device needs its world
# bits — a udev rule on the host. See checkKVM in daemon/setup_checks.go.
KVM_ARG=""
[ -e /dev/kvm ] && KVM_ARG="--device /dev/kvm --group-add keep-groups"
# KOTO_HOST_CPUS: fleet-wide CPU ceiling on cs_host — daemon, proxy and
# every microVM together can never exceed this many host cores (podman
# --cpus = cgroup cpu.max on the container scope; cpu is delegated rootless).
# Default = `nproc - 1` (min 1), so the host itself stays responsive no
# matter what the fleet does. Set an explicit number to override, or
# KOTO_HOST_CPUS=0 for unlimited.
if [ -z "${KOTO_HOST_CPUS:-}" ]; then
  KOTO_HOST_CPUS=$(( $(nproc) - 1 ))
  [ "$KOTO_HOST_CPUS" -lt 1 ] && KOTO_HOST_CPUS=1
fi
CPUS_ARG=""
[ "$KOTO_HOST_CPUS" != "0" ] && CPUS_ARG="--cpus $KOTO_HOST_CPUS"
# KOTO_HOST_MEM_MIB: fleet-wide memory ceiling for the microVMs, so a fleet
# of FC processes can never eat the whole host and freeze it. Deliberately
# NOT podman --memory: that puts the daemon/proxy in the same OOM pool as the
# VMs, and an OOM-killed daemon is a fleet outage. The daemon enforces it
# instead (daemon/fchostmem.go): a spawn is refused up front when the running
# VMs' RAM + the new one would exceed the cap, and the vms/ cgroup parent gets
# memory.max = cap as the hard backstop (only VMMs are ever OOM candidates).
# Default (unset) = 90% of MemTotal, computed by the daemon.
# Set an explicit MiB count to override, or KOTO_HOST_MEM_MIB=0 for unlimited.
# Writable cgroup tree for per-VM caps (daemon/fccgroup.go): the host view is
# mounted rw and the cgroup namespace stays the host's, so the daemon can find
# its own scope (delegated to this user by systemd, hence writable rootless),
# evacuate itself into a leaf, and place each Firecracker process in a
# vms/<group> child with cpu.weight + memory.high at clone time. The VMM
# itself never sees this mount — the jail unshares CLONE_NEWCGROUP and
# chroots to an empty tree. If the mount is rejected or delegation is absent
# the daemon's startup probe degrades to cgroup=off; removing these two args
# is the supported off-switch.
CGROUP_ARGS="--cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw"
# Graceful replacement: a running daemon gets to stop its VMs (guests
# sync+umount their workspace images) before the container goes away — rm -f
# alone SIGKILLs the VMMs mid-write. No-VM daemons exit in well under a
# second, so the dev edit-restart loop doesn't feel the -t 15 ceiling.
podman stop -t 15 "$CS_HOST_NAME" >/dev/null 2>&1 || true
podman rm -f "$CS_HOST_NAME" >/dev/null 2>&1 || true
# .gocache is a persistent Go build cache. Without it, the first compile
# inside cs_host_go takes ~10-15s; with it, incremental rebuilds after a daemon
# edit take ~1s. Cache is local to this project so wiping it doesn't touch
# the host user's ~/.cache/go-build.
# .gomodcache is a persistent Go module (download) cache. cs_host_go runs
# --rm, so without this mount every restart re-fetches all ~20 deps
# (grpc, gvisor-tap-vsock, x/net, ...) from the module proxy over the
# network before the daemon can even compile — measured ~11s of the ~19s
# cold-start-to-gRPC-ready window. Persisting it drops restarts to
# compile-only time. Local to this project, same rationale as .gocache.
podman run -d --rm \
  --name "$CS_HOST_NAME" --network koto-net \
  $PUBLISH_ARG \
  $KVM_ARG \
  $CPUS_ARG \
  $CGROUP_ARGS \
  --security-opt label=disable \
  -v "$HERE:$HERE" \
  -v "$HERE/creds:/root/.claude" \
  -v "$HERE/.gocache:/root/.cache/go-build" \
  -v "$HERE/.gomodcache:/go/pkg/mod" \
  -v /etc/localtime:/etc/localtime:ro \
  $EXTRA_MOUNTS \
  -e KOTO_BIND="$KOTO_BIND" \
  -e KOTO_PORT="$KOTO_PORT" \
  ${KOTO_HOST_MEM_MIB:+-e KOTO_HOST_MEM_MIB="$KOTO_HOST_MEM_MIB"} \
  -e TERM="${TERM:-xterm-256color}" \
  ${TEXTUAL_DEBUG:+-e TEXTUAL_DEBUG="$TEXTUAL_DEBUG"} \
  ${ANTHROPIC_API_KEY:+-e ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY"} \
  -w "$HERE" \
  koto-host >/dev/null
INSTANCE_ARG=""
[ -n "${KOTO_INSTANCE:-}" ] && INSTANCE_ARG=" INSTANCE=$KOTO_INSTANCE"
echo "$CS_HOST_NAME running (daemon + proxy)"
echo "  attach TUI: make tui$INSTANCE_ARG"
echo "  stop:       make stop$INSTANCE_ARG"
