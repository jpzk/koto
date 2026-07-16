# Sentinel-driven build: every image-build target writes a marker into
# .build/ on success. Make compares the marker's mtime against the actual
# Dockerfile + COPY'd source inputs, so `make host-run` / `make tui` only
# rebuild when something relevant actually changed.
#
# Daemon/proxy *.go files are NOT inputs to clawson-host — they're bind-
# mounted at runtime and recompiled via `go run` inside cs_host_go. Only
# host/Dockerfile (and its installed deps) re-triggers a host-image build.

.PHONY: host-build tui-build login host-run tui stop run proxy metrics clean clean-groups clean-creds proto-gen proto-verify pki-init pki-client fc-fetch fc-kernel fc-rootfs fc-assets

# Pinned codegen toolchain (6-week dependency-lag rule). Versions verified
# >=6 weeks old as of 2026-06-14 via proxy.golang.org:
#   protobuf            v1.36.11 — 2025-12-12
#   protoc-gen-go-grpc  v1.6.1   — 2026-02-04
# (grpc runtime v1.80.0 / 2026-04-01 is pinned in the go.mod files.)
PROTOC_GEN_GO_VER      := v1.36.11
PROTOC_GEN_GO_GRPC_VER := v1.6.1

# INSTANCE (opt-in): run a second daemon+TUI side by side, e.g. from a git
# worktree — `make host-run INSTANCE=cli`, `make tui INSTANCE=cli`,
# `make stop INSTANCE=cli`. Every other piece of state (groups/, groups.json,
# creds/, run/, .gocache, .gomodcache) is already resolved relative to the
# working directory, so a second worktree gets its own automatically; only
# the daemon container's *name* is hardcoded enough to collide across
# instances, so that's the only thing this threads through. clawson-net
# stays shared on purpose — containers on the same bridge don't collide on
# port/address, since each has its own network namespace.
INSTANCE ?=
CS_HOST_NAME := cs_host_go$(if $(INSTANCE),_$(INSTANCE))

BUILD := .build

$(BUILD):
	@mkdir -p $@

# --- sidecar scripts --------------------------------------------------------
# sidecar/entrypoint.sh + stream_filter.js + start-chrome.sh run INSIDE the
# firecracker guest (baked into the rootfs — see fc-rootfs). They no longer
# build a standalone podman image (the podman group runtime is retired); this
# just tracks them as inputs so an edit triggers an fc-rootfs rebuild.
SIDECAR_SRC := sidecar/entrypoint.sh sidecar/stream_filter.js sidecar/venice_stream.js sidecar/cs-job sidecar/cs-subagent

# --- clawson-host (daemon image) -------------------------------------------
# Inputs: just host/Dockerfile. Go sources land via bind mount, recompiled
# by `go run` inside the container on every host-run.
$(BUILD)/clawson-host: host/Dockerfile | $(BUILD)
	podman build -t clawson-host -f host/Dockerfile .
	@touch $@

host-build: $(BUILD)/clawson-host

