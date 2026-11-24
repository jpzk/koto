#!/bin/sh
# sbom.sh — ONE CycloneDX 1.6 JSON SBOM for everything a koto release ships.
#
#   scripts/supplychain/sbom.sh OUTDIR [VERSION] [ARCH] [COMMIT] [EPOCH]
#
# Writes OUTDIR/koto_<VERSION>_<ARCH>.cdx.json — the dist asset naming ('_'
# separates fields), so `make dist` hashes it into SHA256SUMS and `make
# release-draft` uploads it while the release is still a DRAFT (releases are
# immutable once published). VERSION defaults to 0.0.0-dev, COMMIT/EPOCH to
# `git rev-parse HEAD` / its commit time (the Makefile passes them, since the
# build container has no git). COMMIT/EPOCH make the document deterministic:
# serialNumber and timestamp derive from them, not from the clock.
#
# Two stages:
#  1. one `cyclonedx-gomod app` run per shipped Go binary, under the SAME build
#     constraints the release build uses (CGO_ENABLED=0, linux, GOWORK=off per
#     module, as govulncheck runs), so the module set is the module set linked
#     into the binary — not the superset in go.mod. -std lists the Go standard
#     library (most past govulncheck findings were stdlib). Intermediate files:
#     OUTDIR/parts/{koto,koto-tui,fc-agent}.cdx.json (not release assets).
#       koto      daemon/   (make koto)
#       koto-tui  tui/      (make koto-tui)
#       fc-agent  fcguest/  (fcguest/build-guest.sh; it ships inside guest.img)
#  2. scripts/supplychain/sbomagg (Go, stdlib only) merges those with the
#     pinned non-Go inputs: Firecracker's crates from its Cargo.lock at
#     FC_COMMIT (fetched by commit, or SBOM_CARGO_LOCK=<file>), vmlinux
#     (KERNEL_COMMIT), and guest.img from fcguest/Dockerfile.guest's ARG pins
#     (node, npm, claude-code, codex, tmux/libevent/ncurses, the fedora base by
#     digest), fcguest/pi/install/package-lock.json (checked against
#     PI_LOCK_SHA256) and fcguest/guest/rpms.lock (build toolchain, scope
#     "excluded": not shipped).
#
# Tool: cyclonedx-gomod from PATH if present (CI installs v1.12.0), else
# `go run ...@v1.12.0` — an exact version, verified by sum.golang.org. Keep
# the version in step with the workflows and sbomagg's metadata.tools.
set -eu
out=${1:?usage: sbom.sh OUTDIR [VERSION] [ARCH] [COMMIT] [EPOCH]}
ver=${2:-0.0.0-dev}
ver=${ver#v}
arch=${3:-x86_64}
GOMOD_VER=v1.12.0 # 2026-08-21
case "$arch" in
  x86_64|amd64) goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  *) echo "sbom.sh: unknown arch $arch" >&2; exit 2 ;;
esac
cd "$(dirname "$0")/../.."
root=$(pwd)
commit=${4:-$(git rev-parse HEAD)}
epoch=${5:-$(git log -1 --format=%ct "$commit")}
mkdir -p "$out/parts"; out=$(cd "$out" && pwd)
if command -v cyclonedx-gomod >/dev/null 2>&1; then
  gen="cyclonedx-gomod"
else
  gen="go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$GOMOD_VER"
fi
export GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$goarch GOTOOLCHAIN=local
# the release build passes GOFLAGS= ; -mod=readonly is the default for
# module mode and is spelled out so a stray -mod=mod cannot sneak in
export GOFLAGS=-mod=readonly
for spec in koto:daemon koto-tui:tui fc-agent:fcguest; do
  name=${spec%%:*}; dir=${spec#*:}
  f="$out/parts/$name.cdx.json"
  echo "==> SBOM part $name ($dir, linux/$goarch) -> $f"
  # stdout of `go run` build chatter goes to stderr; -output writes the file
  (cd "$root/$dir" && $gen app -json -output-version 1.6 -licenses -std -output "$f" .)
  test -s "$f" || { echo "sbom.sh: $f is empty" >&2; exit 1; }
done
agg="$out/koto_${ver}_${arch}.cdx.json"
echo "==> SBOM koto $ver ($commit) -> $agg"
# sbomagg runs on the host platform, not the target one
GOOS= GOARCH= go run scripts/supplychain/sbomagg/main.go \
  -root "$root" -parts "$out/parts" -out "$agg" \
  -version "$ver" -arch "$arch" -commit "$commit" -epoch "$epoch" \
  ${SBOM_CARGO_LOCK:+-cargo-lock "$SBOM_CARGO_LOCK"}
test -s "$agg" || { echo "sbom.sh: $agg is empty" >&2; exit 1; }
