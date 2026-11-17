#!/usr/bin/env bash
# go-env-check.sh — assert the Go supply-chain environment is the default,
# checksum-verified one (supplychain.yml "env"). Reads `go env`, so it sees
# what the go command will actually use (env vars AND `go env -w` settings).
#
#   GOPROXY  must be https://proxy.golang.org,direct (the default)
#   GOSUMDB  must be sum.golang.org (the default)
#   GONOSUMDB, GONOSUMCHECK, GOINSECURE, GOPRIVATE, GONOPROXY  must be empty
#   GOFLAGS  must contain -mod=readonly and no -insecure / -mod=mod
#
# Exit 0 = ok, 1 = a weakened setting (each one reported).
set -euo pipefail
command -v go >/dev/null || { echo "go-env-check: go not found" >&2; exit 1; }

rc=0
bad() { echo "::error::go env: $*"; rc=1; }

v() { go env "$1"; }
[ "$(v GOPROXY)" = "https://proxy.golang.org,direct" ] || bad "GOPROXY='$(v GOPROXY)' (want https://proxy.golang.org,direct)"
[ "$(v GOSUMDB)" = "sum.golang.org" ] || bad "GOSUMDB='$(v GOSUMDB)' (want sum.golang.org)"
for k in GONOSUMDB GOINSECURE GOPRIVATE GONOPROXY; do
  [ -z "$(v $k)" ] || bad "$k='$(v $k)' must be unset"
done
# GONOSUMCHECK is not a go env key; reject it as a raw env var (and the
# GONOSUMDB-like typo family) so nobody relies on it.
[ -z "${GONOSUMCHECK:-}" ] || bad "GONOSUMCHECK must not be set"
flags=" $(v GOFLAGS) "
case "$flags" in *" -mod=readonly "*) ;; *) bad "GOFLAGS='$(v GOFLAGS)' must contain -mod=readonly" ;; esac
case "$flags" in *-insecure*) bad "GOFLAGS contains -insecure" ;; esac
case "$flags" in *" -mod=mod "*|*" -mod=vendor "*) bad "GOFLAGS overrides -mod" ;; esac

if [ $rc -eq 0 ]; then
  echo "go-env-check: ok (GOPROXY=$(v GOPROXY) GOSUMDB=$(v GOSUMDB) GOFLAGS=$(v GOFLAGS))"
fi
exit $rc
