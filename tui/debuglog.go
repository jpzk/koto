package main

// Development log file. The TUI is an alt-screen program: writing diagnostics
// to stdout/stderr would corrupt its own frames, and the runtime image is
// scratch (no shell, no syslog), so the one viable channel is an append-only
// file on the writable mount. Default path is <dir(SOCK_PATH)>/tui.log —
// the same directory tui-state.json persists in, which `make tui` bind-mounts
// from run/tui/ on the host. KOTO_TUI_LOG overrides the path ("off" disables);
// KOTO_TUI_LOG_LEVEL raises the threshold (debug|info|warn|error|off,
// default debug — it's a development log, verbosity is the point).
//
// If the file can't be opened (e.g. no writable mount) logging silently
// disables rather than breaking the TUI: there is nowhere to report the
// failure that wouldn't corrupt the display. Every helper is a no-op until
// initDebugLog succeeds, so tests (which never call it) stay quiet.
//
// Writes go straight to the fd (O_APPEND, no bufio) so a crash loses
// nothing and `tail -f run/tui/tui.log` follows in real time. The file
// rotates once to tui.log.old when it exceeds dbgRotateBytes.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	dbgLvlDebug = iota
	dbgLvlInfo
	dbgLvlWarn
	dbgLvlError
	dbgLvlOff
)

const dbgRotateBytes = 5 << 20 // 5 MiB, then rotate to .old once

var (
	dbgMu    sync.Mutex // guards everything below; log calls come from stream goroutines too
	dbgFile  *os.File
	dbgPath  string
	dbgSize  int64
	dbgLevel = dbgLvlOff
)

func initDebugLog(env func(string) string, sock string) {
	level := dbgLvlDebug
	switch env("KOTO_TUI_LOG_LEVEL") {
	case "", "debug":
	case "info":
		level = dbgLvlInfo
	case "warn":
		level = dbgLvlWarn
	case "error":
		level = dbgLvlError
	default: // "off" and anything unparseable
		level = dbgLvlOff
	}
	path := env("KOTO_TUI_LOG")
	switch path {
	case "":
		path = filepath.Join(filepath.Dir(sock), "tui.log")
	case "off", "none", "0":
		level = dbgLvlOff
	}
	if level == dbgLvlOff {
		return
	}
	// O_NOFOLLOW: this path is under the daemon's writable state tree while
	// the TUI runs as the operator — see openNoFollow (audit M39).
	f, err := openNoFollow(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return // no writable mount — stay disabled
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}
	dbgMu.Lock()
	dbgFile, dbgPath, dbgSize, dbgLevel = f, path, st.Size(), level
	dbgMu.Unlock()
}

func logDbg(subsys, format string, args ...any)  { dbgWrite(dbgLvlDebug, "DBG", subsys, format, args) }
func logInfo(subsys, format string, args ...any) { dbgWrite(dbgLvlInfo, "INF", subsys, format, args) }
func logWarn(subsys, format string, args ...any) { dbgWrite(dbgLvlWarn, "WRN", subsys, format, args) }
func logErr(subsys, format string, args ...any)  { dbgWrite(dbgLvlError, "ERR", subsys, format, args) }

func dbgWrite(level int, tag, subsys, format string, args []any) {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile == nil || level < dbgLevel {
		return
	}
	// The message is FLATTENED and SCRUBBED (audit 2026-09-11 L53). This is a
	// line-oriented record — one per line, read back with tail(1) or an editor
	// — and the values interpolated into it include daemon, provider and guest
	// error text, which the daemon's sanitizer deliberately leaves newlines in.
	// So an influenced string forged whole extra records in the operator's own
	// diagnostic log, and an escape sequence in one reached whatever they read
	// it with. Same invariant as the daemon's own log (L23), stated at this
	// logger's single write.
	line := fmt.Sprintf("%s %s %s: %s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), tag, subsys,
		flattenLogValue(fmt.Sprintf(format, args...)))
	n, err := dbgFile.WriteString(line)
	if err != nil {
		// Mount went away / disk full: disable rather than retry per line.
		dbgFile.Close()
		dbgFile = nil
		return
	}
	dbgSize += int64(n)
	if dbgSize > dbgRotateBytes {
		dbgRotate()
	}
}

// dbgRotate renames the live file to <path>.old (replacing any previous one)
// and reopens fresh. Called under dbgMu. On any failure it keeps the current
// file — an oversized log beats a lost one.
func dbgRotate() {
	if err := os.Rename(dbgPath, dbgPath+".old"); err != nil {
		dbgSize = 0 // don't retry the rename on every subsequent write
		return
	}
	f, err := openNoFollow(dbgPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		// Keep writing to the renamed fd; better than going dark.
		dbgSize = 0
		return
	}
	dbgFile.Close()
	dbgFile = f
	dbgSize = 0
}

// flattenLogValue makes one debug-log record one line: terminal controls out,
// line breaks folded to spaces. See dbgWrite.
func flattenLogValue(s string) string {
	s = scrubVTStrict(s)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\v', '\f', 0x85, 0x2028, 0x2029:
			return ' '
		}
		return r
	}, s)
}
