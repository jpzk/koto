package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// #169: the status-bar inbox counter is right-aligned — it ends exactly one
// cell before the first visible cell of the right side (activity / tok-s),
// the free space is on its left, and the right side keeps its exact cells.
func TestInboxStatusRightAligned(t *testing.T) {
	m := ibMixed(t)
	m.inboxView = inboxViewState{}
	empty := m
	empty.inboxItems, empty.approvals = nil, nil
	want := map[int]string{50: "≡ 3·2", 80: "≡ 3 decide · 2 note", 120: "≡ 3 decide · 2 note"}
	for _, w := range []int{50, 80, 120} {
		m.width, empty.width = w, w
		bar := ansi.Strip(m.renderStatusBar(""))
		if cellWidth(bar) != w {
			t.Fatalf("w=%d: bar %d cells wide: %q", w, cellWidth(bar), bar)
		}
		right := strings.TrimLeft(ansi.Strip(m.renderStatusRight("")), " ")
		form := want[w]
		suffix := form + " " + right
		if !strings.HasSuffix(bar, suffix) {
			t.Fatalf("w=%d: counter %q not 1 cell left of %q in %q", w, form, right, bar)
		}
		pre := strings.TrimSuffix(bar, suffix)
		if !strings.HasSuffix(pre, " ") {
			t.Fatalf("w=%d: no space between left chips and counter: %q", w, bar)
		}
		if strings.Contains(pre, "≡") {
			t.Fatalf("w=%d: counter drawn twice: %q", w, bar)
		}
		// bar is exactly w cells and ends with the whole right side, so the
		// metrics hold the last cells, untruncated — the counter pushed nothing.
		// (At 50 the loading bar, which clips them without a counter, gives way.)
	}
	// Too narrow for even the short form: hidden, never clipped, and the bar
	// is exactly what it would be with an empty inbox.
	for _, w := range []int{20, 35, 44} {
		m.width, empty.width = w, w
		bar, eb := ansi.Strip(m.renderStatusBar("")), ansi.Strip(empty.renderStatusBar(""))
		if strings.Contains(bar, "≡") || bar != eb {
			t.Fatalf("w=%d: counter should hide: with %q without %q", w, bar, eb)
		}
	}
}

// #169: nothing waiting -> no segment, no stray spaces: the bar is the plain
// left + gap + right layout.
func TestInboxStatusZeroNoSegment(t *testing.T) {
	m := ibMixed(t)
	m.inboxView = inboxViewState{}
	m.inboxItems, m.approvals = nil, nil
	for _, w := range []int{50, 80, 120} {
		m.width = w
		bar := ansi.Strip(m.renderStatusBar(""))
		if strings.Contains(bar, "≡") || strings.Contains(bar, "decide") {
			t.Fatalf("w=%d: empty inbox shows a counter: %q", w, bar)
		}
		l, r := ansi.Strip(m.renderStatusLeft()), ansi.Strip(m.renderStatusRight(""))
		if gap := w - cellWidth(l) - cellWidth(r); gap >= 0 && bar != l+strings.Repeat(" ", gap)+r {
			t.Fatalf("w=%d: zero-inbox bar %q != plain layout", w, bar)
		}
	}
}

// treeLineCol is the cell column where label starts on the tree line that
// contains it (ANSI stripped, measured with cellWidth, not bytes).
func treeLineCol(t *testing.T, tree, label string) int {
	t.Helper()
	for _, l := range strings.Split(ansi.Strip(tree), "\n") {
		if i := strings.Index(l, label); i >= 0 {
			return cellWidth(l[:i])
		}
	}
	t.Fatalf("no %q in tree:\n%s", label, ansi.Strip(tree))
	return -1
}

// #170: "inbox" starts in the same column as "main", glyphs on and off, empty
// and non-empty, and the row stays exactly contentW+1 cells.
func TestInboxTreeRowColumnMatchesMain(t *testing.T) {
	pn, pm := nerdFont, monoMode
	t.Cleanup(func() { nerdFont, monoMode = pn, pm })
	for _, nf := range []bool{true, false} {
		for _, mono := range []bool{false, true} {
			nerdFont, monoMode = nf, mono
			for _, items := range [][]int{nil, {1}} {
				m := ibModel(t)
				if items != nil {
					m = ibFeed(t, m, ibAsk("q1", 100, "a", "b"), ibNote("n1", 101, "hi"))
				}
				tree := m.renderTree(8)
				ci, cm := treeLineCol(t, tree, "inbox ("), treeLineCol(t, tree, "main")
				if ci != cm {
					t.Fatalf("nerdFont=%v mono=%v n=%d: inbox at col %d, main at col %d\n%s", nf, mono, m.inboxCount(), ci, cm, ansi.Strip(tree))
				}
				for _, hov := range []bool{false, true} {
					if w := cellWidth(m.renderInboxTreeRow(hov)); w != leftPaneWidth-1 {
						t.Fatalf("nerdFont=%v mono=%v hov=%v: row %d cells, want %d", nf, mono, hov, w, leftPaneWidth-1)
					}
				}
			}
		}
	}
}

