package main

import (
	"strconv"
	"strings"
	"testing"
)

// The option table is the single source: every select knob has a default
// equal to its first value, values are unique, and the daemon-mirrored
// allowlists are exactly what the daemon accepts today.
func TestGroupOptTable(t *testing.T) {
	want := map[string]string{
		"provider":  "claudesdk|venice|openai|maple",
		"effort":    "default|low|medium|high|xhigh|max",
		"network":   "none|wan|lan|full",
		"size":      "small|medium|large|xlarge",
		"root":      "no|yes",
		"autostart": "no|yes",
		"kvm":       "no|yes",
		"model":     "",
		"ports":     "",
	}
	defaults := map[string]string{"provider": "claudesdk", "effort": "default", "network": "none",
		"size": "small", "root": "no", "autostart": "no", "kvm": "no", "model": "", "ports": ""}
	if len(groupOptTable) != len(want) {
		t.Fatalf("table has %d rows, want %d", len(groupOptTable), len(want))
	}
	for _, o := range groupOptTable {
		if got := strings.Join(o.values, "|"); got != want[o.key] {
			t.Errorf("%s values = %s, want %s", o.key, got, want[o.key])
		}
		if d := groupOptDefault(o.key); d != defaults[o.key] {
			t.Errorf("%s default = %q, want %q", o.key, d, defaults[o.key])
		}
		seen := map[string]bool{}
		for _, v := range o.values {
			if seen[v] {
				t.Errorf("%s: duplicate %q", o.key, v)
			}
			seen[v] = true
		}
		for r := range o.risky {
			if !groupOptValid(o.key, r) {
				t.Errorf("%s: risky value %q not in allowlist", o.key, r)
			}
		}
	}
	if groupOptValid("provider", "openai-typo") || !groupOptValid("size", "xlarge") {
		t.Error("groupOptValid wrong")
	}
	if c := groupOptChoices("provider"); c != "claudesdk, venice, openai, or maple" {
		t.Errorf("choices = %q", c)
	}
}

func TestValidateNewGroupName(t *testing.T) {
	for _, g := range []string{"a", "researcher", "btc-signal_2", strings.Repeat("a", 32)} {
		if e := validateNewGroupName(g); e != "" {
			t.Errorf("%q rejected: %s", g, e)
		}
	}
	for _, g := range []string{"", "Upper", "has space", "-lead", "a/b", strings.Repeat("a", 33), "main", "ünï"} {
		if e := validateNewGroupName(g); e == "" {
			t.Errorf("%q accepted", g)
		}
	}
}

func TestParsePortList(t *testing.T) {
	ok := map[string]string{"": "", "8080": "8080", "8080, 3000": "8080,3000", "1024 65535": "1024,65535"}
	for in, want := range ok {
		got, err := parsePortList(in)
		if err != nil || got != want {
			t.Errorf("parsePortList(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"80", "1023", "65536", "0", "abc", "8080,8080", "-1"} {
		if _, err := parsePortList(in); err == nil {
			t.Errorf("parsePortList(%q) accepted", in)
		}
	}
	many := ""
	for p := 2000; p < 2000+portMaxCount+1; p++ {
		many += " " + strconv.Itoa(p)
	}
	if _, err := parsePortList(many); err == nil {
		t.Error("33 ports accepted")
	}
}

// TestGroupOptRestartFlags pins which keys need a VM restart after a
// config_set — the table above groupOptTable cites the daemon code for each.
// Text rows are exactly model and ports and are never selects.
func TestGroupOptRestartFlags(t *testing.T) {
	want := map[string]bool{
		"provider": false, "model": false, "effort": false, "autostart": false,
		"network": true, "size": true, "root": true, "kvm": true, "ports": true,
	}
	got := map[string]bool{}
	for _, o := range groupOptTable {
		got[o.key] = o.restart
		if o.restart != groupOptNeedsRestart(o.key) {
			t.Errorf("%s: groupOptNeedsRestart disagrees with the row", o.key)
		}
		if o.text != (o.key == "model" || o.key == "ports") {
			t.Errorf("%s: text = %v", o.key, o.text)
		}
		if o.text && isSelectField(o.key) {
			t.Errorf("%s: text row reported as a select", o.key)
		}
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s restart = %v (present %v), want %v", k, g, ok, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("table keys %v, want %v", got, want)
	}
	if groupOptNeedsRestart("nope") {
		t.Error("unknown key needs restart")
	}
	if o, _ := groupOptByKey("autostart"); o.note == "" {
		t.Error("autostart lost its daemon-start note")
	}
}
