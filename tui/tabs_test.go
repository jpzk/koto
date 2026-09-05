package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestExpandTabs(t *testing.T) {
	for in, want := range map[string]string{
		"a\tb":            "a       b",
		"\t":              "        ",
		"12345678\tx":     "12345678        x",
		"1234567\tx\n\ty": "1234567 x\n        y",
		"中\tx":            "中      x",
		"none":            "none",
	} {
		if got := expandTabs(in); got != want {
			t.Errorf("expandTabs(%q) = %q, want %q", in, got, want)
		}
	}
}

// A transcript line carrying a raw tab used to reach the terminal as a tab:
// measured as zero cells, padded to the column width, then advanced by the
// terminal to its next stop — a row wider than the frame, which wraps, which
// scrolls the whole screen. No frame may contain a tab, and every row must
// be exactly the terminal width.
func TestFrameHasNoTabsAndExactRows(t *testing.T) {
	for _, sz := range [][2]int{{140, 40}, {80, 24}} {
		m := newModel("", 200000)
		m.groups = map[string]GroupInfo{"g": {Running: true, Provider: "claudesdk", Model: "claude-sonnet-5"}}
		m.cur = "g"
		nm, _ := m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		m = nm.(Model)
		row := strings.Repeat("7d\t2026-08-27\tr=0.881\tz=1.84\tdelta=-0.004\tdelta_sd=-0.01 ", 3)
		nm, _ = m.Update(historyMsg{group: "g", events: []Event{
			{Event: "prompt", Msg: "tabs\there\ttoo", Ts: 1785000000},
			{Event: "done", Text: "```\n" + row + "\n```\n\nprose\twith\ttabs " + row, Ts: 1785000001},
			{Event: "tool", Name: "Bash", Input: `{"command":"jq"}`, Ts: 1785000002},
			{Event: "tool_result_begin", Ts: 1785000002},
			{Event: "tool_result_done", Body: row + "\n" + row, Ts: 1785000002},
			{Event: "err", Text: "bad\tthing", Ts: 1785000003},
			{Event: "turn_end", Ts: 1785000003},
		}})
		m = nm.(Model)
		nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD}) // expand tool output
		m = nm.(Model)
		for step := 0; step < 6; step++ {
			frame := m.View()
			if strings.Contains(frame, "\t") {
				t.Fatalf("%dx%d step %d: frame contains a raw tab", sz[0], sz[1], step)
			}
			rows := strings.Split(frame, "\n")
			if len(rows) != sz[1] {
				t.Fatalf("%dx%d step %d: %d rows", sz[0], sz[1], step, len(rows))
			}
			for i, r := range rows {
				if w := ansi.StringWidth(r); w != sz[0] {
					t.Fatalf("%dx%d step %d row %d: width %d: %q", sz[0], sz[1], step, i, w, ansi.Strip(r))
				}
			}
			nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
			m = nm.(Model)
		}
	}
}
