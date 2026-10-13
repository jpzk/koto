package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestWrapLineWordAware: prose must break at spaces, not mid-word (audit F3:
// "Ex|pected", "timeou|t" at 40 cols).
func TestWrapLineWordAware(t *testing.T) {
	ln := "Expected the VM to come up before the boot timeout elapsed"
	segs := wrapLine(ln, 20)
	words := map[string]bool{}
	for _, w := range strings.Fields(ln) {
		words[w] = true
	}
	for _, s := range segs {
		if w := ansi.StringWidth(s); w > 20 {
			t.Fatalf("segment %q is %d cells > 20", s, w)
		}
		for _, w := range strings.Fields(s) {
			if !words[w] {
				t.Errorf("word split mid-token: %q in %q (all: %q)", w, s, segs)
			}
		}
	}
	if got := strings.Join(strings.Fields(strings.Join(segs, " ")), " "); got != ln {
		t.Errorf("text lost/changed: %q", got)
	}
}

// TestWrapLineLongToken: an unbreakable token (hash/URL) still fits the budget.
func TestWrapLineLongToken(t *testing.T) {
	ln := "sha " + strings.Repeat("a", 50) + " end"
	segs := wrapLine(ln, 16)
	for _, s := range segs {
		if w := ansi.StringWidth(s); w > 16 {
			t.Fatalf("segment %q is %d cells > 16", s, w)
		}
	}
	if got := strings.ReplaceAll(strings.Join(segs, ""), " ", ""); got != strings.ReplaceAll(ln, " ", "") {
		t.Errorf("content lost: %q", got)
	}
}

// TestWrapLinePathBreak: long paths prefer breaking after "/".
func TestWrapLinePathBreak(t *testing.T) {
	segs := wrapLine("/workspace/goal/tui-improve/audit.md", 20)
	if len(segs) < 2 {
		t.Fatalf("expected wrap, got %q", segs)
	}
	for _, s := range segs[:len(segs)-1] {
		if !strings.HasSuffix(s, "/") && !strings.HasSuffix(s, "-") {
			t.Errorf("path segment %q not broken at / or -: %q", s, segs)
		}
	}
}
