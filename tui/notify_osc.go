package main

// Desktop notifications: a `notification` event doesn't only draw the in-TUI
// banner, it also asks the *terminal* to raise a real window-manager
// notification, so an operator who isn't looking at the TUI still finds out.
//
// The mechanism is an OSC escape sequence written to stdout — the only channel
// cs_tui has (it runs --network=none with a single socket mounted, so no
// libnotify, no D-Bus, no `notify-send`). Terminals disagree on which OSC
// means "notify", hence the mode table below.
//
//	osc9    ESC ] 9 ; text BEL            iTerm2, WezTerm, Windows Terminal, ghostty
//	osc777  ESC ] 777 ; notify ; t ; b ST foot, urxvt, WezTerm, ghostty
//	osc99   ESC ] 99 ; …chunked… ST       kitty, ghostty
//	bell    BEL only                      universal urgency hint, no text
//
// `auto` (the default) picks one from TERM/TERM_PROGRAM. Override with
// KOTO_TUI_NOTIFY=off|bell|osc9|osc777|osc99|all — `all` emits every form for
// terminals we guessed wrong about (and duplicates on ones that support more
// than one). A multiplexer in between (zellij, tmux) may swallow the sequence;
// that's what `off` and the banner are for.
//
// Severity maps to urgency: "high" additionally emits a bare BEL, which is
// what most window managers turn into the urgency hint / taskbar flash.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"unicode"
)

// notifyOut is the sink for escape sequences — stdout in production, a buffer
// in tests. Writes are single Write calls so a sequence can't interleave with
// a Bubble Tea frame mid-string.
var notifyOut io.Writer = os.Stdout

// osc99Seq numbers OSC-99 notifications; kitty needs a per-notification id to
// join the title and body chunks.
var osc99Seq atomic.Uint64

const (
	bel = "\a"
	esc = "\x1b"
	st  = "\x1b\\"
)

// notifyLimits keep a runaway agent from writing a novel into the WM popup.
const (
	notifyTitleMax = 120
	notifyBodyMax  = 400
)

// notifyModeForStdout is resolveNotifyMode plus the "is anyone there?" check:
// with stdout redirected (a pipe, a log file, `go test`) an escape sequence is
// just garbage in the output, so notifications go off.
func notifyModeForStdout(env func(string) string) string {
	fi, err := os.Stdout.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return "off"
	}
	return resolveNotifyMode(env)
}

// resolveNotifyMode reads the operator override, falling back to terminal
// sniffing. Unknown values fall back to auto rather than silently disabling
// notifications.
func resolveNotifyMode(env func(string) string) string {
	switch strings.ToLower(strings.TrimSpace(env("KOTO_TUI_NOTIFY"))) {
	case "off", "none":
		return "off"
	case "bell":
		return "bell"
	case "osc9":
		return "osc9"
	case "osc777":
		return "osc777"
	case "osc99":
		return "osc99"
	case "all":
		return "all"
	}
	// KOTO_TUI_TERM carries the *host* terminal's TERM: `podman run -t`
	// overwrites TERM with "xterm" inside the container, and we don't want to
	// override that back (it's what bubbletea/lipgloss capability-detect on),
	// so the Makefile forwards the real value under its own name.
	term := env("KOTO_TUI_TERM")
	if term == "" {
		term = env("TERM")
	}
	return autoNotifyMode(term, env("TERM_PROGRAM"))
}

// autoNotifyMode guesses the escape a terminal understands. Note that cs_tui
// only sees TERM/TERM_PROGRAM if the Makefile forwards them into the
// container; with neither set we emit OSC 9, which terminals that don't
// implement it ignore harmlessly.
func autoNotifyMode(term, prog string) string {
	t, p := strings.ToLower(term), strings.ToLower(prog)
	switch {
	case strings.Contains(t, "kitty"):
		return "osc99"
	case strings.Contains(t, "foot"), strings.Contains(t, "rxvt"):
		return "osc777"
	case strings.Contains(p, "ghostty"), strings.Contains(t, "ghostty"):
		return "osc9"
	default:
		return "osc9"
	}
}

// sanitizeNotify flattens control characters (the payload reaches the terminal
// raw — a stray ESC would let a notification body inject its own escape
// sequence) and truncates. C0, DEL, and the C1 range all flatten: on
// C1-interpreting terminals (xterm in UTF-8 mode) U+009C is ST — it would
// terminate the OSC early — and U+009B is CSI. Unicode format characters
// (bidi overrides etc., category Cf) go too, mirroring the daemon's
// sanitizer; this function must stand alone because the payload bypasses the
// renderer's sanitized path. Truncation counts runes, not bytes, so a cut
// never splits a UTF-8 sequence.
func sanitizeNotify(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// notifyEscape renders the escape sequence for one notification. Pure — the
// whole thing is one string so callers emit it in a single Write.
func notifyEscape(mode, sev, group, title, msg string) string {
	if mode == "off" {
		return ""
	}
	title = sanitizeNotify(title, notifyTitleMax)
	msg = sanitizeNotify(msg, notifyBodyMax)
	// The group is the operator's only clue about *which* agent spoke, so it
	// leads the title rather than living in the body.
	full := "koto/" + group
	if title != "" {
		full += ": " + title
	}
	body := msg
	if body == "" {
		body = full
	}

	var b strings.Builder
	if mode == "osc9" || mode == "all" {
		line := full
		if msg != "" {
			line += " — " + msg
		}
		b.WriteString(esc + "]9;" + line + bel)
	}
	if mode == "osc777" || mode == "all" {
		// OSC 777 is semicolon-delimited: a ';' inside title or body would
		// split the fields.
		b.WriteString(esc + "]777;notify;" +
			strings.ReplaceAll(full, ";", ",") + ";" +
			strings.ReplaceAll(body, ";", ",") + st)
	}
	if mode == "osc99" || mode == "all" {
		id := fmt.Sprintf("koto%d", osc99Seq.Add(1))
		b.WriteString(esc + "]99;i=" + id + ":d=0:p=title;" + full + st)
		b.WriteString(esc + "]99;i=" + id + ":d=1:p=body;" + body + st)
	}
	// High severity also rings the bell: window managers turn that into the
	// urgency hint, which is the part an operator notices from another
	// workspace. Normal severity stays silent — the popup is enough.
	if sev == "high" || mode == "bell" {
		b.WriteString(bel)
	}
	return b.String()
}

// emitDesktopNotify writes the sequence for a live notification event.
func (m Model) emitDesktopNotify(sev, group, title, msg string) {
	if s := notifyEscape(m.notifyMode, sev, group, title, msg); s != "" {
		logDbg("notify", "desktop notify: mode=%s sev=%s group=%s title=%q", m.notifyMode, sev, group, title)
		io.WriteString(notifyOut, s)
	}
}
