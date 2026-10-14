package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// assertRowsFit fails if any row is wider than w display columns, measured
// ANSI-aware (escapes free, wide runes 2) — the same measure the terminal
// applies, so a pass here means nothing is clipped at the pane edge.
func assertRowsFit(t *testing.T, rows []string, w int) {
	t.Helper()
	for i, r := range rows {
		if got := ansi.StringWidth(r); got > w {
			t.Fatalf("row %d is %d cols wide, limit %d: %q", i, got, w, r)
		}
	}
}

// assertNoBrokenEscapes fails if a row contains an ESC that does not start
// a complete sequence — i.e. the wrap split one. Strip removes every
// well-formed sequence, so any ESC (or CSI remnant) left behind is broken.
func assertNoBrokenEscapes(t *testing.T, rows []string) {
	t.Helper()
	for i, r := range rows {
		plain := ansi.Strip(r)
		if strings.ContainsRune(plain, '\x1b') {
			t.Fatalf("row %d has a split escape sequence: %q", i, r)
		}
		if strings.Contains(plain, "[38;") || strings.Contains(plain, "[1m") {
			t.Fatalf("row %d leaked escape-sequence text: %q", i, plain)
		}
	}
}

// plainJoin re-joins wrapped rows into the text a reader sees: indent
// stripped from continuation rows, rows separated by one space where the
// wrap broke at a space.
func plainRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		p := ansi.Strip(r)
		if i > 0 {
			p = strings.TrimPrefix(p, logWrapIndent)
		}
		out[i] = p
	}
	return out
}

func TestWrapLogLineShortUnchanged(t *testing.T) {
	s := "\x1b[1mINFO\x1b[m short"
	rows := wrapLogLine(s, 40)
	if len(rows) != 1 || rows[0] != s {
		t.Fatalf("short line changed: %q", rows)
	}
}

// A plain line longer than the width becomes several rows, each within the
// width, continuation rows indented, no words lost.
func TestWrapLogLinePlainLong(t *testing.T) {
	s := "10:00:00 INFO fc: group=ghost microVM booted in 412ms with 2 vcpus and 1024 MiB of memory, rootfs overlay ready"
	const w = 40
	rows := wrapLogLine(s, w)
	if len(rows) < 3 {
		t.Fatalf("want >=3 rows at width %d, got %d: %q", w, len(rows), rows)
	}
	assertRowsFit(t, rows, w)
	for i, r := range rows[1:] {
		if !strings.HasPrefix(r, logWrapIndent) || strings.HasPrefix(r, logWrapIndent+" ") {
			t.Fatalf("continuation row %d not indented by exactly %q: %q", i+1, logWrapIndent, r)
		}
	}
	if got := strings.Join(plainRows(rows), " "); got != s {
		t.Fatalf("word-wrap lost text:\n got %q\nwant %q", got, s)
	}
}

// A single token longer than the row is hard-broken, not clipped.
func TestWrapLogLineHardBreaksLongWord(t *testing.T) {
	tok := strings.Repeat("abcdefghij", 9) // 90 cols, no spaces
	s := "ERROR " + tok
	const w = 20
	rows := wrapLogLine(s, w)
	assertRowsFit(t, rows, w)
	if got := strings.Join(plainRows(rows), ""); strings.ReplaceAll(got, " ", "") != strings.ReplaceAll(s, " ", "") {
		t.Fatalf("hard-break lost text: %q", got)
	}
}

// Styled lines (the shape formatLogLine produces: SGR around timestamp,
// level, prefix) wrap without splitting escape sequences, and a style open
// at the break is reset at row end and re-opened on the next row.
func TestWrapLogLineANSI(t *testing.T) {
	red := "\x1b[1;38;5;196m"
	s := "\x1b[38;5;245m10:00:00\x1b[m " + red + "ERROR\x1b[m \x1b[1;38;5;130mfc:\x1b[m " +
		red + strings.Repeat("styled payload word ", 8) + "\x1b[m"
	const w = 30
	rows := wrapLogLine(s, w)
	if len(rows) < 3 {
		t.Fatalf("want >=3 rows, got %d", len(rows))
	}
	assertRowsFit(t, rows, w)
	assertNoBrokenEscapes(t, rows)
	for i, r := range rows[:len(rows)-1] {
		if !strings.HasSuffix(r, ansi.ResetStyle) && !strings.HasSuffix(r, "\x1b[m") {
			t.Fatalf("row %d ends inside a style without reset: %q", i, r)
		}
	}
	// the red payload continues red on row 2+
	if !strings.Contains(rows[1], red) {
		t.Fatalf("style not carried onto continuation row: %q", rows[1])
	}
	if got := strings.Join(plainRows(rows), " "); strings.TrimSpace(got) != strings.TrimSpace(ansi.Strip(s)) {
		t.Fatalf("ANSI wrap lost text:\n got %q\nwant %q", got, ansi.Strip(s))
	}
}

