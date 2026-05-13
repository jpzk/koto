# clawson-tui: dedicated container for the Go (Bubble Tea) TUI.
# Network: none. Only access is the bind-mounted clawson.sock.
#
# Multi-stage: build a static binary in golang:alpine, drop into scratch.
# The runtime image has NO shell, NO toolchain, NO ca-certs — just the
# binary. Unix socket only, no outbound network. Smallest trust surface
# we can give a tier-2.5 component.

FROM docker.io/library/golang:1.24-alpine AS builder
WORKDIR /src
# The shared wire types live in ../protocol as a separate stdlib-only Go
# module; tui/go.mod's `replace` points one dir up. Lay out a matching
# parent-sibling pair inside /src so `replace clawson-protocol => ../protocol`
# resolves during go mod download and the build.
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
ENV SOCK_PATH=/clawson-run/clawson.sock \
    TERM=xterm-256color
ENTRYPOINT ["/clawson-tui"]
