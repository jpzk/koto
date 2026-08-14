package main

// ctlDispatch tests for the `notify` verb — the full verb→queue→marker→parser
// round-trip against a temp ROOT. No VM, no daemon: the verb queues a marker
// which tryFlushNotify (normally the tailer's job) appends to the group's
// host-side log file. Each test uses its own group name: the notify queue and
// rate-limit buckets are process-global, keyed by group.

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func notifyLine(t *testing.T, sev, title, msg string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"cmd":      "notify",
		"severity": sev,
		"title":    base64.StdEncoding.EncodeToString([]byte(title)),
		"msg":      base64.StdEncoding.EncodeToString([]byte(msg)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func setupNotifyRoot(t *testing.T, g string) {
	t.Helper()
	ROOT = filepath.Join(t.TempDir(), "groups")
	if err := os.MkdirAll(filepath.Join(ROOT, g, ".cs"), 0o755); err != nil {
		t.Fatal(err)
	}
	markTailed(g)
	// Drop markers queued before this test: any warn/error log line an
	// earlier test emitted incidentally queues a forwarded alert against
	// main (logalert.go), and delivering it here would skew event counts.
	notifyQueueMu.Lock()
	delete(notifyQueue, g)
	notifyQueueMu.Unlock()
}

// markTailed makes ensureTail a no-op for g: the verb under test would
// otherwise spawn a REAL tailLog goroutine, which outlives the test, chases
// the process-global ROOT into later tests' tempdirs, and races their
// cleanup (logAppend O_CREATEs the file back mid-RemoveAll). The tests play
// the tailer's role themselves via deliverNotify.
// markTailed pretends every one of g's stream tailers is already running, so
// ensureTail/ensureSlotTail start no real goroutine: a live tailer would
// create the stream files and race the assertions about what the log contains.
func markTailed(g string) {
	subsLock.Lock()
	for _, p := range logPaths(g) {
		tails[p] = true
	}
	subsLock.Unlock()
}

// deliverNotify plays the tailer's role: deliver the group's queued markers at
// a clean parse point (no partial line, no open block).
func deliverNotify(g string) {
	tryFlushNotify(g, &logParser{}, true)
}

// readGroupLog parses the group's log file through the shared parser.
func readGroupLog(t *testing.T, g string) []Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ROOT, g, ".cs", "log"))
	if err != nil {
		t.Fatal(err)
	}
	return feedAll(t, strings.TrimRight(string(b), "\n"))
}

func TestCtlNotifyRoundTrip(t *testing.T) {
	g := "nround"
	setupNotifyRoot(t, g)
	resp := ctlDispatch(g, notifyLine(t, "high", "Deploy failed", "prod is down"))
	if br, ok := resp.(baseResp); !ok || !br.OK {
		t.Fatalf("want ok resp, got %+v", resp)
	}
	deliverNotify(g)
	evs := readGroupLog(t, g)
	if len(evs) != 1 {
		t.Fatalf("want 1 event in log, got %v", names(evs))
	}
	e := evs[0]
	if e.Event != "notification" || e.Severity != "high" ||
		e.Title != "Deploy failed" || e.Text != "prod is down" || e.Ts == 0 {
		t.Fatalf("bad round-tripped event: %+v", e)
	}
}

func TestCtlNotifySeverityClampAndTruncate(t *testing.T) {
	g := "nclamp"
	setupNotifyRoot(t, g)
	long := strings.Repeat("x", notifyTitleMax+50)
	resp := ctlDispatch(g, notifyLine(t, "urgent", long, ""))
	if br, ok := resp.(baseResp); !ok || !br.OK {
		t.Fatalf("want ok resp, got %+v", resp)
	}
	deliverNotify(g)
	evs := readGroupLog(t, g)
	if len(evs) != 1 || evs[0].Severity != "normal" {
		t.Fatalf("want clamped severity, got %+v", evs)
	}
	if len(evs[0].Title) != notifyTitleMax {
		t.Fatalf("want title truncated to %d bytes, got %d", notifyTitleMax, len(evs[0].Title))
	}
}

func TestCtlNotifyEmptyRejected(t *testing.T) {
	g := "nempty"
	setupNotifyRoot(t, g)
	resp := ctlDispatch(g, notifyLine(t, "high", "", ""))
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), "empty title and message") {
		t.Fatalf("want empty-rejection error, got %s", b)
	}
	deliverNotify(g)
	if _, err := os.Stat(filepath.Join(ROOT, g, ".cs", "log")); !os.IsNotExist(err) {
		t.Fatalf("empty notify must not touch the log (stat err=%v)", err)
	}
}

