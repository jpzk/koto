package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// ---- alt+[ on the wire --------------------------------------------------------

// keyRecorder is a throwaway Bubble Tea model: it records every key the real
// v1.3.10 input reader produces and quits on ctrl+c.
type keyRecorder struct{ keys *[]string }

func (r keyRecorder) Init() tea.Cmd { return nil }
func (r keyRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		if k.Type == tea.KeyCtrlC {
			return r, tea.Quit
		}
		*r.keys = append(*r.keys, k.String())
	}
	return r, nil
}
func (r keyRecorder) View() string { return "" }

// parseReads runs each chunk through Bubble Tea's own reader as ONE read
// (io.Pipe hands every Write to exactly one Read) and returns the key
// strings it produced, in order.
func parseReads(t *testing.T, chunks ...string) []string {
	t.Helper()
	pr, pw := io.Pipe()
	var keys []string
	p := tea.NewProgram(keyRecorder{&keys}, tea.WithInput(pr), tea.WithOutput(io.Discard),
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
	return keys
}

// The raw bytes of alt+[ (ESC '[') in one read are alt+[; full CSI
// sequences in one read are still their own keys, never alt+[.
func TestAltBracketRawBytes(t *testing.T) {
	if got := parseReads(t, "\x1b["); len(got) != 1 || got[0] != keyTreeToggle {
		t.Fatalf(`"\x1b[" parsed as %q, want [%q]`, got, keyTreeToggle)
	}
	for raw, want := range map[string]string{
		"\x1b[A": "up", "\x1b[B": "down", "\x1b[C": "right", "\x1b[D": "left",
		"\x1b[Z": "shift+tab", "\x1b[1;5C": "ctrl+right", "\x1b[1;3D": "alt+left",
		"\x1b[5~": "pgup", "\x1b[6~": "pgdown", "\x1b[3~": "delete", "\x1b[H": "home",
		"\t": "tab", "\x1b": "esc", "\x1b\x1b": "alt+esc", "[": "[",
	} {
		if got := parseReads(t, raw); len(got) != 1 || got[0] != want {
			t.Errorf("%q parsed as %q, want [%q]", raw, got, want)
		}
	}
	// An unknown CSI sequence (cursor show) is not a key at all — and in
	// particular not alt+[ followed by stray runes.
	if got := parseReads(t, "\x1b[?25h"); len(got) != 0 {
		t.Errorf(`"\x1b[?25h" produced keys %q, want none`, got)
	}
	// Back-to-back: alt+[ then an arrow in separate reads, and an arrow
	// then alt+[ — each read keeps its own meaning.
	if got := parseReads(t, "\x1b[", "\x1b[A", "\x1b[B", "\x1b["); strings.Join(got, ",") != "alt+[,up,down,alt+[" {
		t.Errorf("sequence parsed as %q", got)
	}
}

func altBracket() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}, Alt: true} }

// The parsed alt+[ toggles the tree in and out; arrows keep their meaning;
// esc / ctrl+[ still toggle with no turn running; alt+[ works mid-turn.
func TestAltBracketTogglesTree(t *testing.T) {
	m := focusModel(t)
	if m.focus != focusInput {
		t.Fatalf("start focus %v", m.focus)
	}
	m, _ = dpress(t, m, altBracket())
	if m.focus != focusTree {
		t.Fatalf("alt+[ did not open the tree (focus %v)", m.focus)
	}
	m = press(t, m, tea.KeyDown)
	if m.focus != focusTree {
		t.Fatal("down left the tree")
	}
	m, _ = dpress(t, m, altBracket())
	if m.focus != focusInput {
		t.Fatalf("alt+[ did not close the tree (focus %v)", m.focus)
	}
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusTree {
		t.Fatal("esc no longer opens the tree")
	}
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusInput {
		t.Fatal("esc no longer closes the tree")
	}
	m.busy[m.curKey()] = true
	m, _ = dpress(t, m, altBracket())
	if m.focus != focusTree {
		t.Fatal("alt+[ must toggle mid-turn (esc interrupts then)")
	}
	// tab is no longer a tree toggle.
	m.busy[m.curKey()] = false
	recordCycles(t)
	m = press(t, m, tea.KeyTab)
	if m.focus != focusTree {
		t.Fatal("tab toggled the tree")
	}
}

