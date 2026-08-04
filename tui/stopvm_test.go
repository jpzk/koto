package main

// /stopvm — the TUI's VM power-off command (daemon `stop` verb). Distinct
// from /stop, which interrupts the in-flight turn; these tests pin the
// difference so the two can't drift back into one another.

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

// Bare /stopvm targets the focused group; an explicit argument overrides it.
// Both must return a command (the daemon call) and announce the stop.
func TestStopVMDispatch(t *testing.T) {
	m := inputModel(t, "")

	if cmd := m.dispatchInput("/stopvm"); cmd == nil {
		t.Fatal("/stopvm returned no daemon command")
	}
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "stopping main's VM") {
		t.Fatalf("bare /stopvm line = %q, want it to target the focused group (main)", txt)
	}

	if cmd := m.dispatchInput("/stopvm ghost"); cmd == nil {
		t.Fatal("/stopvm ghost returned no daemon command")
	}
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "stopping ghost's VM") {
		t.Fatalf("targeted /stopvm line = %q, want ghost", txt)
	}
}

// The stop response renders success/failure and never gets swallowed.
func TestStopVMResponseRenders(t *testing.T) {
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

// /stop must keep meaning "interrupt the turn" — it must NOT stop the VM.
func TestStopStaysInterrupt(t *testing.T) {
	m := inputModel(t, "")
	if cmd := m.dispatchInput("/stop"); cmd == nil {
		t.Fatal("/stop returned no command")
	}
	for _, l := range m.lines {
		if strings.Contains(l.text, "VM") {
			t.Fatalf("/stop rendered a VM-stop line: %q", l.text)
		}
	}
}
