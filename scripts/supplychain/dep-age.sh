#!/usr/bin/env bash
# dep-age.sh — dependency-age gate (CLAUDE.md/AGENTS.md "Conventions":
# every dependency must be >= 6 weeks old when it is pinned).
#
#   dep-age.sh <base-ref>   check module versions in go.mod that are NEW or
#                           CHANGED relative to <base-ref> (PR / push mode)
#   dep-age.sh --all        check every module version in every go.mod
#
# For each candidate "module version" it reads the Go module proxy's
# $GOPROXY/<escaped-module>/@v/<version>.info and compares its Time against
# now - MIN_AGE_DAYS (default 42). Anything younger fails the gate unless it
# is listed in the allowlist (ALLOW_FILE, default
# .github/supplychain/dep-age-allow.txt) as
#     <module> <version> <reason...>
# The reason is mandatory: the documented exception is a pin that closes a
# REACHABLE govulncheck finding, and the reason should say which.
#
# Scope: `require` and `replace` versions. The `go`/`toolchain` lines are not
# checked — the toolchain tracks the newest patch of a supported series by
# convention. Local-path replaces carry no version and are skipped, as are
# modules whose first path element has no dot (koto-protocol, wired in by a
# local replace) — no proxy can serve those, Go itself refuses to fetch them.
#
# Exit: 0 = pass, 1 = too-new dependency (or proxy lookup failure — fail
# closed: an unverifiable age is not a pass), 2 = usage error.
set -euo pipefail

MIN_AGE_DAYS=${MIN_AGE_DAYS:-42}
MODULES=${MODULES:-"daemon fcguest tui protocol"}
ALLOW_FILE=${ALLOW_FILE:-.github/supplychain/dep-age-allow.txt}
PROXY=${DEP_AGE_PROXY:-https://proxy.golang.org}
NOW=${DEP_AGE_NOW:-$(date -u +%s)}   # override only for tests

[ $# -eq 1 ] || { echo "usage: $0 <base-ref>|--all" >&2; exit 2; }
base=$1

# Print "module version" for every require/replace target in a go.mod on
# stdin. Handles both single-line and block forms; strips comments.
modvers() {
  awk '
    { sub(/\/\/.*/, "") }
    /^[ \t]*$/ { next }
    /^(require|replace)[ \t]*\($/ { blk = $1; next }
    blk != "" && /^[ \t]*\)/ { blk = ""; next }
    {
      line = $0; kind = blk
      if (kind == "") {
        if ($1 != "require" && $1 != "replace") next
        kind = $1; sub(/^[ \t]*(require|replace)[ \t]+/, "", line)
      }
      n = split(line, f, /[ \t]+/)
      # drop empty leading field from indentation
      j = 0; for (i = 1; i <= n; i++) if (f[i] != "") g[++j] = f[i]
      if (kind == "require" && j >= 2) print g[1], g[2]
      if (kind == "replace") {
        for (i = 1; i <= j; i++) if (g[i] == "=>") break
        if (i + 2 <= j) print g[i+1], g[i+2]   # remote replace: path version
      }
      delete g
    }'
}

# Module path -> proxy escaping: each upper-case letter becomes !lower.
escape() { sed -E 's/([A-Z])/!\L\1/g' <<<"$1"; }

allowed() {
  [ -f "$ALLOW_FILE" ] || return 1
  awk -v m="$1" -v v="$2" '
    /^[ \t]*(#|$)/ { next }
    $1 == m && $2 == v && NF >= 3 { found = 1 }
    END { exit !found }' "$ALLOW_FILE"
}

cands=$(mktemp); tmpb=$(mktemp); tmph=$(mktemp)
trap 'rm -f "$cands" "$tmpb" "$tmph"' EXIT
if [ "$base" != --all ]; then
  git rev-parse --verify -q "$base^{commit}" >/dev/null \
    || { echo "dep-age: base ref '$base' not found (fetch it first)" >&2; exit 2; }
fi
for m in $MODULES; do
  f=$m/go.mod
  [ -f "$f" ] || { echo "dep-age: $f missing" >&2; exit 2; }
  if [ "$base" = --all ]; then
    modvers <"$f"
  else
    # New or changed lines only: a version present at HEAD but not at base
    # (a go.mod absent at base = everything in it is new). Temp files rather
    # than <(...): /dev/fd is not available in every sandbox.
    { git show "$base:$f" 2>/dev/null || true; } | modvers | sort -u >"$tmpb"
    modvers <"$f" | sort -u >"$tmph"
    comm -13 "$tmpb" "$tmph"
  fi | awk '$1 ~ /^[^\/]*\./' | sed "s|^|$m |"
done | sort -u >"$cands"

total=$(wc -l <"$cands")
echo "dep-age: base=$base min_age=${MIN_AGE_DAYS}d candidates=$total"
[ "$total" -gt 0 ] || { echo "dep-age: no new/changed module versions — pass"; exit 0; }

cutoff=$((NOW - MIN_AGE_DAYS * 86400))
rc=0
declare -A seen
while read -r where mod ver; do
  key="$mod@$ver"
  if [ -z "${seen[$key]:-}" ]; then
    info=$(curl -4 -fsS -m 20 --retry 2 "$PROXY/$(escape "$mod")/@v/$ver.info" 2>/dev/null) || info=
    t=$(sed -n 's/.*"Time":"\([^"]*\)".*/\1/p' <<<"$info")
    seen[$key]=${t:-?}
  fi
  t=${seen[$key]}
  if [ "$t" = "?" ]; then
    echo "FAIL  $where $mod $ver: proxy .info lookup failed (fail closed)"; rc=1; continue
  fi
  ts=$(date -u -d "$t" +%s)
  age=$(( (NOW - ts) / 86400 ))
  if [ "$ts" -le "$cutoff" ]; then
    echo "ok    $where $mod $ver  ${t%%T*} (${age}d)"
  elif allowed "$mod" "$ver"; then
    echo "ALLOW $where $mod $ver  ${t%%T*} (${age}d) — allowlisted in $ALLOW_FILE"
  else
    echo "FAIL  $where $mod $ver  ${t%%T*} (${age}d < ${MIN_AGE_DAYS}d)"; rc=1
  fi
done <"$cands"
[ $rc -eq 0 ] && echo "dep-age: pass" || echo "dep-age: FAIL — bump is younger than ${MIN_AGE_DAYS} days; wait, or allowlist with a reason in $ALLOW_FILE"
exit $rc
