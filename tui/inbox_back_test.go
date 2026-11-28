package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The back button: leftmost cells of the inbox title row, clickable and
// reachable with ← / shift+tab off the first filter.

func ibHeader(m Model) string {
	return strings.SplitN(ansi.Strip(m.View()), "\n", 2)[0]
}

func TestInboxBackLeftmost(t *testing.T) {
	for _, mono := range []bool{false, true} {
		t.Run(fmt.Sprintf("mono=%v", mono), func(t *testing.T) {
			defer func(prev bool) { monoMode = prev }(monoMode)
			monoMode = mono
			m := ibMixed(t)
			want := " ‹ back  "
			if mono {
				want = " < back  "
			}
			for _, w := range []int{80, 120} {
				m.width = w
				for _, focus := range []bool{false, true} {
					m.inboxView.back = focus
					h := ibHeader(m)
					if !strings.HasPrefix(h, want) {
						t.Fatalf("w=%d focus=%v: header %q, want prefix %q", w, focus, h, want)
					}
					if strings.Index(h, "back") > strings.Index(h, "inbox (") {
						t.Fatalf("back is not left of the title: %q", h)
					}
				}
			}
			if mono && strings.ContainsRune(ibHeader(m), '‹') {
				t.Fatal("mono header still has the unicode chevron")
			}
		})
	}
}

// Focused, the button carries the active-filter highlight; unfocused it
// does not.
func TestInboxBackFocusStyle(t *testing.T) {
	withColor(t)
	m := ibMixed(t)
	off := m.inboxBackButton()
	m.inboxView.back = true
	if on := m.inboxBackButton(); on == off || ansi.Strip(on) != ansi.Strip(off) {
		t.Fatalf("focus style: on %q off %q", on, off)
	}
}

func TestInboxBackMouseClickExits(t *testing.T) {
	for x := 0; x < ansi.StringWidth(inboxBackLabel()); x++ {
		m := ibClick(t, ibMixed(t), x, 0)
		if m.inboxView.open {
			t.Fatalf("click at x=%d on the back button did not leave the inbox", x)
		}
		if m.approvalCard.open {
			t.Fatalf("click at x=%d opened an item", x)
		}
	}
	// just past the button: the title, stays open
	if m := ibClick(t, ibMixed(t), ansi.StringWidth(inboxBackLabel())+1, 0); !m.inboxView.open {
		t.Fatal("click on the title left the inbox")
	}
}

// Filter clicks still land on their (shifted) labels, at both widths.
func TestInboxBackFilterClicksShifted(t *testing.T) {
	for _, w := range []int{80, 120} {
		m := ibMixed(t)
		m.width = w
		h := ibHeader(m)
		sp := m.inboxFilterSpans()
		for f, name := range inboxFilterNames {
			lbl := fmt.Sprintf("%s (%d)", name, m.inboxFilterCounts()[f])
			i := strings.Index(h, lbl)
			if i < 0 {
				t.Fatalf("w=%d: %q missing from %q", w, lbl, h)
			}
			if got := ansi.StringWidth(h[:i]); got != sp[f][0] || sp[f][1]-sp[f][0] != ansi.StringWidth(lbl) {
				t.Fatalf("w=%d %s: span %v, label at %d", w, name, sp[f], got)
			}
			if sp[f][0] < ansi.StringWidth(inboxBackLabel()) {
				t.Fatalf("w=%d %s: span %v overlaps the back button", w, name, sp[f])
			}
		}
		for _, f := range []int{inboxFilterNotes, inboxFilterDecisions, inboxFilterAll} {
			for _, x := range []int{sp[f][0], sp[f][1] - 1} {
				m = ibClick(t, m, x, 0)
				if m.inboxFilter != f || !m.inboxView.open {
					t.Fatalf("w=%d click x=%d on %s: filter %d open %v", w, x, inboxFilterNames[f], m.inboxFilter, m.inboxView.open)
				}
			}
		}
	}
}

