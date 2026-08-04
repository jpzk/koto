package main

// logalert.go — warn/error daemon-log lines double as operator alerts.
//
// The daemon log is the only place many failures surface (a schedule that
// stopped firing, a self-heal circuit breaker opening, an auth reject storm),
// and nobody watches it continuously. So every warn/error line emitted
// through emitLog/emitLogG — daemon-global or group-attributed — is also
// forwarded as a HIGH-severity operator notification against `main`, riding
// the same [[notify]]-marker path as cs-notify and the resource alerts: TUI
// banner, OSC desktop notification, and the main event stream, no new
// channel. Both levels map to "high" deliberately: the daemon's
// informational tier is "info", so a warn already means something needs
// eyes, and the notification exists to interrupt.
//
// Two hazards shape the code:
//
//   - Feedback: the forwarding path must never log at warn/error itself — a
//     delivery failure that warned would re-enter the forwarder. Failures
//     are silent by design (the line being forwarded already reached the
//     log ring and stderr; only the banner is lost). Lines emitted BY the
//     notification machinery use emitLogfQuiet and skip forwarding —
//     resNotifyOperator pairs its log line with its own queueNotify, and
//     forwarding it too would banner one resource alert twice with
//     contradictory severities (its 80% tier is "normal" by design).
//
//   - Spam: some warn lines are per-event and attacker-influenceable (one
//     line per BLOCKED egress flow, per rejected auth call). A per-
//     (subsystem, group) token bucket caps sustained flow so a storm on one
//     subsystem collapses without eliding an unrelated group's first error.
//     Suppressed lines still reach the daemon log ring — only the banner is
//     elided, and the operator-attention channel stays worth attending to.

import (
	"strings"
	"sync"
	"time"
)

const (
	// Burst covers a failure's first few distinct lines; sustained flow on
	// one (subsystem, group) key drips one banner a minute — enough to keep
	// an ongoing condition visible without training the operator to ignore
	// the channel.
	logAlertBurst  = 5
	logAlertRefill = time.Minute
)

var (
	logAlertMu      sync.Mutex
	logAlertBuckets = map[string]*notifyBucket{}
)

func logAlertAllow(subsystem, group string) bool {
	logAlertMu.Lock()
	defer logAlertMu.Unlock()
	key := subsystem + "\x00" + group
	b := logAlertBuckets[key]
	if b == nil {
		b = &notifyBucket{tokens: logAlertBurst, last: time.Now()}
		logAlertBuckets[key] = b
	}
	return b.take(logAlertBurst, logAlertRefill)
}

// logAlertTitle renders the notification title for a forwarded line:
// "WARN egress [dev]", "ERROR daemon". The message carries the line itself.
func logAlertTitle(subsystem, group, level string) string {
	t := strings.ToUpper(level) + " " + subsystem
	if group != "" {
		t += " [" + group + "]"
	}
	return t
}

// forwardLogAlert raises one high-severity operator notification for a
// warn/error log line. Called from emitLogG on every line; anything below
// warn returns immediately.
func forwardLogAlert(subsystem, group, level, msg string) {
	if level != "warn" && level != "error" {
		return
	}
	if !logAlertAllow(subsystem, group) {
		return
	}
	// A full backlog drops the banner silently — logging it would re-enter
	// this function against the same full queue. notifyDeliver's info
	// mirror is loop-safe (info is below the forwarding threshold).
	notifyDeliver(ctlMainGroup, "high", "", logAlertTitle(subsystem, group, level), msg)
}