// Fleet view: alt+[ shows/hides the tree; tab does not.
func TestFleetViewAltBracketTree(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlK)
	if m.focus != focusTop {
		t.Fatalf("ctrl+k focus %v", m.focus)
	}
	before := m.preTopFocus
	m = press(t, m, tea.KeyTab)
	if m.preTopFocus != before {
		t.Fatal("tab still toggles the tree in the fleet view")
	}
	m, _ = dpress(t, m, altBracket())
	if m.preTopFocus == before {
		t.Fatal("alt+[ did not toggle the tree in the fleet view")
	}
	if h := ansi.Strip(m.renderTopHint()); strings.Contains(h, "tab tree") {
		t.Errorf("fleet hint still says tab: %q", h)
	}
}

// ---- tab cycles the mode -----------------------------------------------------

type sentCycle struct{ g, s, mode, prev string }

func recordCycles(t *testing.T) *[]sentCycle {
	t.Helper()
	var sent []sentCycle
	orig := modeCycleCmdFn
	modeCycleCmdFn = func(_, g, s, mode, prev string) tea.Cmd {
		sent = append(sent, sentCycle{g, s, mode, prev})
		return func() tea.Msg { return nil }
	}
	t.Cleanup(func() { modeCycleCmdFn = orig })
	return &sent
}

func modeGroups(provider string) map[string]GroupInfo {
	return map[string]GroupInfo{"alpha": {Running: true, Provider: provider,
		Sessions: []string{"goal-x", "triage"}, SessionModes: map[string]string{}}}
}

// ack answers the last sent cycle as the daemon would.
func ack(t *testing.T, m Model, c sentCycle) (Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(modeCycledMsg{group: c.g, session: c.s, mode: c.mode, prev: c.prev, got: c.mode})
	return nm.(Model), cmd
}

func chip(m Model) string { return ansi.Strip(m.renderModeChip()) }

func cycleSeq(t *testing.T, m Model, k tea.KeyType, n int) (Model, []string) {
	t.Helper()
	sent := recordCycles(t)
	var chips []string
	for i := 0; i < n; i++ {
		m = press(t, m, k)
		chips = append(chips, strings.TrimSpace(chip(m)))
		if len(*sent) != i+1 {
			t.Fatalf("press %d: sent %+v", i, *sent)
		}
		m, _ = ack(t, m, (*sent)[i])
		if len(m.modeWant) != 0 {
			t.Fatalf("modeWant not cleared after ack: %v", m.modeWant)
		}
	}
	return m, chips
}

// tab: yolo → plan → confirm → yolo; the chip changes before the daemon
// answers; shift+tab walks the other way.
func TestTabCyclesModeForwardShiftTabBackward(t *testing.T) {
	m := treeModel(t, modeGroups(""), "alpha", "", "")
	m.focus = focusInput
	sent := recordCycles(t)
	m = press(t, m, tea.KeyTab)
	if c := chip(m); !strings.HasPrefix(c, "plan") {
		t.Fatalf("chip before ack = %q, want plan (immediate)", c)
	}
	if len(*sent) != 1 || (*sent)[0] != (sentCycle{"alpha", "", modePlan, modeYolo}) {
		t.Fatalf("sent %+v", *sent)
	}
	m, _ = ack(t, m, (*sent)[0])
	if m.groups["alpha"].SessionModes[""] != modePlan {
		t.Fatalf("registry after ack: %v", m.groups["alpha"].SessionModes)
	}
	m, chips := cycleSeq(t, m, tea.KeyTab, 3)
	if strings.Join(chips, ",") != "conf›,yolo›,plan›" {
		t.Fatalf("tab chips %q", chips)
	}
	m, chips = cycleSeq(t, m, tea.KeyShiftTab, 3)
	if strings.Join(chips, ",") != "yolo›,conf›,plan›" {
		t.Fatalf("shift+tab chips %q", chips)
	}
	// Works from the tree too, on the current session, without leaving it.
	m.focus = focusTree
	m, chips = cycleSeq(t, m, tea.KeyTab, 1)
	if chips[0] != "conf›" || m.focus != focusTree {
		t.Fatalf("tree tab: chip %q focus %v", chips, m.focus)
	}
}

