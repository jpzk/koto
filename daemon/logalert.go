package main

// logalert.go — error daemon-log lines double as operator alerts.
//
// The daemon log is the only place many failures surface (a schedule that
// stopped firing, a self-heal circuit breaker opening), and nobody watches
// it continuously. So every error line emitted through emitLog/emitLogG —
// daemon-global or group-attributed — is also forwarded as a HIGH-severity
// operator notification against `main`, riding the same [[notify]]-marker
// path as cs-notify and the resource alerts: TUI banner, OSC desktop
// notification, and the main event stream, no new channel. Warn lines stay
// log-only (2026-08-04: warn is too chatty a tier to interrupt for —
// BLOCKED-flow and auth-reject warns are per-event and attacker-
// influenceable, and were burning the banner's signal; the log ring still
// records them).
//
// Two hazards shape the code:
//
//   - Feedback: the forwarding path must never log at error itself — a
//     delivery failure that errored would re-enter the forwarder. Failures
//     are silent by design (the line being forwarded already reached the
//     log ring and stderr; only the banner is lost). Lines emitted BY the
//     notification machinery use emitLogfQuiet and skip forwarding —
//     resNotifyOperator pairs its log line with its own queueNotify, and
//     forwarding it too would banner one resource alert twice.
//
//   - Spam: error lines can repeat per-event (a crash-looping VM, a failing
//     schedule firing every minute). A per-(subsystem, group) token bucket
//     caps sustained flow so a storm on one subsystem collapses without
//     eliding an unrelated group's first error. Suppressed lines still
//     reach the daemon log ring — only the banner is elided, and the
//     operator-attention channel stays worth attending to.

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

// The fleet-wide ceiling on forwarded log banners. Sized so a handful of
// subsystems failing at once still all reach the operator, and a fleet failing
// at once does not consume the queue that resource alerts share.
const (
	logAlertGlobalBurst  = 20
	logAlertGlobalRefill = 10 * time.Second
)

var logAlertGlobal = &notifyBucket{tokens: logAlertGlobalBurst, last: time.Now()}

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

// logAlertForgetGroup drops every bucket belonging to g. Called from destroy,
// with the rest of the name-keyed teardown (audit 2026-09-11 L98): group names
// are reusable, so a replacement inherited the destroyed group's spent tokens
// and had its first error banners suppressed — and the map itself had no
// removal path, so churning distinct names grew it without bound.
func logAlertForgetGroup(g string) {
	suffix := "\x00" + g
	logAlertMu.Lock()
	for k := range logAlertBuckets {
		if strings.HasSuffix(k, suffix) {
			delete(logAlertBuckets, k)
		}
	}
	logAlertMu.Unlock()
}

// logAlertTitle renders the notification title for a forwarded line:
// "ERROR fc [dev]", "ERROR daemon". The message carries the line itself.
func logAlertTitle(subsystem, group, level string) string {
	t := strings.ToUpper(level) + " " + subsystem
	if group != "" {
		t += " [" + group + "]"
	}
	return t
}

// forwardLogAlert raises one high-severity operator notification for an
// error log line. Called from emitLogG on every line; anything below error
// (warn included) returns immediately.
func forwardLogAlert(subsystem, group, level, msg string) {
	if level != "error" {
		return
	}
	if !logAlertAllow(subsystem, group) {
		return
	}
	// A GLOBAL budget on top of the per-(subsystem, group) one (audit
	// 2026-09-11 L147). Each pair gets its own bucket, and every one of them
	// empties into main's single notification queue — so enough distinct pairs
	// (a fleet in trouble is exactly when there are many) fill that queue
	// between them, and notifyDeliver drops what does not fit WITHOUT keeping
	// it. What gets lost behind a crowd of forwarded log lines is the resource
	// alert: the one notification the operator most needs, on the same queue,
	// from the subject that is silent by design until it is catastrophic.
	//
	// So forwarded log lines — which are a convenience mirror of something
	// already in the daemon log — yield to it. The line is still logged; only
	// the banner is elided, which is the same trade the per-pair bucket makes.
	if !logAlertGlobal.take(logAlertGlobalBurst, logAlertGlobalRefill) {
		return
	}
	// A full backlog drops the banner silently — logging it would re-enter
	// this function against the same full queue. notifyDeliver's info
	// mirror is loop-safe (info is below the forwarding threshold).
	notifyDeliver(ctlMainGroup, "high", "", logAlertTitle(subsystem, group, level), msg)
}
