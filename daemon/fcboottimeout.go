package main

// fcboottimeout.go — how long fcSpawn waits for the guest agent to answer
// its first Init over vsock (CONNECT 10000).
//
// The deadline used to be a hard-coded 30s. On a loaded or nested host
// (load ~8 on 4 vCPU) a healthy guest reaches vsock at ~29-35s, so spawns
// failed with a bare "vsock CONNECT 10000: EOF" and every send to the group
// errored. The cap is now KOTO_GUEST_BOOT_TIMEOUT (Go duration, default 90s).
// A longer cap costs nothing on a fast boot: the wait loop returns as soon as
// the agent answers.

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	guestBootTimeoutEnv     = "KOTO_GUEST_BOOT_TIMEOUT"
	defaultGuestBootTimeout = 90 * time.Second
)

// fcGuestBootTimeout is resolved once at daemon start by fcGuestBootTimeoutInit.
var fcGuestBootTimeout = defaultGuestBootTimeout

// parseGuestBootTimeout maps the raw env value to a timeout. Empty → default,
// no warning. Unparsable or <= 0 → default plus a non-empty warning.
func parseGuestBootTimeout(v string) (time.Duration, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return defaultGuestBootTimeout, ""
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultGuestBootTimeout, fmt.Sprintf(
			"%s=%q invalid (want a positive Go duration, e.g. 90s); using default %s",
			guestBootTimeoutEnv, v, defaultGuestBootTimeout)
	}
	return d, ""
}

// fcGuestBootTimeoutInit reads KOTO_GUEST_BOOT_TIMEOUT once. Never fatal.
func fcGuestBootTimeoutInit() {
	d, warn := parseGuestBootTimeout(os.Getenv(guestBootTimeoutEnv))
	if warn != "" {
		emitLogf("fc", "warn", "%s", warn)
	}
	fcGuestBootTimeout = d
}

// hostLoadAvg returns the 1/5/15-min load averages from /proc/loadavg, or
// "unknown" when it can't be read.
func hostLoadAvg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unknown"
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return "unknown"
	}
	return strings.Join(f[:3], " ")
}

// guestBootTimeoutError builds the spawn error when the guest agent never
// answered: elapsed time, configured cap, host load + nCPU (the usual cause
// is an overloaded host), the remedy, and the console log to read.
func guestBootTimeoutError(elapsed, timeout time.Duration, load string, ncpu int, cause error, console string) error {
	return fmt.Errorf("guest agent didn't come up after %s (timeout %s; host load %s on %d CPUs): %w — "+
		"if the host is busy, raise %s (e.g. %s=%s); see %s",
		elapsed.Round(100*time.Millisecond), timeout, load, ncpu, cause,
		guestBootTimeoutEnv, guestBootTimeoutEnv, 2*timeout, console)
}

// guestBootSlow reports whether a successful boot used more than half the
// cap — an early signal that the timeout is about to start biting.
func guestBootSlow(elapsed, timeout time.Duration) bool {
	return elapsed > timeout/2
}

// hostNCPU is indirected for tests.
var hostNCPU = runtime.NumCPU