// sgrBefore is the last SGR sequence ahead of label in raw — the style the
// label is drawn in.
func sgrBefore(t *testing.T, raw, label string) string {
	t.Helper()
	i := strings.Index(raw, label)
	if i < 0 {
		t.Fatalf("no %q in %q", label, raw)
	}
	j := strings.LastIndex(raw[:i], "\x1b[")
	if j < 0 {
		return ""
	}
	return raw[j : j+strings.IndexByte(raw[j:], 'm')+1]
}

// #170 (operator clarification): empty (N=0) the inbox row stays gray; with
// something waiting (N>0) it wears main's normal-state foreground and weight
// (no yellow bold), following main when main is stopped.
func TestInboxTreeRowColorMatchesMain(t *testing.T) {
	withColor(t)
	resetTheme()
	for _, running := range []bool{true, false} {
		for _, n := range []int{0, 2} {
			m := ibModel(t)
			if n > 0 {
				m = ibFeed(t, m, ibAsk("q1", 100, "a", "b"), ibNote("n1", 101, "hi"))
			}
			g := m.groups["main"]
			g.Running = running
			m.groups["main"] = g
			mainRow := m.renderTreeRow(treeRow{group: "main"}, false, false, false, padCells)
			inboxRow := m.renderInboxTreeRow(false)
			ms, is := sgrBefore(t, mainRow, "main"), sgrBefore(t, inboxRow, "inbox (")
			if n == 0 {
				ms = sgrBefore(t, fg(cGray, "inbox ("), "inbox (")
			}
			if ms == "" || ms != is {
				t.Fatalf("running=%v n=%d: inbox style %q != want %q (n=0 gray, n>0 main)", running, m.inboxCount(), is, ms)
			}
			if gray := sgrBefore(t, fg(cGray, "x"), "x"); n > 0 && running && is == gray {
				t.Fatalf("n=%d with main running: inbox row still gray %q", n, is)
			}
			if !strings.Contains(inboxRow, "inbox (") || strings.Count(inboxRow, "\x1b[") > 2 {
				t.Fatalf("running=%v n=%d: inbox row not one uniform span: %q", running, n, inboxRow)
			}
		}
	}
	// the cursor bar is the shared amber one
	m := ibModel(t)
	if a, b := sgrBefore(t, m.renderInboxTreeRow(true), "inbox ("), sgrBefore(t, m.renderTreeRow(treeRow{group: "main"}, false, true, false, padCells), "main"); a != b {
		t.Fatalf("hover style %q != main hover %q", a, b)
	}
}

// The inbox logo must be a 1-cell glyph: "≡ " (glyph + space) is exactly 2
// cells, so it fills the tree marker slot like "● " and the status counter
// measures what the terminal draws. A future East-Asian-Wide glyph (like
// the old U+2630 trigram, 2 cells to ansi.StringWidth) fails here.
func TestInboxGlyphOneCell(t *testing.T) {
	pn, pm := nerdFont, monoMode
	t.Cleanup(func() { nerdFont, monoMode = pn, pm })
	monoMode = false
	if w := cellWidth("≡ "); w != 2 {
		t.Fatalf("cellWidth(%q) = %d, want 2", "≡ ", w)
	}
	if w := cellWidth(gl("≡ ", "")); w != treeMarkerW {
		t.Fatalf("logo+space %d cells, want treeMarkerW %d", w, treeMarkerW)
	}
	m := ibFeed(t, ibModel(t), ibAsk("q1", 100, "a", "b"))
	row := ansi.Strip(m.renderInboxTreeRow(false))
	if !strings.HasPrefix(row, " ≡ inbox (") {
		t.Fatalf("tree row %q, want \" ≡ inbox (\" prefix", row)
	}
	ci, cm := treeLineCol(t, m.renderTree(8), "inbox ("), treeLineCol(t, m.renderTree(8), "main")
	if ci != cm || ci != 1+treeMarkerW {
		t.Fatalf("inbox col %d, main col %d, want both %d", ci, cm, 1+treeMarkerW)
	}
}
