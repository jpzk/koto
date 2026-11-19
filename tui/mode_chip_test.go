package main

import (
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// ---- the mode chip ----------------------------------------------------------

// The chip color is the palette VAR of each role, read at render time — not a
// hex: poison each var with a sentinel and the chip must follow it.
func TestModeChipColorIsTheVar(t *testing.T) {
	withBuiltinPalette(t)
	cBrick, cMagenta, cAmber, cRed = "#010101", "#020202", "#030303", "#040404"
	for _, c := range []struct {
		mode string
		want lipgloss.Color
		role string
	}{
		{modeYolo, cBrick, "cBrick"},
		{modePlan, cMagenta, "cMagenta (transcript tool glyph, view.go case \"tool\")"},
		{modeConfirm, cAmber, "cAmber (tree cursor, view.go inv(cFgInv, cAmber))"},
	} {
		if got := modeChipColor(c.mode); got != c.want {
			t.Errorf("%s chip = %q, want the %s var (%q)", c.mode, got, c.role, c.want)
		}
	}
	if modeChipColor(modeYolo) == cRed {
		t.Fatal("yolo chip must not reuse cRed (error)")
	}
	// The transcript's tool glyph and the tree cursor really are those
	// vars: render both under truecolor with the sentinels in place.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	tool := strings.Join(renderBlockLines(renderedBlock{kind: "tool", ts: 1754000000, rendered: "Bash ls"}, 60), "\n")
	if want := hexSGR(string(cMagenta)); want == "" || !strings.Contains(tool, want) {
		t.Errorf("tool glyph is not drawn in cMagenta (%s): %q", want, tool)
	}
	m := treeModel(t, map[string]GroupInfo{"alpha": {Running: true}}, "alpha", "", "")
	pad := func(s string, w int) string { return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s))) }
	cursor := m.renderTreeRow(treeRow{group: "alpha"}, true, true, false, pad) // hov = the tree cursor
	if want := strings.Replace(hexSGR(string(cAmber)), "38;", "48;", 1); want == "" || !strings.Contains(cursor, want) {
		t.Errorf("tree cursor is not painted in cAmber (%s): %q", want, cursor)
	}
}

// Each mode's chip renders in its var, inside the input box, within 40 cols.
func TestModeChipInInputBox40Cols(t *testing.T) {
	withBuiltinPalette(t)
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	for _, md := range []string{modeYolo, modePlan, modeConfirm} {
		g := map[string]GroupInfo{"alpha": {Running: true, SessionModes: map[string]string{}}}
		if md != modeYolo {
			g["alpha"].SessionModes[""] = md
		}
		m := treeModel(t, g, "alpha", "", "")
		sm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
		m = sm.(Model)
		for _, val := range []string{"", "please write a long prompt that wraps across the box rows"} {
			m.input.SetValue(val)
			box := m.renderInput()
			assertFits(t, box, 40)
			plain := ansi.Strip(box)
			if !strings.Contains(plain, modeChipLabel(md)+"›") {
				t.Errorf("%s value=%q: chip %q missing:\n%s", md, val, modeChipLabel(md), plain)
			}
			if want := lipgloss.NewStyle().Foreground(modeChipColor(md)).Bold(true).Render(modeChipLabel(md) + "›"); !strings.Contains(box, want) {
				t.Errorf("%s: chip not rendered in its var %q", md, modeChipColor(md))
			}
		}
	}
}

// Mono strips color, so the chip TEXT alone must tell the modes apart.
func TestModeChipMonoDistinct(t *testing.T) {
	defer func(prev bool) { monoMode = prev }(monoMode)
	monoMode = true
	seen := map[string]string{}
	for _, md := range permModes {
		m := treeModel(t, map[string]GroupInfo{"alpha": {Running: true, SessionModes: map[string]string{"": md}}}, "alpha", "", "")
		chip := ansi.Strip(monoFrame(m.renderModeChip()))
		if prev, dup := seen[chip]; dup {
			t.Fatalf("mono chip %q for %s equals %s", chip, md, prev)
		}
		seen[chip] = md
		if !strings.HasPrefix(chip, modeChipLabel(md)+">") {
			t.Errorf("mono chip for %s = %q", md, chip)
		}
		if cellWidth(chip) != m.modeChipW() {
			t.Errorf("chip %q is %d cells, budget %d", chip, cellWidth(chip), m.modeChipW())
		}
	}
}

