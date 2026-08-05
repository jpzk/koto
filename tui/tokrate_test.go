package main

import (
	"strings"
	"testing"
)

// The tok/s chip is a status-bar fixture: current group rate plus the
// fleet-wide Σ, rendered even when idle (zeros), fed from WatchState frames.
func TestStatusBarShowsTokRates(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 120, 30
	m.groups = map[string]GroupInfo{"main": {Running: true, TokPerSec: 42.4}}
	m.cur = "main"
	m.globalTokRate = 99.6
	right := m.renderStatusRight("⠋")
	if !strings.Contains(right, "42 tok/s") || !strings.Contains(right, "Σ 100") {
		t.Fatalf("status right missing tok/s chip (want \"42 tok/s\" and \"Σ 100\"):\n%q", right)
	}

	m.groups = map[string]GroupInfo{"main": {Running: true}}
	m.globalTokRate = 0
	right = m.renderStatusRight("⠋")
	if !strings.Contains(right, "0 tok/s") || !strings.Contains(right, "Σ 0") {
		t.Fatalf("idle chip must still render zeros:\n%q", right)
	}
}

// A List-sourced listMsg (no global field on ListResp) must not zero the
// stored fleet rate; a WatchState-sourced one updates it.
func TestGlobalTokRateGatedOnSource(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 120, 30
	nm, _ := m.Update(listMsg{groups: map[string]GroupInfo{"main": {}}, globalTok: 50, hasGlobalTok: true})
	m = nm.(Model)
	if m.globalTokRate != 50 {
		t.Fatalf("globalTokRate = %f after state frame, want 50", m.globalTokRate)
	}
	nm, _ = m.Update(listMsg{groups: map[string]GroupInfo{"main": {}}})
	m = nm.(Model)
	if m.globalTokRate != 50 {
		t.Fatalf("List-sourced msg clobbered globalTokRate: %f, want 50", m.globalTokRate)
	}
}
