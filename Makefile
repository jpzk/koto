# Sentinel-driven build: every image-build target writes a marker into
# .build/ on success. Make compares the marker's mtime against the actual
# Dockerfile + COPY'd source inputs, so `make host-run` / `make tui` only
# rebuild when something relevant actually changed.
#
# Podman is a BUILD-time dependency only: it compiles the Go binaries (so the
# host needs no Go toolchain) and builds the guest kernel and rootfs. Nothing
# koto runs at runtime is a container — the daemon is a systemd service on the
# host and the TUI is a plain binary.

.PHONY: build setup install tui-walk tui-build login host-run tui stop run proxy ctl-build metrics clean clean-groups clean-creds proto-gen proto-verify pki-init pki-client fc-fetch fc-kernel fc-rootfs fc-assets

# Pinned codegen toolchain (6-week dependency-lag rule). Versions verified
# >=6 weeks old as of 2026-06-14 via proxy.golang.org:
#   protobuf            v1.36.11 — 2025-12-12
#   protoc-gen-go-grpc  v1.6.1   — 2026-02-04
# (grpc runtime v1.80.0 / 2026-04-01 is pinned in the go.mod files.)
PROTOC_GEN_GO_VER      := v1.36.11
PROTOC_GEN_GO_GRPC_VER := v1.6.1

# --- first run ---------------------------------------------------------------
# `make setup` is the ONE command a newcomer runs. It builds the koto binary
# inside a container (so the host needs no Go — only git, make and podman) and
# hands over to the interactive wizard, which checks the host, builds the
# images and guest assets, mints the PKI, connects your Anthropic credentials
# and installs koto as a systemd service. Safe to re-run: every step detects
# whether it is already done, so an interrupted install resumes here.
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
# NOTE: `make fc-rootfs` is podman-ONLY, unlike these targets. It uses
# `podman unshare` to preserve in-image uid/gid ownership through
# `mkfs.ext4 -d`, and docker has no equivalent. A docker-only host can build
# every binary but not the guest rootfs; use a prebuilt fcassets/rootfs.img,
# or install podman for that one step.
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
# image and produce the same bytes. `golang:1.24-alpine` is a moving tag that
# silently changes toolchain patch versions under you; the digest does not.
# Combined with -trimpath and CGO_ENABLED=0, two builds of the same commit
# give identical binaries, except for the version string stamped below.
# Update deliberately: podman/docker pull golang:1.24-alpine, then
#   podman inspect --format '{{index .RepoDigests 0}}' golang:1.24-alpine
GO_IMAGE ?= docker.io/library/golang@sha256:757779acac4af1b349a20f357c7296097b4a0b89da4ad0e370b339060077282a
# Cache paths are passed as env rather than mounted over /root, so the same
# invocation works whether we are root in the container (podman) or not.
GO_BUILD_RUN = $(CONTAINER) run --rm --security-opt label=disable \
	  $(CONTAINER_USER) \
	  -v $(PWD):/src -w /src \
	  -v $(PWD)/.gocache:/gocache -v $(PWD)/.gomodcache:/gomodcache \
	  -e GOCACHE=/gocache -e GOMODCACHE=/gomodcache -e HOME=/tmp \
	  -e CGO_ENABLED=0 -e GOFLAGS= \
	  $(GO_IMAGE)

define need-container
@test -n "$(CONTAINER)" || { echo "podman or docker is required to build koto (sudo dnf install podman / sudo apt install podman)"; exit 1; }
@mkdir -p .gocache .gomodcache
endef

