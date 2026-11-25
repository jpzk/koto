package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// recordOpener swaps openURLFunc for a recorder for the test's duration.
func recordOpener(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := openURLFunc
	openURLFunc = func(u string) error { got = append(got, u); return nil }
	t.Cleanup(func() { openURLFunc = prev })
	return &got
}

// linkModel is the plain chat view (input focus, no tree) with one response
// block rendered through the real pipeline: addLine → refreshLog → glamour →
// scrubVT → m.vpLines.
func linkModel(t *testing.T, width int, md string) Model {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = width, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	m.focus = focusInput
	m.vpReady = true
	m.resizeViewport()
	m.addLine(logLine{kind: "response", group: "main", text: md})
	m.refreshLog()
	return m
}

// setRows bypasses the renderer: the viewport shows exactly rows. Used for
// the hostile cases the real pipeline would already have scrubbed, to prove
// the click path holds on its own.
func setRows(m *Model, rows ...string) {
	m.vp.SetContent(strings.Join(rows, "\n"))
	m.vpLines = rows
	m.vp.GotoTop()
}

// cellOf returns the screen (x, y) of the k-th cell of the first visible
// occurrence of needle in the chat transcript.
func cellOf(t *testing.T, m *Model, needle string, k int) (int, int) {
	t.Helper()
	x0, ok := m.chatTextOrigin()
	if !ok {
		t.Fatalf("chat transcript is not a link surface (focus=%v)", m.focus)
	}
	off := m.vp.YOffset
	for i := off; i < min(len(m.vpLines), off+m.chatRows()); i++ {
		plain := ansi.Strip(m.vpLines[i])
		if j := strings.Index(plain, needle); j >= 0 {
			return x0 + ansi.StringWidth(plain[:j]) + k, 1 + i - off
		}
	}
	t.Fatalf("%q not on screen; rows:\n%s", needle, ansi.Strip(strings.Join(m.vpLines, "\n")))
	return 0, 0
}

// lclick delivers a left press through Update (the real routing: shell
// forwarding first, then handleLeftClickCmd) and runs whatever command
// comes back, feeding a urlOpenedMsg back in like the runtime would.
func lclick(t *testing.T, m Model, x, y int) Model {
	t.Helper()
	nm, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if cmd != nil {
		if msg, ok := cmd().(urlOpenedMsg); ok {
			nm, _ = m.Update(msg)
			m = nm.(Model)
		}
	}
	return m
}

func wantOpened(t *testing.T, got *[]string, want ...string) {
	t.Helper()
	if len(*got) != len(want) {
		t.Fatalf("opener calls = %q, want %q", *got, want)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Fatalf("opener call %d = %q, want %q", i, (*got)[i], want[i])
		}
	}
}

// (a) a click on an http(s) URL in rendered output opens exactly that URL,
// and the status bar says so.
func TestClickURLInMessageOpensIt(t *testing.T) {
	got := recordOpener(t)
	m := linkModel(t, 100, "See https://example.com/docs?a=1&b=2 for details, or (http://plain.example.org/x).")
	x, y := cellOf(t, &m, "https://example.com", 10)
	m = lclick(t, m, x, y)
	wantOpened(t, got, "https://example.com/docs?a=1&b=2")
	if m.linkNote != "opened example.com" || m.linkNoteErr {
		t.Fatalf("status note = %q (err=%v), want %q", m.linkNote, m.linkNoteErr, "opened example.com")
	}
	if !strings.Contains(ansi.Strip(m.renderStatusBar("")), "opened example.com") {
		t.Fatal("status bar does not show the opened host")
	}
	// Parenthesised + trailing punctuation: the ')' and '.' are prose.
	x, y = cellOf(t, &m, "http://plain", 3)
	m = lclick(t, m, x, y)
	wantOpened(t, got, "https://example.com/docs?a=1&b=2", "http://plain.example.org/x")
	// First and last cell of the URL both count.
	*got = nil
	x, y = cellOf(t, &m, "https://example.com", 0)
	m = lclick(t, m, x, y)
	x, y = cellOf(t, &m, "b=2", 2)
	lclick(t, m, x, y)
	wantOpened(t, got, "https://example.com/docs?a=1&b=2", "https://example.com/docs?a=1&b=2")
}

