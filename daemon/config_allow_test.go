package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"

	pb "koto-protocol/pb"
)

func allowReq(g, v string) configReq {
	s := v
	return fromPBConfigReq(&pb.ConfigReq{Group: g, Allow: &s}) // the gRPC/CLI path
}

// koto config <g> -allow "..." round-trips: canonical entries land in the
// group's config.json, the effective config echoes them, `list` (gRPC and
// the ctl plane) shows them, and "" clears back to no allowlist.
func TestConfigAllowRoundTrip(t *testing.T) {
	fcHarness(t)
	r := configCmd(allowReq("tg", " 203.0.113.7, 10.0.0.0/8,192.168.1.*, 10.1.0.5-10.1.0.9,2001:db8::/32 ,Example.COM,*.example.org"))
	if !r.OK {
		t.Fatalf("valid allowlist refused: %+v", r)
	}
	want := []string{"203.0.113.7", "10.0.0.0/8", "192.168.1.*", "10.1.0.5-10.1.0.9", "2001:db8::/32", "example.com", "*.example.org"}

	// on disk: JSON array of canonical entries
	b, err := os.ReadFile(groupConfigPath("tg"))
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		Allow []string `json:"allow"`
	}
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(disk.Allow) != fmt.Sprint(want) {
		t.Fatalf("config.json allow = %v, want %v", disk.Allow, want)
	}
	if got := r.Config["allow"]; got != strings.Join(want, ",") {
		t.Fatalf("effective allow = %v", got)
	}
	if got, _ := r.Config["allow_apply"].(string); !strings.Contains(got, "applies when the VM next starts") {
		t.Fatalf("allow_apply for a stopped group = %q", got)
	}
	// read-only config call echoes it too
	if got := configCmd(configReq{Group: "tg"}).Config["allow"]; got != strings.Join(want, ",") {
		t.Fatalf("read-back allow = %v", got)
	}

	// list: daemon struct, gRPC message, ctl plane JSON
	if gi := listGroups()["tg"]; fmt.Sprint(gi.Allow) != fmt.Sprint(want) {
		t.Fatalf("listGroups allow = %v", gi.Allow)
	}
	if pbi := toPBGroupInfo(listGroups()["tg"]); fmt.Sprint(pbi.GetAllow()) != fmt.Sprint(want) {
		t.Fatalf("pb GroupInfo allow = %v", pbi.GetAllow())
	}
	lb, _ := json.Marshal(ctlDispatch(ctlMainGroup, []byte(`{"cmd":"list"}`)))
	var lr struct {
		Groups map[string]struct {
			Allow []string `json:"allow"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(lb, &lr); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(lr.Groups["tg"].Allow) != fmt.Sprint(want) {
		t.Fatalf("ctl list allow = %v (raw %s)", lr.Groups["tg"].Allow, lb)
	}
	// other groups are untouched
	if a := listGroups()["peer"].Allow; len(a) != 0 {
		t.Fatalf("peer got an allowlist: %v", a)
	}

	// replace
	if r := configCmd(allowReq("tg", "198.51.100.1")); !r.OK || r.Config["allow"] != "198.51.100.1" {
		t.Fatalf("replace: %+v", r)
	}
	// clear with ""
	if r := configCmd(allowReq("tg", "")); !r.OK || r.Config["allow"] != "(none)" {
		t.Fatalf("clear: %+v", r)
	}
	if _, ok := loadGroupConfig("tg")["allow"]; ok {
		t.Fatal("cleared allowlist still on disk")
	}
	if a := listGroups()["tg"].Allow; len(a) != 0 {
		t.Fatalf("list after clear = %v", a)
	}
	// JSON array form (ctl-plane wire shape) is accepted as well
	r = configCmd(configReq{Group: "tg", Allow: json.RawMessage(`["10.0.0.1","*.example.com"]`)})
	if !r.OK || r.Config["allow"] != "10.0.0.1,*.example.com" {
		t.Fatalf("array form: %+v", r)
	}
}

// A bad entry is REFUSED with a readable error and the previous list stays —
// never silently dropped (which would leave an unexpected list in force).
func TestConfigAllowRejectsBadEntries(t *testing.T) {
	fcHarness(t)
	if r := configCmd(allowReq("tg", "10.0.0.1")); !r.OK {
		t.Fatal(r.Error)
	}
	for val, want := range map[string]string{
		"*.example.*":          "glob",
		"example.com, ~^a.*$":  "regex",
		"10.0.0.0/33":          "CIDR",
		"10.0.0.9-10.0.0.1":    "reversed range",
		"10.0.*.1":             "wildcard",
		"exa mple.com":         "hostname",
		"10.0.0.1-2001:db8::1": "mixes",
		"10.0.0.1/8":           "host bits",
	} {
		r := configCmd(allowReq("tg", val))
		if r.OK || !strings.Contains(r.Error, want) {
			t.Fatalf("allow=%q: want refusal containing %q, got %+v", val, want, r)
		}
		if a := loadGroupConfig("tg").allowDisplay(); fmt.Sprint(a) != "[10.0.0.1]" {
			t.Fatalf("allow=%q: stored list changed on refusal: %v", val, a)
		}
	}
	if r := configCmd(configReq{Group: "tg", Allow: json.RawMessage(`42`)}); r.OK {
		t.Fatal("non-string allow accepted")
	}
}

// allow widens or narrows egress: operator-only, like network. A peer (even
// main) cannot set it through the ctl plane, and on gRPC it needs the
// config_posture grant.
func TestConfigAllowIsPosture(t *testing.T) {
	fcHarness(t)
	r := ctlConfigSet(t, `{"cmd":"config_set","group":"tg","allow":"0.0.0.0/0"}`)
	if r["ok"] == true || !strings.Contains(fmt.Sprint(r["error"]), "allow is an operator-only setting") {
		t.Fatalf("ctl config_set allow not refused: %v", r)
	}
	if _, ok := loadGroupConfig("tg")["allow"]; ok {
		t.Fatal("refused ctl config_set still wrote allow")
	}
	s := "10.0.0.1"
	if v := postureVerb("config", &pb.ConfigReq{Group: "tg", Allow: &s}); v != "config_posture" {
		t.Fatalf("postureVerb = %q", v)
	}
}

// The live policy slot: a config write swaps it for the running gates; spawn
// re-seeds from disk; a corrupt stored list is enforced as deny-all.
func TestAllowLivePolicy(t *testing.T) {
	fcHarness(t)
	ip := netip.MustParseAddr("10.0.0.1")
	if p := seedAllow("tg"); p.active() {
		t.Fatalf("no allowlist must not narrow: %+v", p)
	}
	if r := configCmd(allowReq("tg", "10.0.0.1")); !r.OK {
		t.Fatal(r.Error)
	}
	if p := currentAllow("tg"); !p.active() || !p.list.AllowIP(ip) {
		t.Fatalf("config write not published live: %+v", p)
	}
	if r := configCmd(allowReq("tg", "10.0.0.2")); !r.OK {
		t.Fatal(r.Error)
	}
	if p := currentAllow("tg"); p.list.AllowIP(ip) {
		t.Fatal("replaced list still admits the old address")
	}
	// hand-corrupted config.json -> deny-all at the next seed, visible in list
	if _, err := updateGroupConfig("tg", func(c map[string]any) { c["allow"] = []any{"*.evil.*"} }); err != nil {
		t.Fatal(err)
	}
	if p := seedAllow("tg"); !p.denyAll || !p.active() {
		t.Fatalf("corrupt list not fail-closed: %+v", p)
	}
	if a := listGroups()["tg"].Allow; len(a) != 1 || a[0] != allowCorruptMarker {
		t.Fatalf("corrupt list display = %v", a)
	}
}
