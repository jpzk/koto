package main

// shell_key_test.go — alt+] opens/closes the shared terminal pane (operator
// request 2026-11-13; ctrl+] stays the fallback). alt+] arrives as ESC ']',
// the OSC introducer, so the wire tests run the raw bytes through Bubble
// Tea v1.3.10's real input reader before the parsed keys reach the Model.

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// msgRecorder keeps every parsed KeyMsg (not just its string) so the test
// can replay them into the real Model.
type msgRecorder struct{ msgs *[]tea.KeyMsg }

func (r msgRecorder) Init() tea.Cmd { return nil }
func (r msgRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		if k.Type == tea.KeyCtrlC {
			return r, tea.Quit
		}
		*r.msgs = append(*r.msgs, k)
	}
	return r, nil
}
func (r msgRecorder) View() string { return "" }

// parseKeyMsgs: each chunk is ONE read through a real tea.Program (io.Pipe
// hands every Write to exactly one Read); returns the parsed KeyMsgs.
func parseKeyMsgs(t *testing.T, chunks ...string) []tea.KeyMsg {
	t.Helper()
	pr, pw := io.Pipe()
	var msgs []tea.KeyMsg
	p := tea.NewProgram(msgRecorder{&msgs}, tea.WithInput(pr), tea.WithOutput(io.Discard),
		tea.WithoutRenderer(), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	for _, c := range append(chunks, "\x03") {
		if _, err := pw.Write([]byte(c)); err != nil {
			t.Fatalf("write %q: %v", c, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("program: %v", err)
		}
	case <-time.After(5 * time.Second):
		p.Kill()
		t.Fatal("program did not quit")
	}
	_ = pw.Close()
	return msgs
}

func keyStrings(ks []tea.KeyMsg) string {
	s := make([]string, len(ks))
	for i, k := range ks {
		s[i] = k.String()
	}
	return strings.Join(s, ",")
}

// Raw ESC ']' in one read is alt+]; it consumes exactly two bytes, so what
// follows in the same read (runes, an arrow, another alt+]) is not swallowed
// as an OSC payload.
func TestAltCloseBracketRawBytes(t *testing.T) {
	for _, tc := range []struct {
		chunks []string
		want   string
	}{
		{[]string{"\x1b]"}, "alt+]"},
		{[]string{"\x1b]ls"}, "alt+],ls"},
		{[]string{"\x1b]\x1b[A"}, "alt+],up"},
		{[]string{"\x1b]\r"}, "alt+],enter"},
		{[]string{"\x1b]\x1b]"}, "alt+],alt+]"},
		{[]string{"\x1b]", "x", "\x1b]"}, "alt+],x,alt+]"},
		{[]string{"\x1d"}, "ctrl+]"},
		{[]string{"]"}, "]"},
		{[]string{"\x1b["}, "alt+["},
	} {
		if got := keyStrings(parseKeyMsgs(t, tc.chunks...)); got != tc.want {
			t.Errorf("%q parsed as %q, want %q", tc.chunks, got, tc.want)
		}
	}
	if keyShellToggle != "alt+]" || keyShellToggleFallback != "ctrl+]" {
		t.Fatalf("bindings drifted: %q / %q", keyShellToggle, keyShellToggleFallback)
	}
}

func feedKeys(t *testing.T, m Model, ks []tea.KeyMsg) Model {
	t.Helper()
	for _, k := range ks {
		nm, _ := m.Update(k)
		m = nm.(Model)
	}
	return m
}

func sentBytes(rec *recordShellStream) string {
	var b strings.Builder
	for _, s := range rec.sent {
		b.Write(s)
	}
	return b.String()
}

// From the message bar, raw "\x1b]ls" opens the pane focused and the "ls"
// that followed in the same read lands in the guest pty. Inside the pane,
// raw "pwd\x1b]hi" sends "pwd", CLOSES the pane without forwarding ESC ']',
// and "hi" lands in the message bar.
func TestAltCloseBracketRawOpensAndClosesShell(t *testing.T) {
	m, rec := shellKeyModel(t)
	m.closeShell()
	m.input.Focus()
	if m.shellOpen || m.focus == focusShell {
		t.Fatalf("precondition: shellOpen=%v focus=%v", m.shellOpen, m.focus)
	}
	m = feedKeys(t, m, parseKeyMsgs(t, "\x1b]ls"))
	if !m.shellOpen || m.focus != focusShell {
		t.Fatalf("alt+] from input: shellOpen=%v focus=%v, want open + focusShell", m.shellOpen, m.focus)
	}
	if got := sentBytes(rec); got != "ls" {
		t.Fatalf("guest received %q, want %q (keys after alt+] swallowed?)", got, "ls")
	}
	rec.sent = nil
	m = feedKeys(t, m, parseKeyMsgs(t, "pwd\x1b]hi"))
	if got := sentBytes(rec); got != "pwd" {
		t.Fatalf("guest received %q, want %q — alt+] must not reach the pty", got, "pwd")
	}
	if m.shellOpen || m.focus == focusShell {
		t.Fatalf("alt+] in the pane: shellOpen=%v focus=%v, want closed", m.shellOpen, m.focus)
	}
	if m.shell == nil || m.shell.ended {
		t.Error("alt+] tore down the session — it must survive for reopen")
	}
	if got := m.input.Value(); got != "hi" {
		t.Errorf("input = %q after closing, want %q (keys after alt+] swallowed?)", got, "hi")
	}
}

// From tree focus alt+] opens the pane too (same as toggleShellPane), and
// with the pane open but unfocused it closes it.
func TestAltCloseBracketFromTreeAndSplit(t *testing.T) {
	m := splitShellModel(t)
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}, Alt: true})
	if m.shellOpen {
		t.Fatal("alt+] with the pane open but unfocused did not close it")
	}
	m, _ = dpress(t, m, altBracket())
	if m.focus != focusTree {
		t.Fatalf("alt+[ focus %v", m.focus)
	}
	m = feedKeys(t, m, parseKeyMsgs(t, "\x1b]"))
	if !m.shellOpen || m.focus != focusShell {
		t.Fatalf("alt+] from the tree: shellOpen=%v focus=%v", m.shellOpen, m.focus)
	}
}

