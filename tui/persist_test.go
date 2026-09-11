package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-09-11 M39: the TUI's run directory is inside the DAEMON's writable
// state tree, but the TUI runs as the operator — so the daemon names the files
// the operator opens for writing. A symlink at tui-state.json must not
// redirect that write onto an operator-owned file.
func TestStateWriteRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "koto.sock")
	victim := filepath.Join(t.TempDir(), "operator-secret")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, statePath(sock)); err != nil {
		t.Fatal(err)
	}
	saveState(sock, persistedState{Cur: "main", Draft: "hello"})
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatalf("symlinked state path redirected the write: %q", b)
	}
	// A read through the same symlink is refused too, so planted content
	// cannot reach the input bar either.
	if s := loadState(sock); s.Cur != "" || s.Draft != "" {
		t.Fatalf("read followed the symlink: %+v", s)
	}
	// Without the symlink, state round-trips as before.
	os.Remove(statePath(sock))
	saveState(sock, persistedState{Cur: "main", Draft: "hello"})
	if s := loadState(sock); s.Cur != "main" || s.Draft != "hello" {
		t.Fatalf("ordinary round-trip broken: %+v", s)
	}
}

// 2026-09-11 M62: the raw-frame branch of JobTail exists for daemons that
// predate JobTailReq.parsed — i.e. exactly the peers that do NOT sanitize. The
// raw peek renderer preserves control sequences, so a job's output could drive
// the operator's terminal the moment they hovered the row.
func TestJobTailRawFramesAreScrubbed(t *testing.T) {
	ESC := "\x1b"
	for _, payload := range []string{
		ESC + "]52;c;cGF5bG9hZA==\x07steal clipboard",
		ESC + "]0;retitled\x07title",
		ESC + "[2J" + ESC + "[Hclear screen",
		ESC + "[?1049hprivate mode",
		ESC + "_dcs" + ESC + "\\device control",
		"\u009bC1 CSI",
		"benign\u202etxet idlab",
	} {
		got := scrubVT(payload)
		for _, r := range got {
			if r == 0x1b || (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) {
				t.Errorf("scrubVT(%q) left %q in %q", payload, r, got)
			}
		}
	}
	// Plain text and a pure SGR survive — the peek pane still renders colour.
	if got := scrubVT("plain"); got != "plain" {
		t.Errorf("plain text mangled: %q", got)
	}
	if sgr := ESC + "[31mred" + ESC + "[0m"; scrubVT(sgr) != sgr {
		t.Errorf("SGR dropped: %q", scrubVT(sgr))
	}
}

// 2026-09-11 M73: sanitizeEvent runs on Event.Input while it is still
// serialized JSON, where an escape is the six printable bytes \\u001b --
// nothing for a control-character scrub to find. json.Unmarshal turns them
// back into real control bytes, and the value goes into the rendered tool
// line, which lipgloss preserves verbatim.
func TestToolInputControlsAreScrubbedAfterDecoding(t *testing.T) {
	for _, c := range []struct{ tool, input string }{
		{"Bash", `{"command":"echo \u001b]52;c;cGF5bG9hZA==\u0007"}`},
		{"Read", `{"file_path":"/tmp/\u001b[2J\u001b[H"}`},
		{"WebFetch", `{"url":"http://x/\u001b]0;retitled\u0007"}`},
		{"Grep", `{"pattern":"\u009bC1","path":"/w"}`},
		{"Task", `{"description":"benign\u202e reversed"}`},
	} {
		got := formatTool(c.tool, c.input)
		for _, r := range got {
			if r == 0x1b || (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) {
				t.Errorf("formatTool(%s, %s) left %q in %q", c.tool, c.input, r, got)
			}
		}
	}
	// Ordinary arguments are untouched.
	if got := formatTool("Bash", `{"command":"ls -la /workspace"}`); got != "Bash $ ls -la /workspace" {
		t.Errorf("plain command mangled: %q", got)
	}
	if got := formatTool("Read", `{"file_path":"/workspace/a b.txt"}`); got != "Read /workspace/a b.txt" {
		t.Errorf("plain path mangled: %q", got)
	}
}

// 2026-09-11 M96: goalOpCmd puts the selected run in extra["name"] and the
// five lifecycle RPCs dropped it, so the daemon fell back to implicit
// selection. A group runs several goals at once by design, so a command aimed
// at the row you picked could resolve to a different eligible run.
func TestGoalLifecycleRPCsCarryTheName(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"GoalApprove", "GoalPause", "GoalInterrupt", "GoalResume", "GoalCancel"} {
		i := strings.Index(string(src), "cl."+op+"(ctx,")
		if i < 0 {
			t.Fatalf("%s call not found", op)
		}
		line := string(src)[i:]
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		if !strings.Contains(line, `Name: s("name")`) {
			t.Errorf("%s does not pass the selected run name: %s", op, line)
		}
	}
}

