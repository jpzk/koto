package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// steer entry points: ctl JSON verb, guest proto adapter, gRPC Steer + ACL.

func waitSteerDone(t *testing.T, d <-chan error, what string) {
	t.Helper()
	select {
	case <-d:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never finished", what)
	}
}

// TestSteerCtlPeerSteersOnlyItself: a non-main group's steer targets ITSELF
// whatever "group" says (the goal_interrupt rule), and interrupts its own
// in-flight turn of the named session.
func TestSteerCtlPeerSteersOnlyItself(t *testing.T) {
	fcHarness(t)
	r := newSteerRecorder()
	hit := make(chan string, 4)
	withSteerInterrupt(t, func(g, s string) error { hit <- g + "/" + s; return nil })
	withTurnFn(r.turn, func() {
		dA, err := enqueueSend("tg", "chat", "block-A")
		if err != nil {
			t.Fatal(err)
		}
		waitStarted(t, r, "block-A")
		resp := ctlDispatch("tg", []byte(`{"cmd":"steer","group":"peer","session":"chat","msg":"S1"}`))
		sr, ok := resp.(steerCtlResp)
		if !ok || !sr.OK || !sr.Interrupted {
			t.Fatalf("peer self-steer: %#v", resp)
		}
		select {
		case who := <-hit:
			if who != "tg/chat" {
				t.Fatalf("peer steer interrupted %q, want tg/chat (group field must be ignored)", who)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no interrupt")
		}
		waitSteerDone(t, dA, "block-A")
		waitStarted(t, r, "S1")
		if n := queueDepth("peer"); n != 0 {
			t.Fatalf("peer queue touched by tg's steer: depth %d", n)
		}
	})
}

// TestSteerCtlMainSteersPeers: main may name any registered group; an idle
// target is a priority send (interrupted=false); unknown groups, goal
// sessions, a reply window and empty messages are refused.
func TestSteerCtlMainSteersPeers(t *testing.T) {
	fcHarness(t)
	r := newSteerRecorder()
	withSteerInterrupt(t, func(g, s string) error {
		t.Errorf("interrupt on idle %s/%s", g, s)
		return nil
	})
	withTurnFn(r.turn, func() {
		resp := ctlDispatch(ctlMainGroup, []byte(`{"cmd":"steer","group":"peer","session":"x","msg":"S2"}`))
		sr, ok := resp.(steerCtlResp)
		if !ok || !sr.OK || sr.Interrupted {
			t.Fatalf("main→peer idle steer: %#v", resp)
		}
		waitStarted(t, r, "S2")
	})
	for _, tc := range []struct{ line, want string }{
		{`{"cmd":"steer","group":"nosuch","msg":"m"}`, "no such group"},
		{`{"cmd":"steer","group":"peer","session":"goal-x","msg":"m"}`, "goal_steer"},
		{`{"cmd":"steer","group":"peer","msg":"m","reply":true}`, "no reply"},
		{`{"cmd":"steer","group":"peer","msg":"  "}`, "needs a message"},
		{`{"cmd":"steer","group":"peer","msg":"/compact"}`, "not a steer"},
		{`{"cmd":"steer","group":"Bad Name","msg":"m"}`, "invalid group"},
	} {
		resp := ctlDispatch(ctlMainGroup, []byte(tc.line))
		br, ok := resp.(baseResp)
		if !ok || br.OK || !strings.Contains(br.Error, tc.want) {
			t.Errorf("%s → %#v, want error containing %q", tc.line, resp, tc.want)
		}
	}
	// A peer naming a goal session of its own is refused the same way.
	br, _ := ctlDispatch("tg", []byte(`{"cmd":"steer","session":"goal-x","msg":"m"}`)).(baseResp)
	if br.OK || !strings.Contains(br.Error, "goal_steer") {
		t.Fatalf("peer goal-session steer: %#v", br)
	}
}

// TestSteerCtlPBRoundTrip: the guest-side strict proto adapter knows the
// verb, carries interrupted back, and refuses send-only fields (reply).
func TestSteerCtlPBRoundTrip(t *testing.T) {
	fcHarness(t)
	r := newSteerRecorder()
	withSteerInterrupt(t, func(string, string) error { return nil })
	withTurnFn(r.turn, func() {
		req, err := ctlRequestFromJSON([]byte(`{"cmd":"steer","session":"s","msg":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		if req.GetSteer().GetMsg() != "hi" {
			t.Fatalf("decoded %v", req)
		}
		resp := ctlDispatchPB("tg", req)
		if !resp.Ok {
			t.Fatalf("pb steer: %v", resp.Error)
		}
		if _, ok := resp.Result.(*pb.CtlResponse_Interrupted); !ok {
			t.Fatalf("result %T, want interrupted", resp.Result)
		}
		waitStarted(t, r, "hi")
	})
	if _, err := ctlRequestFromJSON([]byte(`{"cmd":"steer","msg":"m","reply":true}`)); err == nil {
		t.Fatal("strict adapter must refuse reply on steer")
	}
}

// TestSteerACLIsSendPlusInterrupt: gRPC authorization — a `steer` grant, or
// send AND interrupt on the target. Either alone is not enough.
func TestSteerACLIsSendPlusInterrupt(t *testing.T) {
	acl := parseACL([]byte(`{
	  "sendonly": {"send": "*"},
	  "intronly": {"interrupt": "*"},
	  "both":     {"send": ["peer"], "interrupt": ["peer"]},
	  "split":    {"send": ["peer"], "interrupt": ["tg"]},
	  "explicit": {"steer": ["peer"]}
	}`))
	for _, tc := range []struct {
		roles  []string
		target string
		want   bool
	}{
		{[]string{"sendonly"}, "peer", false},
		{[]string{"intronly"}, "peer", false},
		{[]string{"sendonly", "intronly"}, "peer", true}, // union of roles
		{[]string{"both"}, "peer", true},
		{[]string{"both"}, "tg", false},
		{[]string{"split"}, "peer", false},
		{[]string{"split"}, "tg", false},
		{[]string{"explicit"}, "peer", true},
		{[]string{"explicit"}, "tg", false},
		{[]string{"admin"}, "anything", true},
	} {
		if got := steerAllowed(acl, tc.roles, tc.target); got != tc.want {
			t.Errorf("roles %v on %s: allowed=%v, want %v", tc.roles, tc.target, got, tc.want)
		}
	}
	// And through the real interceptor decision (acl.json under creds/).
	fcHarness(t)
	if err := os.MkdirAll(filepath.Join(HERE, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credFile("acl.json"), []byte(`{"sendonly":{"send":"*"},"both":{"send":"*","interrupt":"*"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	verb := verbFromMethod(pb.Koto_Steer_FullMethodName)
	if verb != "steer" {
		t.Fatalf("verb %q", verb)
	}
	req := &pb.SendReq{Group: "peer", Msg: "m"}
	if err := aclCheck(context.Background(), clientIdentity{Name: "a", Roles: []string{"sendonly"}}, verb, req); err == nil {
		t.Fatal("send-only role must not steer")
	}
	if err := aclCheck(context.Background(), clientIdentity{Name: "b", Roles: []string{"both"}}, verb, req); err != nil {
		t.Fatalf("send+interrupt role refused: %v", err)
	}
}

// TestSteerGRPC: the RPC handler's validation and its interrupted flag.
func TestSteerGRPC(t *testing.T) {
	fcHarness(t)
	s := &kotoServer{}
	r := newSteerRecorder()
	hit := make(chan string, 2)
	withSteerInterrupt(t, func(g, ss string) error { hit <- g + "/" + ss; return nil })
	withTurnFn(r.turn, func() {
		dA, _ := enqueueSend("peer", "", "block-A")
		waitStarted(t, r, "block-A")
		resp, err := s.Steer(context.Background(), &pb.SendReq{Group: "peer", Msg: "S"})
		if err != nil || !resp.Ok || !resp.Interrupted {
			t.Fatalf("grpc steer: %v %v", resp, err)
		}
		if who := <-hit; who != "peer/" {
			t.Fatalf("interrupted %q", who)
		}
		waitSteerDone(t, dA, "block-A")
		waitStarted(t, r, "S")
	})
	for _, tc := range []struct {
		req  *pb.SendReq
		want string
	}{
		{&pb.SendReq{Group: "peer", Msg: "m", Session: "goal-x"}, "goal_steer"},
		{&pb.SendReq{Group: "peer", Msg: "m", Image: []byte{1}}, "attachments"},
		{&pb.SendReq{Group: "nosuch", Msg: "m"}, "no such group"},
		{&pb.SendReq{Group: "../x", Msg: "m"}, "invalid group"},
	} {
		resp, _ := s.Steer(context.Background(), tc.req)
		if resp.Ok || !strings.Contains(resp.Error, tc.want) {
			t.Errorf("%v → %v, want %q", tc.req, resp, tc.want)
		}
	}
}