// A mode switch the daemon accepted shows in the chip on the very next
// frame, before any state frame.
func TestModeChipUpdatesOnSwitch(t *testing.T) {
	m := treeModel(t, map[string]GroupInfo{"alpha": {Running: true}}, "alpha", "", "")
	if !strings.Contains(ansi.Strip(m.renderInput()), "yolo›") {
		t.Fatal("fresh group chip is not yolo")
	}
	m.handleDaemonResp(daemonRespMsg{op: "config", group: "alpha",
		resp: map[string]any{"ok": true, "config": map[string]any{"mode": "plan", "mode_session": "default"}}})
	if !strings.Contains(ansi.Strip(m.renderInput()), "plan›") {
		t.Fatalf("chip did not switch to plan:\n%s", ansi.Strip(m.renderInput()))
	}
	m.handleDaemonResp(daemonRespMsg{op: "config", group: "alpha",
		resp: map[string]any{"ok": true, "config": map[string]any{"mode": "yolo", "mode_session": "default"}}})
	if !strings.Contains(ansi.Strip(m.renderInput()), "yolo›") {
		t.Fatal("chip did not switch back to yolo")
	}
}

// cBrick is a fixed status hue: its shade follows the ground like cRed's,
// and the terminal palette falls back to 256-color 124.
func TestBrickHueByGround(t *testing.T) {
	withBuiltinPalette(t)
	if cBrick != "124" {
		t.Fatalf("built-in cBrick = %q, want 124", cBrick)
	}
	for _, c := range []struct {
		name string
		want string
	}{{"teletext", statusOnDark.brick}, {"tape", statusOnLight.brick}} {
		p, err := loadTheme("/nonexistent/koto.sock", c.name)
		if err != nil {
			t.Fatal(err)
		}
		applyTheme(p)
		if string(cBrick) != c.want || cBrick == cRed {
			t.Errorf("%s: cBrick = %q (cRed %q), want %q", c.name, cBrick, cRed, c.want)
		}
	}
	if statusOnDark.brick != "#b5403a" {
		t.Errorf("dark brick = %s", statusOnDark.brick)
	}
	resetTheme()
	if cBrick != "124" {
		t.Fatalf("reset cBrick = %q", cBrick)
	}
	if err := applyThemeName("", "amber", func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if cBrick != "124" {
		t.Fatalf("amber cBrick = %q", cBrick)
	}
}

// ---- TUI-created sessions default to plan ----------------------------------

type createCall struct{ group, session, mode string }

func recordCreates(t *testing.T, refuse func(extra map[string]any) error) *[]createCall {
	t.Helper()
	var mu sync.Mutex
	var calls []createCall
	orig := sessionCreateCallFn
	sessionCreateCallFn = func(_ string, extra map[string]any) (map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		md, _ := extra["mode"].(string)
		calls = append(calls, createCall{extra["group"].(string), extra["session"].(string), md})
		if refuse != nil {
			if err := refuse(extra); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": true}, nil
	}
	t.Cleanup(func() { sessionCreateCallFn = orig })
	return &calls
}

func TestTUICreatedSessionStartsPlan(t *testing.T) {
	calls := recordCreates(t, nil)
	m := sessModel(t)
	m.groups["alpha"] = GroupInfo{Running: true, Provider: "claudesdk"}
	cmd := m.dispatchInput("/session review")
	// Optimistic: the chip shows plan from the first frame.
	if !strings.Contains(ansi.Strip(m.renderInput()), "plan›") {
		t.Fatalf("new session chip:\n%s", ansi.Strip(m.renderInput()))
	}
	r, ok := cmd().(sessionCreatedMsg)
	if !ok || r.err != nil || r.mode != modePlan || r.notice != "" {
		t.Fatalf("create result %+v", r)
	}
	// The mode rode in the SAME create RPC.
	if len(*calls) != 1 || (*calls)[0] != (createCall{"alpha", "review", "plan"}) {
		t.Fatalf("create calls %+v", *calls)
	}
	m, _ = update(t, m, r)
	if m.sessionMode("alpha", "review") != modePlan || m.creating[sessKey("alpha", "review")] != nil {
		t.Fatalf("after create: mode=%s creating=%v", m.sessionMode("alpha", "review"), m.creating)
	}
}

// A send typed before the create returned waits for it (and, when it gets
// there first, performs the create itself — mode included): the session's
// first turn can never be registered through Send as yolo.
func TestFirstSendWaitsForPlanCreate(t *testing.T) {
	calls := recordCreates(t, nil)
	m := sessModel(t)
	m.groups["alpha"] = GroupInfo{Running: true, Provider: "claudesdk"}
	create := m.dispatchInput("/session review")
	sent := false
	gated := m.gateSend("alpha", "review", "hi", func() tea.Msg { sent = true; return nil })
	gated() // the send's goroutine wins the race
	if !sent || len(*calls) != 1 || (*calls)[0].mode != modePlan {
		t.Fatalf("send ran without a plan create first: sent=%v calls=%+v", sent, *calls)
	}
	create() // the create cmd then finds it done: no second RPC
	if len(*calls) != 1 {
		t.Fatalf("create issued twice: %+v", *calls)
	}
}

// A failed create blocks the send (no unmoded registration) and the next
// send retries the create with the mode.
func TestFailedCreateBlocksSend(t *testing.T) {
	fail := true
	calls := recordCreates(t, func(map[string]any) error {
		if fail {
			return errString("daemon: unavailable")
		}
		return nil
	})
	m := sessModel(t)
	m.groups["alpha"] = GroupInfo{Running: true, Provider: "claudesdk"}
	create := m.dispatchInput("/session review")
	m, _ = update(t, m, create())
	sent := false
	r := m.gateSend("alpha", "review", "hi", func() tea.Msg { sent = true; return nil })()
	if sent {
		t.Fatal("send went out after a failed create")
	}
	if dm, ok := r.(daemonRespMsg); !ok || dm.err == nil {
		t.Fatalf("gated send result %#v", r)
	}
	fail = false
	m.gateSend("alpha", "review", "hi", func() tea.Msg { sent = true; return nil })()
	if !sent || (*calls)[len(*calls)-1].mode != modePlan {
		t.Fatalf("retry: sent=%v calls=%+v", sent, *calls)
	}
}

// codex and pi have no plan yet: the session is still created, in yolo,
// with a one-line notice; the chip shows yolo.
func TestTUICreatedSessionCodexPiYolo(t *testing.T) {
	for prov, harness := range map[string]string{"openai": "codex", "minimax": "pi"} {
		t.Run(harness, func(t *testing.T) {
			calls := recordCreates(t, nil)
			m := sessModel(t)
			m.groups["alpha"] = GroupInfo{Running: true, Provider: prov}
			cmd := m.dispatchInput("/session review")
			if !strings.Contains(ansi.Strip(m.renderInput()), "yolo›") {
				t.Fatal("chip should show yolo")
			}
			r := cmd().(sessionCreatedMsg)
			if r.err != nil || len(*calls) != 1 || (*calls)[0].mode != "" {
				t.Fatalf("create %+v calls %+v", r, *calls)
			}
			m, _ = update(t, m, r)
			want := "plan not supported for " + harness + " yet — session is yolo"
			if !hasLine(m, want) {
				t.Fatalf("notice %q missing", want)
			}
			if m.sessionMode("alpha", "review") != modeYolo {
				t.Fatal("mode should be yolo")
			}
		})
	}
}

// The daemon refusing plan (provider changed under the snapshot) falls back
// to a yolo create with the notice, instead of failing the create.
func TestTUICreateFallsBackOnHarnessRefusal(t *testing.T) {
	calls := recordCreates(t, func(extra map[string]any) error {
		if extra["mode"] != nil {
			return errString("daemon: permission mode plan is not supported for this harness yet (pi)")
		}
		return nil
	})
	m := sessModel(t) // Provider "" -> assumed claude
	r := m.dispatchInput("/session review")().(sessionCreatedMsg)
	if r.err != nil || r.mode != "" || !strings.Contains(r.notice, "plan not supported") || len(*calls) != 2 {
		t.Fatalf("fallback %+v calls %+v", r, *calls)
	}
}

func hasLine(m Model, sub string) bool {
	for _, l := range m.lines {
		if strings.Contains(l.text, sub) {
			return true
		}
	}
	return false
}

// A new group's default session is yolo: the TUI never asks for a mode on
// spawn, and a fresh group's chip reads yolo.
func TestNewGroupDefaultSessionYolo(t *testing.T) {
	calls := recordCreates(t, nil)
	m := sessModel(t)
	m.groups["fresh"] = GroupInfo{Running: true, Provider: "claudesdk"}
	m.cur = "fresh"
	m.selectTreeRow(treeRow{group: "fresh"})
	if cmd := m.switchSession("fresh", ""); cmd != nil {
		t.Fatal("the default session is never created by the TUI")
	}
	if m.sessionMode("fresh", "") != modeYolo || !strings.Contains(ansi.Strip(m.renderInput()), "yolo›") {
		t.Fatal("new group default session is not yolo")
	}
	if len(*calls) != 0 {
		t.Fatalf("unexpected creates %+v", *calls)
	}
}

// goal-<name> sessions belong to the driver: the TUI never creates them, so
// they never get the plan default.
func TestGoalSessionStaysYolo(t *testing.T) {
	calls := recordCreates(t, nil)
	m := sessModel(t)
	m.groups["alpha"] = GroupInfo{Running: true, Provider: "claudesdk"}
	if cmd := m.switchSession("alpha", "goal-x"); cmd != nil {
		t.Fatal("goal session created by the TUI")
	}
	if len(*calls) != 0 || m.creating[sessKey("alpha", "goal-x")] != nil || m.sessionMode("alpha", "goal-x") != modeYolo {
		t.Fatalf("goal session: calls=%+v mode=%s", *calls, m.sessionMode("alpha", "goal-x"))
	}
}
