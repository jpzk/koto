package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// fakeApprovalClock pins approvalNow; the returned func moves it forward.
func fakeApprovalClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	orig := approvalNow
	approvalNow = func() time.Time { return now }
	t.Cleanup(func() { approvalNow = orig })
	return func(d time.Duration) { now = now.Add(d) }
}

// typing is a 40x30 model on alpha's default session, prompt focused, with
// a half-typed draft and no request yet.
func typing(t *testing.T, draft string) Model {
	t.Helper()
	m := treeModel(t, approvalGroups(), "alpha", "", "")
	sm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = sm.(Model)
	m.exitTree()
	m.input.SetValue(draft)
	return m
}

func apFeed(t *testing.T, m Model, ev Event) Model {
	t.Helper()
	nm, _ := m.Update(streamEventMsg(ev))
	return nm.(Model)
}

func resolved(id string) Event {
	return Event{Event: "approval_resolved", Group: "alpha", Session: "", ID: id, Msg: "denied", Name: "tui"}
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// A request of the VIEWED session opens the selection inline, in place of
// the prompt box (not a centered modal), and takes the keyboard; the draft
// stays in the (hidden) prompt.
func TestApprovalInlineAutoFocus(t *testing.T) {
	fakeApprovalClock(t)
	m := typing(t, "half typed")
	m.focus = focusTree
	m = apFeed(t, m, approvalReq("ap1"))
	if !m.approvalCard.open || !m.approvalCard.inline {
		t.Fatalf("card open=%v inline=%v, want an inline card", m.approvalCard.open, m.approvalCard.inline)
	}
	if m.focus != focusInput {
		t.Fatalf("focus = %v, want the prompt zone (the card)", m.focus)
	}
	view := m.View()
	assertFits(t, view, 40)
	v := ansi.Strip(view)
	for _, want := range []string{"approve Edit?", "‹ Deny ›", "inbox 1"} {
		if !strings.Contains(v, want) {
			t.Errorf("inline view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "half typed") {
		t.Errorf("draft still drawn while the card holds the prompt area:\n%s", v)
	}
	lines := strings.Split(v, "\n")
	if !strings.Contains(lines[0], "inbox") {
		t.Errorf("first line %q is not the status bar — card drawn as a modal?", lines[0])
	}
	if len(lines) != 30 {
		t.Errorf("frame is %d lines, want 30 (the chat shrinks for the card)", len(lines))
	}
	if m.input.Value() != "half typed" {
		t.Fatalf("draft = %q, want it kept", m.input.Value())
	}
}

// Keys typed while the card is up never reach the draft or allow anything;
// once the request resolves the prompt comes back with the draft intact.
func TestApprovalInlineDraftPreservedAndRestored(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := typing(t, "half typed")
	m = apFeed(t, m, approvalReq("ap1"))
	adv(time.Second)
	for _, k := range []tea.KeyMsg{runes("y"), runes("Y"), runes("x"), {Type: tea.KeyBackspace}} {
		m, _ = dpress(t, m, k)
	}
	if len(*sent) != 0 || m.input.Value() != "half typed" {
		t.Fatalf("stray keys: sent=%+v draft=%q", *sent, m.input.Value())
	}
	m, _ = dpress(t, m, keyEnter) // enter = deny
	if len(*sent) != 1 || (*sent)[0].allow {
		t.Fatalf("enter: sent=%+v, want one deny", *sent)
	}
	if !m.approvalInputLocked() {
		t.Fatal("prompt unlocked before the daemon resolved the request")
	}
	m = apFeed(t, m, resolved("ap1"))
	if m.approvalCard.open || m.approvalInputLocked() {
		t.Fatalf("after resolve: card=%v locked=%v", m.approvalCard.open, m.approvalInputLocked())
	}
	if m.focus != focusInput || m.input.Value() != "half typed" {
		t.Fatalf("after resolve: focus=%v draft=%q", m.focus, m.input.Value())
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "half typed") {
		t.Fatalf("draft not drawn after resolve:\n%s", v)
	}
	m, _ = dpress(t, m, runes("!"))
	if m.input.Value() != "half typed!" {
		t.Fatalf("prompt not re-enabled: draft=%q", m.input.Value())
	}
}

// esc hides the card and moves to the tree; the request stays pending and
// the prompt stays disabled ("waiting for approval"); typing in the tree
// does not reach the draft, enter does not submit it but brings the card
// back.
func TestApprovalInlineEscKeepsInputDisabled(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := typing(t, "draft")
	m = apFeed(t, m, approvalReq("ap1"))
	m, _ = dpress(t, m, keyEsc)
	if m.approvalCard.open || m.focus != focusTree || len(*sent) != 0 {
		t.Fatalf("esc: card=%v focus=%v sent=%+v", m.approvalCard.open, m.focus, *sent)
	}
	if n := m.pendingTotal(); n != 1 {
		t.Fatalf("esc dropped the request: pending=%d", n)
	}
	view := m.View()
	assertFits(t, view, 40)
	if v := ansi.Strip(view); !strings.Contains(v, "waiting for approval") || strings.Contains(v, "approve Edit?") {
		t.Fatalf("hidden card / disabled prompt not shown:\n%s", v)
	}
	m, _ = dpress(t, m, runes("zz"))
	if m.input.Value() != "draft" {
		t.Fatalf("typing in the tree reached the disabled prompt: %q", m.input.Value())
	}
	// A tick must not pop the hidden card back up by itself.
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = nm.(Model)
	if m.approvalCard.open {
		t.Fatal("hidden card re-opened by itself")
	}
	m, _ = dpress(t, m, keyEnter)
	if m.input.Value() != "draft" || len(*sent) != 0 {
		t.Fatalf("enter in the tree submitted: draft=%q sent=%+v", m.input.Value(), *sent)
	}
	if !m.approvalCard.open || !m.approvalCard.inline || m.focus != focusInput {
		t.Fatalf("enter did not bring the card back: card=%v focus=%v", m.approvalCard.open, m.focus)
	}
	adv(time.Second)
	m, _ = dpress(t, m, keyRight)
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || !(*sent)[0].allow {
		t.Fatalf("right+enter on the re-opened card: sent=%+v", *sent)
	}
}

// An enter already in flight when the card pops up is dropped (~300ms);
// after that, enter answers.
func TestApprovalInlineEnterGuard(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := typing(t, "go on")
	m = apFeed(t, m, approvalReq("ap1"))
	m, _ = dpress(t, m, keyRight)
	m, _ = dpress(t, m, keyEnter)
	adv(200 * time.Millisecond)
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 0 || !m.approvalCard.open {
		t.Fatalf("enter inside the guard answered: sent=%+v open=%v", *sent, m.approvalCard.open)
	}
	adv(150 * time.Millisecond)
	m, _ = dpress(t, m, keyLeft)
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow {
		t.Fatalf("enter after the guard: sent=%+v, want one deny", *sent)
	}
}

// Several requests in the viewed session: one at a time, "i/N", the next
// one offered as soon as the previous is answered.
func TestApprovalInlineQueue(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := typing(t, "")
	m = apFeed(t, m, approvalReq("ap1"))
	ev := approvalReq("ap2")
	ev.Deadline += 30
	m = apFeed(t, m, ev)
	ev = approvalReq("ap3")
	ev.Deadline += 60
	m = apFeed(t, m, ev)
	view := m.View()
	assertFits(t, view, 40)
	if v := ansi.Strip(view); !strings.Contains(v, "1/3") {
		t.Fatalf("queue position 1/3 missing:\n%s", v)
	}
	adv(time.Second)
	m, _ = dpress(t, m, keyEnter)
	if !m.approvalCard.open || m.approvalCard.key != approvalKey("alpha", "", "ap2") {
		t.Fatalf("after answering ap1 the card shows %q, want ap2", m.approvalCard.key)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "2/3") {
		t.Fatalf("queue position 2/3 missing:\n%s", v)
	}
	// The next card is new: its own guard applies.
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 {
		t.Fatalf("enter right after the next card appeared answered it: %+v", *sent)
	}
	adv(time.Second)
	m, _ = dpress(t, m, keyRight)
	m, _ = dpress(t, m, keyEnter)
	m = apFeed(t, m, resolved("ap1"))
	m = apFeed(t, m, resolved("ap2"))
	if m.approvalCard.key != approvalKey("alpha", "", "ap3") {
		t.Fatalf("card on %q, want ap3", m.approvalCard.key)
	}
	if len(*sent) != 2 || (*sent)[0].allow || !(*sent)[1].allow {
		t.Fatalf("sent=%+v, want deny ap1, allow ap2", *sent)
	}
}

// The inline card fits 36 and 40 columns with the chat still above it.
func TestApprovalInlineFits36And40(t *testing.T) {
	fakeApprovalClock(t)
	for _, w := range []int{36, 40} {
		m := treeModel(t, approvalGroups(), "alpha", "", "")
		sm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 24})
		m = apFeed(t, sm.(Model), approvalReq("ap1"))
		if !m.approvalCard.inline {
			t.Fatalf("w=%d: no inline card", w)
		}
		view := m.View()
		assertFits(t, view, w)
		if n := len(strings.Split(view, "\n")); n != 24 {
			t.Errorf("w=%d: frame %d lines, want 24", w, n)
		}
		v := ansi.Strip(view)
		for _, want := range []string{"approve Edit?", "Deny", "Allow", "esc later"} {
			if !strings.Contains(v, want) {
				t.Errorf("w=%d: inline card lacks %q:\n%s", w, want, v)
			}
		}
	}
}

// alt+a on another session's request is the centered modal, not inline,
// and leaves the viewed session's prompt alone once closed.
func TestApprovalOtherSessionAltAIsModal(t *testing.T) {
	m := treeModel(t, approvalGroups(), "alpha", "triage", "")
	sm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = apFeed(t, sm.(Model), approvalReq("ap1"))
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if !m.approvalCard.open || m.approvalCard.inline {
		t.Fatalf("alt+a: open=%v inline=%v, want the modal", m.approvalCard.open, m.approvalCard.inline)
	}
	assertFits(t, m.View(), 40)
	m, _ = dpress(t, m, keyEsc)
	if m.approvalCard.open || m.approvalInputLocked() {
		t.Fatalf("esc: open=%v locked=%v", m.approvalCard.open, m.approvalInputLocked())
	}
}
