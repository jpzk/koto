#!/bin/sh
set -e
# NOTE: the host user's own podman (below) creates clawson-net and the cs_host
# container — that's tier-1 authority, by definition. What cs_host does NOT get
# is the podman *socket*: there is no DooD mount anymore (groups are microVMs
# launched via the firecracker binary, not podman). cs_host therefore has no
# path to the host's podman daemon at all.
podman network exists clawson-net || podman network create clawson-net >/dev/null
# HERE = project root. The script lives in host/, so go up one level. The
# matching-path bind mount (`-v "$HERE:$HERE"`) requires HERE to resolve to
# the same absolute path inside cs_host_go and on the real host, which is
# only true when HERE is the project root (the path the user is in).
HERE=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$HERE/groups" "$HERE/creds" "$HERE/.gocache" "$HERE/.gomodcache"
[ -f "$HERE/creds/.credentials.json" ] || { echo "no creds: run \`make login\` first"; exit 1; }
[ -f "$HERE/creds/server.crt" ] || { echo "no daemon TLS cert: run \`make pki-init\` first"; exit 1; }
# gRPC listener address. Inside cs_host_go (on the private clawson-net) 0.0.0.0
# is reachable only by clawson-net peers — the port is NOT host-published. For
# an off-box daemon, set CLAWSON_BIND to the WireGuard interface IP instead.
CLAWSON_BIND="${CLAWSON_BIND:-0.0.0.0}"
CLAWSON_PORT="${CLAWSON_PORT:-8443}"
# CLAWSON_PUBLISH (opt-in): host endpoint to publish the gRPC port to, e.g.
# 127.0.0.1:8443. Needed for a local Android emulator, which reaches the host
# loopback via 10.0.2.2 — set CLAWSON_PUBLISH=127.0.0.1:8443 so the guest can
# dial 10.0.2.2:8443. Left unset by default to keep the port off the host (mTLS
# +token still gate it, but loopback-only is the safer default).
PUBLISH_ARG=""
[ -n "${CLAWSON_PUBLISH:-}" ] && PUBLISH_ARG="-p ${CLAWSON_PUBLISH}:${CLAWSON_PORT}"
# Firecracker runtime: pass /dev/kvm through when the host has it so the
# daemon can boot microVM groups (config.json "runtime": "firecracker").
# /dev/kvm is 0666 on Fedora — no group juggling needed. Hosts without KVM
# still run fine; the fc runtime just fails its preflight with a clear error.
KVM_ARG=""
[ -e /dev/kvm ] && KVM_ARG="--device /dev/kvm"
podman rm -f cs_host_go >/dev/null 2>&1 || true
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
  --name cs_host_go --network clawson-net \
  $PUBLISH_ARG \
  $KVM_ARG \
  --security-opt label=disable \
  -v "$HERE:$HERE" \
  -v "$HERE/creds:/root/.claude" \
  -v "$HERE/.gocache:/root/.cache/go-build" \
  -v "$HERE/.gomodcache:/go/pkg/mod" \
  -v /etc/localtime:/etc/localtime:ro \
  -e CLAWSON_BIND="$CLAWSON_BIND" \
  -e CLAWSON_PORT="$CLAWSON_PORT" \
  -e TERM="${TERM:-xterm-256color}" \
  ${TEXTUAL_DEBUG:+-e TEXTUAL_DEBUG="$TEXTUAL_DEBUG"} \
  -w "$HERE" \
  clawson-host >/dev/null
echo "cs_host_go running (daemon + proxy)"
echo "  attach TUI: make tui"
echo "  stop:       make stop"
