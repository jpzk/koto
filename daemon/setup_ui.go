package main

// Terminal-dialog primitives for `koto setup`. Deliberately plain: raw ANSI,
// a bufio reader on stdin, no bubbletea. That is not only a dependency
// decision — because the wizard never enters raw mode or an alternate screen,
// it can hand the real tty to an interactive child (`claude auth login` under
// `podman run -it`) with nothing to save or restore.

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
	in    *bufio.Reader
	color bool
	yes   bool // -y: take defaults, never block on a y/n
}

func newSetupUI(assumeYes, noColor bool) *setupUI {
	return &setupUI{in: bufio.NewReader(os.Stdin), color: !noColor && colorOK(), yes: assumeYes}
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

func (u *setupUI) ok(format string, a ...any)   { u.printf("%s %s", u.green("✓"), fmt.Sprintf(format, a...)) }
func (u *setupUI) fail(format string, a ...any) { u.printf("%s %s", u.red("✗"), fmt.Sprintf(format, a...)) }
func (u *setupUI) warn(format string, a ...any) { u.printf("%s %s", u.yellow("!"), fmt.Sprintf(format, a...)) }
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
func (u *setupUI) readLine() (string, error) {
	s, err := u.in.ReadString('\n')
	if err == io.EOF && strings.TrimSpace(s) == "" {
		u.blank()
		return "", errSetupAborted
	} else if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(s), nil
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