// (a) a long URL that glamour soft-wraps over several rows opens whole,
// whichever row is clicked.
func TestClickSoftWrappedURLOpensWholeURL(t *testing.T) {
	got := recordOpener(t)
	long := "https://github.com/jpzk/koto/pull/12345/files/some-very-long-path-segment/that/keeps/going/and-going?query=abc123&x=1"
	m := linkModel(t, 60, "Long: "+long+" end of line words here.\n\nnext paragraph")
	var rows []int
	for i, ln := range m.vpLines {
		p := ansi.Strip(ln)
		if strings.Contains(p, "github") || strings.Contains(p, "some-") || strings.Contains(p, "query=") ||
			strings.Contains(p, "path-segment") || strings.Contains(p, "and-") {
			rows = append(rows, i)
		}
	}
	if len(rows) < 2 {
		t.Fatalf("URL was not wrapped, test is vacuous; rows:\n%s", ansi.Strip(strings.Join(m.vpLines, "\n")))
	}
	for _, needle := range []string{"https://github", "query=abc1", "path-segment"} {
		x, y := cellOf(t, &m, needle, 2)
		m = lclick(t, m, x, y)
	}
	wantOpened(t, got, long, long, long)
	// The prose after the link on its last row is not part of it.
	x, y := cellOf(t, &m, "end of line", 1)
	lclick(t, m, x, y)
	wantOpened(t, got, long, long, long)
}

// (a) raw text hard-wrapped at the column limit (wrapLine) joins too, while
// a URL ending a short row never swallows the next row's first word.
func TestClickHardWrappedURL(t *testing.T) {
	got := recordOpener(t)
	m := linkModel(t, 100, "x")
	cols := m.logContentCols()
	long := "https://example.com/" + strings.Repeat("a", cols+15)
	rows := wrapLine(long, cols)
	if len(rows) < 2 {
		t.Fatal("not wrapped")
	}
	setRows(&m, append(rows, "https://short.example/p", "nextword here")...)
	x, y := cellOf(t, &m, "aaaa", 0)
	m = lclick(t, m, x, y+1) // the continuation row
	wantOpened(t, got, long)
	x, y = cellOf(t, &m, "short.example", 0)
	lclick(t, m, x, y)
	wantOpened(t, got, long, "https://short.example/p")
}

// (b) non-URL text and non-http(s) schemes never reach the opener.
func TestClickNonURLDoesNotOpen(t *testing.T) {
	got := recordOpener(t)
	m := linkModel(t, 100, "hello world\n\njavascript:alert(document.cookie)\n\nfile:///etc/passwd\n\nftp://ftp.example.com/pub\n\ndata:text/html,PHNjcmlwdD4=\n\nhttps:/broken and https:// alone")
	for _, needle := range []string{"hello", "world", "javascript:", "alert", "file:///", "passwd", "ftp://", "ftp.example", "data:text", "PHNjcmlwdD4", "https:/broken", "https:// alone"} {
		x, y := cellOf(t, &m, needle, 1)
		m = lclick(t, m, x, y)
	}
	// Blank cells / past the text / the status bar row.
	m = lclick(t, m, 1, 0)
	x, y := cellOf(t, &m, "hello", 0)
	m = lclick(t, m, x+60, y)
	wantOpened(t, got)
	if m.linkNote != "" && !m.linkNoteErr {
		t.Fatalf("unexpected success note %q", m.linkNote)
	}
}

// (b) control characters, bidi/zero-width format runes, credentials,
// spaces and over-long URLs are refused — at the click and at openURLCmd.
func TestClickURLWithControlCharsDoesNotOpen(t *testing.T) {
	got := recordOpener(t)
	m := linkModel(t, 100, "x")
	setRows(&m,
		"go https://exa‮mple.com/x now",
		"go https://zero​width.example/x now",
		"go https://trusted.example@evil.example/ now",
		"go https://ok.example/\x1b[31mred\x1b[0m now", // SGR mid-URL is styling, not text: this one IS fine
	)
	for _, needle := range []string{"https://exa", "https://zero", "https://trusted"} {
		x, y := cellOf(t, &m, needle, 2)
		m = lclick(t, m, x, y)
		if !m.linkNoteErr {
			t.Fatalf("%s: no error note after a refused link", needle)
		}
	}
	wantOpened(t, got)
	x, y := cellOf(t, &m, "https://ok", 2)
	lclick(t, m, x, y)
	wantOpened(t, got, "https://ok.example/red")

	*got = nil
	for _, bad := range []string{
		"https://a.example/\x07", "https://a.example/\x1b]52;c;eA==\x07", "https://a.example/ x",
		"https://a.example/\tx", "https://a.example/\u0085", "https://a.example/ ",
		"javascript:alert(1)", "file:///etc/passwd", "ftp://a.example/", "data:text/html,x",
		"https:///nohost", "http://", "HTTPS//a.example", "https://" + strings.Repeat("a", maxClickURLLen),
		"https://user:pw@a.example/",
	} {
		msg := openURLCmd(bad)().(urlOpenedMsg)
		if msg.err == nil {
			t.Errorf("openURLCmd(%q) accepted", bad)
		}
	}
	wantOpened(t, got)
	if msg := openURLCmd("HTTPS://A.example/Path")().(urlOpenedMsg); msg.err != nil || msg.host != "A.example" {
		t.Fatalf("upper-case scheme: %+v", msg)
	}
	wantOpened(t, got, "HTTPS://A.example/Path")
}

