#!/bin/sh
# koto installer. One command, all four stages:
#
#   curl -fsSL https://kotovm.com/install.sh | sh
#   curl -fsSL https://kotovm.com/install.sh | sh -s -- --version 1.0.0
#
#   1. fetch      clone the release tag to a temp dir, download the signed
#                 artifacts into it and verify them         (= make fetch)
#   2. install    koto install: /var/lib/koto, binaries, unit (= make install)
#   3. setup      koto setup: TLS identities, credentials, start (= make wizard)
#   4. tui        koto tui: /new <name> spawns your first agent, /exit detaches
#
# WHY A CLONE. `koto install` integrates more than the five artifacts — the
# harness prompts (prompts/global.md is the policy every group runs under) come
# from the source tree — so the script installs from a shallow checkout of the
# release tag, exactly as a user who cloned would. The clone also carries the
# committed artifacts.sha256, which gives the download a second, independent
# check (below). It lives in a temp dir and is removed once installed.
#
# Stages 2-4 are interactive, and under `curl | sh` this script's stdin is the
# pipe it is being read from — so they read the terminal (/dev/tty) instead.
# With no terminal (CI, a provisioning script) it stops after stage 2 and
# prints the rest.
#
# This is also the ONE implementation of fetch-and-verify: `make fetch` runs it
# with `--download-only --dest . --manifest artifacts.sha256`, so the piped
# installer and the clone route cannot drift apart.
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
#      carry the version of the thing inside them (firecracker_1.17.0_…,
#      vmlinux_6.1.186_…, rootfs.img_fedora44_…), so the names cannot be
#      guessed ahead of time — and taking them from a signed file means a
#      tampered index cannot point us at something else.
#
#   4. Check every downloaded asset against SHA256SUMS before anything is
#      decompressed, and stage everything in a directory beside the
#      destination. Nothing lands in the destination until the whole set has
#      passed — including a check of the UNPACKED bytes against the committed
#      artifacts.sha256, whose checksums arrive over git rather than with the
#      assets.
#
# WHAT IT DOES NOT PROVE. That the release is what the koto authors intended —
# only that it was signed by the key this script names. The source clone is
# fetched over HTTPS from the repository and is not signature-checked, so it
# is trusted for the prompts and the manifest only: before `koto install` runs
# this script deletes any creds/ and daemon state (groups, groups*.json,
# schedules.json, goals.json, metrics.jsonl) a tag may have committed, and
# `koto install` itself never imports them without -migrate-clone. If
# this script itself was served to you modified, the fingerprint in it could
# be modified too. Read it before piping it to a shell; that advice is not a
# formality here.
set -eu

REPO="${KOTO_REPO:-jpzk/koto}"
REPO_URL="${KOTO_REPO_URL:-https://github.com/$REPO}"
KEY_FPR="${KOTO_KEY_FPR:-A6E69ED6BC4779F3281721484CC1AFDE15B64EA3}"
KEYSERVER="${KOTO_KEYSERVER:-https://keys.openpgp.org/vks/v1/by-fingerprint}"
ARCH="${KOTO_ARCH:-x86_64}"
VERSION="${KOTO_VERSION:-latest}"
DEST="${KOTO_DEST:-}"
# KOTO_RELEASE_PUBKEY=<file> skips the keyserver (air-gapped hosts). The
# fingerprint check still applies, so it is a delivery choice, not a bypass.
PUBKEY="${KOTO_RELEASE_PUBKEY:-}"
MANIFEST=""
NEXT=""
DO_INSTALL=1
ATTACH=1
YES=""

usage() {
	cat <<EOF
usage: install.sh [--version X] [--no-attach] [--yes] [--download-only [--dest DIR] [--manifest FILE]]

  --version X       release to install (default: latest)
  --no-attach       stop after koto setup instead of opening the TUI
                    (what `koto update` runs)
  --yes             accept the defaults of koto install and koto setup,
                    the daemon stop on an upgrade included
  --download-only   fetch and verify the artifacts only, into --dest
  --dest DIR        where --download-only unpacks (default: the current dir)
  --manifest FILE   also check the unpacked artifacts against FILE, a
                    sha256sum(1) list that did not come from the release
  --next TEXT       the command to suggest once a --download-only run is done
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
	--dest) DEST="${2:?--dest needs a value}"; shift 2 ;;
	--download-only) DO_INSTALL=0; shift ;;
	--manifest) MANIFEST="${2:?--manifest needs a value}"; shift 2 ;;
	--next) NEXT="${2:?--next needs a value}"; shift 2 ;;
	--no-attach) ATTACH=0; shift ;;
	--yes) YES=-y; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

