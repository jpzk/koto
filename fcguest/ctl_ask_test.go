package main

import (
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// An ask rides its own round trip (the shared forwarder is not held), every
// live turn clock is paused until the answer, and the answer line lands in
// ctl.out in the flat shape cs-ask parses.
func TestCtlAskPausesClocksAndAppendsAnswer(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	prevRT, prevOut := ctlAskRoundTrip, ctlAskOut
	t.Cleanup(func() { ctlAskRoundTrip, ctlAskOut = prevRT, prevOut })
	ctlAskRoundTrip = func(req *pb.CtlRequest) (*pb.CtlResponse, error) {
		if req.GetAsk().GetQuestion() != "which db?" {
			t.Errorf("req = %v", req)
		}
		close(entered)
		<-release
		return &pb.CtlResponse{Ok: true, Result: &pb.CtlResponse_Ask{Ask: &pb.AskResp{AskId: "ask-1", Answered: true, Answer: `use "pg"`}}}, nil
	}
	got := make(chan string, 1)
	ctlAskOut = func(b []byte) { got <- string(b) }

	clk := newWorkerClock(50*time.Millisecond, 0)
	fired := make(chan struct{}, 1)
	defer registerLiveClock(clk)()
	clk.start(func() { fired <- struct{}{} })
	defer clk.stop()

	req, err := ctlRequestFromJSON([]byte(`{"cmd":"ask","id":"g1","question":"which db?","options":["pg","sqlite"]}`))
	if err != nil {
		t.Fatal(err)
	}
	startCtlAsk(req)
	<-entered
	if clk.pausedN() != 1 {
		t.Fatalf("clock paused = %d", clk.pausedN())
	}
	select {
	case <-fired:
		t.Fatal("turn budget ran out while the operator was deciding")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	line := <-got
	for _, want := range []string{`"ok":true`, `"ask_id":"ask-1"`, `"answered":true`, `"answer":"use \"pg\""`} {
		if !strings.Contains(line, want) {
			t.Fatalf("ctl.out line %q lacks %s", line, want)
		}
	}
	select {
	case <-fired: // resumed: the remaining budget runs again
	case <-time.After(2 * time.Second):
		t.Fatal("clock never resumed after the answer")
	}
}

// Asks beyond ctlAskMax are refused at once instead of queueing goroutines.
func TestCtlAskBounded(t *testing.T) {
	release := make(chan struct{})
	prevRT, prevOut := ctlAskRoundTrip, ctlAskOut
	t.Cleanup(func() { close(release); ctlAskRoundTrip, ctlAskOut = prevRT, prevOut })
	ctlAskRoundTrip = func(*pb.CtlRequest) (*pb.CtlResponse, error) { <-release; return &pb.CtlResponse{Ok: true}, nil }
	got := make(chan string, ctlAskMax+2)
	ctlAskOut = func(b []byte) { got <- string(b) }
	req, _ := ctlRequestFromJSON([]byte(`{"cmd":"ask","question":"q"}`))
	for i := 0; i < ctlAskMax; i++ {
		startCtlAsk(req)
	}
	for len(ctlAskSem) < ctlAskMax {
		time.Sleep(time.Millisecond)
	}
	startCtlAsk(req)
	select {
	case l := <-got:
		if !strings.Contains(l, "already waiting") {
			t.Fatalf("excess ask = %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("excess ask not refused")
	}
}
