# Every build target is keyed on the REAL FILE it produces — the koto and
# koto-tui binaries, fcassets/{firecracker,vmlinux,rootfs.img} — compared
# against its actual source inputs, so a target is a no-op once its output is
# current and rebuilds the moment the output is deleted or its inputs change.
# (The .build/ marker files this used to use are gone; `clean` still removes
# the directory so an upgraded checkout does not keep stale ones around.)
#
# Podman is a BUILD-time dependency only: it compiles the Go binaries (so the
# host needs no Go toolchain) and builds the guest kernel and rootfs. Nothing
# koto runs at runtime is a container — the daemon is a systemd service on the
# host and the TUI is a plain binary.

.PHONY: hooks secrets-scan build fetch verify wizard require-artifacts setup install uninstall dev dev-tui dev-env dev-env-off dev-shell tui-walk tui-build login host-run tui stop run proxy ctl-build metrics clean clean-groups clean-creds proto-gen proto-verify pki-init pki-client firecracker kernel rootfs assets

# Pinned codegen toolchain (6-week dependency-lag rule). Versions verified
# >=6 weeks old as of 2026-06-14 via proxy.golang.org:
#   protobuf            v1.36.11 — 2025-12-12
#   protoc-gen-go-grpc  v1.6.1   — 2026-02-04
# (grpc runtime v1.80.0 / 2026-04-01 is pinned in the go.mod files.)
PROTOC_GEN_GO_VER      := v1.36.11
PROTOC_GEN_GO_GRPC_VER := v1.6.1

# --- first run ---------------------------------------------------------------
# GETTING koto RUNNING IS THREE STAGES, and each one owns exactly its own job:
#
#   1. ACQUIRE     the five artifacts, by one of two routes
#   2. INTEGRATE   them into the system            (make install)
#   3. CONFIGURE   the installed system            (make wizard)
#
#   make fetch && make install && make wizard      download — minutes
#   make build && make install && make wizard      build it yourself — ~40 min
#
# The two acquire routes produce the SAME five files: ./koto, ./koto-tui, and
# fcassets/{firecracker,vmlinux,rootfs.img}. Nothing downstream can tell which
# route produced them.
#
# WHY BOTH ROUTES. `make fetch` is for people who want to run koto. `make
# build` is the don't-trust-verify path for people who cloned the repo: it
# builds the daemon, the TUI, the Firecracker VMM, the guest kernel and the
# guest rootfs from source, sequentially, each inside a digest-pinned
# container, so the host needs no toolchain of its own — only git, make and
# podman/docker. `make verify` closes the loop: build from source, then check
# the bytes you produced against the checksums published for the release.
#
# WHY THE WIZARD IS LAST. It configures an INSTALLED system — it mints the PKI
# and connects credentials straight into the state dir, so there is one place
# credentials live and no copy from the clone to get wrong. It follows that a
# fresh `make install` enables the unit but does NOT start it: the daemon
# cannot come up before the wizard has minted its server certificate.
#
# Each stage refuses to do the previous one's work, and says which command
# does. Safe to re-run: make skips what is up to date and every wizard step
# detects whether it is already done, so an interrupted run resumes here.
# --- container runtime (BUILD-time only) ------------------------------------
# The binaries are built in a container so THE HOST NEVER NEEDS GO. That is a
# hard requirement, not a convenience: `make build`, `make install` and
# `make setup` all compile inside the image below. The only targets that touch
# a host Go toolchain are `run`/`proxy`, which exist purely for fast iteration
# by people who already have one.
#
# Either docker or podman works, docker preferred when both are present.
#
# One build covers every target distribution. The binaries are CGO_ENABLED=0
# pure Go, so a single static executable runs on Fedora, Ubuntu, Debian,
# Alpine — anything linux/amd64. There is nothing per-distro to produce; what
# would need a separate build is a different ARCHITECTURE, and koto is x86_64
# only (Firecracker assets and the guest kernel are built for it, and
# checkPlatform enforces it).
#
# Every build step honours this, including `make rootfs` — it used to be
# podman-only (it needed `podman unshare` to preserve in-image ownership), and
# now does that stage inside a container instead, which both engines can do.
# $(CURDIR), never $(PWD), throughout this file. $(PWD) is the CALLER'S shell
# variable, inherited from the environment — under `make -C /path/to/koto` from
# somewhere else it still holds the caller's directory, so every path built
# from it points outside the checkout. Measured: `cd /tmp && make -C ~/koto`
# gave PWD=/tmp and CURDIR=~/koto, which would have bind-mounted
# /tmp as the build source and put the dev state dir in /tmp/.dev. $(CURDIR) is
# make's own working directory and is always this file's.
CONTAINER ?= $(shell command -v docker 2>/dev/null || command -v podman 2>/dev/null)
CONTAINER_NAME := $(notdir $(CONTAINER))
# Rootless podman maps your uid to root inside, so build outputs come back
# owned by you. Rootful docker would write them as root, so ask it for our id
# explicitly — and then point Go's caches somewhere that id can write, since
# /root is not it.
ifeq ($(CONTAINER_NAME),docker)
  CONTAINER_USER := --user $(shell id -u):$(shell id -g)
else
  CONTAINER_USER :=
