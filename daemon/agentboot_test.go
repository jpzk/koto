package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseAgentBootTimeout(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
		warn bool
	}{
		{"", 30 * time.Second, false},      // unset: unchanged default, silent
		{"  ", 30 * time.Second, false},    // blank counts as unset
		{"90s", 90 * time.Second, false},   // valid
		{"2m", 2 * time.Minute, false},     // valid
		{" 45s ", 45 * time.Second, false}, // trimmed
		{"10m", 10 * time.Minute, false},   // the maximum itself is allowed
		{"abc", 30 * time.Second, true},    // not a duration
		{"90", 30 * time.Second, true},     // no unit
		{"0s", 30 * time.Second, true},     // <= 0
		{"-5s", 30 * time.Second, true},    // <= 0
		{"10m1s", 30 * time.Second, true},  // > 10m
		{"90m", 30 * time.Second, true},    // > 10m (typo for 90s)
	}
	for _, c := range cases {
		got, warn := parseAgentBootTimeout(c.raw)
		if got != c.want {
			t.Errorf("parse(%q) = %s, want %s", c.raw, got, c.want)
		}
		if (warn != "") != c.warn {
			t.Errorf("parse(%q) warn = %q, want warn=%v", c.raw, warn, c.warn)
		}
		if c.warn && !strings.Contains(warn, agentBootTimeoutEnv) {
			t.Errorf("parse(%q) warning does not name the variable: %q", c.raw, warn)
		}
	}
	if agentBootTimeoutDefault != 30*time.Second {
		t.Fatalf("default changed: %s", agentBootTimeoutDefault)
	}
}

func setAgentBootTimeout(t *testing.T, raw string) {
	t.Helper()
	prev := agentBootTimeout
	t.Cleanup(func() { agentBootTimeout = prev })
	t.Setenv(agentBootTimeoutEnv, raw)
	agentBootTimeoutInit()
}

func TestAgentBootDeadlineUsesTimeout(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	for raw, want := range map[string]time.Duration{
		"":      30 * time.Second,
		"90s":   90 * time.Second,
		"bogus": 30 * time.Second,
		"11m":   30 * time.Second,
	} {
		setAgentBootTimeout(t, raw)
		if got := agentBootDeadline(start).Sub(start); got != want {
			t.Errorf("%s=%q: deadline %s after start, want %s", agentBootTimeoutEnv, raw, got, want)
		}
	}
}

func TestAgentBootErrorNamesEffectiveTimeout(t *testing.T) {
	cause := errors.New("dial vsock: connection refused")
	setAgentBootTimeout(t, "90s")
	err := agentBootError(cause, "/x/console.log")
	if !strings.Contains(err.Error(), "within 1m30s") || !errors.Is(err, cause) || !strings.Contains(err.Error(), "/x/console.log") {
		t.Fatalf("error = %v", err)
	}
	setAgentBootTimeout(t, "")
	if err := agentBootError(cause, "c"); !strings.Contains(err.Error(), "within 30s") {
		t.Fatalf("default error = %v", err)
	}
}

// The ctl client must not cancel a boot-triggering RPC before the daemon's own
// boot wait expires ("rpc DeadlineExceeded" in the 1.15.1 smoke).
func TestCtlBootVerbsOutlastAgentBootTimeout(t *testing.T) {
	for _, raw := range []string{"", "90s", "10m"} {
		t.Setenv(agentBootTimeoutEnv, raw)
		d, _ := agentBootTimeoutFromEnv()
		for _, verb := range []string{"spawn", "restart"} {
			if got := ctlVerbTimeout(verb); got <= d {
				t.Errorf("%s=%q: ctl %s deadline %s does not exceed boot timeout %s", agentBootTimeoutEnv, raw, verb, got, d)
			}
		}
		if got := ctlBootTimeout(); got != d+ctlBootMargin {
			t.Errorf("ctlBootTimeout = %s, want %s", got, d+ctlBootMargin)
		}
	}
	for _, verb := range []string{"list", "stop", "destroy", "interrupt", "send"} {
		if got := ctlVerbTimeout(verb); got != 30*time.Second {
			t.Errorf("ctl %s deadline = %s, want unchanged 30s", verb, got)
		}
	}
	ctx, cancel := ctlBootCtx()
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) <= agentBootTimeoutMax {
		t.Errorf("ctlBootCtx deadline %v does not exceed the max boot timeout under %s=10m", dl, agentBootTimeoutEnv)
	}
}

// koto.env seeds the placeholder, and a reinstall/upgrade keeps a set value.
func TestEnvFileKeysCarriesAgentBootTimeout(t *testing.T) {
	d, o := envFileKeys("/var/lib/koto", "")
	found := false
	for _, kv := range o {
		if kv[0] == agentBootTimeoutEnv {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s missing from envFileKeys optional", agentBootTimeoutEnv)
	}
	if !strings.Contains(renderEnvFile("", d, o), "#"+agentBootTimeoutEnv+"=\n") {
		t.Error("fresh koto.env lacks the commented placeholder")
	}
	body := renderEnvFile(agentBootTimeoutEnv+"=90s\n", d, o)
	if parseEnvFile(body)[agentBootTimeoutEnv] != "90s" {
		t.Fatalf("reinstall dropped %s:\n%s", agentBootTimeoutEnv, body)
	}
}
