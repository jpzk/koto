#!/usr/bin/env bash
# harness-measure.sh — size of the harness policy (fitness: fewer bytes = better).
# usage: scripts/harness-measure.sh [file]   (default prompts/global.md)
set -euo pipefail
f="${1:-$(dirname "$0")/../prompts/global.md}"
[ -r "$f" ] || { echo "harness-measure: cannot read $f" >&2; exit 2; }
printf 'file:  %s\n' "$f"
printf 'bytes: %s\n' "$(wc -c <"$f" | tr -d ' ')"
printf 'words: %s\n' "$(wc -w <"$f" | tr -d ' ')"
printf 'lines: %s\n' "$(wc -l <"$f" | tr -d ' ')"
