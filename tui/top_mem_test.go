package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The fleet view's memory row: cap/committed/free from the host rollup and a
// per-VM map whose segments carry the running groups' names, sized by their
// committed share. A daemon that reports no cap renders no row and the
// layout budget must agree.
func TestTopMemLine(t *testing.T) {
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{}
	if m.topMemRows() != 0 || m.renderTopMemLine(100) != "" {
		t.Fatalf("no cap → expected no memory row")
	}
	m.hostRes = HostRes{Groups: 3, RunningGroups: 2, MemCapMiB: 14040, MemCommittedMiB: 4096, MemHostTotalMiB: 16000}
	m.resources = map[string]GroupRes{
		"main":  {Running: true, MemMiB: 1024, MemCommittedMiB: 1536},
		"ghost": {Running: true, MemMiB: 2048, MemCommittedMiB: 2560},
		"off":   {Running: false, MemMiB: 1024},
	}
	for _, g := range []string{"main", "ghost", "off"} {
		m.groups[g] = GroupInfo{Running: m.resources[g].Running}
	}
	if m.topMemRows() != 1 {
		t.Fatalf("cap set → expected 1 memory row")
	}
	line := ansi.Strip(m.renderTopMemLine(120))
	for _, want := range []string{"vm mem 4.0G/14G 29%", "free 9.7G", "ghost", "main", "[", "]"} {
		if !strings.Contains(line, want) {
			t.Errorf("memory row %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "off") {
		t.Errorf("stopped group mapped: %q", line)
	}
	if strings.Index(line, "ghost") > strings.Index(line, "main") {
		t.Errorf("segments not biggest-first: %q", line)
	}
	if w := ansi.StringWidth(m.renderTopMemLine(120)); w > 120 {
		t.Errorf("row overflows: %d > 120", w)
	}
}
