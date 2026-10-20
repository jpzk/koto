#!/usr/bin/env bash
# e2e-minimax-send.sh — kept for existing callers; see e2e-send.sh.
# Same env knobs; group name defaults to mme2e<random> as before.
export E2E_GROUP=${E2E_GROUP:-mme2e$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')}
exec "$(dirname "$0")/e2e-send.sh" minimax "$@"
