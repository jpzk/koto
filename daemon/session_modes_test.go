package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"koto-protocol/pb"
)

func rawStr(s string) json.RawMessage { b, _ := json.Marshal(s); return b }

func setProvider(t *testing.T, g, p string) {
	t.Helper()
	if r := configCmd(configReq{Group: g, Provider: rawStr(p)}); !r.OK {
		t.Fatalf("set provider %s: %s", p, r.Error)
	}
	if got := groupProviderName(g); got != p {
		t.Fatalf("provider = %q, want %q", got, p)
	}
}

func setModeCfg(g, sess, mode string) configResp {
	s := mode
	return configCmd(fromPBConfigReq(&pb.ConfigReq{Group: g, Session: sess, Mode: &s}))
}

// The registry is host-only, yolo is the absence of an entry, and nothing in
// config.json (which the guest sees and main can partly write) is read.
func TestSessionModeRegistryHostOnly(t *testing.T) {
	fcHarness(t)
	quietLogAlerts(t)
	if m := sessionMode("tg", ""); m != modeYolo {
		t.Fatalf("default = %q", m)
	}
	if _, err := setSessionMode("tg", "review", "PLAN", providerClaude); err != nil {
		t.Fatal(err)
	}
	if _, err := setSessionMode("tg", "", "confirm", providerClaude); err != nil {
		t.Fatal(err)
	}
	if m := sessionMode("tg", "review"); m != modePlan {
		t.Fatalf("review = %q", m)
	}
	got := listSessionModes("tg")
	if len(got) != 2 || got[""] != modeConfirm || got["review"] != modePlan {
		t.Fatalf("list = %v", got)
	}
	if _, err := setSessionMode("tg", "review", "yolo", providerClaude); err != nil {
		t.Fatal(err)
	}
	if _, ok := listSessionModes("tg")["review"]; ok {
		t.Fatal("yolo must delete the entry")
	}
	if _, err := setSessionMode("tg", "x", "sudo", providerClaude); err == nil {
		t.Fatal("bogus mode accepted")
	}
	// A guest-visible config.json carrying a mode is ignored.
	cfg := filepath.Join(vol("tg"), ".cs", "config.json")
	if err := os.WriteFile(cfg, []byte(`{"mode":"yolo","session_modes":{"":"yolo"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := sessionMode("tg", ""); m != modeConfirm {
		t.Fatalf("config.json relaxed the mode: %q", m)
	}
	// Corrupt registry fails closed.
	if err := os.WriteFile(sessionModePath("tg"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := sessionMode("tg", "anything"); m != modeConfirm {
		t.Fatalf("corrupt registry -> %q, want confirm", m)
	}
	if !strings.HasPrefix(sessionModePath("tg"), vol("tg")) {
		t.Fatal("registry must live in the host group dir")
	}
}

// mode is a posture key: ctl config_set refuses it for every agent incl.
// main, and on gRPC it needs the admin-only config_posture verb.
func TestModeIsPostureKey(t *testing.T) {
	fcHarness(t)
	for _, owner := range []string{"main", "tg"} {
		for _, mode := range []string{"yolo", "plan", "confirm"} {
			resp := ctlDispatch(owner, ctlLine(t, map[string]any{"cmd": "config_set", "group": "tg", "mode": mode, "session": ""}))
			br, ok := resp.(baseResp)
			want := "operator-only"
			if owner != "main" {
				want = "not allowed for non-main"
			}
			if !ok || br.OK || !strings.Contains(br.Error, want) {
				t.Fatalf("%s config_set mode=%s: want operator-only refusal, got %+v", owner, mode, resp)
			}
		}
	}
	if len(listSessionModes("tg")) != 0 {
		t.Fatal("refused mode landed in the registry")
	}
	s := "plan"
	if v := postureVerb("config", &pb.ConfigReq{Group: "tg", Mode: &s}); v != "config_posture" {
		t.Fatalf("postureVerb = %q", v)
	}
	y := "yolo"
	if v := postureVerb("config", &pb.ConfigReq{Group: "tg", Mode: &y, Session: "x"}); v != "config_posture" {
		t.Fatalf("postureVerb(yolo) = %q", v)
	}
	if configPostureKey(configReq{Group: "tg", Mode: rawStr("yolo")}) != "mode" {
		t.Fatal("configPostureKey misses mode")
	}
}

// Operator gRPC Config sets the mode in the registry, not config.json; goal
// sessions cannot be confirm; codex/pi/venice refuse plan/confirm loudly.
func TestConfigModeOperatorPath(t *testing.T) {
	fcHarness(t)
	srv := &kotoServer{}
	pm := "plan"
	r, err := srv.Config(context.Background(), &pb.ConfigReq{Group: "tg", Session: "review", Mode: &pm})
	if err != nil || !r.Ok {
		t.Fatalf("Config mode: %v %q", err, r.GetError())
	}
	if sessionMode("tg", "review") != modePlan {
		t.Fatal("mode not stored")
	}
	if b, _ := os.ReadFile(filepath.Join(vol("tg"), ".cs", "config.json")); strings.Contains(string(b), "plan") {
		t.Fatalf("mode leaked into config.json: %s", b)
	}
	if r := setModeCfg("tg", "default", "confirm"); !r.OK || sessionMode("tg", "") != modeConfirm {
		t.Fatalf("default alias: %+v", r)
	}
	if r := setModeCfg("tg", "goal-x", "confirm"); r.OK || !strings.Contains(r.Error, "goal sessions cannot") {
		t.Fatalf("goal confirm: %+v", r)
	}
	if r := setModeCfg("tg", "goal-x", "plan"); !r.OK {
		t.Fatalf("goal plan should be allowed: %+v", r)
	}
	if r := setModeCfg("tg", "bad name", "plan"); r.OK {
		t.Fatal("bad session accepted")
	}
	if r := setModeCfg("tg", "", "rootkit"); r.OK || !strings.Contains(r.Error, "invalid mode") {
		t.Fatalf("bad mode: %+v", r)
	}
	// Restricted sessions block a provider switch to a harness without modes.
	if r := configCmd(configReq{Group: "tg", Provider: rawStr(providerOpenAI)}); r.OK || !strings.Contains(r.Error, "does not support") {
		t.Fatalf("provider switch stranded a restricted session: %+v", r)
	}
	if groupProviderName("tg") != providerClaude {
		t.Fatal("refused provider switch was written")
	}
}

func TestCodexPiRejectPlanConfirm(t *testing.T) {
	for _, c := range []struct{ provider, harness string }{
		{providerOpenAI, "codex"}, {providerMinimax, "pi"}, {providerVenice, "venice"},
	} {
		t.Run(c.harness, func(t *testing.T) {
			fcHarness(t)
			setProvider(t, "tg", c.provider)
			for _, mode := range []string{"plan", "confirm"} {
				r := setModeCfg("tg", "", mode)
				if r.OK || !strings.Contains(r.Error, "not supported for this harness yet") || !strings.Contains(r.Error, c.harness) {
					t.Fatalf("%s mode=%s: %+v", c.harness, mode, r)
				}
				cr, _ := (&kotoServer{}).SessionCreate(context.Background(), &pb.GroupReq{Group: "tg", Session: "s-" + mode, Mode: mode})
				if cr.Ok || !strings.Contains(cr.Error, "not supported for this harness yet") {
					t.Fatalf("%s SessionCreate mode=%s: %+v", c.harness, mode, cr)
				}
				if _, err := setSessionMode("tg", "x", mode, c.provider); err == nil {
					t.Fatalf("%s setSessionMode %s accepted", c.harness, mode)
				}
			}
			if len(listSessionModes("tg")) != 0 {
				t.Fatal("no silent mode entries")
			}
			if r := setModeCfg("tg", "", "yolo"); !r.OK {
				t.Fatalf("yolo must stay settable: %+v", r)
			}
		})
	}
}

// SessionCreate sets the initial mode of a NEW session only; it can never
// change an existing session's mode (session_create is not posture).
func TestSessionCreateMode(t *testing.T) {
	fcHarness(t)
	s := &kotoServer{}
	ctx := context.Background()
	if r, _ := s.SessionCreate(ctx, &pb.GroupReq{Group: "tg", Session: "fresh", Mode: "plan"}); !r.Ok {
		t.Fatalf("create: %q", r.Error)
	}
	if sessionMode("tg", "fresh") != modePlan {
		t.Fatal("initial mode not set")
	}
	if _, err := setSessionMode("tg", "fresh", "confirm", providerClaude); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"yolo", "plan"} {
		if r, _ := s.SessionCreate(ctx, &pb.GroupReq{Group: "tg", Session: "fresh", Mode: m}); !r.Ok {
			t.Fatalf("idempotent create: %q", r.Error)
		}
		if sessionMode("tg", "fresh") != modeConfirm {
			t.Fatalf("SessionCreate(mode=%s) changed an existing session's mode", m)
		}
	}
	// A cleared (unlisted) session keeps its restriction too.
	removeSession("tg", "fresh")
	if r, _ := s.SessionCreate(ctx, &pb.GroupReq{Group: "tg", Session: "fresh", Mode: "yolo"}); !r.Ok || sessionMode("tg", "fresh") != modeConfirm {
		t.Fatalf("re-create relaxed: %v %q", r.Ok, r.Error)
	}
	if r, _ := s.SessionCreate(ctx, &pb.GroupReq{Group: "tg", Session: "q", Mode: "bogus"}); r.Ok {
		t.Fatal("bogus mode accepted")
	}
}

// GroupInfo carries the modes, and the wire mapping keeps them.
func TestGroupInfoSessionModes(t *testing.T) {
	fcHarness(t)
	if _, err := setSessionMode("tg", "review", "plan", providerClaude); err != nil {
		t.Fatal(err)
	}
	gi := listGroups()["tg"]
	if gi.SessionModes["review"] != modePlan {
		t.Fatalf("GroupInfo.SessionModes = %v", gi.SessionModes)
	}
	if pbi := toPBGroupInfo(gi); pbi.GetSessionModes()["review"] != modePlan {
		t.Fatalf("pb session_modes = %v", pbi.GetSessionModes())
	}
	if listGroups()["peer"].SessionModes != nil {
		t.Fatal("yolo-only group should carry no modes")
	}
}

// fcSendMsg stamps the registry mode (and confirm deadlines) on MsgReq.
func TestFcSendMsgStampsMode(t *testing.T) {
	fcHarness(t)
	got := make(chan *pb.MsgReq, 4)
	ln := fakeFC(t, "tg", func(port int, c net.Conn) {
		defer c.Close()
		req := &pb.AgentRequest{}
		if err := fcReadFrame(c, fcFrameMaxHost, req); err != nil {
			return
		}
		got <- req.GetMsg()
		fcWriteFrame(c, &pb.AgentResponse{Ok: true})
	})
	defer ln.Close()
	if _, err := setSessionMode("tg", "p", "plan", providerClaude); err != nil {
		t.Fatal(err)
	}
	if _, err := setSessionMode("tg", "c", "confirm", providerClaude); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sess, want string
		deadline   bool
	}{{"", "", false}, {"p", "plan", false}, {"c", "confirm", true}} {
		// config.json claims yolo; the host registry wins.
		if err := fcSendMsg("tg", c.sess, 1, "hi", "sp", []byte(`{"mode":"yolo"}`), ""); err != nil {
			t.Fatalf("send %q: %v", c.sess, err)
		}
		m := <-got
		if m.GetMode() != c.want {
			t.Fatalf("session %q: mode %q, want %q", c.sess, m.GetMode(), c.want)
		}
		if c.deadline != (m.GetApprovalTimeoutMs() > 0 && m.GetApprovalHardCapMs() >= m.GetApprovalTimeoutMs()) {
			t.Fatalf("session %q: timeouts %d/%d", c.sess, m.GetApprovalTimeoutMs(), m.GetApprovalHardCapMs())
		}
	}
}

// The plan default is the TUI's choice, passed in SessionCreate.mode; the
// daemon's own default stays yolo for everything else: a fresh group's
// default session, goal-<name> sessions, and sessions created without a mode
// (ctl / send / schedules / main).
func TestDaemonDefaultsStayYolo(t *testing.T) {
	fcHarness(t)
	groupsLock.Lock()
	writeGroups(map[string]int{"main": 8787, "tg": 8788, "peer": 8789, "newg": 8790})
	groupsLock.Unlock()
	if md := sessionMode("newg", ""); md != modeYolo {
		t.Fatalf("new group default session = %s, want yolo", md)
	}
	s := &kotoServer{}
	if r, _ := s.SessionCreate(context.Background(), &pb.GroupReq{Group: "newg", Session: "viactl"}); !r.Ok {
		t.Fatalf("modeless create: %q", r.Error)
	}
	registerSession("newg", "goal-x")
	registerSession("newg", "goal-x-judge")
	for _, sess := range []string{"", "viactl", "goal-x", "goal-x-judge"} {
		if md := sessionMode("newg", sess); md != modeYolo {
			t.Errorf("newg/%q = %s, want yolo", sess, md)
		}
	}
	if len(listSessionModes("newg")) != 0 {
		t.Fatalf("daemon wrote modes on its own: %v", listSessionModes("newg"))
	}
	// Only an explicit mode (the TUI's) makes a plan session.
	if r, _ := s.SessionCreate(context.Background(), &pb.GroupReq{Group: "newg", Session: "fromtui", Mode: modePlan}); !r.Ok || sessionMode("newg", "fromtui") != modePlan {
		t.Fatalf("TUI create with plan: %v %q", r.Ok, r.Error)
	}
}
