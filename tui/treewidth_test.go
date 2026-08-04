package main

// treewidth_test.go — the tree cursor row's amber background must span the
// same fixed width on every row type. The branch glyphs ("├─ ") are 3 cells
// but 7 bytes, so any byte-counted pad math shows up as a background that
// ends early on session/job rows.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestTreeCursorRowConstantWidth renders the hover highlight across a group
// row, a badged group row, a session row, and a job row, and requires one
// width.
func TestTreeCursorRowConstantWidth(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 100, 30
	m.groups = map[string]GroupInfo{
		"main": {Running: true, Sessions: []string{"work"},
			Jobs: []JobInfo{{ID: "j1", Status: "running"}}},
		"busy": {Running: true, Queued: 3},
	}
	m.cur = "main"
	pad := func(s string, w int) string {
		if len(s) >= w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	rows := map[string]treeRow{
		"group":       {group: "main"},
		"group+badge": {group: "busy", branch: "└─ "},
		"session":     {group: "main", session: "work", branch: "├─ "},
		"job":         {group: "main", job: "j1", branch: "│  ├─ "},
	}
	widths := map[string]int{}
	for k, r := range rows {
		widths[k] = lipgloss.Width(m.renderTreeRow(r, false, true, false, pad))
	}
	want := widths["group"]
	for k, w := range widths {
		if w != want {
			t.Errorf("cursor row width varies: %s = %d, group = %d (all: %v)",
				k, w, want, widths)
		}
	}
}