// (c) a markdown link opens only what is visibly on screen: clicking the
// link TEXT opens nothing; the href opens only where glamour prints it, and
// an OSC 8 hyperlink's hidden target is never read.
func TestClickMarkdownLinkOpensOnlyVisibleURL(t *testing.T) {
	got := recordOpener(t)
	m := linkModel(t, 100, "Read [the guide](https://hidden.example.org/secret) and [https://shown.example/](https://other.example/evil) now.")
	x, y := cellOf(t, &m, "the guide", 2)
	m = lclick(t, m, x, y)
	wantOpened(t, got)
	// The link text that LOOKS like a URL opens what it says, never the href
	// it points at.
	x, y = cellOf(t, &m, "https://shown", 3)
	m = lclick(t, m, x, y)
	wantOpened(t, got, "https://shown.example/")
	// glamour prints the href after the text — on screen, so clickable as
	// exactly what is printed.
	visible := strings.Contains(ansi.Strip(strings.Join(m.vpLines, "\n")), "https://hidden.example.org/secret")
	if visible {
		x, y = cellOf(t, &m, "hidden.example", 1)
		m = lclick(t, m, x, y)
		wantOpened(t, got, "https://shown.example/", "https://hidden.example.org/secret")
	}

	*got = nil
	setRows(&m, "see \x1b]8;;https://hidden.example/osc\x1b\\label text\x1b]8;;\x1b\\ end")
	x, y = cellOf(t, &m, "label", 1)
	lclick(t, m, x, y)
	wantOpened(t, got)
	if s := lineCells(m.vpLines[0]); strings.Contains(cellsText(s), "hidden") {
		t.Fatal("OSC 8 target leaked into the visible cells")
	}
}

// (c) a URL wrapped past the bottom of the pane is refused rather than
// opened truncated or completed from rows the user can't see.
func TestClickURLCutByViewportIsRefused(t *testing.T) {
	got := recordOpener(t)
	m := linkModel(t, 100, "x")
	cols := m.logContentCols()
	long := "https://example.com/" + strings.Repeat("b", cols+5)
	rows := make([]string, 0, m.chatRows()+2)
	for len(rows) < m.chatRows()-1 {
		rows = append(rows, "filler")
	}
	rows = append(rows, wrapLine(long, cols)...)
	setRows(&m, rows...)
	m.vp.GotoTop()
	x, y := cellOf(t, &m, "https://example", 2)
	m = lclick(t, m, x, y)
	wantOpened(t, got)
	if !m.linkNoteErr || !strings.Contains(m.linkNote, "cut off") {
		t.Fatalf("note = %q, want a cut-off refusal", m.linkNote)
	}
	// Scrolled so the whole link is on screen: opens.
	m.vp.GotoBottom()
	x, y = cellOf(t, &m, "https://example", 2)
	lclick(t, m, x, y)
	wantOpened(t, got, long)
}

// A URL clipped at the pane's right edge (an unbreakable run glamour lets
// overflow; joinCols cuts it) is refused, not opened truncated.
func TestClickURLClippedAtPaneEdgeIsRefused(t *testing.T) {
	got := recordOpener(t)
	m := linkModel(t, 60, "x")
	long := "https://example.com/" + strings.Repeat("c", m.vp.Width+10)
	setRows(&m, "  "+long, "  https://fits.example/ok")
	x, y := cellOf(t, &m, "https://example", 2)
	m = lclick(t, m, x, y)
	wantOpened(t, got)
	if !m.linkNoteErr {
		t.Fatal("no refusal note for a clipped link")
	}
	x, y = cellOf(t, &m, "https://fits", 2)
	lclick(t, m, x, y)
	wantOpened(t, got, "https://fits.example/ok")
}

