# clawson-tui: dedicated container for the Go (Bubble Tea) TUI.
#
# Transport is now gRPC over mTLS + bearer token (the daemon may run off-box),
# so the TUI needs network reach to the daemon and a mounted creds dir
# (ca.crt + client cert/key + token). It no longer runs --network=none/sock-only
# — see the trust-model note in CLAUDE.md. No system ca-certs are needed: the
# client pins the daemon via our private CA (CLAWSON_CA), not the public roots.
#
# Multi-stage: build a static binary in golang:alpine, drop into scratch.
# The runtime image has NO shell, NO toolchain, NO ca-certs — just the binary.

FROM docker.io/library/golang:1.24-alpine AS builder
WORKDIR /src
# The proto contract (clawson.proto + committed generated pb) lives in
# ../protocol as a separate Go module; tui/go.mod's `replace` points one dir
# up. Lay out a matching parent-sibling pair inside /src so
# `replace clawson-protocol => ../protocol` resolves during go mod download
# and the build.
WORKDIR /src/tui
COPY protocol/ /src/protocol/
# Cache module downloads independently of source changes.
COPY tui/go.mod tui/go.sum* ./
RUN go mod download
COPY tui/*.go ./
# -trimpath: strip absolute paths from the binary
# -ldflags '-s -w': drop debug + symbol tables (~30% smaller)
# CGO_ENABLED=0: pure-Go build, statically linked, no libc dep
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags='-s -w' \
    -o /out/clawson-tui .

FROM scratch
COPY --from=builder /out/clawson-tui /clawson-tui
# TERM tells lipgloss/glamour we're on a 256-color terminal; without this
# they fall back to no-color since scratch has no terminfo db.
# Cred paths default into /clawson-creds (mounted ro by `make tui`); the
# endpoint, server name, and token are overridden per deployment.
ENV TERM=xterm-256color \
    CLAWSON_CA=/clawson-creds/ca.crt \
    CLAWSON_CERT=/clawson-creds/client-tui.crt \
    CLAWSON_KEY=/clawson-creds/client-tui.key \
    CLAWSON_ENDPOINT=cs_host_go:8443 \
    CLAWSON_SERVER_NAME=clawson-daemon
ENTRYPOINT ["/clawson-tui"]