// 2026-09-11 M111: error text is assembled on the CLIENT from gRPC status
// messages, daemon errors and guest errors, and it goes into both the rendered
// frame and the debug log, neither of which strips anything. addLine is the
// one place every error line passes through.
func TestErrorLinesAreScrubbed(t *testing.T) {
	esc := string(rune(0x1b))
	m := &Model{lines: nil, groupVer: map[string]int{}, vpCache: map[string]vpCacheEntry{}}
	m.addLine(logLine{kind: "err", group: "g", text: esc + "]52;c;cGF5bG9hZA==" + string(rune(7)) + "stolen"})
	if len(m.lines) != 1 {
		t.Fatalf("addLine stored %d lines", len(m.lines))
	}
	got := m.lines[0].text
	for _, r := range got {
		if r == 0x1b || (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) {
			t.Fatalf("error line kept %q: %q", r, got)
		}
	}
	// Ordinary error text is untouched.
	m.addLine(logLine{kind: "err", group: "g", text: "no such group \"ghost\""})
	if got := m.lines[1].text; got != "no such group \"ghost\"" {
		t.Fatalf("a plain error was mangled: %q", got)
	}
}

// 2026-09-11 M122: maxLines caps the line COUNT, and nothing capped the bytes
// behind it — 50k lines of one word is nothing, 50k lines of a megabyte each
// is not. The daemon bounds what ARRIVES (M48, M82, the log sink); this is the
// TUI's own retention, which those do not cover.
func TestTranscriptRetentionIsBoundedByBytes(t *testing.T) {
	m := &Model{groupVer: map[string]int{}, vpCache: map[string]vpCacheEntry{}}
	big := strings.Repeat("x", 1<<20) // a 1 MiB line, the parser block cap
	for i := 0; i < (maxLineBytes/len(big))+64; i++ {
		m.addLine(logLine{kind: "response", group: "g", text: big})
	}
	if m.lineBytes > maxLineBytes {
		t.Fatalf("retained %d bytes, budget is %d", m.lineBytes, maxLineBytes)
	}
	if len(m.lines) == 0 {
		t.Fatal("the trim emptied the transcript")
	}
	// The counter matches what is actually held — a drift here would let the
	// budget stop binding.
	sum := 0
	for _, l := range m.lines {
		sum += len(l.text)
	}
	if sum != m.lineBytes {
		t.Fatalf("counter says %d, lines hold %d", m.lineBytes, sum)
	}
	// Newest kept, oldest dropped: a transcript is read from the bottom.
	m.addLine(logLine{kind: "response", group: "g", text: "the newest line"})
	if m.lines[len(m.lines)-1].text != "the newest line" {
		t.Fatal("the newest line was trimmed")
	}
	// And the line-count cap still applies on its own, for many small lines.
	m2 := &Model{groupVer: map[string]int{}, vpCache: map[string]vpCacheEntry{}}
	for i := 0; i < maxLines+100; i++ {
		m2.addLine(logLine{kind: "response", group: "g", text: "short"})
	}
	if len(m2.lines) > maxLines {
		t.Fatalf("retained %d lines, cap is %d", len(m2.lines), maxLines)
	}
	if m2.lineBytes != len(m2.lines)*len("short") {
		t.Fatalf("counter drifted after a count trim: %d", m2.lineBytes)
	}
}

// 2026-09-11 M127: the shared-shell PTY path is deliberately safe — raw guest
// bytes go through the terminal emulator and then scrubVT — but the ended-shell
// ERROR is a guest-authored string concatenated into the hint line and handed
// to lipgloss, which styles content without neutralizing what is in it.
func TestShellErrorTextIsScrubbed(t *testing.T) {
	esc := string(rune(0x1b))
	for _, payload := range []string{
		esc + "]52;c;cGF5bG9hZA==" + string(rune(7)) + "clipboard",
		esc + "[2J" + esc + "[H",
		string(rune(0x9b)) + "C1",
		"bidi" + string(rune(0x202e)) + "flip",
	} {
		got := scrubVT(payload)
		for _, r := range got {
			if r == 0x1b || (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) || isHostileFormat(r) {
				t.Errorf("shell error kept %q from %q: %q", r, payload, got)
			}
		}
	}
	if got := scrubVT("session ended: connection reset"); got != "session ended: connection reset" {
		t.Errorf("a plain shell error was mangled: %q", got)
	}
}