// shellLinkModel is a split view (wide terminal) with the shared shell open,
// a URL on the guest's screen, and a URL in the chat transcript beside it.
func shellLinkModel(t *testing.T, focus focusZone) (Model, <-chan string) {
	t.Helper()
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.cur = "main"
	term := vt.NewEmulator(80, 20)
	t.Cleanup(func() { closeEmulator(term) })
	m.shell = &shellSession{term: term, group: "main", session: "koto-shell", cols: 80, rows: 20}
	m.shellOpen = true
	m.preShellFocus = focusInput
	m.focus = focus
	m.vpReady = true
	m.resizeViewport()
	m.addLine(logLine{kind: "response", group: "main", text: "chat link https://chat.example/x here"})
	m.refreshLog()
	ch := make(chan string, 16)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				ch <- string(buf[:n])
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	// Guest: a URL on screen, mouse reporting on (tmux `mouse on`).
	_, _ = term.Write([]byte("https://guest.example/in-the-pty\r\n\x1b[?1000h\x1b[?1006h"))
	return m, ch
}

func expectForwarded(t *testing.T, drained <-chan string, want string) {
	t.Helper()
	select {
	case got := <-drained:
		if got != want {
			t.Fatalf("guest got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("click was not forwarded to the guest")
	}
}

// (d) shell pane FOCUSED, fullscreen: a left click on a URL in the pty grid
// goes to the guest as mouse reporting and never to the opener.
func TestShellFullscreenClickNeverOpens(t *testing.T) {
	got := recordOpener(t)
	m, drained := shellMouseModel(t)
	m.vpReady = true
	m.resizeViewport()
	setRows(&m, "https://chat.example/behind-the-pane")
	_, _ = m.shell.term.Write([]byte("https://guest.example/x\r\n\x1b[?1000h\x1b[?1006h"))
	ox, oy := m.shellMouseOrigin()
	m = lclick(t, m, ox+3, oy) // on "ps://guest…", grid cell (3,0)
	expectForwarded(t, drained, "\x1b[<0;4;1M")
	// Anywhere else on screen while the pty is focused: still no opener.
	for _, p := range [][2]int{{1, 1}, {ox + 3, oy + 1}, {0, 0}} {
		m = lclick(t, m, p[0], p[1])
	}
	wantOpened(t, got)
	if m.focus != focusShell {
		t.Fatalf("focus = %v, want focusShell", m.focus)
	}
}

// (d) split view, shell FOCUSED: an in-grid click is forwarded exactly as
// before and opens nothing; the chat-half click keeps its old meaning
// (detach focus to chat) and opens nothing either.
func TestShellSplitFocusedClickNeverOpens(t *testing.T) {
	got := recordOpener(t)
	m, drained := shellLinkModel(t, focusShell)
	if !m.shellSplitVisible() {
		t.Fatal("split not visible")
	}
	ox, oy := m.shellMouseOrigin()
	m = lclick(t, m, ox+10, oy)
	expectForwarded(t, drained, "\x1b[<0;11;1M")
	wantOpened(t, got)
	if m.focus != focusShell {
		t.Fatalf("in-grid click moved focus to %v", m.focus)
	}
	// The chat half's URL, clicked while the pty owns focus: detaches, no open.
	x, y := 0, 0
	for i, ln := range m.vpLines {
		if j := strings.Index(ansi.Strip(ln), "https://chat"); j >= 0 {
			x, y = 1+ansi.StringWidth(ansi.Strip(ln)[:j])+2, 1+i-m.vp.YOffset
		}
	}
	if y == 0 {
		t.Fatal("chat URL not on screen")
	}
	m = lclick(t, m, x, y)
	wantOpened(t, got)
	if m.focus == focusShell {
		t.Fatal("chat-half click no longer detaches from the pty")
	}
}

// (d) split view, shell UNFOCUSED: an in-grid click focuses the pane (as
// before) and never opens; the transcript beside it is a link surface.
func TestShellSplitUnfocusedGridClickFocusesNotOpens(t *testing.T) {
	got := recordOpener(t)
	m, _ := shellLinkModel(t, focusInput)
	ox, oy := m.shellMouseOrigin()
	m = lclick(t, m, ox+3, oy)
	wantOpened(t, got)
	if m.focus != focusShell {
		t.Fatalf("grid click: focus = %v, want focusShell", m.focus)
	}
	m.focus = focusInput
	m.resizeViewport()
	x, y := cellOf(t, &m, "https://chat", 2)
	if x >= ox {
		t.Fatalf("chat URL at x=%d is inside the grid (origin %d)", x, ox)
	}
	lclick(t, m, x, y)
	wantOpened(t, got, "https://chat.example/x")
}

// (e) the launcher is exec'd with the URL as ONE argv element: no shell, no
// $BROWSER, metacharacters inert.
func TestOpenerUsesExecWithoutShell(t *testing.T) {
	t.Setenv("BROWSER", "sh -c 'touch /tmp/pwned'")
	u := "https://a.example/$(id);`id`|id&x='\"$BROWSER"
	for goos, bin := range map[string]string{"linux": "xdg-open", "darwin": "open", "freebsd": "xdg-open"} {
		argv, err := openerArgv(goos, u)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if len(argv) != 2 || argv[0] != bin || argv[1] != u {
			t.Fatalf("%s argv = %q, want [%s <url>]", goos, argv, bin)
		}
		cmd, err := buildOpenCmd(goos, u)
		if err != nil {
			t.Fatal(err)
		}
		if len(cmd.Args) != 2 || cmd.Args[0] != bin || cmd.Args[1] != u {
			t.Fatalf("%s exec args = %q", goos, cmd.Args)
		}
		for _, a := range cmd.Args {
			if a == "sh" || a == "-c" || a == "bash" || strings.Contains(a, "touch") {
				t.Fatalf("%s: shell in argv %q", goos, cmd.Args)
			}
		}
		if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
			t.Fatal("launcher must not inherit the TUI's terminal")
		}
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
			t.Fatal("launcher must be detached into its own session")
		}
	}
	if _, err := openerArgv("windows", u); err == nil {
		t.Fatal("unknown platform must not guess a launcher")
	}
	// The production opener is execOpenURL (exec of openerArgv), not a shell
	// string.
	if reflect.ValueOf(openURLFunc).Pointer() != reflect.ValueOf(execOpenURL).Pointer() {
		t.Fatal("default opener is not execOpenURL")
	}
}

