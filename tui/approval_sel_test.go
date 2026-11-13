package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// cardAt is openCard at an arbitrary terminal width.
func cardAt(t *testing.T, w int) Model {
	t.Helper()
	m := narrow(t)
	sm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
	m = sm.(Model)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if !m.approvalCard.open {
		t.Fatal("card did not open")
	}
	return m
}

// Deny is preselected: the card opens with the Deny pill filled.
func TestApprovalSelDefaultDeny(t *testing.T) {
	m := openCard(t)
	if m.approvalCard.allow {
		t.Fatal("card opened with Allow selected")
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "‹ Deny ›") || strings.Contains(v, "‹ Allow ›") {
		t.Fatalf("selection row does not show Deny selected:\n%s", v)
	}
}

// → then enter allows; ← goes back to Deny (enter then denies).
func TestApprovalSelRightEnterAllows(t *testing.T) {
	sent := recordApprovals(t)
	m := openCard(t)
	m, _ = dpress(t, m, keyRight)
	if !m.approvalCard.allow || !strings.Contains(ansi.Strip(m.View()), "‹ Allow ›") {
		t.Fatal("→ did not select Allow")
	}
	if len(*sent) != 0 {
		t.Fatal("→ alone answered the request")
	}
	m, _ = dpress(t, m, keyEnter)
	if m.approvalCard.open || len(*sent) != 1 || !(*sent)[0].allow {
		t.Fatalf("right+enter: open=%v sent=%+v, want one allow", m.approvalCard.open, *sent)
	}

	m = openCard(t)
	m, _ = dpress(t, m, keyRight)
	m, _ = dpress(t, m, keyLeft)
	if m.approvalCard.allow {
		t.Fatal("← did not go back to Deny")
	}
	dpress(t, m, keyEnter)
	if len(*sent) != 2 || (*sent)[1].allow {
		t.Fatalf("right,left,enter: sent=%+v, want deny", *sent)
	}
}

// Every line fits at 36 and 40 columns, in both selection states; the hint
// is never split mid-part and the request id no longer wraps the header.
func TestApprovalSelRenderFits36And40(t *testing.T) {
	for _, w := range []int{36, 40} {
		for _, right := range []bool{false, true} {
			m := cardAt(t, w)
			if right {
				m, _ = dpress(t, m, keyRight)
			}
			view := m.View()
			assertFits(t, view, w)
			v := ansi.Strip(view)
			for _, want := range []string{"approve Edit?", "alpha:default", "Deny", "Allow",
				"select", "confirm", "m deny+msg", "esc later", "auto-deny at"} {
				if !strings.Contains(v, want) {
					t.Errorf("w=%d right=%v: card lacks %q:\n%s", w, right, want, v)
				}
			}
			if strings.Contains(v, "ap1") {
				t.Errorf("w=%d: request id still printed in the card:\n%s", w, v)
			}
		}
	}
}

// The hint is one line when wide enough, otherwise broken at " · " only.
func TestApprovalSelHintWrap(t *testing.T) {
	one := approvalHint(80)
	if len(one) != 1 || !strings.Contains(one[0], "select · ") || !strings.Contains(one[0], "esc later") {
		t.Fatalf("hint at 80 = %q, want one line", one)
	}
	for _, l := range approvalHint(32) {
		if ansi.StringWidth(l) > 32 || strings.HasPrefix(l, " ") || strings.HasSuffix(l, "·") {
			t.Fatalf("hint line %q badly broken at 32", l)
		}
	}
}

// Several requests in one session: the title carries "i/N".
func TestApprovalSelQueuePosition(t *testing.T) {
	m := narrow(t)
	ev := approvalReq("ap2")
	ev.Deadline += 60
	nm, _ := m.Update(streamEventMsg(ev))
	m = nm.(Model)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "1/2") {
		t.Fatalf("card lacks the 1/2 queue position:\n%s", v)
	}
	assertFits(t, m.View(), 40)
	if p, n := m.approvalQueuePos(m.approvals[approvalKey("alpha", "", "ap2")]); p != 2 || n != 2 {
		t.Fatalf("ap2 position = %d/%d, want 2/2", p, n)
	}
}