die() { echo "!! $*" >&2; exit 1; }

# The interactive stages need a terminal. Probe by opening it, not by testing
# the path: /dev/tty exists in a container or under nohup and still fails to
# open when there is no controlling terminal.
HAVE_TTY=0
if (: </dev/tty) 2>/dev/null; then HAVE_TTY=1; fi

# Numbered steps, so a long quiet stretch reads as "step 4 of 9" rather than as
# a hang. The count depends on how far this run will go: the five fetch steps,
# plus the source clone and install, plus setup and the TUI given a terminal.
TOTAL=5
if [ "$DO_INSTALL" -eq 1 ]; then
	TOTAL=7
	[ "$HAVE_TTY" -eq 0 ] || TOTAL=$((8 + ATTACH))
fi
STEP=0
step() { STEP=$((STEP + 1)); echo "==> [$STEP/$TOTAL] $*"; }

# Live progress only on a terminal. curl and git draw their meters on stderr;
# piped into a log, those redraws are noise, so a non-tty run stays quiet.
TTY=0
[ -t 2 ] && TTY=1

size() { # human-readable size of a file
	wc -c <"$1" | awk '{ s = $1; u = "B"
		if (s >= 1048576) { s /= 1048576; u = "MiB" } else if (s >= 1024) { s /= 1024; u = "KiB" }
		printf (u == "B" ? "%d %s" : "%.1f %s"), s, u }'
}

# Refuse to run as root. koto's daemon runs as an unprivileged user and
# `koto install` calls sudo itself for the handful of root-owned files it
# writes, echoing each command first. Running the whole installer as root would
# create the state directory owned by root and leave a fleet nobody can drive.
[ "$(id -u)" -ne 0 ] || die "do not run this as root — koto installs as your own user and calls sudo where it needs to"

case "$(uname -s)/$(uname -m)" in
Linux/x86_64) ;;
*) die "koto is Linux/x86_64 only (this is $(uname -s)/$(uname -m))" ;;
esac

tools="curl gpg zstd sha256sum"
[ "$DO_INSTALL" -eq 0 ] || tools="git $tools"
for t in $tools; do
	command -v "$t" >/dev/null || die "$t is required but not installed"
done
[ -n "$KEY_FPR" ] || die "no release key fingerprint — refusing to accept a signature from whatever key turns up"