// A failing launcher surfaces in the status line, it does not panic or block.
func TestOpenerErrorShownInStatus(t *testing.T) {
	prev := openURLFunc
	openURLFunc = func(string) error { return errors.New("xdg-open: exec: not found") }
	t.Cleanup(func() { openURLFunc = prev })
	m := linkModel(t, 100, "go https://example.net/x")
	x, y := cellOf(t, &m, "https://example", 1)
	m = lclick(t, m, x, y)
	if !m.linkNoteErr || !strings.Contains(m.linkNote, "not found") {
		t.Fatalf("note = %q err=%v", m.linkNote, m.linkNoteErr)
	}
	nm, _ := m.Update(linkNoteClearMsg{seq: m.linkNoteSeq})
	if nm.(Model).linkNote != "" {
		t.Fatal("note not cleared by its timer")
	}
}

// The click is handled by koto, so OSC 8 and OSC 52 keep being stripped from
// model output (criterion: the scrub is not relaxed for links).
func TestScrubVTStillStripsOSC8And52(t *testing.T) {
	in := "a \x1b]8;;https://evil.example\x1b\\link\x1b]8;;\x1b\\ b \x1b]52;c;cHduZWQ=\x07 c \x1b]8;;https://x\alink2\x1b]8;;\a"
	out := scrubVT(in)
	if strings.Contains(out, "]8;") || strings.Contains(out, "]52;") || strings.Contains(out, "evil.example") {
		t.Fatalf("scrubVT kept OSC: %q", out)
	}
	if ansi.Strip(out) != "a link b  c link2" {
		t.Fatalf("visible text changed: %q", ansi.Strip(out))
	}
	r := renderMarkdown("hi &#27;]8;;https://evil.example&#27;\\x&#27;]8;;&#27;\\ &#27;]52;c;eA==&#7;", 60)
	if strings.Contains(r, "\x1b]") {
		t.Fatalf("rendered markdown carries an OSC: %q", r)
	}
}

// (e) end to end through the real exec path: a fake xdg-open on PATH records
// the argv it was started with — exactly one argument, the URL verbatim,
// metacharacters untouched (no shell ever saw them).
func TestExecOpenURLPassesSingleArgv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake opener is xdg-open")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$#\" \"$@\" > " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, "xdg-open"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BROWSER", "touch "+filepath.Join(dir, "pwned"))
	u := "https://a.example/p?q=$(touch%20x);`id`&r='\"|;"
	if err := execOpenURL(u); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			got = b
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if want := "1\n" + u + "\n"; string(got) != want {
		t.Fatalf("opener argv = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("$BROWSER was evaluated")
	}
	// Missing launcher: the error comes back (status line), no panic.
	t.Setenv("PATH", t.TempDir())
	if err := execOpenURL("https://a.example/"); err == nil {
		t.Fatal("missing xdg-open reported success")
	}
}
