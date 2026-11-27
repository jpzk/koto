#!/usr/bin/env bash
# e2e-inbox.sh — the inbox end to end against an ISOLATED daemon (no live
# daemon, no :8443, no /var/lib/koto, no VM needed). Runs daemon TestInboxE2E
# (daemon/inbox_e2e_test.go) in the Makefile's pinned GO_IMAGE:
#
#   peer:   the shipped sidecar/cs-notify + sidecar/cs-ask scripts (ask with
#           options) -> guest-agent shim -> ctlDispatchPB as group "peer"
#   daemon: production mTLS gRPC stack on 127.0.0.1:<ephemeral>, fresh creds
#   client: WatchInbox sees both items; Approve answers the ask with
#           option 2 + a message; InboxDismiss removes the notification
#           (agent identity refused); cs-ask prints "sqlite — smaller ops".
#
# Usage: scripts/e2e-inbox.sh [log file]   (default: stdout only)
# Env:   CONTAINER (default podman), GO_IMAGE (default: the Makefile's pin)
# Exit 0 = pass (the log ends with "E2E PASS"), anything else = fail.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/.." && pwd)
C=${CONTAINER:-podman}
IMG=${GO_IMAGE:-$(sed -n 's/^GO_IMAGE ?= //p' "$REPO/Makefile")}
[ -n "$IMG" ] || { echo "GO_IMAGE not found in Makefile" >&2; exit 2; }
LOG=${1:-/dev/null}
set -o pipefail
"$C" run --rm --security-opt label=disable -v "$REPO":/src -w /src/daemon \
  -e GOWORK=/src/go.work -e GOFLAGS=-mod=readonly -e CGO_ENABLED=0 -e GOTOOLCHAIN=local \
  -e KOTO_INBOX_E2E=1 ${GOPROXY:+-e GOPROXY="$GOPROXY"} ${E2E_PODMAN_OPTS:-} \
  "$IMG" go test -count=1 -run '^TestInboxE2E$' -v . 2>&1 | tee "$LOG"
echo "E2E PASS" | tee -a "$LOG"
