package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// withMono turns B/W mode on for one test and restores it afterwards. The
// markdown renderer cache is keyed by width only, so it has to be dropped on
// both edges — the cached renderer carries the style the mode picked.
func withMono(t *testing.T) {
	t.Helper()
	prev := monoMode
	monoMode = true
	applyMonoProfile()
	invalidateMarkdownCache()
	t.Cleanup(func() {
		monoMode = prev
		invalidateMarkdownCache()
	})
}

// withColor pins the renderer to a color profile for the duration of a test.
// `go test` writes to a pipe, so termenv sniffs Ascii and lipgloss emits no
// escapes at all — which would make every assertion below vacuously pass.
func withColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func TestResolveMono(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"vt100", map[string]string{"KOTO_TUI_TERM": "vt100"}, true},
		{"vt100 variant", map[string]string{"KOTO_TUI_TERM": "vt100-am"}, true},
		{"vt220", map[string]string{"TERM": "vt220"}, true},
		{"dumb", map[string]string{"TERM": "dumb"}, true},
		{"terminfo mono variant", map[string]string{"TERM": "xterm-mono"}, true},
		{"linux-m", map[string]string{"TERM": "linux-m"}, true},
		{"xterm-256color", map[string]string{"TERM": "xterm-256color"}, false},
		{"kitty", map[string]string{"KOTO_TUI_TERM": "xterm-kitty"}, false},
		{"empty", map[string]string{}, false},
		{"vt with color suffix", map[string]string{"TERM": "vt220-color"}, false},
		// The host TERM wins over the container's, same as the notify sniffing:
		// `podman run -t` rewrites TERM to xterm inside cs_tui.
		{"host term wins", map[string]string{"KOTO_TUI_TERM": "vt100", "TERM": "xterm-256color"}, true},
		// Explicit override in both directions.
		{"forced on", map[string]string{"KOTO_TUI_MONO": "on", "TERM": "xterm-256color"}, true},
		{"forced off", map[string]string{"KOTO_TUI_MONO": "off", "TERM": "vt100"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveMono(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Fatalf("resolveMono(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// Every ASCII stand-in must occupy exactly as many cells as the glyph it
// replaces: the fold runs on an already-laid-out frame, so a narrower or wider
// replacement shifts every column to its right.
func TestMonoGlyphWidths(t *testing.T) {
	for r, repl := range monoGlyphs {
		want := ansi.StringWidth(string(r))
		if got := ansi.StringWidth(repl); got != want {
			t.Errorf("glyph %q -> %q: width %d, want %d", string(r), repl, got, want)
		}
		if ansi.StringWidth(repl) != len(repl) {
			t.Errorf("glyph %q -> %q: replacement is not ASCII", string(r), repl)
		}
	}
	if len(latin1Fold) != 0x40 {
		t.Fatalf("latin1Fold covers %d runes, want 64 (U+00C0..U+00FF)", len(latin1Fold))
	}
}

func TestFoldASCIIPreservesWidth(t *testing.T) {
	cases := []string{
		"● main ⚠ ⏳3 🔔 [████░░░░] 42%",
		"plain ascii stays put",
		"gruß — grüße · naïve …",
		"日本語", // wide runes fold to two cells each
	}
	for _, in := range cases {
		out := foldASCII(in)
		if ansi.StringWidth(out) != ansi.StringWidth(in) {
			t.Errorf("foldASCII(%q) = %q: width %d, want %d",
				in, out, ansi.StringWidth(out), ansi.StringWidth(in))
		}
		for _, r := range out {
			if r > 0x7f {
				t.Errorf("foldASCII(%q) = %q: left non-ASCII %q", in, out, string(r))
				break
			}
		}
	}
	if got := foldASCII("grüße"); got != "gruse" {
		t.Errorf("latin-1 fold = %q, want %q", got, "gruse")
	}
}

func TestStripSGRColor(t *testing.T) {
	cases := []struct{ in, want string }{
		// plain color: the whole sequence goes
		{"\x1b[38;5;214mkoto\x1b[0m", "koto\x1b[0m"},
		// attributes survive, the color riding along with them does not
		{"\x1b[1;38;5;214;4mkoto\x1b[0m", "\x1b[1;4mkoto\x1b[0m"},
		// reverse video is the mono highlight — it must never be stripped
		{"\x1b[7;30;43m koto \x1b[0m", "\x1b[7m koto \x1b[0m"},
		// background alone (the black backdrop that breaks a white terminal)
		{"\x1b[40mbar\x1b[0m", "bar\x1b[0m"},
		// truecolor arguments are consumed with their introducer
		{"\x1b[48;2;20;30;40;1mx\x1b[0m", "\x1b[1mx\x1b[0m"},
		// bright fg/bg
		{"\x1b[91;107mx\x1b[m", "x\x1b[m"},
		// non-SGR escapes pass through untouched
		{"\x1b[2Jclear\x1b[3;5H", "\x1b[2Jclear\x1b[3;5H"},
		// so do OSC payloads (the desktop-notification channel)
		{"\x1b]9;hello;31m\x07after", "\x1b]9;hello;31m\x07after"},
		{"no escapes at all", "no escapes at all"},
	}
	for _, tc := range cases {
		if got := stripSGRColor(tc.in); got != tc.want {
			t.Errorf("stripSGRColor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// sgrColorParams reports the color parameters left in s — the assertion behind
// the whole mode: on a B/W terminal we emit no color, and above all no
// background, so the terminal's own pair (black on white just as well as white
// on black) is what shows.
var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

func sgrColorParams(s string) []string {
	var bad []string
	for _, m := range sgrRe.FindAllStringSubmatch(s, -1) {
		for _, f := range strings.Split(m[1], ";") {
			n, err := strconv.Atoi(f)
			if err != nil {
				continue
			}
			switch {
			case n >= 30 && n <= 49, n >= 90 && n <= 97, n >= 100 && n <= 107:
				bad = append(bad, m[0])
			}
		}
	}
	return bad
}

// monoTestModel builds a model with enough state on it that the frame exercises
// the color-carrying furniture: the tree with a cursor row, a live
// notification, metrics chips over the alert threshold, and rendered markdown.
func monoTestModel(t *testing.T) Model {
	t.Helper()
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{
		"main":  {Running: true, Provider: "claudesdk", Model: "claude-sonnet-5"},
		"ghost": {Running: false, Queued: 2},
	}
	m.cur = "main"
	m.metric = map[string]any{"usage": map[string]any{
		"input_tokens": 180000.0, "cache_read_input_tokens": 20000.0,
	}}
	m.globalMetric = map[string]any{"ratelimit": map[string]any{
		"anthropic-ratelimit-unified-5h-utilization": "0.93",
		"anthropic-ratelimit-unified-7d-utilization": "0.12",
	}}
	m.resources = map[string]GroupRes{"main": {
		Running: true, Vcpus: 2, MemMiB: 1024, CPUPct: 190,
		RSSBytes: 900 << 20, AllocBytes: 7 << 30, DeclaredBytes: 8 << 30,
	}}
	m.notifications = append(m.notifications, notifyItem{
		at: time.Now(), group: "main", severity: "high", title: "ERROR fc", msg: "boot failed",
	})
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = nm.(Model)
	nm, _ = m.Update(historyMsg{group: "main", events: []Event{
		{Event: "prompt", Msg: "status? — ünïcode", Ts: 1785000000},
		{Event: "done", Text: "# heading\n\n**bold** and `code` — done ✓", Ts: 1785000001},
	}})
	return nm.(Model)
}

// The frame a B/W terminal receives must carry no color at all — not in the
// bars, not in the tree, not in the markdown glamour renders — and no
// non-ASCII byte.
func TestMonoFrameHasNoColorOrUnicode(t *testing.T) {
	withColor(t)
	withMono(t)
	m := monoTestModel(t)
	for _, focus := range []focusZone{focusInput, focusTree} {
		m.focus = focus
		frame := m.View()
		if bad := sgrColorParams(frame); len(bad) > 0 {
			t.Errorf("focus %v: frame carries color sequences: %q", focus, bad[:min(3, len(bad))])
		}
		for _, r := range frame {
			if r > 0x7f {
				t.Errorf("focus %v: frame carries non-ASCII rune %q", focus, string(r))
				break
			}
		}
	}
}

// Same for the two other full-frame views, which return before the chat
// layout and so bypass its code entirely.
func TestMonoFullFrameViewsHaveNoColor(t *testing.T) {
	withColor(t)
	withMono(t)
	m := monoTestModel(t)
	m.hostRes = HostRes{Groups: 2, RunningGroups: 1,
		FsTotalBytes: 50 << 30, FsFreeBytes: 2 << 30, AllocTotalBytes: 20 << 30, ProvisionedBytes: 200 << 30}
	for _, f := range []focusZone{focusTop, focusLog} {
		m.focus = f
		if bad := sgrColorParams(m.View()); len(bad) > 0 {
			t.Errorf("focus %v: frame carries color sequences: %q", f, bad[:min(3, len(bad))])
		}
	}
}

// Folding must not move any column: no row may exceed the terminal width, and
// the frame must keep its row count.
func TestMonoFrameGeometryUnchanged(t *testing.T) {
	withColor(t)
	m := monoTestModel(t)
	m.focus = focusTree
	colorRows := strings.Split(m.View(), "\n")

	withMono(t)
	m2 := monoTestModel(t)
	m2.focus = focusTree
	monoRows := strings.Split(m2.View(), "\n")

	if len(monoRows) != len(colorRows) {
		t.Fatalf("mono frame has %d rows, color frame %d", len(monoRows), len(colorRows))
	}
	for i, row := range monoRows {
		if w := ansi.StringWidth(row); w > m2.width {
			t.Errorf("row %d is %d cells wide, terminal is %d: %q", i, w, m2.width, row)
		}
	}
}

// Color is not the only thing the frame loses — it also has to gain the
// attributes that stand in for it: reverse video on the tree's cursor row and
// the koto banner, underline on an over-threshold metric chip.
func TestMonoKeepsDistinctions(t *testing.T) {
	withColor(t)
	withMono(t)
	m := monoTestModel(t)
	m.focus = focusTree

	if !strings.Contains(m.renderStatusLeft(), "\x1b[") {
		t.Error("status bar lost all styling in mono")
	}
	if !hasSGRParam(m.renderStatusLeft(), "7") {
		t.Error("koto banner is not reverse video in mono — nothing marks the bar")
	}
	tree := m.renderTree(20)
	if !hasSGRParam(tree, "7") {
		t.Error("tree cursor row is not reverse video in mono — the selection is invisible")
	}
	// 5h utilization is 93% in the fixture, over pctColor's 80% alert tier.
	if !hasSGRParam(m.renderMetricsBar(), "4") {
		t.Error("over-threshold metric chip is not underlined in mono — the alert tier is invisible")
	}
	// Focused vs unfocused prompt box is an amber-vs-gray border in color; in
	// mono the border glyphs themselves have to differ.
	m.focus = focusInput
	focused := m.renderInput()
	m.focus = focusTree
	if unfocused := m.renderInput(); strings.Contains(focused, "=") == strings.Contains(unfocused, "=") {
		t.Error("prompt box border does not change with focus in mono")
	}
}

// hasSGRParam reports whether any SGR sequence in s sets the given parameter.
func hasSGRParam(s, param string) bool {
	for _, m := range sgrRe.FindAllStringSubmatch(s, -1) {
		for _, f := range strings.Split(m[1], ";") {
			if f == param {
				return true
			}
		}
	}
	return false
}

// Color mode must be untouched by all of the above.
func TestColorModeUnaffected(t *testing.T) {
	if monoMode {
		t.Fatal("monoMode leaked out of a mono test")
	}
	withColor(t)
	m := monoTestModel(t)
	m.focus = focusTree
	frame := m.View()
	if len(sgrColorParams(frame)) == 0 {
		t.Error("color frame carries no color at all")
	}
	if !strings.Contains(frame, "●") {
		t.Error("color frame lost its Unicode furniture")
	}
}
