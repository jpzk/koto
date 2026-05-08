#!/bin/sh
set -e
SOCK=/run/user/$(id -u)/podman/podman.sock
[ -S "$SOCK" ] || { echo "enable rootless socket: systemctl --user enable --now podman.socket"; exit 1; }
podman network exists nc-net || podman network create nc-net >/dev/null
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$HERE/groups" "$HERE/creds"
[ -f "$HERE/creds/.credentials.json" ] || { echo "no creds: run \`make login\` first"; exit 1; }
exec podman run --rm -it \
  --name nc_host --network nc-net \
  --security-opt label=disable \
  -v "$SOCK:/run/podman/podman.sock" \
  -v "$HERE/groups:$HERE/groups" \
  -v "$HERE/creds:/root/.claude" \
  -e CONTAINER_HOST=unix:///run/podman/podman.sock \
  -e TERM="${TERM:-xterm-256color}" \
  -w "$HERE" \
  nanoclaw-host
