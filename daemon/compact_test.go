package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// compactTestEnv installs the provider/turn/clear/summary seams for one test
// and records every guest message and clear in delivery order.
type compactTestEnv struct {
	mu    sync.Mutex
	calls []string // "turn:<msg>" | "clear"
}

func (e *compactTestEnv) add(s string) {
	e.mu.Lock()
	e.calls = append(e.calls, s)
	e.mu.Unlock()
}

func (e *compactTestEnv) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func newCompactTestEnv(t *testing.T, g, provider string, turn func(msg string) error) *compactTestEnv {
	t.Helper()
	e := &compactTestEnv{}
	prevProv := compactProviderFn
	compactProviderFn = func(string) string { return provider }
	prevTurn := setTurnFn(func(_, _ string, msg string) error {
		e.add("turn:" + msg)
		if turn != nil {
			return turn(msg)
		}
		return nil
	})
	prevClear, prevSum, prevStatus := compactClearFn, compactSummaryFn, compactStatusFn
	compactStatusFn = func(_, _ string, reset bool) (string, error) {
		if reset {
			return "", nil
		}
		return "ok test harness compacted\n", nil
	}
	compactClearFn = func(string, string) error { e.add("clear"); return nil }
	compactSummaryFn = func(string, string) (string, error) { return "goal: x; next: y", nil }
	t.Cleanup(func() {
		compactProviderFn = prevProv
		setTurnFn(prevTurn)
		compactClearFn, compactSummaryFn, compactStatusFn = prevClear, prevSum, prevStatus
		queuesMu.Lock()
		delete(queues, sessKey(g, ""))
		delete(compactPending, sessKey(g, ""))
		queuesMu.Unlock()
	})
	return e
}

func waitDone(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
		return nil
	}
}

// The mapping is the contract: each harness gets its NATIVE mechanism, an
// unknown/compaction-less provider gets the fallback, and --fresh forces the
// fallback everywhere.
func TestCompactStrategyPerHarness(t *testing.T) {
	cases := map[string]compactMech{
		providerClaude:  mechClaudeSlash,
		"":              mechClaudeSlash,
		providerOpenAI:  mechCodexAppServer,
		providerMinimax: mechPiRPC,
		providerVenice:  mechFallback,
		"someday-new":   mechFallback,
	}
	for prov, want := range cases {
		if got := compactStrategy(prov, false); got != want {
			t.Errorf("compactStrategy(%q) = %s, want %s", prov, got, want)
		}
		if got := compactStrategy(prov, true); got != mechFallback {
			t.Errorf("compactStrategy(%q, fresh) = %s, want fallback", prov, got)
		}
	}
}

func TestBuildCompactMsg(t *testing.T) {
	if got := buildCompactMsg(mechClaudeSlash, ""); got != "/compact" {
		t.Errorf("claude bare = %q", got)
	}
	if got := buildCompactMsg(mechClaudeSlash, "  keep audit  "); got != "/compact keep audit" {
		t.Errorf("claude instr = %q", got)
	}
	for _, m := range []compactMech{mechCodexAppServer, mechPiRPC} {
		got := buildCompactMsg(m, "keep x")
		want := compactPrefix + `{"mode":"` + string(m) + `","instructions":"keep x"}`
		if got != want {
			t.Errorf("%s = %q, want %q", m, got, want)
		}
		if !isReservedCompactMsg(got) {
			t.Errorf("%s message is not recognised as reserved", m)
		}
	}
	long := strings.Repeat("é", compactInstrMax) // 2 bytes each
	got := buildCompactMsg(mechClaudeSlash, long)
	if n := len(got) - len("/compact "); n > compactInstrMax {
		t.Errorf("instructions not capped: %d bytes", n)
	}
	if !strings.HasSuffix(got, "é") {
		t.Errorf("cap split a UTF-8 sequence: %q", got[len(got)-4:])
	}
}

func TestParseCompactCommand(t *testing.T) {
	type want struct {
		instr        string
		fresh, match bool
	}
	cases := map[string]want{
		"/compact":                {"", false, true},
		"  /compact":              {"", false, true},
		"/compact focus on audit": {"focus on audit", false, true},
		"/compact --fresh":        {"", true, true},
		"/compact --fresh keep x": {"keep x", true, true},
		"/compact --freshness":    {"--freshness", false, true},
		"/compactor":              {"", false, false},
		"please /compact":         {"", false, false},
		"/compact\nmulti line ok": {"multi line ok", false, true},
	}
	for in, w := range cases {
		instr, fresh, ok := parseCompactCommand(in)
		if ok != w.match || instr != w.instr || fresh != w.fresh {
			t.Errorf("parse(%q) = (%q,%v,%v), want (%q,%v,%v)", in, instr, fresh, ok, w.instr, w.fresh, w.match)
		}
	}
}

