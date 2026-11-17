#!/usr/bin/env bash
# osv.sh — osv-scanner over koto's full dependency inventory (supplychain.yml).
#
#   osv.sh <outdir>          scan the four go.mod files + Firecracker's
#                            Cargo.lock at FC_COMMIT (build-firecracker.sh)
#   osv.sh --sbom <dir>      scan every *.cdx.json in <dir>
#
# Fails (exit 1) on any finding not ignored in osv-scanner.toml. Ignores are
# [[IgnoredVulns]] entries with a reason AND ignoreUntil (<= 90 days out);
# this script rejects an ignore without either, so an exception cannot
# quietly become permanent.
#
# go.work is not scanned on its own: it carries no versions (only a go
# directive and `use` paths) — the four go.mod files are the inventory.
set -euo pipefail
cd "$(dirname "$0")/../.."
CONFIG=osv-scanner.toml
command -v osv-scanner >/dev/null || { echo "osv: osv-scanner not on PATH" >&2; exit 2; }

# --- config hygiene: every ignore needs reason + ignoreUntil <= 90d out ------
max=$(( $(date -u +%s) + 90*86400 ))
bad=0
ign=$(awk '
  function flush() { if (n) print id "|" u "|" r; n=0; id=""; u=""; r="" }
  /^\[\[/ { flush() }
  /^\[\[IgnoredVulns\]\]/ { n=1; next }
  n && /^[ \t]*id[ \t]*=/          { v=$0; sub(/^[^=]*=[ \t]*/, "", v); gsub(/"/, "", v); id=v }
  n && /^[ \t]*ignoreUntil[ \t]*=/ { v=$0; sub(/^[^=]*=[ \t]*/, "", v); gsub(/"/, "", v); u=v }
  n && /^[ \t]*reason[ \t]*=/      { v=$0; sub(/^[^=]*=[ \t]*/, "", v); gsub(/"/, "", v); r=v }
  END { flush() }
' "$CONFIG")
while IFS='|' read -r id until reason; do
  [ -n "$id" ] || continue
  if [ -z "$reason" ] || [ -z "$until" ]; then
    echo "osv: ignore $id lacks reason/ignoreUntil" >&2; bad=1; continue
  fi
  t=$(date -u -d "${until%%T*}" +%s 2>/dev/null) || { echo "osv: ignore $id: bad ignoreUntil '$until'" >&2; bad=1; continue; }
  [ "$t" -le "$max" ] || { echo "osv: ignore $id: ignoreUntil $until is > 90 days out" >&2; bad=1; }
done <<<"$ign"
[ $bad -eq 0 ] || exit 1

if [ "${1:-}" = --sbom ]; then
  dir=${2:?usage: osv.sh --sbom <dir>}
  args=()
  for f in "$dir"/*.cdx.json; do args+=(-L "$f"); done
  [ ${#args[@]} -gt 0 ] || { echo "osv: no *.cdx.json in $dir" >&2; exit 2; }
  exec osv-scanner scan source --config="$CONFIG" "${args[@]}"
fi

out=${1:?usage: osv.sh <outdir> | --sbom <dir>}
mkdir -p "$out"
fc_commit=$(sed -n 's/^FC_COMMIT="\${FC_COMMIT:-\([0-9a-f]\{40\}\)}".*/\1/p' fcguest/build-firecracker.sh)
[ ${#fc_commit} -eq 40 ] || { echo "osv: could not read FC_COMMIT from build-firecracker.sh" >&2; exit 2; }
# Fetched by COMMIT, not tag, so it is the exact lockfile build-firecracker.sh
# builds. osv-scanner picks the parser from the file name, hence the subdir.
mkdir -p "$out/firecracker"
curl -fsSL --retry 3 -o "$out/firecracker/Cargo.lock" \
  "https://raw.githubusercontent.com/firecracker-microvm/firecracker/$fc_commit/Cargo.lock"
echo "osv: firecracker Cargo.lock @ $fc_commit ($(grep -c '^\[\[package\]\]' "$out/firecracker/Cargo.lock") packages)"

osv-scanner scan source --config="$CONFIG" \
  -L daemon/go.mod -L fcguest/go.mod -L tui/go.mod -L protocol/go.mod \
  -L "$out/firecracker/Cargo.lock"