// 2026-09-11 L53 and L62: the TUI's own diagnostic log and its persisted state
// are both operator-facing surfaces that took outside bytes verbatim. dbgWrite
// interpolated daemon, provider and guest text into a line-oriented record
// without escaping the delimiter — and the daemon's sanitizer deliberately
// keeps newlines, so an influenced string forged whole extra records. loadState
// scrubbed only Draft, while `cur` and the session names go straight into the
// live model and are rendered by renderStatusLeft and the empty-conversation
// banner.
func TestPersistedNamesAndDebugLogAreScrubbed(t *testing.T) {
	const esc = "\x1b"
	const bel = "\x07"

	// L53 — one record stays one line, and carries no terminal control.
	got := flattenLogValue("job done\n[ERR] daemon: override accepted" + esc + "]0;pwned" + bel + " tail")
	if strings.ContainsAny(got, "\n\r"+esc+bel) {
		t.Errorf("a debug record carries a break or a control: %q", got)
	}
	for _, want := range []string{"job done", "override accepted", "tail"} {
		if !strings.Contains(got, want) {
			t.Errorf("flattening lost %q: %q", want, got)
		}
	}

	// L62 — a state file under the writable mount cannot put escapes on screen.
	dir := t.TempDir()
	sock := filepath.Join(dir, "koto.sock")
	if err := os.MkdirAll(filepath.Dir(statePath(sock)), 0o700); err != nil {
		t.Fatal(err)
	}
	st := persistedState{
		Cur:      "main" + esc + "]0;pwned" + bel,
		Draft:    "d" + esc + "[2J",
		Sessions: map[string]string{"main" + esc + "[H": "work" + esc + "]0;x" + bel},
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(sock), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadState(sock)
	if strings.ContainsAny(s.Cur, esc+bel) {
		t.Errorf("cur kept a terminal control: %q", s.Cur)
	}
	if strings.ContainsAny(s.Draft, esc+bel) {
		t.Errorf("draft kept a terminal control: %q", s.Draft)
	}
	for k, v := range s.Sessions {
		if strings.ContainsAny(k, esc+bel) || strings.ContainsAny(v, esc+bel) {
			t.Errorf("session %q=%q kept a terminal control", k, v)
		}
	}
	if !strings.HasPrefix(s.Cur, "main") {
		t.Errorf("scrubbing ate the group name: %q", s.Cur)
	}

	// End to end: a restored state must not put an escape into the frame.
	m := newModel(sock, 200000)
	m.width, m.height = 100, 24
	m.groups = map[string]GroupInfo{s.Cur: {Running: true}}
	m.cur = s.Cur
	if f := m.View(); strings.Contains(f, esc+"]0;") || strings.Contains(f, bel) {
		t.Error("a restored group name put an OSC into the rendered frame")
	}
}

// 2026-09-11 L65: the 0600 on the state file's open applies only when that
// call CREATES it. A tui-state.json already at 0644 — copied, restored,
// migrated, or left by an older build — kept that mode through every later
// write, and what is written is the operator's unsubmitted input bar.
func TestStateFilePermissionsAreRepairedOnWrite(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")
	path := statePath(sock)
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	saveState(sock, persistedState{Draft: "unsubmitted secret"})
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state file is mode %04o after a write; the draft is world-readable", st.Mode().Perm())
	}
	// The write still happened.
	if got := loadState(sock); got.Draft != "unsubmitted secret" {
		t.Fatalf("draft not persisted: %q", got.Draft)
	}
}

// 2026-09-11 L164: the 1 MiB read bounds the FILE; nothing bounded what a
// megabyte of JSON can describe — unrestricted strings and a map with no entry
// limit — and loadState runs twice at startup, once for the theme and once
// building the model, with every value then copied into the live model and
// rendered.
func TestPersistedStateContentsAreBounded(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "daemon.sock")
	sessions := map[string]string{}
	for i := 0; i < persistSessionsMax*2; i++ {
		sessions[fmt.Sprintf("g%05d", i)] = "s"
	}
	b, err := json.Marshal(persistedState{
		Cur:      strings.Repeat("c", persistNameMax*4),
		Draft:    strings.Repeat("d", persistDraftMax*4),
		Theme:    strings.Repeat("t", persistNameMax*4),
		Sessions: sessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(sock), b, 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadState(sock)
	if len(got.Draft) > persistDraftMax {
		t.Errorf("draft is %d bytes, past the %d cap", len(got.Draft), persistDraftMax)
	}
	if got.Cur != "" {
		t.Errorf("an oversized group name was kept: %d bytes", len(got.Cur))
	}
	if got.Theme != "" {
		t.Errorf("an oversized theme name was kept: %d bytes", len(got.Theme))
	}
	if len(got.Sessions) > persistSessionsMax {
		t.Errorf("%d sessions kept, past the %d cap", len(got.Sessions), persistSessionsMax)
	}
	// An ordinary state file still round-trips.
	saveState(sock, persistedState{Cur: "main", Draft: "half typed", Sessions: map[string]string{"main": "ops"}})
	back := loadState(sock)
	if back.Cur != "main" || back.Draft != "half typed" || back.Sessions["main"] != "ops" {
		t.Errorf("an ordinary state file was altered: %+v", back)
	}
}
