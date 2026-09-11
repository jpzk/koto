package main

import (
	"os"
	"path/filepath"
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