// Nobody but the daemon may author a compaction: a forged one would make
// codex/pi abort and compact on an attacker's say-so.
func TestCompactReservedPrefixRefusedInUserSends(t *testing.T) {
	const g = "cmpforge"
	newCompactTestEnv(t, g, providerOpenAI, nil)
	if _, err := enqueueSend(g, "", compactPrefix+`{"mode":"codex-app-server"}`); err == nil {
		t.Fatal("a user send carrying the reserved prefix was admitted")
	}
	if _, err := enqueueSend(g, "", "\x00koto-compact"); err == nil {
		t.Fatal("a truncated reserved prefix was admitted")
	}
	if _, _, err := requestCompact(g, "", "\x00koto-compact\x00{}", false); err == nil {
		t.Fatal("reserved prefix accepted as instructions")
	}
}

// End-to-end dispatch through the real queue/worker for every harness: the
// guest receives exactly the native message (or the fallback sequence).
func TestCompactDispatchPerHarness(t *testing.T) {
	native := []struct {
		g, provider string
		want        string
	}{
		{"cmpclaude", providerClaude, "/compact keep tests"},
		{"cmpcodex", providerOpenAI, compactPrefix + `{"mode":"codex-app-server","instructions":"keep tests"}`},
		{"cmppi", providerMinimax, compactPrefix + `{"mode":"pi-rpc","instructions":"keep tests"}`},
	}
	for _, c := range native {
		t.Run(c.provider, func(t *testing.T) {
			e := newCompactTestEnv(t, c.g, c.provider, nil)
			_, _, done, err := enqueueCompact(c.g, "", "keep tests", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := waitDone(t, done); err != nil {
				t.Fatal(err)
			}
			got := e.snapshot()
			if len(got) != 1 || got[0] != "turn:"+c.want {
				t.Fatalf("guest saw %q, want [turn:%q]", got, c.want)
			}
		})
	}
	for _, c := range []struct{ g, provider string }{
		{"cmpvenice", providerVenice},
		{"cmpfreshpi", providerMinimax}, // --fresh on a native harness
	} {
		t.Run("fallback-"+c.provider, func(t *testing.T) {
			e := newCompactTestEnv(t, c.g, c.provider, nil)
			mech, _, done, err := enqueueCompact(c.g, "", "keep tests", c.provider != providerVenice)
			if err != nil {
				t.Fatal(err)
			}
			if mech != mechFallback {
				t.Fatalf("mech = %s, want fallback", mech)
			}
			if err := waitDone(t, done); err != nil {
				t.Fatal(err)
			}
			got := e.snapshot()
			if len(got) != 3 || !strings.HasPrefix(got[0], "turn:[koto] This session is about to be compacted") ||
				!strings.Contains(got[0], "keep tests") || got[1] != "clear" ||
				!strings.HasPrefix(got[2], "turn:[koto] Session compacted") {
				t.Fatalf("fallback sequence = %q", got)
			}
		})
	}
}

// Never clear a session without a summary to seed the new thread with.
func TestCompactFallbackEmptySummaryDoesNotClear(t *testing.T) {
	const g = "cmpempty"
	e := newCompactTestEnv(t, g, providerVenice, nil)
	compactSummaryFn = func(string, string) (string, error) { return "  \n", nil }
	_, _, done, err := enqueueCompact(g, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "NOT cleared") {
		t.Fatalf("err = %v, want a not-cleared failure", err)
	}
	for _, c := range e.snapshot() {
		if c == "clear" {
			t.Fatal("session cleared without a summary")
		}
	}
}

// A failing native compaction is reported, NOT silently turned into a
// destructive summarize-and-restart.
func TestCompactNativeFailureDoesNotFallBack(t *testing.T) {
	const g = "cmpnatfail"
	e := newCompactTestEnv(t, g, providerOpenAI, func(string) error { return errString("thread not found") })
	_, _, done, err := enqueueCompact(g, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err == nil {
		t.Fatal("native failure swallowed")
	}
	if got := e.snapshot(); len(got) != 1 {
		t.Fatalf("native failure fell through to more work: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Queue, never kill: a compaction requested while a turn runs waits for it,
// does not signal its cancel channel, and a duplicate request is refused.
func TestCompactQueuedBehindRunningTurn(t *testing.T) {
	const g = "cmpbusy"
	started := make(chan struct{})
	release := make(chan struct{})
	e := newCompactTestEnv(t, g, providerMinimax, func(msg string) error {
		if msg == "long running work" {
			close(started)
			<-release
		}
		return nil
	})
	turnDone, err := enqueueSend(g, "", "long running work")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	queuesMu.Lock()
	cancel := turnCancels[sessKey(g, "")]
	queuesMu.Unlock()

	mech, busy, done, err := enqueueCompact(g, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if mech != mechPiRPC || !busy {
		t.Fatalf("mech=%s busy=%v, want pi-rpc busy", mech, busy)
	}
	if _, _, err := requestCompact(g, "", "", false); err == nil || !strings.Contains(err.Error(), "already queued") {
		t.Fatalf("duplicate compact err = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := e.snapshot(); len(got) != 1 {
		t.Fatalf("compaction reached the guest while a turn was running: %q", got)
	}
	select {
	case <-cancel:
		t.Fatal("the running turn was cancelled")
	default:
	}
	close(release)
	if err := waitDone(t, turnDone); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	got := e.snapshot()
	if len(got) != 2 || got[0] != "turn:long running work" || !strings.HasPrefix(got[1], "turn:"+compactPrefix) {
		t.Fatalf("order = %q", got)
	}
	// The pending mark is gone once the job ran: a new request is accepted.
	_, _, done2, err := enqueueCompact(g, "", "", false)
	if err != nil {
		t.Fatalf("compact after a finished compact refused: %v", err)
	}
	waitDone(t, done2)
}

// A stop/drain discards a queued compaction like any other job, and the
// duplicate guard does not outlive it.
func TestCompactDroppedClearsPending(t *testing.T) {
	const g = "cmpdrop"
	started := make(chan struct{})
	release := make(chan struct{})
	newCompactTestEnv(t, g, providerClaude, func(msg string) error {
		if msg == "hold" {
			close(started)
			<-release
		}
		return nil
	})
	defer close(release)
	if _, err := enqueueSend(g, "", "hold"); err != nil {
		t.Fatal(err)
	}
	<-started
	_, _, done, err := enqueueCompact(g, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if n := dropQueuedScope(g, "", true); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	if err := waitDone(t, done); err == nil {
		t.Fatal("dropped compaction reported success")
	}
	queuesMu.Lock()
	pending := compactPending[sessKey(g, "")]
	queuesMu.Unlock()
	if pending {
		t.Fatal("compactPending survived the drop")
	}
}

func TestCompactRefusesGoalSessions(t *testing.T) {
	newCompactTestEnv(t, "cmpgoal", providerClaude, nil)
	if _, _, err := requestCompact("cmpgoal", goalWorkSessionFor("x"), "", false); err == nil {
		t.Fatal("compaction of a goal session accepted")
	}
	if _, _, err := requestCompact("bad name!", "", "", false); err == nil {
		t.Fatal("bad group name accepted")
	}
}

// The guest's status line decides the note: a native turn that "succeeded"
// host-side may still have failed in the harness, and claude may have answered
// /compact as a prompt.
func TestParseCompactStatus(t *testing.T) {
	for _, tc := range []struct {
		mech        compactMech
		st, detail  string
		errContains string
	}{
		{mechPiRPC, "ok pi compacted the session (150.0k → ~32.0k tokens)\n", "pi compacted the session (150.0k → ~32.0k tokens)", ""},
		{mechCodexAppServer, "err codex: thread/resume: thread not found\n", "", "thread not found"},
		{mechCodexAppServer, "err", "", "reported a failure"},
		{mechClaudeSlash, "", "", "no compact_boundary"},
		{mechPiRPC, "", "", "no outcome"},
		{mechPiRPC, "garbage", "", "no outcome"},
	} {
		d, err := parseCompactStatus(tc.mech, tc.st)
		if tc.errContains == "" {
			if err != nil || d != tc.detail {
				t.Errorf("%s %q: got %q, %v", tc.mech, tc.st, d, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.errContains) {
			t.Errorf("%s %q: err = %v, want %q", tc.mech, tc.st, err, tc.errContains)
		}
	}
}

// Native compaction resets the status BEFORE the turn (a stale ok must not
// vouch for this run) and reads it after; a guest-reported failure fails the job.
func TestRunCompactNativeUsesGuestStatus(t *testing.T) {
	for _, prov := range []string{providerClaude, providerOpenAI, providerMinimax} {
		var order []string
		newCompactTestEnv(t, "cstat", prov, func(string) error { order = append(order, "turn"); return nil })
		compactStatusFn = func(_, _ string, reset bool) (string, error) {
			if reset {
				order = append(order, "reset")
				return "", nil
			}
			order = append(order, "read")
			return "err harness said no\n", nil
		}
		err := runCompactJob("cstat", "", &compactReq{mech: compactStrategy(prov, false)})
		if err == nil || !strings.Contains(err.Error(), "harness said no") {
			t.Errorf("%s: err = %v", prov, err)
		}
		if strings.Join(order, ",") != "reset,turn,read" {
			t.Errorf("%s: order = %v", prov, order)
		}
	}
	// The fallback never consults the native status file.
	newCompactTestEnv(t, "cstat", "venice", nil)
	compactStatusFn = func(string, string, bool) (string, error) {
		t.Error("fallback read the native status file")
		return "", nil
	}
	if err := runCompactJob("cstat", "", &compactReq{mech: mechFallback}); err != nil {
		t.Fatalf("fallback: %v", err)
	}
}
