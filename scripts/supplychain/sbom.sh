#!/bin/sh
# sbom.sh — CycloneDX SBOMs (JSON, spec 1.6) for every Go binary koto ships.
#
#   scripts/supplychain/sbom.sh OUTDIR [VERSION] [ARCH]
#
# One `cyclonedx-gomod app` run per shipped binary, under the SAME build
# constraints the release build uses (CGO_ENABLED=0, linux, GOWORK=off per
# module, as govulncheck runs), so the module set in the SBOM is the module
# set linked into the binary — not the superset in go.mod:
#   koto      daemon/   (make koto)
#   koto-tui  tui/      (make koto-tui)
#   fc-agent  fcguest/  (fcguest/build-guest.sh; it ships inside guest.img)
# -std lists the Go standard library as a component: most of our past
# govulncheck findings were stdlib, so a scanner fed this SBOM must see it.
#
# With VERSION, files are <name>_<VERSION>_<ARCH>.cdx.json — the dist asset
# naming ('_' separates fields), so `make dist` hashes them into SHA256SUMS and
# `make release-draft` uploads them while the release is still a DRAFT.
# Without VERSION (CI sanity job): <name>.cdx.json.
#
# Tool: cyclonedx-gomod from PATH if present (CI installs v1.12.0), else
# `go run ...@v1.12.0` — an exact version, verified by sum.golang.org. Keep
# the version in step with supplychain.yml.
#
# KNOWN GAP — Firecracker: no SBOM. It is a Rust binary built from FC_COMMIT
# (fcguest/build-firecracker.sh); an SBOM needs cargo-cyclonedx built in a
# Rust container, which costs a full cargo toolchain + crate fetch per release.
# Its Cargo.lock at FC_COMMIT IS covered for vulnerabilities by osv.sh.
# vmlinux and guest.img's Alpine packages are likewise not described here.
set -eu
out=${1:?usage: sbom.sh OUTDIR [VERSION] [ARCH]}
ver=${2:-}
arch=${3:-x86_64}
GOMOD_VER=v1.12.0 # 2026-08-21
case "$arch" in
  x86_64|amd64) goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  *) echo "sbom.sh: unknown arch $arch" >&2; exit 2 ;;
esac
cd "$(dirname "$0")/../.."
root=$(pwd)
mkdir -p "$out"; out=$(cd "$out" && pwd)
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
  if [ -n "$ver" ]; then f="$out/${name}_${ver}_${arch}.cdx.json"; else f="$out/$name.cdx.json"; fi
  echo "==> SBOM $name ($dir, linux/$goarch) -> $f"
  # stdout of `go run` build chatter goes to stderr; -output writes the file
  (cd "$root/$dir" && $gen app -json -output-version 1.6 -licenses -std -output "$f" .)
  test -s "$f" || { echo "sbom.sh: $f is empty" >&2; exit 1; }
done
