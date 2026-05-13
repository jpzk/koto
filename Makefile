# Sentinel-driven build: every image-build target writes a marker into
# .build/ on success. Make compares the marker's mtime against the actual
# Dockerfile + COPY'd source inputs, so `make host-run` / `make tui` only
# rebuild when something relevant actually changed.
#
# Daemon/proxy *.go files are NOT inputs to clawson-host — they're bind-
# mounted at runtime and recompiled via `go run` inside cs_host_go. Only
# host/Dockerfile (and its installed deps) re-triggers a host-image build.

.PHONY: build host-build tui-build login host-run tui stop run proxy metrics clean clean-creds

BUILD := .build

$(BUILD):
	@mkdir -p $@

# --- clawson (sidecar image) ------------------------------------------------
# Build context is sidecar/ so the Dockerfile's `COPY entrypoint.sh ...`
# style stays unqualified. Inputs are tracked so a script edit triggers a
# rebuild; the daemon's bind-mount of entrypoint.sh + stream_filter.js
# overlays the COPY at runtime, but the COPY is still the source of truth
# for standalone `podman run -it clawson sh` smoke tests.
SIDECAR_SRC := sidecar/Dockerfile sidecar/entrypoint.sh sidecar/start-chrome.sh sidecar/stream_filter.js
$(BUILD)/clawson: $(SIDECAR_SRC) | $(BUILD)
	podman build -t clawson sidecar
	@touch $@

build: $(BUILD)/clawson

# --- clawson-host (daemon image) -------------------------------------------
# Inputs: just host/Dockerfile. Go sources land via bind mount, recompiled
# by `go run` inside the container on every host-run.
$(BUILD)/clawson-host: host/Dockerfile $(BUILD)/clawson | $(BUILD)
	podman build -t clawson-host -f host/Dockerfile .
	@touch $@

host-build: $(BUILD)/clawson-host

# --- clawson-tui (TUI image) -----------------------------------------------
# Build context stays at project root so the Dockerfile's `COPY protocol/`
# and `COPY tui/...` paths resolve. Inputs cover the actual sources COPYed.
TUI_GO_SRC := $(wildcard tui/*.go) tui/go.mod $(wildcard tui/go.sum)
PROTO_SRC  := $(wildcard protocol/*.go) protocol/go.mod
$(BUILD)/clawson-tui: tui/Dockerfile $(TUI_GO_SRC) $(PROTO_SRC) | $(BUILD)
	podman build -t clawson-tui -f tui/Dockerfile .
	@touch $@

tui-build: $(BUILD)/clawson-tui

# --- run / interactive targets ---------------------------------------------
login: $(BUILD)/clawson-host
	@mkdir -p creds
	podman run --rm -it --security-opt label=disable -v $(PWD)/creds:/root/.claude --entrypoint claude clawson-host auth login

host-run: $(BUILD)/clawson-host
	./host/run-host.sh

# The /reload inner loop re-invokes `$(MAKE) tui-build` so the same sentinel
# logic kicks in: if the user edited any tui/*.go before pressing /reload,
# Make rebuilds; otherwise it's a no-op and the TUI just respawns.
tui: $(BUILD)/clawson-tui
	@test -S $(PWD)/run/clawson.sock || { echo "no run/clawson.sock — run \`make host-run\` first"; exit 1; }
	@while :; do \
	  podman run --rm -it \
	    --network=none \
	    --security-opt label=disable \
	    -v $(PWD)/run:/clawson-run \
	    -v /etc/localtime:/etc/localtime:ro \
	    clawson-tui; ec=$$?; \
	  [ $$ec -eq 75 ] || exit $$ec; \
	  echo "/reload: rebuilding clawson-tui…"; \
	  $(MAKE) tui-build || exit $$?; \
	done

stop:
	-podman rm -f cs_host_go
	@podman ps -a --format '{{.Names}}' | grep -E '^cs_.*_go$$' | xargs -r podman rm -f

run:
	go run . daemon
proxy:
	go run . proxy

metrics:
	@jq -s 'group_by(.group)|map({group:.[0].group,n:length,usage:(map(.usage)|add)})' metrics.jsonl 2>/dev/null || tail -n 20 metrics.jsonl

clean: stop
	rm -rf groups metrics.jsonl groups.json proxy.log run $(BUILD)
clean-creds:
	rm -rf creds
