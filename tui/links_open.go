package main

// links_open.go — validating and launching a clicked URL (see links.go for
// the click → URL mapping). The URL is text the model wrote, so it is treated
// as hostile all the way to the exec: only http(s), no control characters or
// whitespace, a length cap, a real net/url parse, and the launcher gets it as
// ONE argv element — no shell, no $BROWSER string, nothing that re-tokenises
// or interprets it.

import (
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// maxClickURLLen bounds what a click will hand to the browser launcher.
const maxClickURLLen = 2048

// validateClickURL accepts s only when it is a plain absolute http(s) URL
// with a host. It returns the parsed URL for display (host); callers open
// the ORIGINAL string s, never a re-serialisation, so what is opened is
// byte-for-byte what was on screen.
func validateClickURL(s string) (*url.URL, error) {
	if s == "" {
		return nil, errors.New("empty url")
	}
	if len(s) > maxClickURLLen {
		return nil, errors.New("url too long")
	}
	for _, r := range s {
		// C0, DEL, C1, any whitespace, and format characters (bidi
		// overrides, zero-width joiners) — none belong in a URL a
		// person can read off the screen.
		if r < 0x21 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
			unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return nil, errors.New("url has control or space characters")
		}
	}
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return nil, errors.New("only http and https links open")
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("unparseable url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("only http and https links open")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("url has no host")
	}
	if u.User != nil {
		// https://trusted.example@evil.example/ reads as the first host
		// and goes to the second.
		return nil, errors.New("url carries credentials")
	}
	return u, nil
}

// openerArgv is the launcher command for goos: the platform opener plus the
// URL as a single trailing argument. No shell is ever involved.
func openerArgv(goos, u string) ([]string, error) {
	switch goos {
	case "darwin":
		return []string{"open", u}, nil
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		return []string{"xdg-open", u}, nil
	}
	return nil, errors.New("no url opener for " + goos)
}

// buildOpenCmd turns openerArgv into an exec.Cmd: argv only, stdio detached
// (nil → /dev/null, so a chatty launcher can't scribble on the TUI's
// terminal), and its own session so a ctrl+c in the TUI doesn't reach it.
func buildOpenCmd(goos, u string) (*exec.Cmd, error) {
	argv, err := openerArgv(goos, u)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

// openWait is how long execOpenURL waits for the launcher to fail fast
// (missing binary, no handler) before calling the open a success. xdg-open
// can stay alive as long as the browser it started, so it is never waited
// on past this — a reaper goroutine collects it.
const openWait = 1500 * time.Millisecond

// execOpenURL launches the platform opener for u (already validated).
func execOpenURL(u string) error {
	cmd, err := buildOpenCmd(runtime.GOOS, u)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return errors.New(cmd.Args[0] + ": " + err.Error())
		}
	case <-time.After(openWait):
	}
	return nil
}

// openURLFunc is the launcher seam; tests swap it for a recorder.
var openURLFunc = execOpenURL

// urlOpenedMsg reports the launcher's outcome back to Update.
type urlOpenedMsg struct {
	host string
	err  error
}

// openURLCmd validates raw and returns a tea.Cmd that launches it off the
// Update goroutine. A URL that fails validation never reaches the opener;
// the rejection is reported the same way as a launch failure.
func openURLCmd(raw string) tea.Cmd {
	u, err := validateClickURL(raw)
	if err != nil {
		return func() tea.Msg { return urlOpenedMsg{err: err} }
	}
	host := u.Hostname()
	open := openURLFunc
	return func() tea.Msg {
		return urlOpenedMsg{host: host, err: open(raw)}
	}
}
