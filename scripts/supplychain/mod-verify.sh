#!/usr/bin/env bash
# mod-verify.sh — `go mod verify` for every module given (default: $MODULES).
#
# Why a wrapper: koto-protocol is wired into daemon/ and tui/ by a local
# directory replace (`replace koto-protocol => ../protocol`) and its path has
# no dot. `go mod verify` (Go 1.26, modcmd/verify.go verifyMod) still asks
# the module cache for its zip; the cache path of a dot-less module is
# invalid, so it reports "koto-protocol v0.0.0: missing ziphash" and exits 1
# even though there is nothing in the cache to verify — the code is in this
# repo and is checked by review, not by a checksum.
#
# This script runs the real `go mod verify` and forgives exactly that one
# error for exactly the modules that are local-directory replaces (computed
# from `go list -m`, not hardcoded). Any other error — a modified zip or dir,
# a missing ziphash for a proxied module — still fails.
set -euo pipefail
cd "$(dirname "$0")/../.."
mods=${*:-${MODULES:?usage: mod-verify.sh <module dir>... (or set MODULES)}}
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
rc=0
for m in $mods; do
  echo "== go mod verify $m"
  # modules replaced by a local directory (Replace.Version is empty)
  (cd "$m" && go list -m -f '{{with .Replace}}{{if not .Version}}{{$.Path}}{{end}}{{end}}' all) \
    | sed '/^$/d' >"$tmp/local"
  set +e
  (cd "$m" && go mod verify) >"$tmp/out" 2>&1
  r=$?
  set -e
  if [ $r -eq 0 ]; then cat "$tmp/out"; continue; fi
  : >"$tmp/rest"
  while IFS= read -r line; do
    mp=${line%% *}
    case "$line" in
      *": missing ziphash: "*)
        if grep -qxF -- "$mp" "$tmp/local"; then
          echo "skip  $line  (local directory replace, nothing in module cache)"
          continue
        fi ;;
    esac
    printf '%s\n' "$line" >>"$tmp/rest"
  done <"$tmp/out"
  if [ -s "$tmp/rest" ]; then
    cat "$tmp/rest"; echo "::error::$m: go mod verify failed"; rc=1
  else
    echo "all non-local modules verified"
  fi
done
exit $rc
