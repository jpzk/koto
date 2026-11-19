package main

// /steer — redirect the session on screen through the daemon's Steer RPC
// (steer_cmd.go). These pin the parse, that the request goes to the CURRENT
// group+session over the real callRPC wire, the idle and cut renderings, the
// goal-session refusal (shown, never dropped) and the empty-command usage.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"koto-protocol/pb"
)

// fakeSteerClient records Steer requests and answers with resp/err. Any
// other RPC panics (nil embedded interface): /steer must only ever Steer.
type fakeSteerClient struct {
	pb.KotoClient
	mu   sync.Mutex
	got  []*pb.SendReq
	resp *pb.SteerResp
}

func (f *fakeSteerClient) Steer(_ context.Context, r *pb.SendReq, _ ...grpc.CallOption) (*pb.SteerResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, r)
	return f.resp, nil
}

func useSteerClient(t *testing.T, resp *pb.SteerResp) *fakeSteerClient {
	t.Helper()
	f := &fakeSteerClient{resp: resp}
	clientMu.Lock()
	prev := client
	client = f
	clientMu.Unlock()
	t.Cleanup(func() {
		clientMu.Lock()
		client = prev
		clientMu.Unlock()
	})
	return f
}

func TestParseSteer(t *testing.T) {
	cases := []struct {
		in   string
		text string
		ok   bool
	}{
		{"/steer", "", true},
		{"/steer ", "", true},
		{"/steer   ", "", true},
		{"/steer hello", "hello", true},
		{"/steer   reply with X  ", "reply with X", true},
		{"/steer\tgo", "go", true},
		{"/steer line one\nline two", "line one\nline two", true},
		{"/steerx", "", false},
		{"/steering wheel", "", false},
		{"steer hello", "", false},
		{"/stop", "", false},
	}
	for _, c := range cases {
		text, ok := parseSteer(c.in)
		if ok != c.ok || text != c.text {
			t.Errorf("parseSteer(%q) = (%q, %v), want (%q, %v)", c.in, text, ok, c.text, c.ok)
		}
	}
}

// Bare /steer renders usage and sends nothing.
func TestSteerEmptyShowsUsage(t *testing.T) {
	f := useSteerClient(t, &pb.SteerResp{Ok: true})
	for _, in := range []string{"/steer", "/steer   "} {
		m := inputModel(t, "")
		if cmd := m.dispatchInput(in); cmd != nil {
			t.Fatalf("%q returned a command, want none", in)
		}
		if txt := lastLineText(t, m, "err"); !strings.Contains(txt, "usage: /steer <text>") {
			t.Fatalf("%q rendered %q, want the usage line", in, txt)
		}
	}
	if len(f.got) != 0 {
		t.Fatalf("empty /steer reached the daemon: %+v", f.got)
	}
}

// /steer goes to the CURRENT group and the session being viewed, through the
// Steer RPC (not Send, not Interrupt), with the text verbatim.
func TestSteerRoutesToCurrentSession(t *testing.T) {
	f := useSteerClient(t, &pb.SteerResp{Ok: true, Interrupted: true})
	m := inputModel(t, "")
	m.groups["alpha"] = GroupInfo{Running: true}
	m.cur = "alpha"
	m.setActiveSession("alpha", "triage")

	cmd := m.dispatchInput("/steer reply with the token STEER-1")
	if cmd == nil {
		t.Fatal("/steer returned no command")
	}
	msg, ok := cmd().(daemonRespMsg)
	if !ok || msg.op != "steer" {
		t.Fatalf("got %#v, want a steer daemonRespMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("steer err = %v", msg.err)
	}
	if len(f.got) != 1 {
		t.Fatalf("Steer calls = %d, want 1", len(f.got))
	}
	r := f.got[0]
	if r.GetGroup() != "alpha" || r.GetSession() != "triage" || r.GetMsg() != "reply with the token STEER-1" {
		t.Fatalf("Steer req = {group:%q session:%q msg:%q}, want {alpha triage \"reply with the token STEER-1\"}",
			r.GetGroup(), r.GetSession(), r.GetMsg())
	}

	// Default session goes out as "" — the daemon's default for a send-like op.
	m.setActiveSession("alpha", "")
	if c := m.dispatchInput("/steer again"); c == nil {
		t.Fatal("no command")
	} else {
		c()
	}
	if r := f.got[1]; r.GetGroup() != "alpha" || r.GetSession() != "" || r.GetMsg() != "again" {
		t.Fatalf("default-session Steer req = %+v", r)
	}

	// A cut renders as such.
	m.handleDaemonResp(msg)
	if txt := lastLineText(t, m, "sys"); !strings.Contains(txt, "running turn cut") {
		t.Fatalf("cut rendered %q", txt)
	}
}

// Idle session: the daemon ran it as a priority send; the TUI says so.
func TestSteerIdleSaysSentAsMessage(t *testing.T) {
	useSteerClient(t, &pb.SteerResp{Ok: true, Interrupted: false})
	m := inputModel(t, "")
	msg := m.dispatchInput("/steer hi")().(daemonRespMsg)
	m.handleDaemonResp(msg)
	if txt := lastLineText(t, m, "sys"); txt != "nothing to steer — sent as message" {
		t.Fatalf("idle rendered %q", txt)
	}
}

// Goal session: the request still goes to the daemon (which owns the rule),
// and its refusal is rendered as an error with the goal-verb hint.
func TestSteerGoalSessionRefusalShown(t *testing.T) {
	daemonErr := "goal sessions are steered with goal_steer, not steer"
	f := useSteerClient(t, &pb.SteerResp{Ok: false, Error: daemonErr})
	m := inputModel(t, "")
	m.setActiveSession("main", "goal-build")

	cmd := m.dispatchInput("/steer do something else")
	if cmd == nil {
		t.Fatal("goal-session /steer produced no command — the refusal would be silent")
	}
	msg := cmd().(daemonRespMsg)
	if msg.err == nil {
		t.Fatal("daemon refusal did not surface as an error")
	}
	if len(f.got) != 1 || f.got[0].GetSession() != "goal-build" {
		t.Fatalf("Steer req = %+v, want session goal-build", f.got)
	}
	m.handleDaemonResp(msg)
	txt := lastLineText(t, m, "err")
	for _, want := range []string{"steer refused", daemonErr, "/goals steer", "koto ctl goal steer"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("refusal line %q missing %q", txt, want)
		}
	}
	if strings.Contains(txt, "daemon: daemon:") {
		t.Fatalf("refusal line double-prefixed: %q", txt)
	}
}

// Other daemon errors render too (never silently dropped).
func TestSteerOtherErrorShown(t *testing.T) {
	m := inputModel(t, "")
	m.handleDaemonResp(daemonRespMsg{op: "steer", group: "main", err: errors.New("daemon: no such group x — spawn it first")})
	if txt := lastLineText(t, m, "err"); txt != "steer refused: no such group x — spawn it first" {
		t.Fatalf("error rendered %q", txt)
	}
}

// /steer is in the cheatsheet and the command palette.
func TestSteerListedInHelpAndPalette(t *testing.T) {
	found := false
	for _, sec := range helpSections() {
		for _, e := range sec.entries {
			if strings.HasPrefix(e.key, "/steer") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("/steer missing from helpSections")
	}
	if !strings.Contains(helpContent(120), "/steer <text>") {
		t.Fatal("/steer missing from the rendered help view")
	}
	m := inputModel(t, "")
	pal := false
	for _, it := range m.paletteItems() {
		if it.run == "/steer " {
			pal = true
		}
	}
	if !pal {
		t.Fatal("/steer missing from the command palette")
	}
}
