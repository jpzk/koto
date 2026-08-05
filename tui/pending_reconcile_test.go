package main

// pending_reconcile_test.go — the optimistic ⏳ backlog stays in sync with the
// daemon's authoritative Queued count. popPending only fires on an exact
// (session, text) match at the head of a `prompt` event, so a send that is
// enqueued and then never started (daemon restart, /stop, /restart, /clear)
// used to strand its row for the life of the process. See reconcilePending
// (model.go).

import (
	"testing"
	"time"
)

// pendingModel is a model holding n stranded ⏳ rows for group g, with the
// send-ack grace window already expired so reconcilePending will act.
func pendingModel(g string, n int) Model {
	m := newModel("", 0)
	for i := 0; i < n; i++ {
		m.pending[g] = append(m.pending[g], pendingPrompt{text: string(rune('a' + i))})
	}
	m.sendAckAt[g] = time.Now().Add(-2 * pendingGrace)
	return m
}

func TestReconcileDropsPhantomsWhenDaemonQueueEmpty(t *testing.T) {
	m := pendingModel("main", 2)
	if !m.reconcilePending(map[string]GroupInfo{"main": {Running: true, Queued: 0}}) {
		t.Fatal("reconcile reported no change but two phantom rows were held")
	}
	if p := m.pending["main"]; len(p) != 0 {
		t.Fatalf("pending still holds %d row(s) after the daemon reported an empty queue", len(p))
	}
}

func TestReconcileTrimsToQueuedFromTheHead(t *testing.T) {
	// Three rows held, daemon says one is really enqueued: the two oldest are
	// the strays. popPending consumes from the front, so the survivor must be
	// the newest — trimming the tail instead would leave a head that never
	// matches and blocks every later pop.
	m := pendingModel("main", 3)
	m.reconcilePending(map[string]GroupInfo{"main": {Running: true, Queued: 1}})
	p := m.pending["main"]
	if len(p) != 1 {
		t.Fatalf("pending has %d row(s), want 1 to match Queued", len(p))
	}
	if p[0].text != "c" {
		t.Fatalf("survivor is %q, want the newest row %q", p[0].text, "c")
	}
}

func TestReconcileKeepsRowsTheDaemonAccountsFor(t *testing.T) {
	m := pendingModel("main", 2)
	if m.reconcilePending(map[string]GroupInfo{"main": {Running: true, Queued: 2}}) {
		t.Fatal("reconcile trimmed rows the daemon confirmed are queued")
	}
	if len(m.pending["main"]) != 2 {
		t.Fatalf("pending has %d row(s), want both kept", len(m.pending["main"]))
	}
	// Queued counts every source (ctl, scheduler, peer TUIs), so it can exceed
	// what this client sent. That is not evidence of a phantom.
	if m.reconcilePending(map[string]GroupInfo{"main": {Running: true, Queued: 5}}) {
		t.Fatal("reconcile acted on a queue deeper than our own backlog")
	}
}

func TestReconcileSpareInFlightSend(t *testing.T) {
	// The race the grace window exists for: the row is added optimistically,
	// the Send RPC has not been acknowledged, and a state frame computed
	// before it lands reports an empty queue. Dropping here would erase a
	// legitimate row.
	m := newModel("", 0)
	m.pending["main"] = []pendingPrompt{{text: "hello"}}
	m.sendInFlight["main"] = 1
	if m.reconcilePending(map[string]GroupInfo{"main": {Running: true}}) {
		t.Fatal("reconcile dropped a row whose Send is still in flight")
	}

	// Acked, but inside the grace window: a frame in transit can still be
	// older than the send.
	delete(m.sendInFlight, "main")
	m.sendAckAt["main"] = time.Now()
	if m.reconcilePending(map[string]GroupInfo{"main": {Running: true}}) {
		t.Fatal("reconcile dropped a row inside the post-ack grace window")
	}

	// Grace expired and the daemon still reports nothing queued → phantom.
	m.sendAckAt["main"] = time.Now().Add(-2 * pendingGrace)
	if !m.reconcilePending(map[string]GroupInfo{"main": {Running: true}}) {
		t.Fatal("reconcile never acted once the grace window expired")
	}
}

// The reconcile is only useful if a WatchState frame drives it — the listMsg
// handler is the wiring, and a phantom row is invisible to every other path.
func TestStateFrameDrivesReconcile(t *testing.T) {
	m := pendingModel("main", 1)
	m.cur = "main"
	nm, _ := m.Update(listMsg{groups: map[string]GroupInfo{"main": {Running: true, Queued: 0}}})
	m = nm.(Model)
	if p := m.pending["main"]; len(p) != 0 {
		t.Fatalf("a state frame reporting an empty queue left %d phantom row(s)", len(p))
	}
}

func TestReconcileDropsRowsForVanishedGroup(t *testing.T) {
	m := pendingModel("ghost", 1)
	if !m.reconcilePending(map[string]GroupInfo{"main": {Running: true}}) {
		t.Fatal("reconcile kept rows for a group the daemon no longer lists")
	}
	if _, ok := m.pending["ghost"]; ok {
		t.Fatal("pending still keyed by a destroyed group")
	}
}

// The send ack path must decrement the in-flight counter it gated on, or the
// gate latches and reconciliation never runs again for that group.
func TestSendAckClearsInFlightGate(t *testing.T) {
	m := newModel("", 0)
	m.cur = "main"
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.session = map[string]string{}
	m.dispatchInput("hello")
	if m.sendInFlight["main"] != 1 {
		t.Fatalf("dispatch left sendInFlight at %d, want 1", m.sendInFlight["main"])
	}
	if len(m.pending["main"]) != 1 {
		t.Fatalf("dispatch left %d pending row(s), want 1", len(m.pending["main"]))
	}
	m.handleDaemonResp(daemonRespMsg{op: "send", group: "main"})
	if n, ok := m.sendInFlight["main"]; ok {
		t.Fatalf("send ack left sendInFlight at %d, want the key gone", n)
	}
	if m.sendAckAt["main"].IsZero() {
		t.Fatal("send ack did not stamp sendAckAt; the grace window would never start")
	}
}
