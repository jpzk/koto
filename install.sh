#!/bin/sh
# koto installer — fetch a release, verify it, hand off to `koto install`.
#
#   curl -fsSL https://kotovm.com/install.sh | sh
#   curl -fsSL https://kotovm.com/install.sh | sh -s -- --version 1.0.0
#
# WHAT THIS VERIFIES, AND IN WHAT ORDER
#
# The chain is signature first, then names, then bytes, and the order is the
# design rather than an implementation detail:
#
#   1. Fetch the release signing key from the keyserver BY FINGERPRINT, and
#      check the key that came back has that fingerprint. Fetching by email and
#      trusting the answer would make the keyserver the trust anchor; fetching
#      by fingerprint makes it a delivery mechanism, and the anchor is the
#      KEY_FPR line below — which reaches you inside this script, over HTTPS,
#      from the site you chose to run it from.
#
#   2. Verify the detached signature on SHA256SUMS, and require the VALIDSIG
#      line to name that same fingerprint. `gpg --verify` succeeds for a good
#      signature from ANY key in the keyring, so verifying without checking
#      WHICH key signed is close to not verifying at all.
#
#   3. Take the asset NAMES from the now-trusted SHA256SUMS. Release assets
#      carry the version of the thing inside them (firecracker-1.16.1-…,
#      vmlinux-6.1.176-…, rootfs.img-fedora44-…), so the names cannot be
#      guessed ahead of time — and taking them from a signed file means a
#      tampered index cannot point us at something else.
#
#   4. Check every downloaded asset against SHA256SUMS before anything is
#      decompressed, and stage everything in a temp directory. Nothing lands in
#      the install directory until the whole set has passed.
#
# WHAT IT DOES NOT PROVE. That the release is what the koto authors intended —
# only that it was signed by the key this script names. If this script itself
# was served to you modified, the fingerprint in it could be modified too. Read
# it before piping it to a shell; that advice is not a formality here.
set -eu

REPO="${KOTO_REPO:-jpzk/koto}"
KEY_FPR="${KOTO_KEY_FPR:-A6E69ED6BC4779F3281721484CC1AFDE15B64EA3}"
KEYSERVER="${KOTO_KEYSERVER:-https://keys.openpgp.org/vks/v1/by-fingerprint}"
ARCH="${KOTO_ARCH:-x86_64}"
VERSION="${KOTO_VERSION:-latest}"
DEST="${KOTO_DEST:-$HOME/.local/share/koto/release}"
DO_INSTALL=1