// goal-* sessions never land on confirm.
func TestTabGoalSessionSkipsConfirm(t *testing.T) {
	m := treeModel(t, modeGroups(""), "alpha", "goal-x", "")
	m.focus = focusInput
	if m.activeSession("alpha") != "goal-x" {
		t.Fatalf("active session %q", m.activeSession("alpha"))
	}
	m, chips := cycleSeq(t, m, tea.KeyTab, 3)
	if strings.Join(chips, ",") != "plan›,yolo›,plan›" {
		t.Fatalf("goal tab chips %q", chips)
	}
	// from plan (where the forward walk ended): back to yolo, then plan — never confirm.
	_, chips = cycleSeq(t, m, tea.KeyShiftTab, 2)
	if strings.Join(chips, ",") != "yolo›,plan›" {
		t.Fatalf("goal shift+tab chips %q", chips)
	}
}

// codex / pi (and venice): one-line notice, nothing sent, chip stays yolo.
func TestTabUnsupportedHarnessNotice(t *testing.T) {
	for prov, h := range map[string]string{"openai": "codex", "minimax": "pi", "venice": "venice"} {
		m := treeModel(t, modeGroups(prov), "alpha", "", "")
		m.focus = focusInput
		sent := recordCycles(t)
		n := len(m.lines)
		for _, k := range []tea.KeyType{tea.KeyTab, tea.KeyShiftTab} {
			m = press(t, m, k)
		}
		if len(*sent) != 0 {
			t.Fatalf("%s: sent %+v", prov, *sent)
		}
		if c := chip(m); !strings.HasPrefix(c, "yolo") {
			t.Fatalf("%s: chip %q", prov, c)
		}
		if got := m.lines[n:]; len(got) != 2 || got[0].text != "plan/confirm not supported for "+h+" yet — session stays yolo" {
			t.Fatalf("%s: lines %+v", prov, got)
		}
	}
}

// Mid-turn: still switches (host applies it from the next turn) and the one
// sys line says so.
func TestTabMidTurnNote(t *testing.T) {
	m := treeModel(t, modeGroups(""), "alpha", "", "")
	m.focus = focusInput
	m.busy[m.curKey()] = true
	sent := recordCycles(t)
	n := len(m.lines)
	m = press(t, m, tea.KeyTab)
	if len(*sent) != 1 {
		t.Fatalf("sent %+v", *sent)
	}
	got := m.lines[n:]
	if len(got) != 1 || !strings.Contains(got[0].text, "→ plan") || !strings.Contains(got[0].text, "applies from the next turn") {
		t.Fatalf("lines %+v", got)
	}
	m.busy[m.curKey()] = false
	n = len(m.lines)
	m, _ = ack(t, m, (*sent)[0])
	m = press(t, m, tea.KeyTab)
	if strings.Contains(m.lines[len(m.lines)-1].text, "next turn") {
		t.Fatal("idle switch carries the mid-turn note")
	}
}

// Presses while an RPC is in flight: one RPC at a time, the answer sends
// the newest wish; a refusal rolls the chip back.
func TestTabInflightChainAndRollback(t *testing.T) {
	m := treeModel(t, modeGroups(""), "alpha", "", "")
	m.focus = focusInput
	sent := recordCycles(t)
	m = press(t, m, tea.KeyTab) // plan, sent
	m = press(t, m, tea.KeyTab) // confirm, held
	if len(*sent) != 1 || !strings.HasPrefix(chip(m), "conf") {
		t.Fatalf("sent %+v chip %q", *sent, chip(m))
	}
	m, cmd := ack(t, m, (*sent)[0])
	if cmd == nil || len(*sent) != 2 || (*sent)[1] != (sentCycle{"alpha", "", modeConfirm, modePlan}) {
		t.Fatalf("chain: sent %+v", *sent)
	}
	nm, _ := m.Update(modeCycledMsg{group: "alpha", session: "", mode: modeConfirm, prev: modePlan,
		err: errors.New("rpc error: refused")})
	m = nm.(Model)
	if c := chip(m); !strings.HasPrefix(c, "plan") || len(m.modeWant) != 0 {
		t.Fatalf("rollback chip %q want %v", c, m.modeWant)
	}
	if l := m.lines[len(m.lines)-1]; l.kind != "err" || !strings.Contains(l.text, "refused") {
		t.Fatalf("no refusal line: %+v", l)
	}
}

