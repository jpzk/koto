package main

import (
	"strings"
	"testing"
)

// A confirm -> yolo change cuts a turn holding an approval. The turn
// goroutine's own sweep (send.go, on its closed cancel channel) can deny the
// approval before applyModeChange's sweep does. modeCutHook pins that
// interleaving: the turn's sweep runs inside the window, after the cut was
// recorded and before applyModeChange sweeps. The deny must still read
// mode-change.
func TestModeDenyLabelTurnSweepFirst(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	f := newFakeHarness(t, "tg")
	mustMode(t, "tg", "dl", "confirm")
	var rig *approvalRig
	f.onRun = func(r fakeRun) {
		if r.mode == modeConfirm {
			rig = newApprovalRig(t, 4, "dl")
			rig.request("dl-1", "Bash")
		}
	}
	hooked, sawMode := 0, false
	modeCutHook = func(g, session string) {
		if g != "tg" || session != "dl" {
			return
		}
		hooked++
		if n := approvalsPending(g, session); n != 1 {
			t.Errorf("pending in the cut window = %d, want 1", n)
		}
		sawMode = denyCutTurnApprovals(g, session)
	}
	t.Cleanup(func() { modeCutHook = nil })
	done, err := enqueueSend("tg", "dl", "ship")
	if err != nil {
		t.Fatal(err)
	}
	f.waitRun(t)
	waitPending(t, "tg", "dl", 1)
	if r := mustMode(t, "tg", "dl", "yolo"); r.Config["mode_apply"] != "turn restarted" {
		t.Fatalf("mode_apply = %v", r.Config["mode_apply"])
	}
	if hooked != 1 || !sawMode {
		t.Fatalf("cut window: hooked=%d modeCancelPending=%v", hooked, sawMode)
	}
	if rp := rig.reply(); rp.GetId() != "dl-1" || rp.GetAllow() {
		t.Fatalf("reply: %v", rp)
	}
	if n := approvalsPending("tg", "dl"); n != 0 {
		t.Fatalf("%d pending", n)
	}
	rig.end()
	tr := rig.transcript()
	if !strings.Contains(tr, "[[approval dl-1 deny by mode-change]]") || strings.Contains(tr, "deny by interrupt") {
		t.Fatalf("deny label:\n%s", tr)
	}
	if re := f.waitRun(t); re.mode != modeYolo {
		t.Fatalf("re-run mode %q", re.mode)
	}
	if err := waitJob(t, done); err != nil {
		t.Fatal(err)
	}
}

// A request that reaches the host after a mode change cut its turn is denied
// by mode-change, not interrupt.
func TestModeDenyLabelLateRequestAfterModeCut(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	k := sessKey("tg", "dm")
	ch := make(chan struct{})
	queuesMu.Lock()
	turnCancels[k] = ch
	queuesMu.Unlock()
	t.Cleanup(func() {
		queuesMu.Lock()
		delete(turnCancels, k)
		delete(modeRestarts, k)
		queuesMu.Unlock()
	})
	rig := newApprovalRig(t, 5, "dm")
	mr := &modeRestart{from: modeConfirm, to: modeYolo, ran: make(chan struct{})}
	if cut, already := cutTurnForMode("tg", "dm", ch, mr); !cut || already {
		t.Fatalf("cutTurnForMode = %v, %v", cut, already)
	}
	rig.request("dm-1", "Bash")
	if rp := rig.reply(); rp.GetId() != "dm-1" || rp.GetAllow() {
		t.Fatalf("late request: %v", rp)
	}
	if n := approvalsPending("tg", "dm"); n != 0 {
		t.Fatalf("%d pending", n)
	}
	rig.end()
	tr := rig.transcript()
	if !strings.Contains(tr, "[[approval dm-1 deny by mode-change]]") || strings.Contains(tr, "deny by interrupt") {
		t.Fatalf("deny label:\n%s", tr)
	}
}

// An operator interrupt (no mode change recorded) still denies by interrupt —
// both the turn's sweep and a late request after it.
func TestModeDenyLabelOperatorInterrupt(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	k := sessKey("tg", "di")
	ch := make(chan struct{})
	queuesMu.Lock()
	turnCancels[k] = ch
	queuesMu.Unlock()
	t.Cleanup(func() {
		queuesMu.Lock()
		delete(turnCancels, k)
		queuesMu.Unlock()
	})
	rig := newApprovalRig(t, 6, "di")
	rig.request("di-1", "Bash")
	waitPending(t, "tg", "di", 1)
	if !cancelTurn("tg", "di", ch) {
		t.Fatal("cancelTurn did not cut the turn")
	}
	if denyCutTurnApprovals("tg", "di") {
		t.Fatal("operator interrupt reported as a mode change")
	}
	if rp := rig.reply(); rp.GetId() != "di-1" || rp.GetAllow() {
		t.Fatalf("reply: %v", rp)
	}
	rig.request("di-2", "Bash")
	if rp := rig.reply(); rp.GetId() != "di-2" || rp.GetAllow() {
		t.Fatalf("late request: %v", rp)
	}
	if n := approvalsPending("tg", "di"); n != 0 {
		t.Fatalf("%d pending", n)
	}
	rig.end()
	tr := rig.transcript()
	if !strings.Contains(tr, "[[approval di-1 deny by interrupt]]") || !strings.Contains(tr, "[[approval di-2 deny by interrupt]]") || strings.Contains(tr, "mode-change") {
		t.Fatalf("deny label:\n%s", tr)
	}
}