func TestInboxBackKeyboardLeftEnterExits(t *testing.T) {
	for _, k := range []tea.KeyMsg{{Type: tea.KeyLeft}, keyShiftTab} {
		for _, act := range []tea.KeyMsg{keyEnter, {Type: tea.KeySpace, Runes: []rune{' '}}} {
			m := ibMixed(t)
			m, _ = dpress(t, m, k)
			if !m.inboxView.back || !m.inboxView.open {
				t.Fatalf("%s from filter 0: back %v open %v", k, m.inboxView.back, m.inboxView.open)
			}
			if !strings.Contains(ansi.Strip(m.View()), "enter/space") {
				t.Fatal("hint does not explain the focused back button")
			}
			m, _ = dpress(t, m, k) // leftmost: stays
			if !m.inboxView.back {
				t.Fatalf("%s on the back button moved focus", k)
			}
			m, _ = dpress(t, m, act)
			if m.inboxView.open || m.approvalCard.open {
				t.Fatalf("%q on the back button: open %v card %v", act.String(), m.inboxView.open, m.approvalCard.open)
			}
		}
	}
}

func TestInboxBackRightReturnsToFilters(t *testing.T) {
	for _, k := range []tea.KeyMsg{{Type: tea.KeyRight}, keyTab} {
		m := ibMixed(t)
		m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyLeft})
		m, _ = dpress(t, m, k)
		if m.inboxView.back || m.inboxFilter != inboxFilterAll || !m.inboxView.open {
			t.Fatalf("%s from back: back %v filter %d", k, m.inboxView.back, m.inboxFilter)
		}
		m, _ = dpress(t, m, k) // and on through the filters as before
		if m.inboxFilter != inboxFilterDecisions {
			t.Fatalf("%s again: filter %d, want decisions", k, m.inboxFilter)
		}
	}
}

func TestInboxBackEscStillExits(t *testing.T) {
	for _, focus := range []bool{false, true} {
		for _, k := range []tea.KeyMsg{keyEsc, runeKey('q')} {
			m := ibMixed(t)
			m.inboxView.back = focus
			m, _ = dpress(t, m, k)
			if m.inboxView.open {
				t.Fatalf("%s (back focus %v) did not leave the inbox", k, focus)
			}
		}
	}
	// reopening starts with the focus on the filters
	m := ibMixed(t)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = dpress(t, m, keyEsc)
	if m = openList(t, m); m.inboxView.back {
		t.Fatal("reopened inbox kept the back focus")
	}
}

// Unfocused, enter on a row still opens it (rows unchanged).
func TestInboxBackUnfocusedEnterOpensRow(t *testing.T) {
	m := ibMixed(t)
	m, _ = dpress(t, m, runeKey('j')) // q1, an ask
	m, _ = dpress(t, m, keyEnter)
	if !m.approvalCard.open || !m.inboxView.open {
		t.Fatalf("enter on q1: card %v open %v", m.approvalCard.open, m.inboxView.open)
	}
}

// Evidence dump: ANSI-stripped header (and hint) at 80/120, focused and
// not; written to $INBOX_BACK_RENDER when set. The ^ line marks the cells
// drawn with the highlight style (back button or active filter).
func TestInboxBackRenderDump(t *testing.T) {
	m := ibMixed(t)
	var b strings.Builder
	for _, w := range []int{80, 120} {
		m.width = w
		for _, focus := range []bool{false, true} {
			m.inboxView.back = focus
			lines := strings.Split(ansi.Strip(m.View()), "\n")
			hl := [2]int{0, ansi.StringWidth(inboxBackLabel())}
			if !focus {
				hl = m.inboxFilterSpans()[m.inboxFilter]
			}
			fmt.Fprintf(&b, "== width=%d back-focused=%v\n%s\n%s%s\n%s\n\n", w, focus, lines[0],
				strings.Repeat(" ", hl[0]), strings.Repeat("^", hl[1]-hl[0]), lines[len(lines)-1])
		}
	}
	t.Log("\n" + b.String())
	if p := os.Getenv("INBOX_BACK_RENDER"); p != "" {
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
