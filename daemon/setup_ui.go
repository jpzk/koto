package main

// Terminal-dialog primitives for `koto setup`. Deliberately plain: raw ANSI,
// a bufio reader on stdin, no bubbletea. That is not only a dependency
// decision — because the wizard never enters raw mode or an alternate screen,
// it can hand the real tty to an interactive child (`claude auth login`, the
// TUI) without saving a mode of its own.
//
// It does have to save the CHILD's, though: the child enters raw mode, and
// one that exits abnormally leaves it that way for everything after it. See
// ttyGuard (the prevention) and repairTTY (the cure).

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type setupUI struct {
	in       *bufio.Reader
	color    bool
	yes      bool // -y: take defaults, never block on a y/n
	repaired bool // repairTTY has run (once per process)
}

func newSetupUI(assumeYes, noColor bool) *setupUI {
	u := &setupUI{in: bufio.NewReader(os.Stdin), color: !noColor && colorOK(), yes: assumeYes}
	// Up front rather than at the first read, so the notice lands on a line
	// of its own instead of halfway through "choice [1]: ".
	u.repairTTY()
	return u
}

// colorOK: honor NO_COLOR and dumb terminals, and skip styling when stdout is
// redirected (an escape sequence with nobody to interpret it is log noise).
func colorOK() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || os.Getenv("TERM") == "" {
		return false
	}
	_, err := unix.IoctlGetTermios(int(os.Stdout.Fd()), unix.TCGETS)
	return err == nil
}

func isTTY(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

func (u *setupUI) sgr(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *setupUI) bold(s string) string   { return u.sgr("1", s) }
func (u *setupUI) dim(s string) string    { return u.sgr("2", s) }
func (u *setupUI) green(s string) string  { return u.sgr("32", s) }
func (u *setupUI) yellow(s string) string { return u.sgr("33", s) }
func (u *setupUI) red(s string) string    { return u.sgr("31", s) }
func (u *setupUI) cyan(s string) string   { return u.sgr("36", s) }

func (u *setupUI) printf(format string, a ...any) { fmt.Printf(format+"\n", a...) }
func (u *setupUI) blank()                         { fmt.Println() }

// header draws a step banner: ── [3/11] PKI ──────────────
func (u *setupUI) header(n, total int, title string) {
	label := fmt.Sprintf("── [%d/%d] %s ", n, total, title)
	if pad := 72 - len([]rune(label)); pad > 0 {
		label += strings.Repeat("─", pad)
	}
	u.blank()
	fmt.Println(u.bold(u.cyan(label)))
}

// wrap reflows prose to 76 columns so explain text reads as paragraphs
// rather than one long line in a narrow terminal.
func (u *setupUI) prose(s string) {
	for _, para := range strings.Split(strings.TrimSpace(s), "\n\n") {
		words := strings.Fields(para)
		line := ""
		for _, w := range words {
			if line != "" && len(line)+1+len(w) > 76 {
				fmt.Println(line)
				line = ""
			}
			if line == "" {
				line = w
			} else {
				line += " " + w
			}
		}
		if line != "" {
			fmt.Println(line)
		}
		fmt.Println()
	}
}

func (u *setupUI) ok(format string, a ...any) {
	u.printf("%s %s", u.green("✓"), fmt.Sprintf(format, a...))
}
func (u *setupUI) fail(format string, a ...any) {
	u.printf("%s %s", u.red("✗"), fmt.Sprintf(format, a...))
}
func (u *setupUI) warn(format string, a ...any) {
	u.printf("%s %s", u.yellow("!"), fmt.Sprintf(format, a...))
}
func (u *setupUI) info(format string, a ...any) { u.printf("  %s", fmt.Sprintf(format, a...)) }

// hint prints remediation text indented under a failed check.
func (u *setupUI) hint(s string) {
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		fmt.Println("    " + u.yellow(strings.TrimSpace(line)))
	}
}

var errSetupAborted = fmt.Errorf("aborted")

