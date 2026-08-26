package main

import (
	"encoding/json"
	"strings"
	"testing"

	"koto-protocol/pb"
)

func TestCtlRequestFromJSON(t *testing.T) {
	req, err := ctlRequestFromJSON([]byte(`{"cmd":"job_done","id":"abc","rc":"0","out":"aGk=","total":2,"session":"-"}`))
	if err != nil {
		t.Fatal(err)
	}
	jd := req.GetJobDone()
	if jd == nil || jd.Id != "abc" || string(jd.Out) != "hi" || jd.Total != 2 {
		t.Fatalf("decoded %v", req)
	}
	for _, bad := range []string{
		`{"cmd":"nope"}`, `{"cmd":"notify","severity":1}`, `{"cmd":"notify","zzz":1}`, `{}`, `garbage`,
	} {
		if _, err := ctlRequestFromJSON([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// ctl.out keeps the flat shape the tools and prompts/global.md document.
func TestCtlResponseJSONFlat(t *testing.T) {
	cases := map[string]*pb.CtlResponse{
		`{"ok":true,"port":18888}`:    {Ok: true, Result: &pb.CtlResponse_Port{Port: 18888}},
		`{"error":"boom","ok":false}`: {Ok: false, Error: "boom"},
		`{"ok":true,"text":"a\nb"}`:   {Ok: true, Result: &pb.CtlResponse_Tail{Tail: &pb.CtlTailResp{Text: "a\nb"}}},
		`{"item":{"cron":"@daily","enabled":true,"group":"g","id":"x","msg":"m"},"ok":true}`: {Ok: true,
			Result: &pb.CtlResponse_Sched{Sched: &pb.SchedAddResp{Ok: true, Item: &pb.ScheduleItem{Id: "x", Group: "g", Cron: "@daily", Msg: "m", Enabled: true}}}},
	}
	for want, resp := range cases {
		got := strings.TrimSpace(string(ctlResponseJSON(resp)))
		var a, b any
		_ = json.Unmarshal([]byte(got), &a)
		_ = json.Unmarshal([]byte(want), &b)
		if ga, _ := json.Marshal(a); string(ga) != func() string { gb, _ := json.Marshal(b); return string(gb) }() {
			t.Errorf("got %s want %s", got, want)
		}
	}
}
