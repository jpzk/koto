package main

// allow_test.go — the egress allowlist ("allow") in the group dialogs: the
// client-side pre-check, the effective-config prefill, exactly-the-change
// reaching the daemon with no restart, the daemon's allow_apply in the saved
// line, a daemon refusal inline under the field, and the new-group path.

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestParseAllowList(t *testing.T) {
	ok := map[string]string{
		"":                             "",
		" , ,":                         "",
		"1.2.3.4":                      "1.2.3.4",
		" 10.0.0.0/8 , 192.168.1.* ":   "10.0.0.0/8,192.168.1.*",
		"10.0.0.1 - 10.0.0.9":          "10.0.0.1 - 10.0.0.9", // range keeps its spaces
		"2001:db8::/32,example.com":    "2001:db8::/32,example.com",
		"*.github.com,api.example.org": "*.github.com,api.example.org",
		"10.0.0.0/33":                  "10.0.0.0/33", // the daemon's call
		"10.0.0.9-10.0.0.1":            "10.0.0.9-10.0.0.1",
	}
	for in, want := range ok {
		if got, err := parseAllowList(in); err != nil || got != want {
			t.Errorf("parseAllowList(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]string{
		"~^api\\.":           "regex",
		"*.example.*":        "wildcards",
		"foo*.example.com":   "wildcards",
		"*":                  "wildcards",
		"*.*.example.com":    "wildcards",
		"10.*.0.1":           "wildcards",
		"a.com,A.com":        "repeated",
		"1.2.3.4, ~x, b.com": "regex",
	}
	for in, want := range bad {
		if _, err := parseAllowList(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseAllowList(%q) err = %v, want %q", in, err, want)
		}
	}
	var parts []string
	for i := 0; i <= allowMaxEntries; i++ {
		parts = append(parts, "h"+strconv.Itoa(i)+".com")
	}
	if _, err := parseAllowList(strings.Join(parts, ",")); err == nil || !strings.Contains(err.Error(), "max") {
		t.Errorf("over-long list err = %v", err)
	}
	if _, err := parseAllowList(strings.Join(parts[:allowMaxEntries], ",")); err != nil {
		t.Errorf("list at the cap refused: %v", err)
	}
}

func TestAllowOrigFromConfig(t *testing.T) {
	for _, c := range []struct {
		v     any
		want  string
		known bool
	}{
		{"(none)", "", true},
		{"1.2.3.4,*.github.com", "1.2.3.4,*.github.com", true},
		{"!invalid (deny-all)", "", false},
		{nil, "", false},
		{42.0, "", false},
	} {
		cfg := map[string]any{}
		if c.v != nil {
			cfg["allow"] = c.v
		}
		got, known := origFromConfig(cfg)[ngAllow]
		if known != c.known || got != c.want {
			t.Errorf("allow %v → %q known=%v; want %q known=%v", c.v, got, known, c.want, c.known)
		}
	}
	if !groupOptByKeyMust(t, ngAllow).text || groupOptNeedsRestart(ngAllow) {
		t.Error("allow must be a text row that applies without a restart")
	}
}

func groupOptByKeyMust(t *testing.T, k string) groupOpt {
	t.Helper()
	o, ok := groupOptByKey(k)
	if !ok {
		t.Fatalf("%s not in groupOptTable", k)
	}
	return o
}

// TestEditGroupAllowLive: typing a list into allow marks the field changed,
// keeps the plain Save (no restart), sends exactly allow, and the saved
// line carries the daemon's allow_apply.
func TestEditGroupAllowLive(t *testing.T) {
	fk := startFakeKoto(t)
	cfg := editCfg()
	cfg["allow"] = "1.2.3.4"
	m := openEdit(t, fk, cfg)
	if got := m.newGroup.allow.Value(); got != "1.2.3.4" {
		t.Fatalf("allow prefill = %q", got)
	}
	m = gotoField(t, m, ngAllow)
	m = typeText(t, m, ", *.github.com")
	if lbl, on := editButton(t, m); lbl != "[ Save ]" || !on {
		t.Fatalf("button = %q enabled=%v, want plain Save", lbl, on)
	}
	out := stripANSI(m.View())
	for _, w := range []string{"allow*", "save allow=1.2.3.4,*.github.com", "applies live"} {
		if !strings.Contains(out, w) {
			t.Errorf("frame missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "restart:") {
		t.Errorf("allow edit mentions a restart:\n%s", out)
	}
	m = saveEdit(t, m)
	want := []string{"read group=main", "config allow=1.2.3.4,*.github.com group=main"}
	if got := fk.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
	if m.newGroup.open {
		t.Fatal("dialog still open after save")
	}
	if !strings.Contains(stripANSI(m.View()), "allow live (applied") {
		t.Errorf("allow_apply not shown:\n%s", stripANSI(m.View()))
	}
}

// TestEditGroupAllowClearAndErrors: clearing sends the daemon's clear ("");
// a v1-invalid entry blocks Save with the reason under the field and sends
// nothing; a list the daemon refuses lands inline under allow.
func TestEditGroupAllowClearAndErrors(t *testing.T) {
	fk := startFakeKoto(t)
	cfg := editCfg()
	cfg["allow"] = "example.com"
	m := openEdit(t, fk, cfg)
	m.newGroup.allow.SetValue("")
	p, at, e := m.newGroup.editPlan()
	if e != "" || p.restart || !reflect.DeepEqual(p.config, map[string]any{"allow": ""}) {
		t.Fatalf("clear plan = %+v at=%s e=%s", p, at, e)
	}
	if s := editChangeText(p, true); s != "allow=(none)" {
		t.Errorf("clear summary = %q", s)
	}

	m.newGroup.allow.SetValue("*.example.*")
	m = saveEdit(t, m)
	if f := m.newGroup; !f.open || f.errAt != ngAllow || !strings.Contains(f.err, "wildcards") {
		t.Fatalf("glob not refused inline: open=%v at=%s err=%q", f.open, f.errAt, f.err)
	}
	if got := fk.got(); len(got) != 1 {
		t.Fatalf("invalid list reached the daemon: %v", got)
	}

	fk.failAllow = `allow entry "10.0.0.0/33": bad CIDR`
	m.newGroup.allow.SetValue("10.0.0.0/33")
	m.newGroup.err, m.newGroup.errAt = "", ""
	m = saveEdit(t, m)
	if f := m.newGroup; !f.open || f.errAt != ngAllow || !strings.Contains(f.err, "bad CIDR") {
		t.Fatalf("daemon refusal not under allow: open=%v at=%s err=%q", f.open, f.errAt, f.err)
	}
	if m.newGroup.edit.orig[ngAllow] != "example.com" {
		t.Errorf("refused list became the baseline: %q", m.newGroup.edit.orig[ngAllow])
	}
}

// TestEditGroupAllowUnknownLocked: a daemon that does not report allow (or
// a stored list that no longer parses) leaves the field unknown: locked,
// typed keys ignored, never sent.
func TestEditGroupAllowUnknownLocked(t *testing.T) {
	fk := startFakeKoto(t)
	cfg := editCfg()
	cfg["allow"] = "!invalid (deny-all)"
	m := openEdit(t, fk, cfg)
	if !m.newGroup.edit.locked(ngAllow) {
		t.Fatal("corrupt allow not locked")
	}
	m = gotoField(t, m, ngAllow)
	m = typeText(t, m, "a.com")
	if p, _, _ := m.newGroup.editPlan(); len(p.config) != 0 {
		t.Errorf("locked allow sent: %v", p.config)
	}
}

// TestNewGroupAllowInConfig: the new-group dialog sends a typed allowlist in
// its config_set; an empty one sends nothing.
func TestNewGroupAllowInConfig(t *testing.T) {
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	f := m.newGroup
	f.name.SetValue("res")
	p, at, e := f.plan(map[string]GroupInfo{})
	if e != "" {
		t.Fatalf("plan err %s at %s", e, at)
	}
	if _, ok := p.config["allow"]; ok {
		t.Errorf("empty allow sent: %v", p.config)
	}
	f.allow.SetValue(" 1.2.3.4 ,*.github.com")
	if p, _, e = f.plan(map[string]GroupInfo{}); e != "" || p.config["allow"] != "1.2.3.4,*.github.com" {
		t.Errorf("plan config = %v err=%s", p.config, e)
	}
	f.allow.SetValue("~.*")
	if _, at, e = f.plan(map[string]GroupInfo{}); at != ngAllow || !strings.Contains(e, "regex") {
		t.Errorf("regex not refused at allow: at=%s e=%s", at, e)
	}
}

// TestBuildConfigReqAllow: the gRPC request carries allow tri-state like
// every other config string (absent = unset, "" = clear).
func TestBuildConfigReqAllow(t *testing.T) {
	if r := buildConfigReq(map[string]any{"group": "g"}); r.Allow != nil {
		t.Errorf("absent allow set: %q", *r.Allow)
	}
	if r := buildConfigReq(map[string]any{"group": "g", "allow": ""}); r.Allow == nil || *r.Allow != "" {
		t.Error("clear allow not sent as empty")
	}
	if r := buildConfigReq(map[string]any{"group": "g", "allow": "a.com"}); r.GetAllow() != "a.com" {
		t.Errorf("allow = %q", r.GetAllow())
	}
}

// longAllowList: allowMaxEntries distinct 253-byte hostnames (the daemon's
// longest legal entry), ~65 KiB — far past the old 4096-byte input cap.
func longAllowList() string {
	l63 := strings.Repeat("x", 63)
	out := make([]string, allowMaxEntries)
	for i := range out {
		first := "h" + strconv.Itoa(1000+i) + strings.Repeat("x", 58)
		out[i] = first + "." + l63 + "." + l63 + "." + strings.Repeat("y", 61)
		if len(out[i]) != 253 {
			panic(len(out[i]))
		}
	}
	return strings.Join(out, ",")
}

// TestEditGroupAllowLongListRoundTrip: a stored list at the daemon's maximum
// (256 entries × 253 bytes) is prefilled byte-for-byte, is NOT sent when the
// operator edits another field, survives typing into the field, and is
// re-sent whole when it does change. Before: CharLimit 4096 cut the prefill
// and an unrelated edit re-sent the cut list (silently dropping entries).
func TestEditGroupAllowLongListRoundTrip(t *testing.T) {
	long := longAllowList()
	if c, err := parseAllowList(long); err != nil || c != long {
		t.Fatalf("long list rejected client-side: %v", err)
	}
	fk := startFakeKoto(t)
	cfg := editCfg()
	cfg["allow"] = long
	m := openEdit(t, fk, cfg)
	if got := m.newGroup.allow.Value(); got != long {
		t.Fatalf("prefill truncated: %d bytes, want %d", len(got), len(long))
	}
	if m.newGroup.edit.orig[ngAllow] != long {
		t.Fatal("long list not the baseline")
	}

	// Edit another field only: allow must not be in the plan.
	m.newGroup.ports.SetValue("9000")
	p, at, e := m.newGroup.editPlan()
	if e != "" {
		t.Fatalf("plan err %s at %s", e, at)
	}
	if _, sent := p.config[ngAllow]; sent {
		t.Fatalf("unrelated edit re-sent allow (%d bytes)", len(p.config[ngAllow].(string)))
	}
	if !reflect.DeepEqual(p.changed, []string{ngPorts}) {
		t.Fatalf("changed = %v, want [ports]", p.changed)
	}
	if got := m.newGroup.allow.Value(); got != long {
		t.Fatalf("allow changed by editing ports: %d bytes", len(got))
	}

	// Typing into the field keeps every byte; an over-count list is refused
	// by count, never cut.
	m.newGroup.ports.SetValue("8080,3000")
	m = gotoField(t, m, ngAllow)
	m = typeText(t, m, ",z.com")
	if got := m.newGroup.allow.Value(); got != long+",z.com" {
		t.Fatalf("typed value truncated: %d bytes, want %d", len(got), len(long)+6)
	}
	if _, at, e = m.newGroup.editPlan(); at != ngAllow || !strings.Contains(e, "max 256") {
		t.Fatalf("257 entries: at=%s e=%q, want max-entries refusal", at, e)
	}

	// Replace one entry (still 256): sent whole.
	repl := "z.com" + long[strings.Index(long, ","):]
	m.newGroup.allow.SetValue(repl)
	p, _, e = m.newGroup.editPlan()
	if e != "" || p.config[ngAllow] != repl {
		t.Fatalf("changed long list not sent whole: e=%q len=%d want %d", e, len(fmt.Sprint(p.config[ngAllow])), len(repl))
	}
}

// TestNewGroupAllowLongList: the new-group dialog takes a max-size list
// without truncation and sends it whole.
func TestNewGroupAllowLongList(t *testing.T) {
	long := longAllowList()
	m := focusModel(t)
	m = press(t, m, tea.KeyCtrlN)
	f := m.newGroup
	f.name.SetValue("res")
	f.allow.SetValue(long)
	if f.allow.Value() != long {
		t.Fatalf("new-group allow truncated to %d bytes", len(f.allow.Value()))
	}
	p, at, e := f.plan(map[string]GroupInfo{})
	if e != "" || p.config["allow"] != long {
		t.Fatalf("plan at=%s e=%s allow len=%d", at, e, len(fmt.Sprint(p.config["allow"])))
	}
}
