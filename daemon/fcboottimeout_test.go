package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGuestBootTimeoutParse(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		warn bool
	}{
		{"", defaultGuestBootTimeout, false},
		{"   ", defaultGuestBootTimeout, false},
		{"90s", 90 * time.Second, false},
		{"2m", 2 * time.Minute, false},
		{" 45s ", 45 * time.Second, false},
		{"1m30s", 90 * time.Second, false},
		{"abc", defaultGuestBootTimeout, true},
		{"90", defaultGuestBootTimeout, true}, // no unit
		{"0s", defaultGuestBootTimeout, true},
		{"0", defaultGuestBootTimeout, true},
		{"-5s", defaultGuestBootTimeout, true},
	}
	for _, c := range cases {
		got, warn := parseGuestBootTimeout(c.in)
		if got != c.want {
			t.Errorf("parse(%q) = %s, want %s", c.in, got, c.want)
		}
		if (warn != "") != c.warn {
			t.Errorf("parse(%q) warn = %q, want warn=%v", c.in, warn, c.warn)
		}
		if c.warn && !strings.Contains(warn, guestBootTimeoutEnv) {
			t.Errorf("parse(%q) warn %q doesn't name %s", c.in, warn, guestBootTimeoutEnv)
		}
	}
	if defaultGuestBootTimeout != 90*time.Second {
		t.Fatalf("default = %s, want 90s", defaultGuestBootTimeout)
	}
}

func TestGuestBootTimeoutInitEnv(t *testing.T) {
	defer func(d time.Duration) { fcGuestBootTimeout = d }(fcGuestBootTimeout)
	for in, want := range map[string]time.Duration{
		"":     90 * time.Second,
		"120s": 120 * time.Second,
		"junk": 90 * time.Second,
		"-1s":  90 * time.Second,
	} {
		t.Setenv(guestBootTimeoutEnv, in)
		fcGuestBootTimeoutInit()
		if fcGuestBootTimeout != want {
			t.Errorf("%s=%q → %s, want %s", guestBootTimeoutEnv, in, fcGuestBootTimeout, want)
		}
	}
}

func TestGuestBootTimeoutErrorMessage(t *testing.T) {
	cause := errors.New("vsock CONNECT 10000: EOF")
	err := guestBootTimeoutError(91234*time.Millisecond, 90*time.Second, "8.12 7.90 6.55", 4, cause, "/state/vms/g/console.log")
	msg := err.Error()
	for _, want := range []string{
		"after 1m31.2s",            // elapsed
		"timeout 1m30s",            // configured cap
		"host load 8.12 7.90 6.55", // loadavg
		"on 4 CPUs",                // nCPU
		"vsock CONNECT 10000: EOF", // underlying cause
		"raise KOTO_GUEST_BOOT_TIMEOUT",
		"/state/vms/g/console.log", // console path kept
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if !errors.Is(err, cause) {
		t.Errorf("cause not wrapped")
	}
}

func TestGuestBootSlow(t *testing.T) {
	if guestBootSlow(45*time.Second, 90*time.Second) {
		t.Error("exactly 50% should not warn")
	}
	if !guestBootSlow(46*time.Second, 90*time.Second) {
		t.Error(">50% should warn")
	}
	if guestBootSlow(2*time.Second, 90*time.Second) {
		t.Error("fast boot should not warn")
	}
}

func TestHostLoadAvgNonEmpty(t *testing.T) {
	if hostLoadAvg() == "" {
		t.Error("hostLoadAvg returned empty")
	}
}
