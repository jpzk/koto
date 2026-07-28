package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestLog builds a group workspace under a temp ROOT and returns the
// group's log path. Restores ROOT on cleanup.
func writeTestLog(t *testing.T, g, content string) string {
	t.Helper()
	prev := ROOT
	ROOT = t.TempDir()
	t.Cleanup(func() { ROOT = prev })
	dir := filepath.Join(vol(g), ".cs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNormalizeSession: aliases fold to the canonical default; bad names are
// rejected.
func TestNormalizeSession(t *testing.T) {
	for _, alias := range []string{"", "-", "default"} {
		if s, err := normalizeSession(alias); err != nil || s != "" {
			t.Fatalf("normalizeSession(%q) = %q, %v", alias, s, err)
		}
	}
	if s, err := normalizeSession("research-2"); err != nil || s != "research-2" {
		t.Fatalf("normalizeSession(research-2) = %q, %v", s, err)
	}
	for _, bad := range []string{"has space", "../x", "-lead", "a\tb", "x/y"} {
		if _, err := normalizeSession(bad); err == nil {
			t.Fatalf("normalizeSession(%q) accepted", bad)
		}
	}
}

// TestReadHistorySessionAttribution: [[session]] markers switch the session
// stamped on parsed events; everything before the first marker (pre-session
// logs) is the default session.
func TestReadHistorySessionAttribution(t *testing.T) {
	log := "[ts:1000]\n>>> old default prompt\nold default reply\n[[turn_end]]\n" +
		"[[session]] side\n[ts:2000]\n>>> side prompt\nside reply\n[[turn_end]]\n" +
		"[[session]] -\n[ts:3000]\n>>> new default prompt\nnew default reply\n[[turn_end]]\n"
	writeTestLog(t, "sess-hist", log)
	evs, _ := readHistory("sess-hist", 0, 0)
	want := map[string]string{
		"old default prompt": "", "old default reply": "",
		"side prompt": "side", "side reply": "side",
		"new default prompt": "", "new default reply": "",
	}
	seen := 0
	for _, ev := range evs {
		key := ev.Msg
		if key == "" {
			key = ev.Text
		}
		if s, ok := want[key]; ok {
			seen++
			if ev.Session != s {
				t.Errorf("event %q: session = %q, want %q", key, ev.Session, s)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d/%d expected events; got %+v", seen, len(want), evs)
	}
}

// TestFilterLogSession: dropping one session's segments keeps every other
// line (including the pre-marker default prefix) byte-identical.
func TestFilterLogSession(t *testing.T) {
	log := "[ts:1000]\n>>> default prompt\ndefault reply\n[[turn_end]]\n" +
		"[[session]] side\n[ts:2000]\n>>> side prompt\nside reply\n[[turn_end]]\n" +
		"[[session]] keep\n[ts:2500]\n>>> keep prompt\nkeep reply\n[[turn_end]]\n" +
		"[[session]] -\n[ts:3000]\n>>> default again\n[[turn_end]]\n"
	p := writeTestLog(t, "sess-filter", log)

	if err := filterLogSession(p, "side"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "[ts:1000]\n>>> default prompt\ndefault reply\n[[turn_end]]\n" +
		"[[session]] keep\n[ts:2500]\n>>> keep prompt\nkeep reply\n[[turn_end]]\n" +
		"[[session]] -\n[ts:3000]\n>>> default again\n[[turn_end]]\n"
	if string(got) != want {
		t.Fatalf("after drop side:\n%q\nwant:\n%q", got, want)
	}

	// Dropping the default session removes the pre-marker prefix and the
	// `[[session]] -` segment but keeps the named session.
	if err := filterLogSession(p, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(p)
	want = "[[session]] keep\n[ts:2500]\n>>> keep prompt\nkeep reply\n[[turn_end]]\n"
	if string(got) != want {
		t.Fatalf("after drop default:\n%q\nwant:\n%q", got, want)
	}
}

// TestSessionRegistry: register/list/remove round-trip, default never listed.
func TestSessionRegistry(t *testing.T) {
	prev := ROOT
	ROOT = t.TempDir()
	t.Cleanup(func() { ROOT = prev })
	g := "sess-reg"
	registerSession(g, "")
	registerSession(g, "beta")
	registerSession(g, "alpha")
	registerSession(g, "beta") // dup ignored
	got := listSessions(g)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("listSessions = %v", got)
	}
	removeSession(g, "alpha")
	got = listSessions(g)
	if len(got) != 1 || got[0] != "beta" {
		t.Fatalf("after remove: %v", got)
	}
}