// readLine returns the next line, or errSetupAborted on EOF (piped stdin with
// nothing left — better a clear abort than a silent infinite default).
//
// It reads byte-wise and ends the line on EITHER \n or \r, which is not
// pedantry: a terminal left with ICRNL cleared (by an interactive child that
// died without restoring it — see ttyGuard) delivers Enter as a bare \r, and
// a ReadString('\n') then blocks forever while the user's keystrokes echo as
// ^M and the prompt appears frozen. That was observed. repairTTY fixes the
// terminal we are prompting on; this makes the read itself survive a
// terminal it did not get to fix.
func (u *setupUI) readLine() (string, error) {
	u.repairTTY()
	var b strings.Builder
	for {
		c, err := u.in.ReadByte()
		if err == io.EOF {
			if strings.TrimSpace(b.String()) == "" {
				u.blank()
				return "", errSetupAborted
			}
			break
		} else if err != nil {
			return "", err
		}
		if c == '\n' {
			break
		}
		if c == '\r' {
			// CRLF from a piped file: swallow the LF so it does not read as
			// an empty answer to the next prompt. Buffered() keeps this from
			// blocking on a tty, where the LF never comes.
			if u.in.Buffered() > 0 {
				if p, err := u.in.Peek(1); err == nil && p[0] == '\n' {
					_, _ = u.in.ReadByte()
				}
			}
			break
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String()), nil
}

// repairTTY puts stdin back into a line-editable state before the first
// prompt, when something else left it otherwise.
//
// The wizard hands the real terminal to interactive children (`claude auth
// login`, the TUI). Those enter raw mode, and one that dies without restoring
// leaves the line discipline broken for every program that follows —
// including the next run of this command, whose prompt then swallows every
// keystroke. ttyGuard stops us causing that; this repairs the terminal when
// something already did, because the alternative is an operator staring at a
// prompt that cannot be answered and no clue that `stty sane` is the way out.
//
// Deliberately narrow: only the three input flags that make a prompt
// answerable, only when at least one is missing, and only once. It does NOT
// restore what it found on exit — the state it repairs is damage, and handing
// damage back would defeat the point.
func (u *setupUI) repairTTY() {
	if u.repaired {
		return
	}
	u.repaired = true
	fd := int(os.Stdin.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return // not a terminal: nothing to repair, nothing to break
	}
	if t.Iflag&unix.ICRNL != 0 && t.Lflag&unix.ICANON != 0 && t.Lflag&unix.ECHO != 0 {
		return
	}
	fixed := *t
	fixed.Iflag |= unix.ICRNL
	fixed.Lflag |= unix.ICANON | unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &fixed); err != nil {
		return
	}
	u.warn("terminal input was left in raw mode by an earlier program — repaired")
}

// ttyGuard snapshots the terminal state before an interactive child gets the
// tty, and returns the restore. Every caller that sets cmd.Stdin = os.Stdin
// on a program that draws its own UI needs it: the child owns the terminal
// while it runs, and one that exits abnormally (killed, crashed, ^C at the
// wrong moment) leaves raw mode behind for whatever runs next. Cheap
// insurance — two ioctls — against a wedged terminal.
//
// A no-op when stdin is not a tty. Safe to call twice.
func ttyGuard() (restore func()) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	done := false
	return func() {
		if done {
			return
		}
		done = true
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
	}
}

// yesno asks a y/n question. Under -y it answers with the default and says so.
func (u *setupUI) yesno(q string, def bool) bool {
	choices := "[y/N]"
	if def {
		choices = "[Y/n]"
	}
	if u.yes {
		u.printf("%s %s %s", u.bold(q), choices, u.dim("(-y)"))
		return def
	}
	for {
		fmt.Printf("%s %s ", u.bold(q), choices)
		ans, err := u.readLine()
		if err != nil {
			return def
		}
		switch strings.ToLower(ans) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
}

// choice presents a numbered menu and returns the chosen index.
func (u *setupUI) choice(q string, opts []string, def int) int {
	u.printf("%s", u.bold(q))
	for i, o := range opts {
		mark := " "
		if i == def {
			mark = "*"
		}
		u.printf("  %s %d) %s", mark, i+1, o)
	}
	if u.yes {
		u.printf("  → %d (-y)", def+1)
		return def
	}
	for {
		fmt.Printf("choice [%d]: ", def+1)
		ans, err := u.readLine()
		if err != nil {
			return def
		}
		if ans == "" {
			return def
		}
		if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(opts) {
			return n - 1
		}
	}
}

// text prompts for a line, returning def when the user just hits enter.
func (u *setupUI) text(q, def string) string {
	if u.yes {
		return def
	}
	if def != "" {
		fmt.Printf("%s [%s]: ", u.bold(q), def)
	} else {
		fmt.Printf("%s: ", u.bold(q))
	}
	ans, err := u.readLine()
	if err != nil || ans == "" {
		return def
	}
	return ans
}

// secret prompts with terminal echo disabled (API keys). Falls back to a
// visible prompt when stdin isn't a tty, with a warning.
func (u *setupUI) secret(q string) (string, error) {
	// Before the snapshot, never after: repairTTY turns ECHO back ON, so
	// letting readLine call it below would print the key being typed.
	u.repairTTY()
	fd := int(os.Stdin.Fd())
	termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		u.warn("stdin is not a terminal — input will be visible")
		fmt.Printf("%s: ", u.bold(q))
		return u.readLine()
	}
	raw := *termios
	raw.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return "", err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, termios) }()
	fmt.Printf("%s: ", u.bold(q))
	s, err := u.readLine()
	u.blank() // the user's Enter was swallowed with the echo
	return s, err
}

// prefixWriter indents streamed subprocess output so it reads as a quoted
// block rather than as the wizard's own voice. Buffers partial lines; Flush
// emits a trailing fragment (progress bars, prompts without a newline).
type prefixWriter struct {
	ui  *setupUI
	buf []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *prefixWriter) Flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

func (w *prefixWriter) emit(line string) {
	line = strings.TrimRight(line, "\r")
	fmt.Println(w.ui.dim("  │ ") + line)
}
