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
	path := filepath.Join(dir, "tui.log")
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
