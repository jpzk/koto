package main

import (
	"encoding/json"
	"testing"

	pb "koto-protocol/pb"

	"google.golang.org/protobuf/encoding/protojson"
)

// A List lands after every send and wholesale-replaces m.groups; it must
// carry the session modes, or the chip / tree badge fall back to yolo while
// the daemon still enforces plan/confirm (e2e run 2 at 7a7aad8: smx shown
// "y" and a fresh plan session shown "yolo›" after a send).
func TestListResponseKeepsSessionModes(t *testing.T) {
	resp := &pb.ListResp{Groups: map[string]*pb.GroupInfo{
		"smx": {
			Provider:         "claudesdk",
			Sessions:         []string{"e12"},
			SessionModes:     map[string]string{"": "confirm", "e12": "plan"},
			PendingApprovals: map[string]int32{"": 2},
			SessionOrigins:   map[string]string{"goal-x": "e12"},
		},
	}}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	groups, _ := raw["groups"].(map[string]any)
	gs := parseListGroups(groups)
	gi := gs["smx"]
	if gi.SessionModes[""] != modeConfirm || gi.SessionModes["e12"] != modePlan {
		t.Fatalf("session modes lost through List: %v", gi.SessionModes)
	}
	if gi.PendingApprovals[""] != 2 {
		t.Fatalf("pending approvals lost through List: %v", gi.PendingApprovals)
	}
	if gi.SessionOrigins["goal-x"] != "e12" {
		t.Fatalf("session origins lost through List: %v", gi.SessionOrigins)
	}

	// End to end through the handler: the chip of the current session and
	// the default session's badge survive a List exactly as a state frame.
	m := newModel("", 200000)
	m.cur = "smx"
	m.groups = gs
	nm, _ := m.Update(listMsg{groups: gs})
	mm := nm.(Model)
	if got := mm.sessionMode("smx", "e12"); got != modePlan {
		t.Fatalf("after List: e12 mode = %q, want plan", got)
	}
	if got := mm.sessionMode("smx", ""); got != modeConfirm {
		t.Fatalf("after List: default mode = %q, want confirm", got)
	}

	// Empty maps stay nil (absent = yolo), as from a state frame.
	gs2 := parseListGroups(map[string]any{"g": map[string]any{"provider": "claudesdk"}})
	if gs2["g"].SessionModes != nil || gs2["g"].PendingApprovals != nil {
		t.Fatalf("empty group should carry nil maps: %+v", gs2["g"])
	}
}