endif
# Pinned by DIGEST, not tag. This is what actually makes the build
# reproducible — the engine does not: docker and podman run the same OCI
# image and produce the same bytes. `golang:1.25-alpine` is a moving tag that
# silently changes toolchain patch versions under you; the digest does not.
# Combined with -trimpath and CGO_ENABLED=0, two builds of the same commit
# give identical binaries, except for the version string stamped below.
# Update deliberately: podman/docker pull golang:1.25-alpine, then
#   podman inspect --format '{{index .RepoDigests 0}}' golang:1.25-alpine
# golang:1.25.14-alpine (2026-08-19). Go 1.24 fell out of support when 1.26
# shipped, so its stdlib no longer receives security fixes: govulncheck on the
# release toolchain (2026-09-05) reported 13 reachable stdlib vulnerabilities
# fixed only in 1.25.x. Go patch releases are the ONE exception to the 6-week
# lag besides Firecracker, for the same reason — they ARE the security fixes,
# and sitting behind them is deliberately running known-vulnerable code.
# Keep in step with the `toolchain` lines in every go.mod / go.work and the
# go-version in .github/workflows/govulncheck.yml.
GO_IMAGE ?= docker.io/library/golang@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59
# Cache paths are passed as env rather than mounted over /root, so the same
# invocation works whether we are root in the container (podman) or not.
GO_BUILD_RUN = $(CONTAINER) run --rm --security-opt label=disable \
	  $(CONTAINER_USER) \
	  -v $(CURDIR):/src -w /src \
	  -v $(CURDIR)/.gocache:/gocache -v $(CURDIR)/.gomodcache:/gomodcache \
	  -e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp \
	  -e CGO_ENABLED=0 -e GOFLAGS= \
	  $(GO_IMAGE)

define need-container
@test -n "$(CONTAINER)" || { echo "podman or docker is required to build koto (sudo dnf install podman / sudo apt install podman)"; exit 1; }
@mkdir -p .gocache .gomodcache
endef

KOTO_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)