func TestCtlNotifyMultibyteTruncationIsRuneSafe(t *testing.T) {
	g := "nrune"
	setupNotifyRoot(t, g)
	long := strings.Repeat("ü", notifyTitleMax) // 2 bytes per rune
	resp := ctlDispatch(g, notifyLine(t, "normal", long, ""))
	if br, ok := resp.(baseResp); !ok || !br.OK {
		t.Fatalf("want ok resp, got %+v", resp)
	}
	deliverNotify(g)
	evs := readGroupLog(t, g)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %v", names(evs))
	}
	title := evs[0].Title
	if len(title) > notifyTitleMax || !strings.HasSuffix(title, "ü") {
		t.Fatalf("truncation split a rune: len=%d tail=%q", len(title), title[len(title)-4:])
	}
}

// TestCtlNotifyNewlinesFlattened: title/msg newlines are collapsed to spaces
// before the marker is written — banner rows are one terminal row.
func TestCtlNotifyNewlinesFlattened(t *testing.T) {
	g := "nflat"
	setupNotifyRoot(t, g)
	resp := ctlDispatch(g, notifyLine(t, "high", "line1\nline2", "a\tb"))
	if br, ok := resp.(baseResp); !ok || !br.OK {
		t.Fatalf("want ok resp, got %+v", resp)
	}
	deliverNotify(g)
	evs := readGroupLog(t, g)
	if len(evs) != 1 || evs[0].Title != "line1 line2" || evs[0].Text != "a b" {
		t.Fatalf("newlines not flattened: %+v", evs)
	}
}

// TestCtlNotifySessionAttribution: the payload's session lands in the marker
// and the parsed event; malformed session degrades to default.
func TestCtlNotifySessionAttribution(t *testing.T) {
	g := "nsess"
	setupNotifyRoot(t, g)
	req := map[string]string{
		"cmd":     "notify",
		"title":   base64.StdEncoding.EncodeToString([]byte("T")),
		"session": "ops",
	}
	b, _ := json.Marshal(req)
	if br, ok := ctlDispatch(g, b).(baseResp); !ok || !br.OK {
		t.Fatal("session notify rejected")
	}
	req["session"] = "../evil"
	b, _ = json.Marshal(req)
	if br, ok := ctlDispatch(g, b).(baseResp); !ok || !br.OK {
		t.Fatal("malformed-session notify must degrade, not error")
	}
	deliverNotify(g)
	evs := readGroupLog(t, g)
	if len(evs) != 2 || evs[0].Session != "ops" || evs[1].Session != "" {
		t.Fatalf("session attribution wrong: %+v", evs)
	}
}

// TestCtlNotifyDeferredMidLine: a queued marker is NOT appended while the
// log's last line is unterminated (the guest is mid-stream); it flushes once
// the line completes. This is the write-side half of the glue-corruption
// defense — the append would otherwise land mid-line and stop parsing as a
// marker.
func TestCtlNotifyDeferredMidLine(t *testing.T) {
	g := "nmid"
	setupNotifyRoot(t, g)
	p := filepath.Join(ROOT, g, ".cs", "log")
	if err := os.WriteFile(p, []byte("streaming partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if br, ok := ctlDispatch(g, notifyLine(t, "high", "T", "M")).(baseResp); !ok || !br.OK {
		t.Fatal("notify rejected")
	}
	deliverNotify(g)
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "[[notify]]") {
		t.Fatalf("marker appended onto an unterminated line: %q", b)
	}
	// Line completes → next flush delivers, on its own line.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("\n")
	f.Close()
	deliverNotify(g)
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "\n[[notify]] ") {
		t.Fatalf("marker not delivered after boundary: %q", b)
	}
}

// TestCtlNotifyDeferredInBlock: the flush refuses while the parser is inside
// a thinking/tool_out block — the marker would be swallowed as block body
// (the parser's nesting rule, which is the forgery defense and must stay).
func TestCtlNotifyDeferredInBlock(t *testing.T) {
	g := "nblock"
	setupNotifyRoot(t, g)
	if br, ok := ctlDispatch(g, notifyLine(t, "high", "T", "M")).(baseResp); !ok || !br.OK {
		t.Fatal("notify rejected")
	}
	tryFlushNotify(g, &logParser{inToolOut: true}, true)
	if _, err := os.Stat(filepath.Join(ROOT, g, ".cs", "log")); !os.IsNotExist(err) {
		t.Fatalf("marker flushed inside an open block (stat err=%v)", err)
	}
	tryFlushNotify(g, &logParser{}, false) // partial line buffered → also no
	if _, err := os.Stat(filepath.Join(ROOT, g, ".cs", "log")); !os.IsNotExist(err) {
		t.Fatal("marker flushed while not at a line boundary")
	}
	deliverNotify(g)
	evs := readGroupLog(t, g)
	if len(evs) != 1 || evs[0].Event != "notification" {
		t.Fatalf("marker not delivered once clear: %v", names(evs))
	}
}

