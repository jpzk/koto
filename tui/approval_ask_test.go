package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var keyDownA = tea.KeyMsg{Type: tea.KeyDown}

func askReq(id string, ts float64, opts ...string) Event {
	return Event{Event: "approval_request", Group: "alpha", Session: "goal-x", ID: id,
		Name: "ask", Kind: "ask", Text: "which database should the migration target?", Options: opts, Ts: ts}
}

// The ask card shows question + every option + other, opens on stop and
// wait, and fits 36/40 cols. Plain enter = deny without a message.
func TestAskCardDefaultsToStopAndWait(t *testing.T) {
	for _, w := range []int{36, 40} {
		adv := fakeApprovalClock(t)
		sent := recordApprovals(t)
		m := goalCard(t, w)
		m = apFeed(t, m, askReq("q1", 1000, "postgres", "sqlite"))
		if !m.approvalCard.open || !m.approvalCard.inline || m.approvalCard.askIdx != 0 {
			t.Fatalf("w%d: ask card not open inline on stop and wait: %+v", w, m.approvalCard)
		}
		view := m.View()
		assertFits(t, view, w)
		v := ansi.Strip(view)
		for _, want := range []string{"decision needed", "which database", "› stop and wait", "postgres", "sqlite", "other (type an answer)", "no timeout"} {
			if !strings.Contains(v, want) {
				t.Fatalf("w%d: card lacks %q:\n%s", w, want, v)
			}
		}
		adv(time.Second)
		m, _ = dpress(t, m, keyEnter)
		if len(*sent) != 1 || (*sent)[0].allow || (*sent)[0].msg != "" {
			t.Fatalf("w%d: sent %+v, want deny no message", w, *sent)
		}
	}
}

// ↓ enter on an option sends allow with exactly that option's text; no
// one-key answer (y / a / digits do nothing).
func TestAskCardOptionSendsText(t *testing.T) {
	for _, w := range []int{36, 40} {
		adv := fakeApprovalClock(t)
		sent := recordApprovals(t)
		m := goalCard(t, w)
		m = apFeed(t, m, askReq("q1", 1000, "postgres", "sqlite"))
		adv(time.Second)
		for _, r := range "ya12" {
			m, _ = dpress(t, m, runeKey(r))
		}
		if len(*sent) != 0 || m.approvalCard.askIdx != 0 || m.approvalCard.msgMode {
			t.Fatalf("w%d: stray keys answered/moved: %+v %+v", w, *sent, m.approvalCard)
		}
		m, _ = dpress(t, m, keyDownA)
		m, _ = dpress(t, m, keyDownA)
		if v := ansi.Strip(m.View()); !strings.Contains(v, "› sqlite") {
			t.Fatalf("w%d: selection not on sqlite:\n%s", w, v)
		}
		m, _ = dpress(t, m, keyEnter)
		if len(*sent) != 1 || !(*sent)[0].allow || (*sent)[0].msg != "sqlite" {
			t.Fatalf("w%d: sent %+v, want allow sqlite", w, *sent)
		}
	}
}

// other opens a text box; the typed answer goes out as typed (trimmed);
// an empty answer is not sent; esc goes back to stop and wait.
func TestAskCardFreeText(t *testing.T) {
	for _, w := range []int{36, 40} {
		adv := fakeApprovalClock(t)
		sent := recordApprovals(t)
		m := goalCard(t, w)
		m = apFeed(t, m, askReq("q1", 1000, "postgres"))
		adv(time.Second)
		m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyShiftTab}) // wraps to other
		m, _ = dpress(t, m, keyEnter)
		if !m.approvalCard.msgMode || !m.approvalCard.allow {
			t.Fatalf("w%d: other did not open the answer box: %+v", w, m.approvalCard)
		}
		view := m.View()
		assertFits(t, view, w)
		if v := ansi.Strip(view); !strings.Contains(v, "your answer:") {
			t.Fatalf("w%d: answer box not rendered:\n%s", w, v)
		}
		m, _ = dpress(t, m, keyEnter)
		if len(*sent) != 0 || !m.approvalCard.msgMode {
			t.Fatalf("w%d: empty answer was sent: %+v", w, *sent)
		}
		m = apType(t, m, " mysql 8, read replica ")
		m, _ = dpress(t, m, keyEnter)
		if len(*sent) != 1 || !(*sent)[0].allow || (*sent)[0].msg != "mysql 8, read replica" {
			t.Fatalf("w%d: sent %+v, want the typed answer", w, *sent)
		}
	}
	// esc back
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	m = apFeed(t, m, askReq("q1", 1000, "postgres"))
	adv(time.Second)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	m, _ = dpress(t, m, keyEnter)
	m = apType(t, m, "draft")
	m, _ = dpress(t, m, keyEsc)
	if m.approvalCard.msgMode || m.approvalCard.allow || m.approvalCard.askIdx != 0 || !m.approvalCard.open {
		t.Fatalf("esc did not go back to stop and wait: %+v", m.approvalCard)
	}
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow {
		t.Fatalf("sent %+v, want deny", *sent)
	}
}

// m = stop and wait with a note for the goal.
func TestAskCardStopWithNote(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	m = apFeed(t, m, askReq("q1", 1000, "postgres"))
	adv(time.Second)
	m, _ = dpress(t, m, runeKey('m'))
	m = apType(t, m, "ask me after lunch")
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow || (*sent)[0].msg != "ask me after lunch" {
		t.Fatalf("sent %+v, want deny with note", *sent)
	}
}

// The ask shares the session's oldest-first queue with tool approvals:
// tool (older) first as 1/2, then the ask as 2/2 once the tool is answered.
func TestAskCardSharesQueue(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	m = apFeed(t, m, goalReq("t1", "Bash", 1000))
	m = apFeed(t, m, askReq("q1", 1001, "postgres"))
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "approve Bash?") || !strings.Contains(v, "1/2") {
		t.Fatalf("oldest (tool) not first with 1/2:\n%s", v)
	}
	adv(time.Second)
	m, _ = dpress(t, m, keyEnter) // deny the tool
	if len(*sent) != 1 || (*sent)[0].a.id != "t1" {
		t.Fatalf("sent %+v", *sent)
	}
	m = apFeed(t, m, Event{Event: "approval_resolved", Group: "alpha", Session: "goal-x", ID: "t1", Msg: "deny", Name: "operator", Ts: 1002})
	v = ansi.Strip(m.View())
	if !strings.Contains(v, "decision needed") || !strings.Contains(v, "which database") {
		t.Fatalf("ask not shown next:\n%s", v)
	}
}

// list_pending seeds kind/options so a TUI that attaches late still gets
// the ask card.
func TestAskSeededFromListPending(t *testing.T) {
	m := goalCard(t, 40)
	m.seedApprovals(map[string]any{"pending": []any{map[string]any{
		"group": "alpha", "session": "goal-x", "id": "q9", "tool": "ask", "kind": "ask",
		"summary": "ship it?", "options": []any{"yes", "no"}, "created": 1000.0}}})
	a := m.approvals[approvalKey("alpha", "goal-x", "q9")]
	if !a.isAsk() || len(a.options) != 2 || a.options[1] != "no" {
		t.Fatalf("seeded %+v", a)
	}
}
