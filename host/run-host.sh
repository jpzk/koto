#!/bin/sh
set -e
SOCK=/run/user/$(id -u)/podman/podman.sock
[ -S "$SOCK" ] || { echo "enable rootless socket: systemctl --user enable --now podman.socket"; exit 1; }
podman network exists clawson-net || podman network create clawson-net >/dev/null
# HERE = project root. The script lives in host/, so go up one level. The
# matching-path bind mount (`-v "$HERE:$HERE"`) requires HERE to resolve to
# the same absolute path inside cs_host_go and on the real host, which is
# only true when HERE is the project root (the path the user is in).
HERE=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$HERE/groups" "$HERE/creds" "$HERE/.gocache"
[ -f "$HERE/creds/.credentials.json" ] || { echo "no creds: run \`make login\` first"; exit 1; }
[ -f "$HERE/creds/server.crt" ] || { echo "no daemon TLS cert: run \`make pki-init\` first"; exit 1; }
# gRPC listener address. Inside cs_host_go (on the private clawson-net) 0.0.0.0
# is reachable only by clawson-net peers — the port is NOT host-published. For
# an off-box daemon, set CLAWSON_BIND to the WireGuard interface IP instead.
CLAWSON_BIND="${CLAWSON_BIND:-0.0.0.0}"
CLAWSON_PORT="${CLAWSON_PORT:-8443}"
podman rm -f cs_host_go >/dev/null 2>&1 || true
# .gocache is a persistent Go build cache. Without it, the first compile
# inside cs_host_go takes ~10-15s; with it, incremental rebuilds after a daemon
# edit take ~1s. Cache is local to this project so wiping it doesn't touch
# the host user's ~/.cache/go-build.
podman run -d --rm \
  --name cs_host_go --network clawson-net \
  --security-opt label=disable \
  -v "$SOCK:/run/podman/podman.sock" \
  -v "$HERE:$HERE" \
  -v "$HERE/creds:/root/.claude" \
  -v "$HERE/.gocache:/root/.cache/go-build" \
  -v /etc/localtime:/etc/localtime:ro \
  -e CONTAINER_HOST=unix:///run/podman/podman.sock \
  -e CLAWSON_BIND="$CLAWSON_BIND" \
  -e CLAWSON_PORT="$CLAWSON_PORT" \
  -e TERM="${TERM:-xterm-256color}" \
  ${TEXTUAL_DEBUG:+-e TEXTUAL_DEBUG="$TEXTUAL_DEBUG"} \
  -w "$HERE" \
  clawson-host >/dev/null
echo "cs_host_go running (daemon + proxy)"
echo "  attach TUI: make tui"
echo "  stop:       make stop"
