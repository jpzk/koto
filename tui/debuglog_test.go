package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetDebugLog restores the package globals so one test's init doesn't leak
// into the next (or into the rest of the suite, which expects logging off).
func resetDebugLog() {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile != nil {
		dbgFile.Close()
	}
	dbgFile, dbgPath, dbgSize, dbgLevel = nil, "", 0, dbgLvlOff
}

func TestDebugLogDefaultsToDebugNextToSock(t *testing.T) {
	defer resetDebugLog()
	dir := t.TempDir()
	initDebugLog(envMap(nil), filepath.Join(dir, "koto.sock"))
	logDbg("test", "debug %d", 1)
	logErr("test", "error %s", "x")
	b, err := os.ReadFile(filepath.Join(dir, "tui.log"))
	if err != nil {
		t.Fatalf("log file not created: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "DBG test: debug 1") {
		t.Errorf("debug line missing (DEBUG must be the default level): %q", s)
	}
	if !strings.Contains(s, "ERR test: error x") {
		t.Errorf("error line missing: %q", s)
	}
}

func TestDebugLogLevelThresholdAndOff(t *testing.T) {
	defer resetDebugLog()
	dir := t.TempDir()
	initDebugLog(envMap(map[string]string{"KOTO_TUI_LOG_LEVEL": "warn"}), filepath.Join(dir, "koto.sock"))
	logDbg("test", "quiet")
	logInfo("test", "quiet")
	logWarn("test", "loud")
	b, _ := os.ReadFile(filepath.Join(dir, "tui.log"))
	if strings.Contains(string(b), "quiet") {
		t.Errorf("sub-threshold lines written: %q", b)
	}
	if !strings.Contains(string(b), "WRN test: loud") {
		t.Errorf("warn line missing: %q", b)
	}

	resetDebugLog()
	initDebugLog(envMap(map[string]string{"KOTO_TUI_LOG": "off"}), filepath.Join(dir, "koto.sock"))
	if dbgFile != nil {
		t.Error("KOTO_TUI_LOG=off must disable logging")
	}
}

func TestDebugLogUnwritablePathStaysSilent(t *testing.T) {
	defer resetDebugLog()
	// A scratch container without the writable mount: open fails, every
	// helper must be a no-op rather than a panic.
	initDebugLog(envMap(nil), "/nonexistent-dir/koto.sock")
	logDbg("test", "into the void") // must not panic
	if dbgFile != nil {
		t.Error("expected logging disabled on unwritable path")
	}
}

func TestDebugLogRotation(t *testing.T) {
	defer resetDebugLog()
	dir := t.TempDir()
	path := filepath.Join(dir, "tui.log") // where initDebugLog puts it, beside the sock
	initDebugLog(envMap(map[string]string{"KOTO_TUI_LOG": path}), filepath.Join(dir, "koto.sock"))
	dbgMu.Lock()
	dbgSize = dbgRotateBytes // next write crosses the threshold
	dbgMu.Unlock()
	logDbg("test", "tripwire")
	logDbg("test", "fresh file")
	old, err := os.ReadFile(path + ".old")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if !strings.Contains(string(old), "tripwire") {
		t.Errorf("rotated file lost the pre-rotation line: %q", old)
	}
	cur, _ := os.ReadFile(path)
	if !strings.Contains(string(cur), "fresh file") {
		t.Errorf("post-rotation line missing from fresh file: %q", cur)
	}
}

// 2026-09-11 L91: the 0600 passed to OpenFile applies only when the call
// CREATES the file, and os.Rename carries an inode's mode into the rotated
// copy — so a tui.log created, copied or restored at 0644 stayed
// world-readable through every write and every rotation, exposing current
// diagnostics and the retained history to anyone who can reach the state dir.
func TestDebugLogPermissionsAreRepairedOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tui.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer resetDebugLog()
	initDebugLog(envMap(nil), filepath.Join(dir, "koto.sock"))
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the log is mode %04o after open; diagnostics are world-readable", st.Mode().Perm())
	}
	logDbg("test", "still writable")
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "still writable") {
		t.Fatalf("tightening the mode broke the logger: %v %q", err, b)
	}
}

// 2026-09-11 L109: dispatchInput logged every slash command verbatim, before
// any validation. The verb is operator intent; the arguments are not — `/sched
// add … <message>` carries an arbitrary prompt, `/goals set` the goal text and
// its acceptance criteria. This log defaults to DEBUG, appends for the life of
// the process and keeps a rotated generation, so what lands in it outlives the
// session.
func TestSlashCommandArgumentsAreNotLogged(t *testing.T) {
	defer resetDebugLog()
	dir := t.TempDir()
	initDebugLog(envMap(nil), filepath.Join(dir, "koto.sock"))

	m := newModel("", 200000)
	m.cur = "g"
	const secret = "deploy-token-hunter2-do-not-log"
	_ = m.dispatchInput("/sched add 0 9 * * 1-5 " + secret)
	_ = m.dispatchInput("/goals set g " + secret)

	b, err := os.ReadFile(filepath.Join(dir, "tui.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("a slash-command argument was written to the debug log:\n%s", b)
	}
	// The verb is still there — following what the TUI did is what this log
	// is for.
	if !strings.Contains(string(b), "/sched") {
		t.Errorf("the command itself was not logged:\n%s", b)
	}
}
