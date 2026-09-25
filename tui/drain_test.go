package main

// /drain — discard a group's QUEUED prompts and nothing else. It is the
// complement of /interrupt (which aborts the running turn and lets the queue
// advance) and deliberately not /stop or /clear, both of which drop the
// backlog only as a side effect of powering the VM off or forgetting the
// conversation. These tests pin the scoping and the local ⏳ bookkeeping.

import (
	"errors"
	"strings"
	"testing"
)

// Bare /drain addresses the session on screen; `all` addresses the group.
// The wire session must differ accordingly — "" means the whole group, so the
// default session has to go out as its alias "-" or a per-session drain would
// silently become a group-wide one.
func TestDrainDispatchScopes(t *testing.T) {
	m := inputModel(t, "")

	// The RPC itself has no daemon to reach here; running the command still
	// yields the daemonRespMsg, whose session field is the scope that went on
	// the wire — which is the whole claim under test.
	scopeOf := func(t *testing.T, in string) string {
		t.Helper()
		cmd := m.dispatchInput(in)
		if cmd == nil {
			t.Fatalf("%s returned no daemon command", in)
		}
		msg, ok := cmd().(daemonRespMsg)
		if !ok {
			t.Fatalf("%s produced %T, want daemonRespMsg", in, cmd())
		}
		if msg.op != "drain" {
			t.Fatalf("%s dispatched op %q, want drain", in, msg.op)
		}
		return msg.session
	}

	if got := scopeOf(t, "/drain"); got != "-" {
		t.Fatalf("/drain sent session %q, want \"-\" — an empty session means the WHOLE GROUP on the wire, so the default session must use its alias", got)
	}
	if got := scopeOf(t, "/drain all"); got != "" {
		t.Fatalf("/drain all sent session %q, want \"\" (every session)", got)
	}
	m.setActiveSession("main", "alpha")
	if got := scopeOf(t, "/drain"); got != "alpha" {
		t.Fatalf("/drain in session alpha sent session %q, want alpha", got)
	}
}

// The response reports the daemon's count, says the running turn survived,
// and drops our optimistic ⏳ rows for the drained scope only.
func TestDrainResponseRendersAndTrimsPending(t *testing.T) {
	m := inputModel(t, "")
	m.pending["main"] = []pendingPrompt{
		{session: "", text: "one"},
		{session: "alpha", text: "two"},
	}

	// Session-scoped ("-" = the default session on the wire).
	m.handleDaemonResp(daemonRespMsg{op: "drain", group: "main", session: "-",
		resp: map[string]any{"ok": true, "dropped": float64(1)}})
	txt := lastLineText(t, m, "sys")
	if !strings.Contains(txt, "discarded 1 queued prompt") || strings.Contains(txt, "prompts") {
		t.Fatalf("line = %q, want a singular count", txt)
	}
	if !strings.Contains(txt, "running turn is untouched") {
		t.Fatalf("line = %q, want it to say the in-flight turn survived", txt)
	}
	if got := m.pending["main"]; len(got) != 1 || got[0].session != "alpha" {
		t.Fatalf("pending = %v, want only alpha's row left", got)
	}

	// Group-wide.
	m.handleDaemonResp(daemonRespMsg{op: "drain", group: "main", session: "",
		resp: map[string]any{"ok": true, "dropped": float64(2)}})
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "discarded 2 queued prompts") {
		t.Fatalf("line = %q, want a plural count", txt)
	}
	if got := m.pending["main"]; len(got) != 0 {
		t.Fatalf("pending = %v, want empty after a group-wide drain", got)
	}
}

// An empty backlog is a success, not an error — and must say so rather than
// rendering "discarded 0".
func TestDrainEmptyBacklogAndFailure(t *testing.T) {
	m := inputModel(t, "")

	m.handleDaemonResp(daemonRespMsg{op: "drain", group: "main", session: "-",
		resp: map[string]any{"ok": true, "dropped": float64(0)}})
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "no queued prompts") {
		t.Fatalf("line = %q, want the empty-backlog wording", txt)
	}

	m.handleDaemonResp(daemonRespMsg{op: "drain", group: "main", err: errors.New("permission denied")})
	if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "permission denied") {
		t.Fatalf("error line = %q", txt)
	}
}
