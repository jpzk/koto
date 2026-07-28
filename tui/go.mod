module koto-tui

go 1.24.2

toolchain go1.24.13

// 6-week dependency lag rule (see /home/<user>/.claude/CLAUDE.md):
// All five pins are stable releases at least 6 weeks old as of 2026-05-11.
//   bubbletea v1.3.10 — 2025-09-17
//   bubbles   v1.0.0  — 2026-02-09
//   lipgloss  v1.1.0  — 2025-03-12
//   glamour   v1.0.0  — 2025-11-24
//   log       v1.0.0  — 2026-03-09
//
// Added for the shared-shell feature (verified via
// proxy.golang.org/<mod>/@v/<ver>.info as of 2026-07-27):
//   charmbracelet/x/vt v0.0.0-20260430013151-79116d1f37bd — 2026-04-30
//     (pinned to a specific commit, not @latest — this submodule has no
//     tagged releases and its HEAD churns daily; the pinned commit is the
//     one that clears the 6-week bar). Same upstream org as the other 5
//     direct deps, chosen over github.com/hinshun/vt10x (unmaintained since
//     2022) for that reason — parses PTY output into a queryable terminal
//     cell grid (vt.Terminal), used by tui/shell_view.go to render the
//     shared shell's live screen state via lipgloss, never by writing raw
//     guest bytes to the real terminal (see the shared-shell plan doc's
//     security note).
require (
	github.com/charmbracelet/bubbles v1.0.0
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/glamour v1.0.0
	github.com/charmbracelet/lipgloss v1.1.1-0.20250404203927-76690c660834
	// x/ansi was already an indirect pin (same org/version); promoted to a
	// direct dep by shell_view.go's cursor overlay (ansi.Cut/StringWidth).
	github.com/charmbracelet/x/ansi v0.11.7
	github.com/charmbracelet/x/vt v0.0.0-20260430013151-79116d1f37bd
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
	koto-protocol v0.0.0
)

// The only code shared with the daemon is the proto contract: the TUI
// imports koto-protocol/pb (committed generated code) and nothing else
// from that module. The generated stubs pull in grpc + protobuf, both
// pinned here under the same 6-week rule.
replace koto-protocol => ../protocol

require (
	github.com/charmbracelet/log v1.0.0
	github.com/muesli/termenv v0.16.0
)

require (
	github.com/alecthomas/chroma/v2 v2.20.0 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.2 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260303162955-0b88c25f3fff // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/exp/slice v0.0.0-20250327172914-2fdc97757edf // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dlclark/regexp2 v1.11.5 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/go-logfmt/logfmt v0.6.1 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.23 // indirect
	github.com/microcosm-cc/bluemonday v1.0.27 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/reflow v0.3.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yuin/goldmark v1.7.13 // indirect
	github.com/yuin/goldmark-emoji v1.0.6 // indirect
	golang.org/x/exp v0.0.0-20231006140011-7918f672742d // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/term v0.39.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260120221211-b8f7ae30c516 // indirect
)
