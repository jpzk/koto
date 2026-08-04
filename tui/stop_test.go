package main

// /stop — the TUI's VM power-off command (daemon `stop` verb). Interrupting
// the in-flight turn is Esc, with /interrupt as its command alias; these
// tests pin that split so the meanings can't silently swap again.

import (
	"errors"
	"strings"
	"testing"
)

func lastLineText(t *testing.T, m Model, kind string) string {
	t.Helper()
	if len(m.lines) == 0 {
		t.Fatal("no lines rendered")
	}
	l := m.lines[len(m.lines)-1]
	if l.kind != kind {
		t.Fatalf("last line kind=%q text=%q, want kind %q", l.kind, l.text, kind)
	}
	return l.text
}

// Bare /stop targets the focused group; an explicit argument overrides it.
// Both must return a command (the daemon call) and announce the stop.
func TestStopDispatch(t *testing.T) {
	m := inputModel(t, "")

	if cmd := m.dispatchInput("/stop"); cmd == nil {
		t.Fatal("/stop returned no daemon command")
	}
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "stopping main's VM") {
		t.Fatalf("bare /stop line = %q, want it to target the focused group (main)", txt)
	}

	if cmd := m.dispatchInput("/stop ghost"); cmd == nil {
		t.Fatal("/stop ghost returned no daemon command")
	}
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "stopping ghost's VM") {
		t.Fatalf("targeted /stop line = %q, want ghost", txt)
	}
}

// The stop response renders success/failure and never gets swallowed.
func TestStopResponseRenders(t *testing.T) {
	m := inputModel(t, "")

	if cmd := m.handleDaemonResp(daemonRespMsg{op: "stop", group: "ghost"}); cmd == nil {
		t.Fatal("stop success must trigger a list refresh")
	}
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "stopped ghost's VM") {
		t.Fatalf("success line = %q", txt)
	}

	m.handleDaemonResp(daemonRespMsg{op: "stop", group: "ghost", err: errors.New("permission denied")})
	if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "permission denied") {
		t.Fatalf("error line = %q", txt)
	}
}

// /interrupt keeps the old turn-kill behavior reachable as a command (Esc is
// the primary gesture) — and must NOT stop the VM.
func TestInterruptCommand(t *testing.T) {
	m := inputModel(t, "")
	if cmd := m.dispatchInput("/interrupt"); cmd == nil {
		t.Fatal("/interrupt returned no command")
	}
	for _, l := range m.lines {
		if strings.Contains(l.text, "VM") {
			t.Fatalf("/interrupt rendered a VM-stop line: %q", l.text)
		}
	}
}

// /stop-plugin must not be shadowed by the /stop prefix match.
func TestStopPluginNotShadowed(t *testing.T) {
	m := inputModel(t, "")
	m.dispatchInput("/stop-plugin nope")
	for _, l := range m.lines {
		if strings.Contains(l.text, "VM") {
			t.Fatalf("/stop-plugin fell through to the VM stop: %q", l.text)
		}
	}
}