// ctrl+] keeps working both ways and is not forwarded either.
func TestCtrlCloseBracketStillToggles(t *testing.T) {
	m, rec := shellKeyModel(t)
	m = feedKeys(t, m, parseKeyMsgs(t, "\x1d"))
	if m.shellOpen || m.focus == focusShell || len(rec.sent) != 0 {
		t.Fatalf("ctrl+] in pane: shellOpen=%v focus=%v sent=%q", m.shellOpen, m.focus, rec.sent)
	}
	m = feedKeys(t, m, parseKeyMsgs(t, "\x1d"))
	if !m.shellOpen || m.focus != focusShell {
		t.Fatalf("ctrl+] reopen: shellOpen=%v focus=%v", m.shellOpen, m.focus)
	}
}

// Every place that teaches the key names alt+] (with ctrl+] as fallback).
func TestShellKeyHintsSayAlt(t *testing.T) {
	help := ansi.Strip(helpContent(200))
	for _, want := range []string{"alt+]", "shared terminal (tmux) for the active session (fallback: ctrl+])",
		"close the pane (fallback: ctrl+])", "same as alt+] / ctrl+]"} {
		if !strings.Contains(help, want) {
			t.Errorf("cheatsheet lacks %q", want)
		}
	}
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlP)
	if out := stripANSI(m.renderPicker(24)); !strings.Contains(out, "alt+] / ctrl+]") {
		t.Errorf("palette hint lacks alt+]:\n%s", out)
	}
	mm := m
	for _, r := range "alt+]" {
		nm, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		mm = nm.(Model)
	}
	if len(mm.picker.matches) == 0 {
		t.Error(`palette query "alt+]" matched nothing`)
	}
	withColor(t)
	b := focusModel(t)
	b.cur = "fresh"
	b.groups["fresh"] = GroupInfo{Running: true}
	lines, _ := b.buildStaticLines(b.logContentCols(), false)
	if s := ansi.Strip(strings.Join(lines, "\n")); !strings.Contains(s, "alt+] opens a shared terminal (or ctrl+])") {
		t.Errorf("banner lacks alt+]:\n%s", s)
	}
	sm, _ := shellKeyModel(t)
	if f := ansi.Strip(sm.View()); !strings.Contains(f, "] close") || strings.Contains(f, "ctrl+] close") {
		t.Errorf("shell footer does not say alt+] close")
	}
	if f := ansi.Strip(focusModel(t).View()); !strings.Contains(f, "] shell") || strings.Contains(f, "^] shell") {
		t.Errorf("hint bar does not say alt+] shell")
	}
}
