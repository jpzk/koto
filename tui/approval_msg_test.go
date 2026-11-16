package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func apType(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = dpress(t, m, runeKey(r))
	}
	return m
}

// a on the Deny preselection does nothing: allow+message still needs the
// selection first (no one-key allow, no stray-key allow box).
func TestApprovalAllowMsgNeedsSelection(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	m = apFeed(t, m, goalReq("g1", "Bash", 1000))
	adv(time.Second)
	m, _ = dpress(t, m, runeKey('a'))
	if m.approvalCard.msgMode || m.approvalCard.allow || !m.approvalCard.open || len(*sent) != 0 {
		t.Fatalf("a on Deny changed the card: %+v sent=%v", m.approvalCard, *sent)
	}
}

// → a <text> enter = allow with that message; the box fits 36/40 cols.
func TestApprovalAllowWithMessage(t *testing.T) {
	for _, w := range []int{36, 40} {
		adv := fakeApprovalClock(t)
		sent := recordApprovals(t)
		m := goalCard(t, w)
		m = apFeed(t, m, goalReq("g1", "Bash", 1000))
		adv(time.Second)
		m, _ = dpress(t, m, keyRight)
		if v := ansi.Strip(m.View()); !strings.Contains(v, "a allow+msg") {
			t.Fatalf("w%d: Allow selected but no a allow+msg hint:\n%s", w, v)
		}
		m, _ = dpress(t, m, runeKey('a'))
		if !m.approvalCard.msgMode || !m.approvalCard.allow {
			t.Fatalf("w%d: a on Allow did not open the allow box: %+v", w, m.approvalCard)
		}
		view := m.View()
		assertFits(t, view, w)
		if v := ansi.Strip(view); !strings.Contains(v, "allow with message:") || !strings.Contains(v, "enter allow") {
			t.Fatalf("w%d: allow box not rendered:\n%s", w, v)
		}
		m = apType(t, m, "use staging")
		m, _ = dpress(t, m, keyEnter)
		if len(*sent) != 1 || !(*sent)[0].allow || (*sent)[0].msg != "use staging" {
			t.Fatalf("w%d: sent %+v, want allow with message", w, *sent)
		}
	}
}

// Deny+message (m) still works and carries the text.
func TestApprovalDenyWithMessage(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	m = apFeed(t, m, goalReq("g1", "Bash", 1000))
	adv(time.Second)
	m, _ = dpress(t, m, runeKey('m'))
	if v := ansi.Strip(m.View()); !strings.Contains(v, "deny with message:") {
		t.Fatalf("deny box not rendered:\n%s", v)
	}
	m = apType(t, m, "not that dir")
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow || (*sent)[0].msg != "not that dir" {
		t.Fatalf("sent %+v, want deny with message", *sent)
	}
}

// esc in the allow box goes back to the card on Deny; plain enter then
// denies without a message.
func TestApprovalAllowMsgEscBackToDeny(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	m = apFeed(t, m, goalReq("g1", "Bash", 1000))
	adv(time.Second)
	m, _ = dpress(t, m, keyRight)
	m, _ = dpress(t, m, runeKey('a'))
	m = apType(t, m, "draft")
	m, _ = dpress(t, m, keyEsc)
	if !m.approvalCard.open || m.approvalCard.msgMode || m.approvalCard.allow {
		t.Fatalf("esc did not return to the card on Deny: %+v", m.approvalCard)
	}
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || (*sent)[0].allow || (*sent)[0].msg != "" {
		t.Fatalf("sent %+v, want plain deny", *sent)
	}
}

// → enter allows WITHOUT a message and without any box (messages optional).
func TestApprovalPlainAllowNoMessage(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 36)
	m = apFeed(t, m, goalReq("g1", "Bash", 1000))
	adv(time.Second)
	m, _ = dpress(t, m, keyRight)
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || !(*sent)[0].allow || (*sent)[0].msg != "" {
		t.Fatalf("sent %+v, want plain allow", *sent)
	}
}

// An empty allow box allows without a message (empty = no message).
func TestApprovalAllowMsgEmptySendsNone(t *testing.T) {
	adv := fakeApprovalClock(t)
	sent := recordApprovals(t)
	m := goalCard(t, 40)
	m = apFeed(t, m, goalReq("g1", "Bash", 1000))
	adv(time.Second)
	m, _ = dpress(t, m, keyRight)
	m, _ = dpress(t, m, runeKey('a'))
	m = apType(t, m, "   ")
	m, _ = dpress(t, m, keyEnter)
	if len(*sent) != 1 || !(*sent)[0].allow || (*sent)[0].msg != "" {
		t.Fatalf("sent %+v, want allow without message", *sent)
	}
}
