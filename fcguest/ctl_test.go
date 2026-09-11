package main

import (
	"bufio"
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

// 2026-09-11 L1: an oversized control record used to end the forwarder for
// good. bufio.Scanner returns false with ErrTooLong, the loop exited without
// checking sc.Err(), and the FIFO's only reader was gone — so every later
// notification, job_done and report from that guest went nowhere until the VM
// was restarted, and writers blocked on a pipe nobody was draining. The worker
// owns this FIFO, so writing one long line was the whole attack.
func TestReadCtlLineResynchronisesAfterAnOversizedRecord(t *testing.T) {
	huge := strings.Repeat("x", ctlLineMax+4096)
	in := "{\"cmd\":\"first\"}\n" + huge + "\n{\"cmd\":\"after\"}\n"
	r := bufio.NewReaderSize(strings.NewReader(in), 4096)

	line, over, err := readCtlLine(r)
	if err != nil || over || string(line) != "{\"cmd\":\"first\"}\n" {
		t.Fatalf("first record: %q over=%v err=%v", line, over, err)
	}

	// The oversized one is reported and DISCARDED through its terminator —
	// nothing of it is retained, which is the memory half of the same bug.
	line, over, err = readCtlLine(r)
	if err != nil || !over {
		t.Fatalf("oversized record: over=%v err=%v", over, err)
	}
	if len(line) != 0 {
		t.Errorf("the oversized record was retained (%d bytes)", len(line))
	}

	// ...and the stream resynchronises: the NEXT record is intact, which is
	// what the old scanner could never do.
	line, over, err = readCtlLine(r)
	if err != nil || over || string(line) != "{\"cmd\":\"after\"}\n" {
		t.Fatalf("record after the oversized one: %q over=%v err=%v", line, over, err)
	}

	// A line arriving in buffer-sized pieces but still under the cap is
	// assembled whole, not mistaken for an overflow.
	long := strings.Repeat("y", 100*1024)
	r = bufio.NewReaderSize(strings.NewReader(long+"\n"), 4096)
	line, over, err = readCtlLine(r)
	if err != nil || over || len(line) != len(long)+1 {
		t.Fatalf("a long-but-legal record: %d bytes over=%v err=%v", len(line), over, err)
	}

	// EOF with no terminator is an error, not a silent truncation.
	r = bufio.NewReaderSize(strings.NewReader("{\"cmd\":\"x\"}"), 4096)
	if _, _, err := readCtlLine(r); err == nil {
		t.Error("an unterminated final record was accepted as complete")
	}
}