# --- clawson-tui (TUI image) -----------------------------------------------
# Build context stays at project root so the Dockerfile's `COPY protocol/`
# and `COPY tui/...` paths resolve. Inputs cover the actual sources COPYed.
TUI_GO_SRC := $(wildcard tui/*.go) tui/go.mod $(wildcard tui/go.sum)
PROTO_SRC  := $(wildcard protocol/pb/*.go) protocol/go.mod protocol/clawson.proto
$(BUILD)/clawson-tui: tui/Dockerfile $(TUI_GO_SRC) $(PROTO_SRC) | $(BUILD)
	podman build -t clawson-tui -f tui/Dockerfile .
	@touch $@

tui-build: $(BUILD)/clawson-tui

# --- run / interactive targets ---------------------------------------------
login: $(BUILD)/clawson-host
	@mkdir -p creds
	podman run --rm -it --security-opt label=disable -v $(PWD)/creds:/root/.claude --entrypoint claude clawson-host auth login

host-run: $(BUILD)/clawson-host
	CLAWSON_INSTANCE=$(INSTANCE) ./host/run-host.sh

# The /reload inner loop re-invokes `$(MAKE) tui-build` so the same sentinel
# logic kicks in: if the user edited any tui/*.go before pressing /reload,
# Make rebuilds; otherwise it's a no-op and the TUI just respawns.
tui: $(BUILD)/clawson-tui
	@test -f $(PWD)/creds/client-tui.crt || { echo "no TUI client cert — run \`make pki-init && make pki-client NAME=tui\`"; exit 1; }
	@while :; do \
	  podman run --rm -it \
	    --network clawson-net \
	    --security-opt label=disable \
	    -v $(PWD)/creds:/clawson-creds:ro \
	    -v /etc/localtime:/etc/localtime:ro \
	    -e CLAWSON_TOKEN="$$(cat $(PWD)/creds/token-tui 2>/dev/null)" \
	    -e CLAWSON_ENDPOINT=$(CS_HOST_NAME):8443 \
	    clawson-tui; ec=$$?; \
	  [ $$ec -eq 75 ] || exit $$ec; \
	  echo "/reload: rebuilding clawson-tui…"; \
	  $(MAKE) tui-build || exit $$?; \
	done

stop:
	-podman rm -f $(CS_HOST_NAME)
	# Sweep leftover podman-era sidecar containers (cs_<group>_go, from the
	# retired group-podman runtime) — NOT daemon containers: cs_host_go and
	# cs_host_go_<instance> both match `cs_.*_go` too, so without the
	# exclusion this would tear down every OTHER instance's daemon on any
	# `make stop INSTANCE=x` (found the hard way — it killed the default
	# instance while tearing down a test one).
	@podman ps -a --format '{{.Names}}' | grep -E '^cs_.*_go$$' | grep -v -E '^cs_host_go(_.+)?$$' | xargs -r podman rm -f

run:
	go run ./daemon daemon
proxy:
	go run ./daemon proxy

metrics:
	@jq -s 'group_by(.group)|map({group:.[0].group,n:length,usage:(map(.usage)|add)})' metrics.jsonl 2>/dev/null || tail -n 20 metrics.jsonl

# --- protobuf / gRPC codegen (host-only; generated code is committed) -------
# Runs entirely in an ephemeral golang:alpine container with pinned plugins.
# Output: protocol/pb/clawson.pb.go + clawson_grpc.pb.go (module-mapped so the
# go_package `clawson-protocol/pb` lands in protocol/pb/). The runtime images
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
	      --go_out=. --go_opt=module=clawson-protocol \
	      --go-grpc_out=. --go-grpc_opt=module=clawson-protocol \
	      clawson.proto'

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
# permissions are the union of its roles. creds/acl.json maps role ->
# {verb -> targets}: verbs are
# snake_case RPC names, targets are group names ("*" = any; targets only
# apply to group-scoped verbs like send/stop/history — see daemon/acl.go).
# The daemon enforces it per call and re-reads both files every time, so
# editing a role or the ACL needs no restart. pki-init seeds acl.json with
# admin (everything) and agent (read/converse on any group, no lifecycle or
# config verbs) — narrow an agent to specific groups by replacing its "*"
# values with group lists, e.g. "send": ["main"].
SERVER_SAN ?= DNS:clawson-daemon,DNS:localhost,IP:127.0.0.1
ROLE ?= admin
pki-init:
	@mkdir -p creds
	@command -v openssl >/dev/null || { echo "openssl required"; exit 1; }
	openssl ecparam -name prime256v1 -genkey -noout -out creds/ca.key
	openssl req -x509 -new -key creds/ca.key -sha256 -days 3650 \
	  -subj "/CN=clawson-ca" -out creds/ca.crt
	openssl ecparam -name prime256v1 -genkey -noout -out creds/server.key
	openssl req -new -key creds/server.key -subj "/CN=clawson-daemon" -out creds/server.csr
	printf 'subjectAltName=%s\n' "$(SERVER_SAN)" > creds/server.ext
	openssl x509 -req -in creds/server.csr -CA creds/ca.crt -CAkey creds/ca.key \
	  -CAcreateserial -sha256 -days 825 -extfile creds/server.ext -out creds/server.crt
	@rm -f creds/server.csr creds/server.ext
	@[ -f creds/acl.json ] || printf '%s\n' \
	  '{' \
	  '  "admin": {"*": "*"},' \
	  '  "agent": {' \
	  '    "list": "*", "send": "*", "history": "*", "metrics": "*",' \
	  '    "skills": "*", "skill_read": "*", "sched_list": "*",' \
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
# run-host.sh passes /dev/kvm into cs_host automatically when present.
fc-fetch:
	./fcguest/fetch-assets.sh

$(BUILD)/fc-kernel: fcguest/build-kernel.sh | $(BUILD)
	./fcguest/build-kernel.sh
	@touch $@

fc-kernel: $(BUILD)/fc-kernel

FCGUEST_SRC := fcguest/main.go fcguest/go.mod fcguest/Dockerfile.rootfs $(SIDECAR_SRC)
$(BUILD)/fc-rootfs: $(FCGUEST_SRC) | $(BUILD)
	./fcguest/build-rootfs.sh
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
# and schedules.json (schedules reference groups; wiping one without the
# other leaves orphans that fire into freshly-respawned empty groups).
# Prompts for confirmation; FORCE=1 skips it for scripts.
clean-groups: stop
	@if [ "$(FORCE)" != "1" ]; then \
	  printf 'This PERMANENTLY deletes ALL group workspaces, sessions, and schedules\n(groups/ + groups.json + schedules.json). There is no undo.\nType "yes" to continue: '; \
	  read ans && [ "$$ans" = "yes" ] || { echo "aborted — nothing deleted"; exit 1; }; \
	fi
	rm -rf groups groups.json schedules.json

clean-creds:
	rm -rf creds