// TestCtlNotifyRateLimited: the per-group bucket passes a burst of
// notifyRateBurst and rejects the next; other groups are unaffected.
func TestCtlNotifyRateLimited(t *testing.T) {
	g := "nrate"
	setupNotifyRoot(t, g)
	for i := 0; i < notifyRateBurst; i++ {
		if br, ok := ctlDispatch(g, notifyLine(t, "normal", "T", "")).(baseResp); !ok || !br.OK {
			t.Fatalf("burst send %d rejected: %+v", i, br)
		}
	}
	resp := ctlDispatch(g, notifyLine(t, "normal", "T", ""))
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), "rate limited") {
		t.Fatalf("want rate-limit rejection, got %s", b)
	}
	// A different group has its own bucket.
	other := "nrate2"
	if err := os.MkdirAll(filepath.Join(ROOT, other, ".cs"), 0o755); err != nil {
		t.Fatal(err)
	}
	markTailed(other)
	if br, ok := ctlDispatch(other, notifyLine(t, "normal", "T", "")).(baseResp); !ok || !br.OK {
		t.Fatal("sibling group caught in another group's rate limit")
	}
	// Drain queues so no cross-test leakage.
	deliverNotify(g)
	deliverNotify(other)
}

// TestCtlResourcesAuthorization: `resources` is a CROSS-GROUP read (every
// peer's disk/mem/cpu), so it follows `list` — main only. A non-main group
// getting it would be precisely the peer-visibility leak this plane exists
// to prevent.
func TestCtlResourcesAuthorization(t *testing.T) {
	req, _ := json.Marshal(map[string]string{"cmd": "resources"})

	got := ctlDispatch(ctlMainGroup, req)
	rr, ok := got.(resourcesResp)
	if !ok || !rr.OK {
		t.Fatalf("main must be allowed resources, got %T %+v", got, got)
	}

	br, ok := ctlDispatch("peer", req).(baseResp)
	if !ok || br.OK {
		t.Fatal("non-main group must be denied resources")
	}
	if !strings.Contains(br.Error, "not allowed for non-main") {
		t.Errorf("denial reason = %q, want the non-main refusal", br.Error)
	}
}

// The two planes must never disagree: an operator reading `ctl resources`
// over gRPC and main reading it over the ctl FIFO have to see the same
// fleet, or a supervising agent will argue with its own logs.
func TestCtlResourcesMatchesSnapshot(t *testing.T) {
	snap, host := resourcesSnapshot()
	resp := resourcesCtlResp()

	if len(resp.Groups) != len(snap) {
		t.Fatalf("ctl reported %d groups, snapshot has %d", len(resp.Groups), len(snap))
	}
	if resp.Host.ProvisionedBytes != host.ProvisionedBytes ||
		resp.Host.FSTotalBytes != host.FSTotalBytes {
		t.Errorf("host rollup diverged: ctl=%+v snapshot=%+v", resp.Host, host)
	}
	for i, g := range resp.Groups {
		if g.Group != snap[i].Group || g.AllocBytes != snap[i].AllocBytes {
			t.Errorf("group %d diverged: ctl=%+v snapshot=%+v", i, g, snap[i])
		}
		// alloc_pct is the derived field agents actually judge on.
		if g.DeclaredBytes > 0 {
			want := roundPct(float64(g.AllocBytes) / float64(g.DeclaredBytes) * 100)
			if g.AllocPct != want {
				t.Errorf("%s alloc_pct = %v, want %v", g.Group, g.AllocPct, want)
			}
		}
		// Same contract for the guest memory mirror and its derived pct.
		if g.GuestMemTotalBytes != snap[i].GuestMemTotal || g.GuestMemAvailBytes != snap[i].GuestMemAvail {
			t.Errorf("%s guest mem diverged: ctl=(%d,%d) snapshot=(%d,%d)", g.Group,
				g.GuestMemTotalBytes, g.GuestMemAvailBytes, snap[i].GuestMemTotal, snap[i].GuestMemAvail)
		}
		if snap[i].GuestMemTotal > 0 {
			want := roundPct(float64(snap[i].GuestMemTotal-snap[i].GuestMemAvail) / float64(snap[i].GuestMemTotal) * 100)
			if g.GuestMemUsedPct != want {
				t.Errorf("%s guest_mem_used_pct = %v, want %v", g.Group, g.GuestMemUsedPct, want)
			}
		}
	}
}
