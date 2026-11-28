package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Guest-agent boot wait (fcSpawn: the Init retry loop after the VMM starts).
//
// It was a hard-coded 30s. On a loaded host (CPU steal 40%+) a guest kernel
// that normally reaches its milestones in ~2.4s takes 7s+, and the agent can
// miss 30s outright — the group fails to boot for real users, not just in the
// release smoke. KOTO_AGENT_BOOT_TIMEOUT (a Go duration: "90s", "2m") lets the
// operator widen it in koto.env. The default is unchanged.
const (
	agentBootTimeoutEnv     = "KOTO_AGENT_BOOT_TIMEOUT"
	agentBootTimeoutDefault = 30 * time.Second
	// Past ten minutes a VM is not slow, it is wedged — and a spawn RPC that
	// holds a fleet slot that long is worse than a clear failure. An absurd
	// value is treated as a typo ("90m" for "90s"), not honoured.
	agentBootTimeoutMax = 10 * time.Minute
)

// agentBootTimeout is the effective wait, resolved once at daemon start by
// agentBootTimeoutInit. The zero-config default holds until then (and in
// tests that never call it).
var agentBootTimeout = agentBootTimeoutDefault

// parseAgentBootTimeout is the pure half: the effective timeout for raw, and
// a non-empty warning when raw was set but rejected. Empty means "unset" and
// is silent.
func parseAgentBootTimeout(raw string) (time.Duration, string) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return agentBootTimeoutDefault, ""
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		return agentBootTimeoutDefault, fmt.Sprintf("%s=%q is not a duration (e.g. 90s, 2m); using %s", agentBootTimeoutEnv, raw, agentBootTimeoutDefault)
	case d <= 0:
		return agentBootTimeoutDefault, fmt.Sprintf("%s=%q must be positive; using %s", agentBootTimeoutEnv, raw, agentBootTimeoutDefault)
	case d > agentBootTimeoutMax:
		return agentBootTimeoutDefault, fmt.Sprintf("%s=%q exceeds the %s maximum; using %s", agentBootTimeoutEnv, raw, agentBootTimeoutMax, agentBootTimeoutDefault)
	}
	return d, ""
}

// agentBootDeadline is when fcSpawn stops retrying the agent Init.
func agentBootDeadline(start time.Time) time.Time { return start.Add(agentBootTimeout) }

// agentBootError is the failure fcSpawn returns, naming the EFFECTIVE wait so
// an operator who raised it can tell it was honoured.
func agentBootError(err error, console string) error {
	return fmt.Errorf("guest agent didn't come up within %s: %w (see %s; raise %s on a loaded host)", agentBootTimeout, err, console, agentBootTimeoutEnv)
}

// agentBootTimeoutFromEnv reads the env var in the current process. The ctl
// client uses it too (ctlBootCtx), so an operator who exports the same value
// in their shell gets a client deadline that covers it.
func agentBootTimeoutFromEnv() (time.Duration, string) {
	return parseAgentBootTimeout(os.Getenv(agentBootTimeoutEnv))
}

// agentBootTimeoutInit resolves agentBootTimeout once at daemon start.
func agentBootTimeoutInit() {
	d, warn := agentBootTimeoutFromEnv()
	if warn != "" {
		emitLogf("fc", "warn", "%s", warn)
	} else if d != agentBootTimeoutDefault {
		emitLogf("fc", "info", "guest agent boot timeout: %s (%s)", d, agentBootTimeoutEnv)
	}
	agentBootTimeout = d
}
