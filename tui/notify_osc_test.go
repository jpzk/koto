package main

// Desktop (window-manager) notifications: mode resolution, escape shape,
// payload sanitization, and the model wiring that decides *when* one is
// emitted (live only, never on history replay).

import (
	"strings"
	"testing"
)

func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestResolveNotifyMode(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"KOTO_TUI_NOTIFY": "off"}, "off"},
		{map[string]string{"KOTO_TUI_NOTIFY": "OSC777"}, "osc777"},
		{map[string]string{"KOTO_TUI_NOTIFY": " all "}, "all"},
		// Unknown override falls back to sniffing, not to silence.
		{map[string]string{"KOTO_TUI_NOTIFY": "wat", "TERM": "xterm-kitty"}, "osc99"},
		{map[string]string{"TERM": "foot"}, "osc777"},
		// The host TERM (forwarded past podman's TERM=xterm) wins.
		{map[string]string{"KOTO_TUI_TERM": "xterm-kitty", "TERM": "xterm"}, "osc99"},
		{map[string]string{"TERM": "rxvt-unicode-256color"}, "osc777"},
		{map[string]string{"TERM_PROGRAM": "ghostty"}, "osc9"},
		{map[string]string{}, "osc9"},
	}
	for _, c := range cases {
		if got := resolveNotifyMode(envMap(c.env)); got != c.want {
			t.Errorf("resolveNotifyMode(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestNotifyEscapeShapes(t *testing.T) {
	s := notifyEscape("osc9", "normal", "main", "Build done", "12 tests passed")
	if !strings.HasPrefix(s, "\x1b]9;") || !strings.HasSuffix(s, "\a") {
		t.Fatalf("osc9 framing: %q", s)
	}
	if !strings.Contains(s, "koto/main: Build done") || !strings.Contains(s, "12 tests passed") {
		t.Fatalf("osc9 payload: %q", s)
	}
	// Normal severity must not ring the bell beyond the OSC 9 terminator.
	if strings.Count(s, "\a") != 1 {
		t.Fatalf("normal severity rang the bell: %q", s)
	}

	s = notifyEscape("osc777", "high", "ghost", "disk", "94% full")
	if !strings.HasPrefix(s, "\x1b]777;notify;koto/ghost: disk;94% full\x1b\\") {
		t.Fatalf("osc777 framing: %q", s)
	}
	if !strings.HasSuffix(s, "\a") {
		t.Fatal("high severity must append a BEL for the WM urgency hint")
	}

	s = notifyEscape("osc99", "normal", "main", "T", "B")
	if strings.Count(s, "\x1b]99;") != 2 || !strings.Contains(s, ":d=0:p=title;") ||
		!strings.Contains(s, ":d=1:p=body;") {
		t.Fatalf("osc99 chunking: %q", s)
	}

	if got := notifyEscape("off", "high", "main", "T", "B"); got != "" {
		t.Fatalf("off mode emitted %q", got)
	}
	if got := notifyEscape("bell", "normal", "main", "T", "B"); got != "\a" {
		t.Fatalf("bell mode: %q", got)
	}
	if all := notifyEscape("all", "normal", "main", "T", "B"); !strings.Contains(all, "]9;") ||
		!strings.Contains(all, "]777;") || !strings.Contains(all, "]99;") {
		t.Fatalf("all mode missing a flavor: %q", all)
	}
}

// A notification body is attacker-influenceable (an agent writes it), and it
// reaches the terminal outside the renderer's sanitizer — an embedded ESC
// would let it emit escapes of its own.
func TestNotifyEscapeSanitizes(t *testing.T) {
	s := notifyEscape("osc777", "normal", "main", "a;b\x1b]0;pwn\a", "line1\nline2\x07")
	body := strings.TrimPrefix(s, "\x1b]777;notify;")
	if strings.Contains(body, "\x1b]0;") || strings.ContainsAny(body, "\a\n") {
		t.Fatalf("control chars survived: %q", s)
	}
	// ';' inside the fields would split OSC 777's arguments.
	if strings.Count(s, ";") != 3 {
		t.Fatalf("unescaped semicolon in osc777 fields: %q", s)
	}

	long := notifyEscape("osc9", "normal", "main", strings.Repeat("x", 500), "")
	if len(long) > notifyTitleMax+64 {
		t.Fatalf("title not truncated: %d bytes", len(long))
	}
}

func TestModelEmitsDesktopNotifyLiveOnly(t *testing.T) {
	var buf strings.Builder
	old := notifyOut
	notifyOut = &buf
	defer func() { notifyOut = old }()

	m := notifyModel(t)
	m.notifyMode = "osc9"

	hist := notifyEv("main", "high", "T1", "M1")
	hist.Historical = true
	m = sendNotify(t, m, hist)
	if buf.Len() != 0 {
		t.Fatalf("history replay poked the window manager: %q", buf.String())
	}

	m = sendNotify(t, m, notifyEv("main", "normal", "T2", "M2"))
	if !strings.Contains(buf.String(), "T2") {
		t.Fatalf("live notification not emitted: %q", buf.String())
	}
}
