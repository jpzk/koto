package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"koto-protocol/pb"

	"google.golang.org/protobuf/encoding/protojson"
)

// Every verb the JSON plane accepts must round-trip through the proto
// adapter: JSON line → CtlRequest (strict) → envelope → dispatch → typed
// CtlResponse. A verb whose response type lacks a ctlRespToPB case shows up
// here as "internal", not in production as an empty reply.
func TestCtlPBEveryVerbMaps(t *testing.T) {
	fcHarness(t)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	lines := []string{
		`{"cmd":"list"}`,
		`{"cmd":"resources"}`,
		`{"cmd":"spawn","group":"zz-ctlpb-never"}`, // refused for non-main; response shape is what's tested
		`{"cmd":"send","group":"peer","msg":"hi","session":"s1","reply":true,"from_session":"x"}`,
		`{"cmd":"stop","group":"peer"}`,
		`{"cmd":"config_set","group":"peer","network":"wan"}`,
		`{"cmd":"tail","group":"peer","n":5}`,
		`{"cmd":"job_done","id":"abc","rc":"0","out":"` + b64("hello") + `","total":5,"session":"-"}`,
		`{"cmd":"notify","severity":"high","title":"` + b64("t") + `","msg":"` + b64("m") + `","session":"-"}`,
		`{"cmd":"report","msg":"` + b64("done") + `"}`,
		`{"cmd":"goal_done","note":"` + b64("n") + `"}`,
		`{"cmd":"goal_verdict","met":true,"reasons":"` + b64("r") + `"}`,
		`{"cmd":"goal_set","text":"t","criteria":"c","plan":false,"max_iterations":1}`,
		`{"cmd":"goal_approve"}`,
		`{"cmd":"goal_status"}`,
		`{"cmd":"goal_pause"}`,
		`{"cmd":"goal_interrupt"}`,
		`{"cmd":"goal_resume"}`,
		`{"cmd":"goal_cancel"}`,
		`{"cmd":"sched_add","cron":"@daily","msg":"x"}`,
		`{"cmd":"sched_list"}`,
		`{"cmd":"sched_del","id":"nope"}`,
		`{"cmd":"sched_toggle","id":"nope","enabled":true}`,
		`{"cmd":"sched_run","id":"nope"}`,
	}
	for _, l := range lines {
		req, err := ctlRequestFromJSON([]byte(l))
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		env, verb, err := ctlReqEnvelope(req)
		if err != nil {
			t.Fatalf("%s: envelope: %v", l, err)
		}
		if !strings.Contains(string(env), `"cmd":"`+verb+`"`) {
			t.Fatalf("%s: envelope lost the verb: %s", l, env)
		}
		resp := ctlDispatchPB("tg", req)
		if strings.Contains(resp.Error, "internal") {
			t.Fatalf("%s: unmapped response: %s", l, resp.Error)
		}
		js, _ := protojson.Marshal(resp)
		t.Logf("%s → %s", verb, js)
	}
	// Main-only verbs on their success path (the typed results: list,
	// resources, config, tail).
	for _, l := range []string{
		`{"cmd":"list"}`, `{"cmd":"resources"}`,
		// model, not network: posture keys are refused on this plane (audit H1).
		`{"cmd":"config_set","group":"tg","model":"claude-opus-5"}`,
		`{"cmd":"tail","group":"tg","n":5}`,
	} {
		req, err := ctlRequestFromJSON([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		resp := ctlDispatchPB(ctlMainGroup, req)
		if !resp.Ok || resp.Result == nil {
			t.Fatalf("%s as main: %v", l, resp)
		}
	}
	// Bytes fields arrive decoded on the JSON side exactly as before.
	req, _ := ctlRequestFromJSON([]byte(`{"cmd":"report","msg":"` + b64("bytes!") + `"}`))
	if string(req.GetReport().Msg) != "bytes!" {
		t.Fatalf("bytes field not decoded: %q", req.GetReport().Msg)
	}
}

func TestCtlPBStrict(t *testing.T) {
	for _, l := range []string{
		`{"cmd":"bogus"}`,
		`{"cmd":"notify","severity":5}`,
		`{"cmd":"notify","unknown_field":"x"}`,
		`{"nocmd":true}`,
		`not json`,
	} {
		if _, err := ctlRequestFromJSON([]byte(l)); err == nil {
			t.Errorf("%s: accepted", l)
		}
	}
	if _, err := ctlRespToPB(struct{}{}); err == nil {
		t.Error("unmapped type accepted")
	}
	resp := ctlDispatchPB("tg", &pb.CtlRequest{})
	if resp.Ok || resp.Error == "" {
		t.Errorf("empty request accepted: %v", resp)
	}
}