KOTO_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)
koto: $(wildcard daemon/*.go) $(wildcard protocol/pb/*.go)
	$(need-container)
	$(GO_BUILD_RUN) \
	  go build -trimpath -ldflags "-s -w -X main.kotoVersion=$(KOTO_VERSION)" -o koto ./daemon

# Everything a host needs to run koto. One static binary each, portable across
# every supported distribution.
build: koto koto-tui
	@echo "built: koto koto-tui ($(KOTO_VERSION), via $(CONTAINER_NAME))"

setup: koto
	@./koto setup

# Install (or upgrade) an existing checkout as a systemd service without the
# wizard's explanatory pass — for people who already know what they want.
install: build
	@./koto install

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

$(BUILD):
	@mkdir -p $@

# --- sidecar scripts --------------------------------------------------------
# + stream_filter.js + start-chrome.sh run INSIDE the
# firecracker guest (baked into the rootfs — see fc-rootfs). They no longer
# build a standalone podman image (the podman group runtime is retired); this
# just tracks them as inputs so an edit triggers an fc-rootfs rebuild.
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

# --- run / interactive targets ---------------------------------------------
# One-time OAuth into ./creds. Uses the host's claude CLI — a runtime
# dependency now, like the e2fsprogs tools (see `koto setup --check`).
login:
	@command -v claude >/dev/null || { echo "claude not found — npm i -g @anthropic-ai/claude-code"; exit 1; }
	@mkdir -p creds
	HOME=$(PWD)/creds claude auth login

# Dev: run the daemon in the foreground, straight from source. It puts itself
# in a user namespace first (daemon/userns.go) so the jailer can hand each VMM
# its own uid — what the podman container used to provide.
host-run: koto
	./koto daemon

tui: koto koto-tui
	@test -f $(PWD)/creds/client-tui.crt || { echo "no TUI client cert — run \`./koto pki init && ./koto pki client tui\`"; exit 1; }
	@KOTO_HOME=$(PWD) ./koto tui

# Stop the dev daemon. SIGTERM reaches it directly now (no podman in the
# middle): it stops every microVM so each guest sync+umounts its workspace
# image, which is what keeps the images clean across restarts.
stop:
	-pkill -TERM -u $$(id -u) -f '^\./koto daemon$$' 2>/dev/null || true
	@echo "sent SIGTERM to the dev daemon (if running); installed service: sudo systemctl stop koto"

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

# --- protobuf / gRPC codegen (host-only; generated code is committed) -------
# Runs entirely in an ephemeral golang:alpine container with pinned plugins.
# Output: protocol/pb/koto.pb.go + koto_grpc.pb.go (module-mapped so the
# go_package `koto-protocol/pb` lands in protocol/pb/). The runtime images
# never invoke protoc — they compile the committed generated code.
proto-gen:
	podman run --rm --security-opt label=disable \
	  -v $(PWD):/src -w /src/protocol \
	  -v $(PWD)/.gocache:/root/.cache/go-build \
	  docker.io/library/golang:1.24-alpine sh -euc '\
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
# fcassets/ (gitignored): the FC binary (fc-fetch, pinned in fetch-assets.sh),
# the guest kernel (fc-kernel — built with CONFIG_TUN for L3 networking, pinned
# in build-kernel.sh), and the golden rootfs (fc-rootfs — rebuild after editing
# sidecar/*.{sh,js} or fcguest/, since microVMs have no live bind mounts).
# The daemon opens /dev/kvm directly; it must be world-accessible because the
# jailed VMM runs as an unprivileged per-VM id (see checkKVM).
fc-fetch:
	./fcguest/fetch-assets.sh

$(BUILD)/fc-kernel: fcguest/build-kernel.sh | $(BUILD)
	./fcguest/build-kernel.sh
	@touch $@

fc-kernel: $(BUILD)/fc-kernel

FCGUEST_SRC := $(filter-out %_test.go,$(wildcard fcguest/*.go)) fcguest/go.mod fcguest/go.sum fcguest/Dockerfile.rootfs $(SIDECAR_SRC)
$(BUILD)/fc-rootfs: $(FCGUEST_SRC) | $(BUILD)
	./fcguest/build-rootfs.sh
	$(prune-dangling)
	@touch $@

fc-rootfs: $(BUILD)/fc-rootfs

fc-assets: fc-fetch fc-kernel fc-rootfs

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
