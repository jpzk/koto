# koto-tui: dedicated container for the Go (Bubble Tea) TUI.
#
# Transport is now gRPC over mTLS + bearer token (the daemon may run off-box),
# so the TUI needs network reach to the daemon and a mounted creds dir
# (ca.crt + client cert/key + token). It no longer runs --network=none/sock-only
# — see the trust-model note in CLAUDE.md. No system ca-certs are needed: the
# client pins the daemon via our private CA (KOTO_CA), not the public roots.
#
# Multi-stage: build a static binary in golang:alpine, drop into scratch.
# The runtime image has NO shell, NO toolchain, NO ca-certs — just the binary.

FROM docker.io/library/golang:1.24-alpine AS builder
WORKDIR /src
# The proto contract (koto.proto + committed generated pb) lives in
# ../protocol as a separate Go module; tui/go.mod's `replace` points one dir
# up. Lay out a matching parent-sibling pair inside /src so
# `replace koto-protocol => ../protocol` resolves during go mod download
# and the build.
WORKDIR /src/tui
COPY protocol/ /src/protocol/
# Cache module downloads independently of source changes.
COPY tui/go.mod tui/go.sum* ./
RUN go mod download
COPY tui/*.go ./
# The bundled color palettes, embedded by theme.go's go:embed. Copied as a
# directory of its own because go:embed reads the real filesystem at compile
# time — a pattern with nothing behind it is a build error, not an empty FS.
COPY tui/themes/ ./themes/
# -trimpath: strip absolute paths from the binary
# -ldflags '-s -w': drop debug + symbol tables (~30% smaller)
# CGO_ENABLED=0: pure-Go build, statically linked, no libc dep
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags='-s -w' \
    -o /out/koto-tui .

FROM scratch
COPY --from=builder /out/koto-tui /koto-tui
# TERM tells lipgloss/glamour we're on a 256-color terminal; without this
# they fall back to no-color since scratch has no terminfo db.
#
# It is the tmux flavour, not xterm-256color, ON PURPOSE. bubbletea v1's
# package init() (tea_init.go) calls lipgloss.HasDarkBackground() before
# main runs, which makes termenv write an OSC 11 + CSI 6n query and then
# read stdin for up to 5s waiting for the reply — discarding every byte
# that isn't the start of one (termenv_unix.go readNextResponse). When the
# reply doesn't make it back through the attach chain the TUI sits blank
# for the full 5s and any key typed meanwhile is eaten (measured 2026-08-25
# in a headless pty: query at 1.7s, first frame at 6.7s). termenv skips the
# query entirely when TERM starts with screen/tmux/dumb (a multiplexer may
# front several terminals), and koto never uses the answer anyway — the
# theme paints its own ground, and mono/notify sniff the host's real
# terminal via KOTO_TUI_TERM. tmux-256color keeps the same ANSI256 profile
# (COLORTERM still promotes to truecolor). bubbletea v2 drops the init hack.
# (Not the cause of "ctrl+p needs two presses" — that was podman's detach
# chord; see the Makefile's tui target.)
ENV TERM=tmux-256color \
    KOTO_CA=/koto-creds/ca.crt \
    KOTO_CERT=/koto-creds/client-tui.crt \
    KOTO_KEY=/koto-creds/client-tui.key \
    KOTO_ENDPOINT=cs_host_go:8443 \
    KOTO_SERVER_NAME=koto-daemon
ENTRYPOINT ["/koto-tui"]