usage() {
	cat <<EOF
usage: install.sh [--version X] [--dest DIR] [--download-only]

  --version X       release to install (default: latest)
  --dest DIR        where to unpack (default: \$HOME/.local/share/koto/release)
  --download-only   verify and unpack, but do not run \`koto install\`
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
	--dest) DEST="${2:?--dest needs a value}"; shift 2 ;;
	--download-only) DO_INSTALL=0; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

die() { echo "!! $*" >&2; exit 1; }
say() { echo "==> $*"; }

# Refuse to run as root. koto's daemon runs as an unprivileged user and
# `koto install` calls sudo itself for the handful of root-owned files it
# writes, echoing each command first. Running the whole installer as root would
# create the state directory owned by root and leave a fleet nobody can drive.
[ "$(id -u)" -ne 0 ] || die "do not run this as root — koto installs as your own user and calls sudo where it needs to"

case "$(uname -s)/$(uname -m)" in
Linux/x86_64) ;;
*) die "koto is Linux/x86_64 only (this is $(uname -s)/$(uname -m))" ;;
esac

for t in curl gpg zstd sha256sum; do
	command -v "$t" >/dev/null || die "$t is required but not installed"
done

# KOTO_BASE_URL overrides where assets come from — a mirror, or a test origin.
# It changes only DELIVERY: the signature check below is unconditional, so a
# mirror cannot serve anything this script will accept without the release key.
if [ -n "${KOTO_BASE_URL:-}" ]; then
	BASE="$KOTO_BASE_URL"
elif [ "$VERSION" = latest ]; then
	BASE="https://github.com/$REPO/releases/latest/download"
else
	BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

STAGE="$(mktemp -d "${TMPDIR:-/tmp}/koto-install.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT INT TERM
GNUPGHOME="$STAGE/gnupg"
export GNUPGHOME
mkdir -p -m 700 "$GNUPGHOME"

# --proto pins https on the request AND on every redirect: curl's default
# happily follows an https -> http downgrade, and GitHub's asset URLs redirect.
get() {
	curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --connect-timeout 20 \
		-o "$2" "$1" || die "could not fetch $1"
}

say "release: $VERSION ($ARCH) from $BASE"

say "fetching the signing key $KEY_FPR"
get "$KEYSERVER/$KEY_FPR" "$STAGE/key.asc"
gpg --batch --quiet --import "$STAGE/key.asc" || die "could not import the release key"
got_fpr="$(gpg --batch --with-colons --list-keys | awk -F: '/^fpr:/ {print $10; exit}')"
[ "$got_fpr" = "$KEY_FPR" ] ||
	die "the keyserver returned $got_fpr, not $KEY_FPR"

say "verifying the signature on SHA256SUMS"
get "$BASE/SHA256SUMS" "$STAGE/SHA256SUMS"
get "$BASE/SHA256SUMS.asc" "$STAGE/SHA256SUMS.asc"
gpg --batch --status-fd=1 --verify "$STAGE/SHA256SUMS.asc" "$STAGE/SHA256SUMS" 2>/dev/null |
	grep -q "^\[GNUPG:\] VALIDSIG $KEY_FPR" ||
	die "SHA256SUMS is not signed by $KEY_FPR — nothing was installed"
echo "    good signature from $KEY_FPR"

# Asset names come out of the signed file. Map each back to the path koto
# expects by stripping the -<version>-<arch>.zst tail.
say "downloading the assets it names"
assets="$(awk '{print $2}' "$STAGE/SHA256SUMS")"
[ -n "$assets" ] || die "SHA256SUMS lists no assets"
for a in $assets; do
	case "$a" in
	*-"$ARCH".zst) ;;
	*) die "SHA256SUMS names $a, which is not a $ARCH asset" ;;
	esac
	echo "    $a"
	get "$BASE/$a" "$STAGE/$a"
done

say "checking the assets against the signed SHA256SUMS"
(cd "$STAGE" && sha256sum -c SHA256SUMS >/dev/null) ||
	die "an asset does not match the signed SHA256SUMS — nothing was installed"

say "unpacking into $DEST"
mkdir -p "$STAGE/out/fcassets"
for a in $assets; do
	base="${a%-"$ARCH".zst}"    # koto-1.0.0        / rootfs.img-fedora44
	name="${base%-*}"           # koto              / rootfs.img
	case "$name" in
	koto | koto-tui) out="$STAGE/out/$name" ;;
	firecracker | vmlinux | rootfs.img) out="$STAGE/out/fcassets/$name" ;;
	*) die "unexpected asset $a" ;;
	esac
	zstd -qdf --sparse "$STAGE/$a" -o "$out" || die "could not decompress $a"
done
chmod +x "$STAGE/out/koto" "$STAGE/out/koto-tui" \
	"$STAGE/out/fcassets/firecracker" "$STAGE/out/fcassets/vmlinux" 2>/dev/null || true

# Record what we verified, so `koto install` can hold the bytes to it rather
# than warning that they are unverified. Be clear about what this is: the
# hashes are taken from files this script has just checked against a signed
# manifest, so it pins the VERIFIED STATE across the gap between unpacking and
# installing. It is not independent evidence — the signature above is that.
(cd "$STAGE/out" && sha256sum koto koto-tui fcassets/firecracker fcassets/vmlinux fcassets/rootfs.img) \
	>"$STAGE/out/artifacts.sha256"

mkdir -p "$(dirname "$DEST")"
rm -rf "$DEST"
mv "$STAGE/out" "$DEST"
say "verified and unpacked: $DEST"

if [ "$DO_INSTALL" -eq 0 ]; then
	echo
	echo "next:  cd $DEST && ./koto install && koto setup"
	exit 0
fi

say "running koto install (it will echo each sudo command before running it)"
cd "$DEST"
./koto install
echo
echo "next:  koto setup     # mint TLS identities, connect credentials, start the daemon"