// CJK and emoji are 2 cols each: rows are measured by display width, and a
// wide rune is never split across rows (it would render as garbage).
func TestWrapLogLineWideRunes(t *testing.T) {
	s := "INFO \x1b[1m日本語のログ行です\x1b[m " + strings.Repeat("🚀🔥", 10) + " 中文日志" + strings.Repeat("漢", 15)
	for _, w := range []int{11, 17, 24, 40} {
		rows := wrapLogLine(s, w)
		assertRowsFit(t, rows, w)
		assertNoBrokenEscapes(t, rows)
		for i, r := range rows {
			if strings.ContainsRune(ansi.Strip(r), '�') {
				t.Fatalf("w=%d row %d split a rune: %q", w, i, r)
			}
		}
		joined := strings.ReplaceAll(strings.Join(plainRows(rows), ""), " ", "")
		if want := strings.ReplaceAll(ansi.Strip(s), " ", ""); joined != want {
			t.Fatalf("w=%d wide-rune wrap lost text:\n got %q\nwant %q", w, joined, want)
		}
	}
}

// logWrapModel is a log-view model at the given terminal width holding one
// very long line plus some short ones.
func logWrapModel(t *testing.T, width int) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = width, 20
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	m.logSubActive = true
	m.enterLog()
	for i := 0; i < 30; i++ {
		m.appendLogEvent(LogEvent{Event: "log", Level: "info", Subsystem: "cron", Msg: "tick"})
	}
	m.appendLogEvent(LogEvent{Event: "log", Level: "warn", Subsystem: "egress",
		Msg: "group=ghost dropped outbound connection to 203.0.113.7:443 (policy=wan, reason=lan-range) — " +
			strings.Repeat("detail ", 20) + "TAILMARK"})
	return m
}

func vpRows(m Model) []string {
	return strings.Split(m.logVP.View(), "\n")
}

// End to end through the viewport: the long frame shows up as multiple rows,
// none wider than the pane, and its tail (which clipping used to hide) is
// visible.
func TestLogViewWrapsLongLine(t *testing.T) {
	m := logWrapModel(t, 40)
	rows := vpRows(m)
	assertRowsFit(t, rows, m.logVP.Width)
	assertNoBrokenEscapes(t, rows)
	if !strings.Contains(ansi.Strip(m.logVP.View()), "TAILMARK") {
		t.Fatalf("tail of the long line not visible (clipped?):\n%s", ansi.Strip(m.logVP.View()))
	}
	if n := m.logVP.TotalLineCount(); n <= 31 {
		t.Fatalf("31 frames should be >31 rows once the long one wraps, got %d", n)
	}
}

// Resizing narrower then wider rewraps from the original line each time, and
// auto-follow stays pinned to the bottom across both rewraps.
func TestLogViewResizeRewrapsAndFollows(t *testing.T) {
	m := logWrapModel(t, 120)
	wide := m.logVP.TotalLineCount()
	if !m.logVP.AtBottom() || !m.logAutoFollow {
		t.Fatalf("expected to start following at bottom")
	}

	resize := func(w int) {
		nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 20})
		m = nm.(Model)
	}
	resize(40)
	narrow := m.logVP.TotalLineCount()
	if narrow <= wide {
		t.Fatalf("narrowing should add rows: wide=%d narrow=%d", wide, narrow)
	}
	assertRowsFit(t, vpRows(m), m.logVP.Width)
	if !m.logVP.AtBottom() || !m.logAutoFollow {
		t.Fatalf("lost bottom after narrowing rewrap (YOffset=%d)", m.logVP.YOffset)
	}
	if !strings.Contains(ansi.Strip(m.logVP.View()), "TAILMARK") {
		t.Fatalf("bottom row not visible after narrowing")
	}

	resize(120)
	if got := m.logVP.TotalLineCount(); got != wide {
		t.Fatalf("widening back should restore %d rows, got %d", wide, got)
	}
	assertRowsFit(t, vpRows(m), m.logVP.Width)
	if !m.logVP.AtBottom() || !m.logAutoFollow {
		t.Fatalf("lost bottom after widening rewrap")
	}

	// A new frame after the rewraps still lands in view.
	m.appendLogEvent(LogEvent{Event: "log", Level: "info", Subsystem: "fc", Msg: "after-resize-frame"})
	if !m.logVP.AtBottom() || !strings.Contains(ansi.Strip(m.logVP.View()), "after-resize-frame") {
		t.Fatalf("auto-follow broken after rewrap")
	}
}

// Scrolled up (not following), a rewrap must not yank the reader to the
// bottom.
func TestLogViewResizeKeepsScrolledUp(t *testing.T) {
	m := logWrapModel(t, 120)
	m.logVP.GotoTop()
	m.logAutoFollow = false
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = nm.(Model)
	if m.logAutoFollow || m.logVP.AtBottom() {
		t.Fatalf("rewrap jumped a scrolled-up reader to the bottom")
	}
}

// The whole log-view frame at a 40-col terminal (tree + pane + scrollbar,
// status/hint bars): no row exceeds the terminal width, and the long line's
// tail is on screen. Run with -v to see the frame (ANSI stripped) — that is
// the golden render quoted in the PR.
func TestLogViewFrameWidth40(t *testing.T) {
	m := logWrapModel(t, 40)
	frame := m.renderLogView()
	rows := strings.Split(frame, "\n")
	assertRowsFit(t, rows, 40)
	assertNoBrokenEscapes(t, rows)
	plain := ansi.Strip(frame)
	if !strings.Contains(plain, "TAILMARK") {
		t.Fatalf("long line tail missing from frame:\n%s", plain)
	}
	t.Logf("width-40 frame:\n%s", plain)
}