# The five files that ARE koto at runtime. Both acquisition routes produce
# exactly these, and nothing downstream can tell which route did it.
FCASSETS  := fcassets
ARTIFACTS := koto koto-tui $(FCASSETS)/firecracker $(FCASSETS)/vmlinux $(FCASSETS)/rootfs.img
koto: $(wildcard daemon/*.go) $(wildcard protocol/pb/*.go)
	$(need-container)
	$(GO_BUILD_RUN) \
	  go build -trimpath -ldflags "-s -w -X main.kotoVersion=$(KOTO_VERSION)" -o koto ./daemon

# --- stage 1, route 1: build every byte yourself ----------------------------
# Sub-makes rather than prerequisites, so the order holds under `make -j`:
# this is the route people run to watch it happen, and a kernel compile
# interleaved with a rootfs build reads as noise. Each step is a no-op once
# its artifact is current, so an interrupted run resumes here.
build:
	@echo "==> building all koto artifacts from source ($(KOTO_VERSION), via $(CONTAINER_NAME))"
	$(MAKE) koto
	$(MAKE) koto-tui
	$(MAKE) firecracker
	$(MAKE) kernel
	$(MAKE) rootfs
	@echo
	@echo "built: $(ARTIFACTS)"
	@echo "next:  make install"

# --- stage 3: configure the installed system --------------------------------
# No prerequisites on purpose: this stage does not acquire and does not
# install. If either earlier stage is incomplete, the wizard's own first step
# says so and names the command that fixes it.
wizard:
	@test -x ./koto || { \
	  echo "./koto is missing — acquire the artifacts first:"; \
	  echo "    make fetch     download them (minutes)"; \
	  echo "    make build     build them yourself (~40 min cold)"; \
	  exit 1; }
	@./koto setup

# Shorthand for the source route end to end. Kept because it is the command
# every doc has pointed at for a year; it is exactly the three stages in order.
setup:
	$(MAKE) build
	$(MAKE) install
	$(MAKE) wizard

# --- stage 1, route 2: download what was published --------------------------
# THE MANIFEST IS THE TRUST ANCHOR, NOT THE HOST. dist/artifacts.sha256 is
# committed to this repo, so the checksums reach you over git — with whatever
# review and signing the repo has — rather than over the same connection as
# the bytes they vouch for. KOTO_DIST_URL can point anywhere, a mirror or a
# file:// path included; the check does not change.
#
# The manifest lists the artifacts as INSTALLED, not as transferred: rootfs.img
# ships compressed and is checked after decompression, so one manifest serves
# both routes — `make verify` holds a local build to the same line.
KOTO_DIST_URL ?= https://kotovm.com/dist
MANIFEST      := dist/artifacts.sha256
DIST_VERSION   = $(strip $(shell cat dist/VERSION 2>/dev/null))

# Published name -> local path. rootfs is the only one transferred compressed;
# at ~2G apparent (mostly holes) it is the one where it matters.
fetch:
	@test -n "$(DIST_VERSION)" || { echo "dist/VERSION is missing or empty"; exit 1; }
	@if [ "$(DIST_VERSION)" = "unreleased" ]; then \
	  echo "no koto release has been published yet, so there is nothing to fetch."; \
	  echo "build the artifacts from source instead:"; \
	  echo "    make build && make install && make wizard"; \
	  exit 1; \
	fi
	@command -v curl >/dev/null || { echo "curl is required to fetch artifacts"; exit 1; }
	@command -v zstd >/dev/null || { echo "zstd is required to unpack rootfs.img.zst"; exit 1; }
	@mkdir -p $(FCASSETS)
	@base="$(KOTO_DIST_URL)/$(DIST_VERSION)"; \
	echo "==> fetching koto $(DIST_VERSION) from $$base"; \
	for pair in koto:koto koto-tui:koto-tui \
	            firecracker:$(FCASSETS)/firecracker vmlinux:$(FCASSETS)/vmlinux; do \
	  name=$${pair%%:*}; dest=$${pair#*:}; \
	  echo "    $$name -> $$dest"; \
	  curl -fsSL --retry 3 -o "$$dest.part" "$$base/$$name" || { \
	    rm -f "$$dest.part"; echo "failed to fetch $$name from $$base"; exit 1; }; \
	  mv "$$dest.part" "$$dest"; \
	done; \
	echo "    rootfs.img.zst -> $(FCASSETS)/rootfs.img (decompressing)"; \
	curl -fsSL --retry 3 -o "$(FCASSETS)/rootfs.img.zst" "$$base/rootfs.img.zst" || { \
	  rm -f "$(FCASSETS)/rootfs.img.zst"; echo "failed to fetch rootfs.img.zst from $$base"; exit 1; }; \
	zstd -qdf --sparse "$(FCASSETS)/rootfs.img.zst" -o "$(FCASSETS)/rootfs.img" || exit 1; \
	rm -f "$(FCASSETS)/rootfs.img.zst"
	@chmod +x koto koto-tui $(FCASSETS)/firecracker $(FCASSETS)/vmlinux
	@$(MAKE) --no-print-directory verify
	@echo "next:  make install"

# --- verify -----------------------------------------------------------------
# Holds whatever is in the working tree — fetched or locally built — to the
# checksums published for this release. Run it after `make build` and you are
# checking that building from source reproduces the bytes on kotovm.com; run
# it after `make fetch` (which does, automatically) and you are checking the
# download.
#
# CAVEAT, stated because a verify step that quietly always fails is worse than
# none: only koto and koto-tui are expected to reproduce bit-for-bit. They are
# CGO_ENABLED=0 and -trimpath, built in a digest-pinned image, with the version
# string the only input that varies — so two builds of the same commit give
# identical bytes. The guest kernel and rootfs embed build timestamps and
# resolved package versions and do NOT reproduce; a mismatch there means your
# image differs from the published one, which is expected, not alarming.
verify:
	@test -s $(MANIFEST) || { echo "$(MANIFEST) is empty — nothing to verify against"; exit 1; }
	@grep -qv '^#' $(MANIFEST) || { \
	  echo "no checksums published yet ($(MANIFEST) has no entries)."; \
	  echo "there is nothing to verify against until a release exists."; \
	  exit 1; }
	@sha256sum -c $(MANIFEST)

# Guard for targets that consume the artifacts without producing them.
require-artifacts:
	@missing=""; for a in $(ARTIFACTS); do [ -e "$$a" ] || missing="$$missing $$a"; done; \
	if [ -n "$$missing" ]; then \
	  echo "missing artifact(s):$$missing"; \
	  echo "acquire them first:"; \
	  echo "    make fetch     download them (minutes)"; \
	  echo "    make build     build them yourself (~40 min cold)"; \
	  exit 1; \
	fi

# --- stage 2: integrate into the system -------------------------------------
# State dir, binaries on PATH, /etc/koto/koto.env, the systemd unit. Takes the
# artifacts as given — acquire them with `make fetch` or `make build` first —
# and configures nothing: a fresh install enables the unit but leaves it
# stopped, because the daemon needs the server certificate `make wizard` mints.
install:
	@$(MAKE) --no-print-directory require-artifacts
	@./koto install
	@echo "next:  make wizard"

# The inverse of `install`, and only of it: stops the service (so every guest
# unmounts its workspace image cleanly), then removes the unit and the
# binaries. Your state dir survives — deleting that is `./koto uninstall
# --purge`, which is prompted, and is deliberately NOT wired to a make target:
# `make clean-groups` is already the destructive verb people know, and a
# second one a tab-completion away from `make install` is a footgun.
uninstall:
	@if [ -x ./koto ]; then ./koto uninstall; \
	elif command -v koto >/dev/null; then koto uninstall; \
	else echo "no koto binary here or on PATH — nothing to uninstall"; fi

# INSTANCE (opt-in): run a second daemon+TUI side by side, e.g. from a git
# worktree — `make host-run INSTANCE=cli`, `make tui INSTANCE=cli`,
# `make stop INSTANCE=cli`. Every other piece of state (groups/, groups.json,
# creds/, run/, .gocache, .gomodcache) is already resolved relative to the
# working directory, so a second worktree gets its own automatically; only
# the daemon container's *name* is hardcoded enough to collide across
# instances, so that's the only thing this threads through. koto-net
# stays shared on purpose — containers on the same bridge don't collide on
# port/address, since each has its own network namespace.
INSTANCE ?=
CS_HOST_NAME := cs_host_go$(if $(INSTANCE),_$(INSTANCE))

BUILD := .build

# --- sidecar scripts --------------------------------------------------------
# + stream_filter.js + start-chrome.sh run INSIDE the
# firecracker guest (baked into the rootfs — see rootfs). They no longer
# build a standalone podman image (the podman group runtime is retired); this
# just tracks them as inputs so an edit triggers an rootfs rebuild.
SIDECAR_SRC := sidecar/stream_filter.js sidecar/venice_stream.js sidecar/cs-job sidecar/cs-subagent sidecar/cs-notify

# Every image rebuild moves its tag, orphaning the previous build as a
# dangling <none> image (~650MB per rootfs build — they once accumulated to
# 7GB and filled the disk). Sweep dangling images after each build target.
# Never touches tagged images or the layers the live containers use.
define prune-dangling
	@podman image prune -f >/dev/null 2>&1 || true
endef

# --- the daemon --------------------------------------------------------
# There is no daemon image any more: koto runs directly on the host, and
# podman is a BUILD-time dependency only (the `koto` target below compiles in
# a container so the host needs no Go; the guest kernel and rootfs likewise).
# `make koto` produces the binary; `make install` puts it under systemd.

# --- koto-tui (host binary) ---------------------------------------------
# Built in a container so the host needs no Go, but the ARTIFACT is a plain
# static binary that runs on the host. The TUI used to run inside a container
# too; that bought nothing (it shells out to nothing, so the supply-chain
# argument is a property of the binary) and cost the real terminal — TERM was
# clobbered, notifications had no D-Bus, and it mounted all of creds/.
TUI_GO_SRC := $(wildcard tui/*.go) tui/go.mod $(wildcard tui/go.sum)
PROTO_SRC  := $(wildcard protocol/pb/*.go) protocol/go.mod protocol/koto.proto protocol/guest.proto
koto-tui: $(TUI_GO_SRC) $(PROTO_SRC)
	$(need-container)
	$(GO_BUILD_RUN) \
	  sh -c 'cd tui && go build -trimpath -ldflags "-s -w" -o /src/koto-tui .'

tui-build: koto-tui

# Frame-integrity gate. tools/tuiwalk/walk.py still drives the TUI as a podman
# container (`podman run --network koto-net -e KOTO_ENDPOINT=cs_host_go:8443`),
# which is the pre-0d5848b architecture — `koto tui` is a plain host binary
# now, so the script cannot run as written and needs porting to exec that
# binary in a pty. Until then this target FAILS rather than silently passing:
# it was named in .PHONY with no rule at all, so `make tui-walk` printed
# "Nothing to be done" and exited 0, which reads as a passing gate.
tui-walk:
	@echo "make: *** tui-walk is not runnable: tools/tuiwalk/walk.py targets the"
	@echo "    container-era TUI (podman run --network koto-net), but koto tui is"
	@echo "    a host binary now. Port walk.py to exec the binary in a pty."
	@echo "    Until then the wrap/scroll gate is NOT covered — do not report it"
	@echo "    as checked. See .claude/skills/release-test/SKILL.md section 7."
	@exit 1

# --- run / interactive targets ---------------------------------------------
# OAuth into ./creds. A thin alias for `koto claude-login` now: that command
# owns the flow (for a dev clone AND an installed system), and going through
# it means the clone gets the same shadowing check and upstream verification
# the installed path gets, rather than a second copy of `claude auth login`
# that drifts. Still needs the host's claude CLI — a runtime dependency, like
# the e2fsprogs tools (see `koto setup --check`).
login: koto
	@mkdir -p creds
	./koto claude-login --method oauth

# Dev: run the daemon in the foreground, straight from source. It puts itself
# in a user namespace first (daemon/userns.go) so the jailer can hand each VMM
# its own uid — what the podman container used to provide.
host-run: koto
	@# $HOME/.claude must resolve to ./creds, or the proxy and the `claude` it
	@# shells out to for token refresh read your PERSONAL ~/.claude. The
	@# container got this by mounting creds/ at /root/.claude.
	@test -e .claude || ln -s creds .claude
	HOME=$(CURDIR) ./koto daemon

tui: koto koto-tui
	@test -f $(CURDIR)/creds/client-tui.crt || { echo "no TUI client cert — run \`./koto pki init && ./koto pki client tui\`"; exit 1; }
	@KOTO_HOME=$(CURDIR) ./koto tui

# Stop the dev daemon. SIGTERM reaches it directly now (no podman in the
# middle): it stops every microVM so each guest sync+umounts its workspace
# image, which is what keeps the images clean across restarts.
stop:
	-pkill -TERM -u $$(id -u) -f '^\./koto daemon$$' 2>/dev/null || true
	@echo "sent SIGTERM to the dev daemon (if running); installed service: sudo systemctl stop koto"

# --- dev loop beside an INSTALLED koto --------------------------------------
# The situation this exists for: koto is installed and serving a real fleet,
# and you are changing the daemon. Restarting the service to try an edit stops
# every running microVM, so instead run a SECOND daemon out of the clone with
# its own state dir, its own ports and its own guests. Nothing you break
# reaches the installed service, and the loop is `make dev` again.
#
# .dev/ MIRRORS THE INSTALLED LAYOUT, and that is the point rather than
# tidiness: it lets HOME=$(DEV) resolve $HOME/.claude to koto's own creds, the
# way the unit does. The clone itself cannot be the state dir — Claude Code
# keeps a project-local .claude/ DIRECTORY here, so $HOME/.claude/.credentials.json
# would name a file that does not exist, the proxy would find no credential,
# and every turn would 401 while `creds/.credentials.json` sat there unread.
#
# BOTH port knobs have to move or the second daemon collides with the first:
# KOTO_PORT is the gRPC listener (8443), PROXY_PORT the base for the per-group
# credential-injecting proxy listeners (8787, one per group).
DEV       := $(CURDIR)/.dev
DEV_PORT  ?= 8444
DEV_PROXY ?= 9500
# WHERE THE DEV DAEMON GETS ITS ANTHROPIC CREDENTIAL, and this one is not a
# preference: OAuth refresh tokens ROTATE. Copying .credentials.json into a
# second state dir forks the chain — the first daemon to refresh rotates the
# token and the other copy is dead for good (measured: the copy came back with
# expiresAt 0 and every turn 401'd). So the dev daemon does not get a copy; it
# is pointed at the INSTALLED credentials file, which is one file with one
# refresh chain that both daemons read and both refreshes land in.
#
# HOME is the knob because that is what resolves the credential: the proxy's
# default CRED_PATH is $HOME/.claude/.credentials.json, and `refresh()` shells
# out to `claude` which writes to that same path. Point them anywhere apart
# and a refresh "succeeds" into a file nobody reads.
#
# With no installed koto, this falls back to the dev state dir and you mint a
# credential of its own there: KOTO_HOME=$(DEV) ./koto claude-login
DEV_HOME  ?= $(shell test -f /var/lib/koto/creds/.credentials.json && echo /var/lib/koto || echo $(CURDIR)/.dev)

dev: koto $(DEV)/.stamp $(DEV)/env $(DEV)/env-off
	@echo "dev daemon → grpc 127.0.0.1:$(DEV_PORT), proxy base $(DEV_PROXY), state $(DEV)"
	@echo "drive it   → . $(DEV)/env      (in another shell; . $(DEV)/env-off to undo)"
	@echo "stop it    → ctrl-c, or \`make stop\` from another shell"
	@echo "credential → $(DEV_HOME)/creds/.credentials.json (shared, not copied)"
	@KOTO_HOME=$(DEV) HOME=$(DEV_HOME) KOTO_PORT=$(DEV_PORT) PROXY_PORT=$(DEV_PROXY) ./koto daemon

# Pointing a SHELL at the dev daemon. make cannot export into your shell —
# recipes run in child processes — so these are the only two honest shapes,
# and both keep the values defined here rather than in a second file that
# drifts:
#
#   . .dev/env                   this shell now talks to the dev daemon
#   . .dev/env-off               ...and back
#   make dev-shell               a subshell that already has them; exit leaves
#   eval "$$(make dev-env)"      the same thing without the generated file
#
# The .dev/env files are GENERATED from the targets below, not hand-written,
# so sourcing one cannot drift from what `make dev` actually runs; they are
# rebuilt whenever this file changes. They exist because `. file` is the
# ordinary way to put variables into a shell and `eval "$$(...)"` is not —
# same effect, one fewer construct to trust.
#
# KOTO_HOME is in the set deliberately, not just the ctl trio: it is what
# `koto tui -state` and `koto claude-login` resolve, so without it the shell
# would be half-switched — ctl talking to dev while a login reconfigured the
# INSTALLED daemon. CURDIR goes on PATH so plain `koto` is the dev build.
# KOTO_CLIENT is tui, not agent: on a dev instance you want every verb,
# including the admin-only ones (runscript, acl, attach-shell).
dev-env:
	@echo 'export KOTO_HOME=$(DEV)'
	@echo 'export KOTO_ADDR=127.0.0.1:$(DEV_PORT)'
	@echo 'export KOTO_CREDS_DIR=$(DEV)/creds'
	@echo 'export KOTO_CLIENT=tui'
	@echo 'export PATH=$(CURDIR):$$PATH'
	@echo '# The prompt says which koto you are talking to, because the whole'
	@echo '# hazard of this setup is forgetting. Tagged with the PORT, so two'
	@echo '# dev daemons from two worktrees are told apart rather than both'
	@echo '# reading "dev". Prefixed ONCE and guarded on the saved copy, so'
	@echo '# sourcing twice does not stack; the saved copy is a plain shell'
	@echo '# variable, never exported, or every child would inherit a prompt.'
	@echo 'if [ -z "$${KOTO_DEV_OLD_PS1+set}" ]; then'
	@echo '  if [ -n "$${ZSH_VERSION:-}" ]; then'
	@echo '    KOTO_DEV_OLD_PS1="$$PROMPT"'
	@echo '    PROMPT="%F{magenta}(koto-dev:$(DEV_PORT))%f $$PROMPT"'
	@echo '  elif [ -n "$${BASH_VERSION:-}" ]; then'
	@echo '    KOTO_DEV_OLD_PS1="$$PS1"'
	@echo '    PS1="\[\033[35m\](koto-dev:$(DEV_PORT))\[\033[0m\] $$PS1"'
	@echo '  fi'
	@echo 'fi'
	@echo '# dev daemon :$(DEV_PORT), state $(DEV), credential $(DEV_HOME)/creds/.credentials.json'
	@echo '# undo: . $(DEV)/env-off   (or: eval "$$(make dev-env-off)")'

dev-env-off:
	@echo 'unset KOTO_HOME KOTO_ADDR KOTO_CREDS_DIR KOTO_CLIENT'
	@echo 'export PATH=$${PATH#$(CURDIR):}'
	@echo 'if [ -n "$${KOTO_DEV_OLD_PS1+set}" ]; then'
	@echo '  if [ -n "$${ZSH_VERSION:-}" ]; then PROMPT="$$KOTO_DEV_OLD_PS1"; else PS1="$$KOTO_DEV_OLD_PS1"; fi'
	@echo '  unset KOTO_DEV_OLD_PS1'
	@echo 'fi'
	@echo '# back to the installed daemon on :8443'

# Generated, with this Makefile as the prerequisite: change a port above and
# the next `make dev` rewrites them. Order-only on the stamp, since the state
# dir has to exist to hold them but its mtime means nothing here.
$(DEV)/env: Makefile | $(DEV)/.stamp
	@$(MAKE) --no-print-directory dev-env > $@

$(DEV)/env-off: Makefile | $(DEV)/.stamp
	@$(MAKE) --no-print-directory dev-env-off > $@

# A subshell with the environment already set. $$SHELL, so you keep your own
# shell and its history; `exit` is the off switch, which is why this needs no
# undo target of its own.
#
# It cannot simply pass the variables in the environment: an interactive shell
# reads its rc AFTER inheriting them, and the rc is what sets PROMPT — so the
# prompt tag would be overwritten a moment after being handed over, and this
# would be the one path missing the marker. So the rc files below chain: your
# real rc first, then .dev/env, which is the order that makes the tag stick.
# zsh takes ZDOTDIR (hence a directory, and a .zshenv so ZDOTDIR does not also
# hide your own); bash takes --rcfile. Any other shell gets the plain
# environment and no tag, which is honest rather than broken.
dev-shell: $(DEV)/env $(DEV)/rc/.zshrc $(DEV)/rc/bashrc
	@echo "koto dev shell → :$(DEV_PORT), state $(DEV). exit to leave."
	@case "$$(basename "$${SHELL:-/bin/sh}")" in \
	  zsh)  KOTO_DEV_RC_HOME="$${ZDOTDIR:-$$HOME}" ZDOTDIR="$(DEV)/rc" "$$SHELL" -i ;; \
	  bash) KOTO_DEV_RC_HOME="$$HOME" "$$SHELL" --rcfile "$(DEV)/rc/bashrc" -i ;; \
	  *)    KOTO_HOME=$(DEV) KOTO_ADDR=127.0.0.1:$(DEV_PORT) \
	        KOTO_CREDS_DIR=$(DEV)/creds KOTO_CLIENT=tui PATH=$(CURDIR):$$PATH "$$SHELL" ;; \
	esac

$(DEV)/rc/.zshrc: Makefile | $(DEV)/.stamp
	@mkdir -p $(DEV)/rc
	@printf '%s\n' \
	  '# generated by `make dev-shell` — your rc, then the dev environment.' \
	  '[ -f "$${KOTO_DEV_RC_HOME:-$$HOME}/.zshrc" ] && . "$${KOTO_DEV_RC_HOME:-$$HOME}/.zshrc"' \
	  '. $(DEV)/env' > $@
	@# ZDOTDIR moves .zshenv too, so hand your own back rather than skipping it.
	@printf '%s\n' \
	  '[ -f "$${KOTO_DEV_RC_HOME:-$$HOME}/.zshenv" ] && . "$${KOTO_DEV_RC_HOME:-$$HOME}/.zshenv"' \
	  > $(DEV)/rc/.zshenv

$(DEV)/rc/bashrc: Makefile | $(DEV)/.stamp
	@mkdir -p $(DEV)/rc
	@printf '%s\n' \
	  '# generated by `make dev-shell` — your rc, then the dev environment.' \
	  '[ -f "$${KOTO_DEV_RC_HOME:-$$HOME}/.bashrc" ] && . "$${KOTO_DEV_RC_HOME:-$$HOME}/.bashrc"' \
	  '. $(DEV)/env' > $@

# The TUI against the dev daemon. Same binary, different endpoint and creds.
dev-tui: koto koto-tui
	@KOTO_ADDR=127.0.0.1:$(DEV_PORT) ./koto tui -state $(DEV)

# One-time bootstrap. Identities are COPIED from the clone's creds/ so the
# `koto ctl` identities you already have keep working (same CA); the guest
# assets are HARDLINKED, because rootfs.img is 2GB and a dev instance has no
# reason to own a second copy of it. Delete .dev/ to start over.
$(DEV)/.stamp:
	@mkdir -p $(DEV)/groups $(DEV)/creds $(DEV)/fcassets $(DEV)/prompts $(DEV)/run
	@ln -sfn creds $(DEV)/.claude
	@test -f creds/ca.crt || { echo "no PKI in creds/ — run \`./koto pki init && ./koto pki client tui\`"; exit 1; }
	@# NO .credentials.json here — see DEV_HOME above. Copying it forks the
	@# OAuth refresh chain and kills one of the two copies.
	@for f in ca.crt ca.key ca.srl server.crt server.key clients.allow tokens.json acl.json \
	          venice.key; do \
	  [ -e creds/$$f ] && cp -a creds/$$f $(DEV)/creds/ || true; done
	@cp -a creds/client-*.crt creds/client-*.key creds/token-* $(DEV)/creds/ 2>/dev/null || true
	@cp -a prompts/*.md $(DEV)/prompts/ 2>/dev/null || true
	@for a in firecracker vmlinux rootfs.img; do \
	  ln -f fcassets/$$a $(DEV)/fcassets/$$a 2>/dev/null || cp fcassets/$$a $(DEV)/fcassets/$$a; done
	@touch $@
	@echo "created $(DEV) (state dir for the dev daemon)"
	@test -f $(DEV_HOME)/creds/.credentials.json || \
	  echo "no shared credential found — mint one for this instance: KOTO_HOME=$(DEV) ./koto claude-login"

# The only targets that need a host Go toolchain. Optional: they exist for
# fast iteration when you already have Go. Everything a user needs to install
# and run koto goes through the containerized build instead.
run:
	go run ./daemon daemon
proxy:
	go run ./daemon proxy

# koto ctl: host-side CLI client for agents (same binary, `ctl` subcommand).
# Kept as an alias for `koto` so the muscle memory still works; it builds in
# a container like everything else, so no host Go is needed.
ctl-build: koto

metrics:
	@jq -s 'group_by(.group)|map({group:.[0].group,n:length,usage:(map(.usage)|add)})' metrics.jsonl 2>/dev/null || tail -n 20 metrics.jsonl

# --- secrets: a per-commit gate and a whole-history sweep -------------------
# Both scanners run from digest-pinned images (≥6 weeks old, like every
# dependency here): betterleaks v1.7.0 (2026-07-23 — gitleaks' author's
# successor project, drop-in flags/config; see tools/hooks/pre-commit for why)
# and trufflehog v3.96.0 (2026-07-24). Neither tool recognises koto's own byte-random tokens or the
# sk-ant-oat01- OAuth token (canary-tested 2026-09-05), so secrets-scan also
# greps history for the LIVE values in the installed creds dir, verbatim (one
# per line — the token files carry no trailing newline, so a bare cat glues
# them into one string that matches nothing), and
# lists every credential-shaped filename ever committed. creds/ staying out of
# the tree is .gitignore's job; these are the checks that it did.
BETTERLEAKS_IMAGE ?= ghcr.io/betterleaks/betterleaks@sha256:06d60954c287c19af7a7171a1b5d466959d2f49b6946d4cf92ea65ba725a633a
TRUFFLEHOG_IMAGE  ?= docker.io/trufflesecurity/trufflehog@sha256:aa821cf4ace8861c7d096d83818cdf7bb9719028a52d37a52eaad44086a52577
SCAN_RUN = $(CONTAINER) run --rm --security-opt label=disable -v "$(CURDIR):/src:ro" -w /src \
  -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/src

# Point git at tools/hooks: pre-commit refuses a commit whose staged diff
# contains a recognisable secret. Per-clone, so run it once after cloning.
hooks:
	git config core.hooksPath tools/hooks
	@echo "hooks installed: $$(git config core.hooksPath)/pre-commit (bypass once: git commit --no-verify)"

secrets-scan:
	$(need-container)
	@echo "==> betterleaks: every commit on every ref"
	$(SCAN_RUN) $(BETTERLEAKS_IMAGE) git /src --log-opts="--all" --no-banner --redact=100
	@echo "==> trufflehog: every commit, verified + unverified"
	$(SCAN_RUN) $(TRUFFLEHOG_IMAGE) git file:///src --results=verified,unverified,unknown --no-update --fail
	@echo "==> credential-shaped filenames ever committed (must print nothing)"
	@git log --all --diff-filter=A --name-only --format= \
	  | grep -iE '(^|/)(creds/|\.credentials\.json|token-|tokens\.json|venice\.key|anthropic-api-key|koto\.env|clients\.allow|.*\.key$$|.*\.pem$$)' \
	  | sort -u | tee /dev/stderr | { ! grep -q .; }
	@echo "==> live credential values from $${KOTO_HOME:-/var/lib/koto}/creds, searched verbatim across history"
	@d="$${KOTO_HOME:-/var/lib/koto}/creds"; if [ -d "$$d" ]; then \
	  { for f in "$$d"/token-* "$$d"/venice.key "$$d"/anthropic-api-key; do [ -f "$$f" ] && { cat "$$f"; echo; }; done; \
	    jq -r '.claudeAiOauth.accessToken, .claudeAiOauth.refreshToken' "$$d"/.credentials.json 2>/dev/null; } \
	  | awk 'length>=32' | sort -u > .build/live-secrets.tmp; \
	  n=$$(git log --all -p --format= | grep -F -c -f .build/live-secrets.tmp || true); \
	  v=$$(wc -l < .build/live-secrets.tmp); rm -f .build/live-secrets.tmp; \
	  echo "    $$v values checked, history lines matching: $$n"; test "$$n" = 0; \
	else echo "    (no creds dir at $$d — skipped)"; fi
	@echo "secrets-scan: clean"

# --- protobuf / gRPC codegen (host-only; generated code is committed) -------
# Runs entirely in an ephemeral golang:alpine container with pinned plugins.
# Output: protocol/pb/koto.pb.go + koto_grpc.pb.go (module-mapped so the
# go_package `koto-protocol/pb` lands in protocol/pb/). The runtime images
# never invoke protoc — they compile the committed generated code.
proto-gen:
	$(CONTAINER) run --rm --security-opt label=disable \
	  -v $(CURDIR):/src -w /src/protocol \
	  -v $(CURDIR)/.gocache:/root/.cache/go-build \
	  $(GO_IMAGE) sh -euc '\
	    apk add --no-cache protobuf protobuf-dev >/dev/null; \
	    go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VER); \
	    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VER); \
	    export PATH=$$PATH:$$(go env GOPATH)/bin; \
	    protoc -I . -I /usr/include \
	      --go_out=. --go_opt=module=koto-protocol \
	      --go-grpc_out=. --go-grpc_opt=module=koto-protocol \
	      koto.proto guest.proto'

# CI drift guard: regenerate and fail if the committed output differs.
proto-verify: proto-gen
	git diff --exit-code -- protocol/pb

# --- PKI (host-only; writes into creds/, gitignored) ------------------------
# pki-init: one-time private CA + daemon server cert. pki-client NAME=phone:
# a client cert signed by the CA, appended to creds/clients.allow by SHA-256
# fingerprint. SAN on the server cert must match the overlay address clients
# dial (override with SERVER_SAN=IP:<wg-ip> or DNS:<name>).
#
# Every client has one or more roles (ROLE=reader,ops — default admin),
# written into its tokens.json entry as a roles array; a client's effective
# permissions are the union of its roles. The admin role is hardcoded in the
# daemon (every verb on every target — acl.json can't narrow it or lock it
# out). All other roles come from creds/acl.json, mapping role ->
# {verb -> targets}: verbs are snake_case RPC names, targets are group names
# ("*" = any; targets only apply to group-scoped verbs like send/stop/history
# — see daemon/acl.go). The daemon enforces it per call and re-reads both
# files every time, so editing a role or the ACL needs no restart. pki-init
# seeds acl.json with an agent role (read/converse on any group, no lifecycle
# or config verbs) — narrow it to specific groups by replacing its "*" values
# with group lists, e.g. "send": ["main"].
SERVER_SAN ?= DNS:koto-daemon,DNS:localhost,IP:127.0.0.1
ROLE ?= admin
pki-init:
	@test ! -f creds/ca.key || { echo "creds/ca.key exists — refusing to regenerate the CA (it would silently invalidate every client cert). rm creds/ca.{key,crt} to force."; exit 1; }
	@mkdir -p creds
	@command -v openssl >/dev/null || { echo "openssl required"; exit 1; }
	openssl ecparam -name prime256v1 -genkey -noout -out creds/ca.key
	openssl req -x509 -new -key creds/ca.key -sha256 -days 3650 \
	  -subj "/CN=koto-ca" -out creds/ca.crt
	openssl ecparam -name prime256v1 -genkey -noout -out creds/server.key
	openssl req -new -key creds/server.key -subj "/CN=koto-daemon" -out creds/server.csr
	printf 'subjectAltName=%s\n' "$(SERVER_SAN)" > creds/server.ext
	openssl x509 -req -in creds/server.csr -CA creds/ca.crt -CAkey creds/ca.key \
	  -CAcreateserial -sha256 -days 825 -extfile creds/server.ext -out creds/server.crt
	@rm -f creds/server.csr creds/server.ext
	@[ -f creds/acl.json ] || printf '%s\n' \
	  '{' \
	  '  "agent": {' \
	  '    "list": "*", "send": "*", "history": "*", "metrics": "*",' \
	  '    "sched_list": "*",' \
	  '    "subscribe_group": "*", "watch_state": "*"' \
	  '  }' \
	  '}' > creds/acl.json
	@echo "CA + server cert written to creds/. Distribute creds/ca.crt to clients."

pki-client:
	@test -n "$(NAME)" || { echo "usage: make pki-client NAME=<client>"; exit 1; }
	@test -f creds/ca.key || { echo "run \`make pki-init\` first"; exit 1; }
	openssl ecparam -name prime256v1 -genkey -noout -out creds/client-$(NAME).key
	openssl req -new -key creds/client-$(NAME).key -subj "/CN=$(NAME)" -out creds/client-$(NAME).csr
	openssl x509 -req -in creds/client-$(NAME).csr -CA creds/ca.crt -CAkey creds/ca.key \
	  -CAcreateserial -sha256 -days 825 -out creds/client-$(NAME).crt
	@rm -f creds/client-$(NAME).csr
	@fp=$$(openssl x509 -in creds/client-$(NAME).crt -noout -fingerprint -sha256 \
	  | sed 's/.*=//; s/://g' | tr 'A-F' 'a-f'); \
	  grep -q "$$fp" creds/clients.allow 2>/dev/null || echo "$$fp $(NAME)" >> creds/clients.allow; \
	  echo "client cert creds/client-$(NAME).{crt,key} (fp $$fp) added to creds/clients.allow"
	@tok=$$(openssl rand -hex 32); \
	  hash=$$(printf '%s' "$$tok" | sha256sum | cut -d' ' -f1); \
	  printf '%s' "$$tok" > creds/token-$(NAME); chmod 600 creds/token-$(NAME); \
	  [ -f creds/tokens.json ] || echo '{}' > creds/tokens.json; \
	  tmp=$$(mktemp); jq --arg n "$(NAME)" --arg h "$$hash" --arg r "$(ROLE)" \
	    '.[$$n]={hash:$$h, roles:($$r | split(",") | map(gsub("^\\s+|\\s+$$";"")) | map(select(. != "")))}' \
	    creds/tokens.json > $$tmp && mv $$tmp creds/tokens.json; \
	  echo "token written to creds/token-$(NAME); roles [$(ROLE)] registered in creds/tokens.json"

# --- Firecracker microVM runtime assets -------------------------------------
# Opt-in per group via config.json `"runtime": "firecracker"`. Assets land in
# fcassets/ (gitignored): the FC binary (firecracker, pinned in fetch-assets.sh),
# the guest kernel (kernel — built with CONFIG_TUN for L3 networking, pinned
# in build-kernel.sh), and the golden rootfs (rootfs — rebuild after editing
# sidecar/*.{sh,js} or fcguest/, since microVMs have no live bind mounts).
# The daemon opens /dev/kvm directly; it must be world-accessible because the
# jailed VMM runs as an unprivileged per-VM id (see checkKVM).
# Keyed on the REAL OUTPUTS, not on .build/ markers. Two reasons, both from
# the wizard no longer building anything:
#
#   - `make setup` now depends on `assets`, so these must be no-ops on a
#     provisioned machine. `firecracker` was phony and rebuilt every time,
#     which only went unnoticed while the wizard's presence-check gated it.
#   - a missing artifact makes the wizard say "run `make kernel`". With a
#     marker file that survives the asset it names, make would answer
#     "nothing to be done" and the operator would be stuck in a loop.
#
# The tradeoff is that a build script newer than the asset it produced now
# rebuilds, where a stale marker would have stayed quiet. That is make being
# right: the asset really is out of date.
$(FCASSETS)/firecracker: fcguest/build-firecracker.sh
	./fcguest/build-firecracker.sh

firecracker: $(FCASSETS)/firecracker

$(FCASSETS)/vmlinux: fcguest/build-kernel.sh
	./fcguest/build-kernel.sh

kernel: $(FCASSETS)/vmlinux

FCGUEST_SRC := $(filter-out %_test.go,$(wildcard fcguest/*.go)) fcguest/go.mod fcguest/go.sum fcguest/Dockerfile.rootfs $(SIDECAR_SRC)
$(FCASSETS)/rootfs.img: $(FCGUEST_SRC)
	./fcguest/build-rootfs.sh
	$(prune-dangling)

rootfs: $(FCASSETS)/rootfs.img

assets: firecracker kernel rootfs

# clean is SAFE: stops the daemon and removes runtime droppings (logs,
# metrics, run/, build sentinels). Group workspaces — every agent's session
# history, prompt.md, and files — are untouched. The destructive wipe is
# clean-groups, split out after a `make clean` deleted five groups' state.
# run/fc holds per-VM droppings created by the JAILED firecracker processes,
# whose per-VM uids map to host subuids — the host user can't rm those
# directly (Permission denied). podman unshare re-enters the same subuid
# userns as root, which can. The daemon removes them on a graceful stop;
# the unshare here catches `podman rm -f` / crash leftovers.
clean: stop
	rm -rf metrics.jsonl proxy.log $(BUILD)
	podman unshare rm -rf run

# clean-groups PERMANENTLY DELETES all group state: groups/ (each group's
# workspace.img = sessions, prompts, files), groups.json (port allocations),
# schedules.json, and goals.json (both reference groups; wiping one without
# the others leaves orphans — a surviving `running` goal is re-driven by
# resumeGoalDrivers at the next daemon start, which respawns its deleted
# group as a fresh empty VM and burns iterations against a blank workspace,
# same failure shape that got schedules.json included here).
# Prompts for confirmation; FORCE=1 skips it for scripts.
clean-groups: stop
	@if [ "$(FORCE)" != "1" ]; then \
	  printf 'This PERMANENTLY deletes ALL group workspaces, sessions, schedules, and goals\n(groups/ + groups.json + schedules.json + goals.json). There is no undo.\nType "yes" to continue: '; \
	  read ans && [ "$$ans" = "yes" ] || { echo "aborted — nothing deleted"; exit 1; }; \
	fi
	rm -rf groups groups.json schedules.json goals.json

clean-creds:
	rm -rf creds
