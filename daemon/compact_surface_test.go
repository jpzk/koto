package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// waitCalls polls until the env has seen n guest calls (the queue worker runs
// them asynchronously behind a ctl/RPC ack).
func waitCalls(t *testing.T, e *compactTestEnv, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := e.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("guest saw %q, want %d calls", e.snapshot(), n)
	return nil
}

// The TUI path: "/compact [--fresh] [instr]" typed into a session reaches the
// daemon as a plain Send and is turned into the koto compaction, mapped to the
// group's harness — never delivered as literal text to a harness whose
// non-interactive mode would not understand it.
func TestGrpcSendInterceptsCompact(t *testing.T) {
	fcHarness(t)
	s := &kotoServer{}
	ctx := context.Background()

	for _, c := range []struct{ provider, want string }{
		{providerClaude, "/compact keep the plan"},
		{providerOpenAI, compactPrefix + `{"mode":"codex-app-server","instructions":"keep the plan"}`},
		{providerMinimax, compactPrefix + `{"mode":"pi-rpc","instructions":"keep the plan"}`},
	} {
		t.Run(c.provider, func(t *testing.T) {
			e := newCompactTestEnv(t, "tg", c.provider, nil)
			r, err := s.Send(ctx, &pb.SendReq{Group: "tg", Msg: "  /compact keep the plan"})
			if err != nil || !r.Ok {
				t.Fatalf("send: ok=%v err=%v resp=%q", r.GetOk(), err, r.GetError())
			}
			if got := waitCalls(t, e, 1); got[0] != "turn:"+c.want {
				t.Fatalf("guest saw %q, want turn:%q", got[0], c.want)
			}
		})
	}

	t.Run("fresh", func(t *testing.T) {
		e := newCompactTestEnv(t, "tg", providerOpenAI, nil)
		if r, _ := s.Send(ctx, &pb.SendReq{Group: "tg", Msg: "/compact --fresh"}); !r.Ok {
			t.Fatalf("send: %q", r.GetError())
		}
		got := waitCalls(t, e, 3)
		if !strings.HasPrefix(got[0], "turn:[koto] This session is about to be compacted") || got[1] != "clear" {
			t.Fatalf("--fresh did not take the fallback: %q", got)
		}
	})

	t.Run("not-a-compact", func(t *testing.T) {
		e := newCompactTestEnv(t, "tg", providerClaude, nil)
		if r, _ := s.Send(ctx, &pb.SendReq{Group: "tg", Msg: "/compactor is a word"}); !r.Ok {
			t.Fatalf("send: %q", r.GetError())
		}
		if got := waitCalls(t, e, 1); got[0] != "turn:/compactor is a word" {
			t.Fatalf("plain message mangled: %q", got)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		newCompactTestEnv(t, "tg", providerClaude, nil)
		for _, req := range []*pb.SendReq{
			{Group: "tg", Msg: "/compact", Image: []byte{1}},                 // attachments
			{Group: "tg", Msg: "/compact", Session: goalWorkSessionFor("x")}, // goal session
			{Group: "nosuchgrp", Msg: "/compact"},                            // unregistered
		} {
			if r, _ := s.Send(ctx, req); r.Ok {
				t.Fatalf("accepted %+v", req)
			}
		}
	})
}

// The ctl path: every group may compact itself; only main may name a peer;
// the request survives the guest's strict protojson → CtlRequest hop.
func TestCtlCompactVerb(t *testing.T) {
	fcHarness(t)

	t.Run("self-via-pb", func(t *testing.T) {
		e := newCompactTestEnv(t, "tg", providerMinimax, nil)
		req, err := ctlRequestFromJSON([]byte(`{"cmd":"compact","instructions":"keep tests"}`))
		if err != nil {
			t.Fatalf("guest-side decode: %v", err)
		}
		if resp := ctlDispatchPB("tg", req); !resp.Ok {
			t.Fatalf("self compact refused: %q", resp.Error)
		}
		want := "turn:" + compactPrefix + `{"mode":"pi-rpc","instructions":"keep tests"}`
		if got := waitCalls(t, e, 1); got[0] != want {
			t.Fatalf("guest saw %q, want %q", got[0], want)
		}
	})

	t.Run("main-targets-peer", func(t *testing.T) {
		e := newCompactTestEnv(t, "peer", providerClaude, nil)
		if r := ctlDispatch(ctlMainGroup, []byte(`{"cmd":"compact","group":"peer"}`)); !r.(baseResp).OK {
			t.Fatalf("main→peer refused: %+v", r)
		}
		if got := waitCalls(t, e, 1); got[0] != "turn:/compact" {
			t.Fatalf("guest saw %q", got[0])
		}
	})

	t.Run("refusals", func(t *testing.T) {
		newCompactTestEnv(t, "tg", providerClaude, nil)
		for _, c := range []struct{ owner, line string }{
			{"tg", `{"cmd":"compact","group":"peer"}`},                              // non-main → peer
			{ctlMainGroup, `{"cmd":"compact","group":"ghost"}`},                     // unregistered
			{ctlMainGroup, `{"cmd":"compact","group":"Bad Name"}`},                  // malformed
			{"tg", `{"cmd":"compact","session":"` + goalWorkSessionFor("x") + `"}`}, // goal session
			{"tg", `{"cmd":"compact","instructions":"\u0000koto-compact{}"}`},       // forged prefix
		} {
			if r := ctlDispatch(c.owner, []byte(c.line)); r.(baseResp).OK {
				t.Fatalf("%s as %s: accepted", c.line, c.owner)
			}
		}
	})
}