# Paths given relative to where we were started, resolved before any cd.
case "$MANIFEST" in "" | /*) ;; *) MANIFEST="$PWD/$MANIFEST" ;; esac
case "$PUBKEY" in "" | /*) ;; *) PUBKEY="$PWD/$PUBKEY" ;; esac
[ -z "$MANIFEST" ] || [ -r "$MANIFEST" ] || die "cannot read --manifest $MANIFEST"
[ -z "$PUBKEY" ] || [ -r "$PUBKEY" ] || die "cannot read KOTO_RELEASE_PUBKEY $PUBKEY"

# --proto pins https on the request AND on every redirect: curl's default
# happily follows an https -> http downgrade, and GitHub's asset URLs redirect.
# A file:// base is allowed only when it is the base you named (a local mirror,
# a test); redirects stay https-only, and the signature is what vouches for
# the bytes either way.
PROTO='=https'
case "${KOTO_BASE_URL:-}" in file://*) PROTO='=https,file' ;; esac

get() { # quiet: small files
	curl -fsSL --proto "$PROTO" --proto-redir '=https' --retry 3 --connect-timeout 20 \
		-o "$2" "$1" || die "could not fetch $1"
}
getp() { # with a progress bar on a terminal: the assets
	if [ "$TTY" -eq 1 ]; then
		curl -fL --progress-bar --proto "$PROTO" --proto-redir '=https' --retry 3 --connect-timeout 20 \
			-o "$2" "$1" || die "could not fetch $1"
	else
		get "$1" "$2"
	fi
}

# The source clone needs a TAG, and the signed SHA256SUMS is checked against
# the version asked for, so "latest" is resolved to one up front: GitHub
# answers /releases/latest with a redirect to /releases/tag/<tag>. A mirror
# (KOTO_BASE_URL) has no such redirect — name the version you want from it.
if [ "$VERSION" = latest ]; then
	[ -z "${KOTO_BASE_URL:-}" ] ||
		die "KOTO_BASE_URL needs --version: the signed SHA256SUMS must name the release you asked for"
	VERSION="$(curl -fsSI --proto '=https' --connect-timeout 20 "https://github.com/$REPO/releases/latest" |
		tr -d '\r' | awk 'tolower($1) == "location:" { n = split($2, p, "/"); print p[n] }' | tail -1)"
	[ -n "$VERSION" ] && [ "$VERSION" != latest ] ||
		die "could not resolve the latest koto release — pass --version"
fi
# The version as release tooling writes it into SHA256SUMS and asset names.
VER="${VERSION#v}"

# KOTO_BASE_URL overrides where assets come from — a mirror, or a test origin.
# It changes only DELIVERY: the signature check below is unconditional, so a
# mirror cannot serve anything this script will accept without the release key.
if [ -n "${KOTO_BASE_URL:-}" ]; then
	BASE="$KOTO_BASE_URL"
else
	BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

echo "koto release $VERSION ($ARCH) from $BASE"

# --- the source tree -------------------------------------------------------
# A full install fetches INTO a fresh shallow clone of the release tag, and
# holds the download to that tag's committed manifest.
SRC=""
if [ "$DO_INSTALL" -eq 1 ]; then
	# /var/tmp, not /tmp: /tmp is tmpfs on Fedora, and the clone holds the
	# unpacked 2 GiB rootfs until `koto install` has copied it into place.
	SRC="$(mktemp -d "${TMPDIR:-/var/tmp}/koto-install.XXXXXX")"
	trap 'rm -rf "$SRC"' EXIT INT TERM
	step "source: $REPO_URL @ $VERSION"
	gq=--quiet
	[ "$TTY" -eq 0 ] || gq=--progress
	git -c advice.detachedHead=false clone $gq --depth 1 --branch "$VERSION" "$REPO_URL" "$SRC/koto" ||
		die "could not clone $REPO_URL at $VERSION"
	DEST="$SRC/koto"
	MANIFEST="$DEST/artifacts.sha256"
	# The tag is not signed. A tag author without the release key could commit
	# trust anchors (creds/ca.key, server.key, clients.allow, tokens.json,
	# acl.json) or standing orders (schedules.json, goals.json) for a fresh
	# install to adopt. Nothing legitimate ships them, so remove them outright.
	( cd "$DEST" && rm -rf -- creds groups groups*.json schedules.json goals.json metrics.jsonl ) ||
		die "could not scrub daemon state from the clone"
fi
[ -n "$DEST" ] || DEST="$PWD"

# --- fetch and verify ------------------------------------------------------
# Stage BESIDE the destination: the same filesystem makes the final move a
# rename rather than a second copy of a 2 GiB rootfs.
mkdir -p "$DEST"
DEST="$(cd "$DEST" && pwd)"
STAGE="$(mktemp -d "$DEST/.koto-fetch.XXXXXX")"
trap 'rm -rf "$STAGE" ${SRC:+"$SRC"}' EXIT INT TERM
GNUPGHOME="$STAGE/gnupg"
export GNUPGHOME
mkdir -p -m 700 "$GNUPGHOME"

step "release signing key $KEY_FPR"
if [ -n "$PUBKEY" ]; then
	cp "$PUBKEY" "$STAGE/key.asc"
	echo "    from $PUBKEY"
else
	get "$KEYSERVER/$KEY_FPR" "$STAGE/key.asc"
	echo "    from $KEYSERVER"
fi
gpg --batch --quiet --import "$STAGE/key.asc" 2>/dev/null || die "could not import the release key"
got_fpr="$(gpg --batch --with-colons --list-keys | awk -F: '/^fpr:/ {print $10; exit}')"
[ "$got_fpr" = "$KEY_FPR" ] ||
	die "the key delivered has fingerprint $got_fpr, not $KEY_FPR"

step "signature on SHA256SUMS"
get "$BASE/SHA256SUMS" "$STAGE/SHA256SUMS"
get "$BASE/SHA256SUMS.asc" "$STAGE/SHA256SUMS.asc"
# Judge gpg by its exit status AND its status lines. VALIDSIG alone is not
# enough: gpg prints it for a signature from an expired or revoked key too
# (alongside EXPKEYSIG / REVKEYSIG), and a pipe into grep would hide the exit.
grc=0
gpg --batch --status-fd=1 --verify "$STAGE/SHA256SUMS.asc" "$STAGE/SHA256SUMS" \
	>"$STAGE/gpg.status" 2>/dev/null || grc=$?
[ "$grc" -eq 0 ] ||
	die "gpg rejected the signature on SHA256SUMS (exit $grc) — nothing was installed"
if grep -Eq '^\[GNUPG:\] (EXPKEYSIG|REVKEYSIG|EXPSIG|BADSIG|ERRSIG|NO_PUBKEY)( |$)' "$STAGE/gpg.status"; then
	die "SHA256SUMS is signed by an expired or revoked key, or the signature is bad: $(grep -Eo '(EXPKEYSIG|REVKEYSIG|EXPSIG|BADSIG|ERRSIG|NO_PUBKEY)' "$STAGE/gpg.status" | head -1) — nothing was installed"
fi
grep -q '^\[GNUPG:\] GOODSIG ' "$STAGE/gpg.status" &&
	grep -q "^\[GNUPG:\] VALIDSIG $KEY_FPR " "$STAGE/gpg.status" ||
	die "SHA256SUMS is not signed by $KEY_FPR — nothing was installed"
echo "    good signature from $KEY_FPR"

# The signature covers WHICH release this is, not just "some release by this
# key": release tooling writes exactly one '# koto-version: X.Y.Z' line into
# the signed file. Anything else is an older (or other) signed set served
# under this tag or by a mirror — a rollback — and is refused.
signed_ver="$(awk '/^# koto-version: / { v = $3; n++ } END { if (n != 1) exit 1; print v }' "$STAGE/SHA256SUMS")" ||
	die "SHA256SUMS does not name exactly one koto-version — it is not a $VER release, nothing was installed"
[ "$signed_ver" = "$VER" ] ||
	die "SHA256SUMS is signed for koto $signed_ver, not the $VER you asked for — refusing a rollback, nothing was installed"
echo "    signed for koto $signed_ver"
grep -v '^#' "$STAGE/SHA256SUMS" >"$STAGE/SHA256SUMS.body"

# Asset names come out of the signed file: <name>_<version>_<arch>.zst, where
# the version is the component's own (firecracker_1.17.0, vmlinux_6.1.186,
# rootfs.img_fedora44). Map each back to the path koto expects by stripping
# the _<version>_<arch>.zst tail. A release is all five, each once, and koto
# and koto-tui carry the release's own version: a set missing one would fall
# back to whatever the clone has, so it is refused before anything downloads.
assets="$(awk '{print $2}' "$STAGE/SHA256SUMS.body")"
[ -n "$assets" ] || die "SHA256SUMS lists no assets"
have=" "
for a in $assets; do
	case "$a" in
	*_"$ARCH".zst) ;;
	*) die "SHA256SUMS names $a, which is not a $ARCH asset" ;;
	esac
	base="${a%_"$ARCH".zst}"
	name="${base%_*}"
	case "$name" in
	koto | koto-tui)
		[ "${base##*_}" = "$VER" ] ||
			die "SHA256SUMS lists $a, which is not koto $VER — nothing was installed" ;;
	firecracker | vmlinux | rootfs.img) ;;
	*) die "unexpected asset $a" ;;
	esac
	case "$have" in *" $name "*) die "SHA256SUMS lists $name twice" ;; esac
	have="$have$name "
done
for want in koto koto-tui firecracker vmlinux rootfs.img; do
	case "$have" in
	*" $want "*) ;;
	*) die "SHA256SUMS has no $want asset — a release is all five, nothing was installed" ;;
	esac
done
n=$(echo "$assets" | wc -w)
step "downloading $n assets"
i=0
for a in $assets; do
	i=$((i + 1))
	echo "    ($i/$n) $a"
	getp "$BASE/$a" "$STAGE/$a"
	echo "          $(size "$STAGE/$a")"
done

step "checking the assets against the signed SHA256SUMS"
(cd "$STAGE" && sha256sum -c SHA256SUMS.body >/dev/null) ||
	die "an asset does not match the signed SHA256SUMS — nothing was installed"
echo "    all $n match"

step "unpacking into $DEST"
mkdir -p "$STAGE/out/fcassets"
outs=""
for a in $assets; do
	base="${a%_"$ARCH".zst}"    # koto-tui_1.0.0    / rootfs.img_fedora44
	name="${base%_*}"           # koto-tui          / rootfs.img
	case "$name" in
	koto | koto-tui) rel="$name" ;;
	firecracker | vmlinux | rootfs.img) rel="fcassets/$name" ;;
	*) die "unexpected asset $a" ;;
	esac
	echo "    $rel"
	zstd -qdf --sparse "$STAGE/$a" -o "$STAGE/out/$rel" || die "could not decompress $a"
	outs="$outs $rel"
done
chmod +x "$STAGE/out/koto" "$STAGE/out/koto-tui" \
	"$STAGE/out/fcassets/firecracker" "$STAGE/out/fcassets/vmlinux" 2>/dev/null || true
if [ -n "$MANIFEST" ]; then
	grep -qv '^#' "$MANIFEST" ||
		die "$(basename "$MANIFEST") at $VERSION has no entries — that is not a published release, nothing was installed"
	(cd "$STAGE/out" && sha256sum -c "$MANIFEST" >/dev/null) ||
		die "the unpacked artifacts do not match $MANIFEST — nothing was installed"
	echo "    unpacked bytes match $(basename "$MANIFEST")"
fi

# Move the files in one at a time. An `rm -rf` of the destination followed by
# a rename would wipe a clone when the destination is one (`make fetch` passes
# --dest .).
mkdir -p "$DEST/fcassets"
for rel in $outs; do
	mv -f "$STAGE/out/$rel" "$DEST/$rel"
done
rm -rf "$STAGE"
echo "verified and unpacked $n artifacts into $DEST"

if [ "$DO_INSTALL" -eq 0 ]; then
	[ -z "$NEXT" ] || echo "next:  $NEXT"
	exit 0
fi

# --- install, configure, attach --------------------------------------------
cd "$DEST"
if [ "$HAVE_TTY" -eq 0 ]; then
	step "koto install (it echoes each sudo command before running it)"
	./koto install $YES </dev/null
	echo
	echo "no terminal, so stopping here. To finish:"
	echo "  koto setup     # mint TLS identities, connect credentials, start the daemon"
	echo "  koto tui       # /new <name> spawns your first agent, /exit detaches"
	exit 0
fi

step "koto install (it echoes each sudo command before running it)"
./koto install $YES </dev/tty

# Everything the install needed from the clone is now in the state dir and on
# PATH, so drop the clone before handing over — the last stage replaces this
# process, and nothing would be left to clean it up.
cd /
rm -rf "$SRC"
trap - EXIT INT TERM

KOTO=/usr/local/bin/koto
step "koto setup — TLS identities, credentials, first start"
"$KOTO" setup $YES </dev/tty ||
	die "setup did not finish — re-run \`koto setup\`; every step it completed is detected and skipped"

if [ "$ATTACH" -eq 0 ]; then
	echo "koto $("$KOTO" version) is installed and running."
	exit 0
fi

step "koto tui — /new <name> spawns your first agent, /exit detaches"
exec "$KOTO" tui </dev/tty