// ---- tab keeps its local meaning ---------------------------------------------

func assertNoModeChange(t *testing.T, m Model, sent *[]sentCycle, where string) {
	t.Helper()
	if len(*sent) != 0 || len(m.modeWant) != 0 {
		t.Fatalf("%s: tab changed the mode (sent %+v want %v)", where, *sent, m.modeWant)
	}
}

func TestTabInApprovalCardMovesFocusOnly(t *testing.T) {
	sent := recordCycles(t)
	m := openCard(t)
	before := m.sessionMode("alpha", "")
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.approvalCard.open || !m.approvalCard.allow {
		t.Fatal("tab did not move focus to Allow")
	}
	m, _ = dpress(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if !m.approvalCard.open || m.approvalCard.allow {
		t.Fatal("shift+tab did not move focus back to Deny")
	}
	assertNoModeChange(t, m, sent, "approval card")
	if m.sessionMode("alpha", "") != before {
		t.Fatal("mode changed under the card")
	}
}

func TestTabInConfirmButtonsMovesFocusOnly(t *testing.T) {
	for _, d := range confirmDialogs {
		m, _ := openDialog(t, d)
		sent := recordCycles(t)
		m, _ = dpress(t, m, keyTab)
		if !d.isOpen(m) || !d.yes(m) {
			t.Fatalf("%s: tab did not move to Yes", d.name)
		}
		assertNoModeChange(t, m, sent, d.name)
	}
}

func TestTabInNewGroupDialogMovesFieldOnly(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	if !m.newGroup.open {
		t.Fatal("dialog not open")
	}
	sent := recordCycles(t)
	f := m.newGroup.field()
	m = press(t, m, tea.KeyTab)
	if !m.newGroup.open || m.newGroup.field() == f {
		t.Fatalf("tab did not move the field (still %q)", f)
	}
	assertNoModeChange(t, m, sent, "new-group dialog")
}

func TestTabInModePickerMovesOnly(t *testing.T) {
	m := treeModel(t, modeGroups(""), "alpha", "", "")
	m.openModeSel()
	sent := recordCycles(t)
	m = press(t, m, tea.KeyTab)
	if !m.modeSel.open || m.modeSel.idx != 1 {
		t.Fatalf("picker: %+v", m.modeSel)
	}
	assertNoModeChange(t, m, sent, "mode picker")
}

// ---- help text -------------------------------------------------------------------

func TestHelpNoLongerSaysTabTogglesTree(t *testing.T) {
	h := strings.ToLower(ansi.Strip(helpContent(120)))
	for _, line := range strings.Split(h, "\n") {
		if strings.Contains(line, "tab") && strings.Contains(line, "tree") {
			t.Errorf("help still ties tab to the tree: %q", line)
		}
	}
	for _, want := range []string{"alt+[", "esc / ctrl+[", "tab / shift+tab", "cycle this session's mode"} {
		if !strings.Contains(h, want) {
			t.Errorf("help missing %q", want)
		}
	}
	m := focusModel(t)
	for _, hint := range []string{m.renderHint()} {
		if p := ansi.Strip(hint); strings.Contains(p, "tab/esc") || strings.Contains(p, "⇥/⎋") {
			t.Errorf("hint still says tab toggles the tree: %q", p)
		}
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m = nm.(Model)
	if l := m.lines[len(m.lines)-1].text; strings.Contains(l, "tab into") || !strings.Contains(l, "alt+[") {
		t.Errorf("ctrl+x hint: %q", l)
	}
}

// With the terminal pane focused tab still belongs to the guest: no mode RPC.
func TestTabInShellPaneGoesToGuest(t *testing.T) {
	m := splitShellModel(t)
	m.focusShellPane()
	sent := recordCycles(t)
	m = press(t, m, tea.KeyTab)
	m = press(t, m, tea.KeyShiftTab)
	if m.focus != focusShell {
		t.Fatalf("focus %v, want focusShell", m.focus)
	}
	assertNoModeChange(t, m, sent, "shell pane")
}
