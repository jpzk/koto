package main

// goal_live_test.go — /steer (! = redirect), /amend (editor) and the steer›
// chip on a goal work session, end to end through a fake daemon, and their
// rendering at phone widths (36/40 cols).

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// liveFake records the GoalSteer / GoalAmend requests the TUI sends and
// answers GoalList with one running goal (run "x" on alpha).
type liveFake struct {
	pb.UnimplementedKotoServer
	mu     sync.Mutex
	steers []*pb.GoalSteerReq
	amends []*pb.GoalAmendReq
	goals  []*pb.GoalItem
}

func (f *liveFake) GoalSteer(_ context.Context, r *pb.GoalSteerReq) (*pb.GoalResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, r)
	return &pb.GoalResp{Ok: true, Item: &pb.GoalItem{Id: "7b68b7d62d4c", Group: r.Group, Name: "x", Iteration: 3, MaxIterations: 12}}, nil
}

func (f *liveFake) GoalAmend(_ context.Context, r *pb.GoalAmendReq) (*pb.GoalResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.amends = append(f.amends, r)
	return &pb.GoalResp{Ok: true, Item: &pb.GoalItem{Id: "7b68b7d62d4c", Group: r.Group, Name: "x", Version: int32(len(f.amends) + 1)}}, nil
}

func (f *liveFake) GoalList(_ context.Context, _ *pb.GoalListReq) (*pb.GoalListResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pb.GoalListResp{Ok: true, Goals: f.goals}, nil
}

func startLiveFake(t *testing.T) *liveFake {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "koto.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fk := &liveFake{goals: []*pb.GoalItem{
		{Id: "7b68b7d62d4c", Group: "alpha", Name: "x", Status: "running", Iteration: 3, MaxIterations: 12,
			Text: "ship the thing", Criteria: "1. tests pass\n2. pushed", Version: 1},
		{Id: "deadbeef0000", Group: "alpha", Name: "old", Status: "done", MaxIterations: 2, Text: "old", Criteria: "old"},
	}}
	srv := grpc.NewServer()
	pb.RegisterKotoServer(srv, fk)
	go srv.Serve(ln)
	cc, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	clientMu.Lock()
	prev := client
	client = pb.NewKotoClient(cc)
	clientMu.Unlock()
	t.Cleanup(func() {
		clientMu.Lock()
		client = prev
		clientMu.Unlock()
		cc.Close()
		srv.Stop()
	})
	return fk
}

// liveRun executes a tea.Cmd and feeds its message back into the model.
func liveRun(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	sm, _ := m.Update(cmd())
	return sm.(Model)
}

// Selecting the goal WORK row in the tree puts a steer› chip on the input
// (next to the mode chip); enter on plain text sends a live note for that
// run; "!" makes it a redirect. The judge row gets no chip.
func TestSteerChipEnterSendsNote(t *testing.T) {
	fk := startLiveFake(t)
	m := treeModel(t, goalGroups(), "alpha", "", "")
	sm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = sm.(Model)
	if strings.Contains(ansi.Strip(m.renderInput()), "steer›") {
		t.Fatal("steer chip on the default session")
	}
	m.selectTreeRow(treeRow{group: "alpha", session: "goal-x"})
	m.exitTree()
	box := ansi.Strip(m.renderInput())
	if !strings.Contains(box, "conf› steer›") {
		t.Fatalf("goal work row: no steer chip next to the mode chip:\n%s", box)
	}
	m = liveRun(t, m, m.dispatchInput("use the staging db"))
	m = liveRun(t, m, m.dispatchInput("!stop, rebase first"))
	fk.mu.Lock()
	got := append([]*pb.GoalSteerReq(nil), fk.steers...)
	fk.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("steers = %v", got)
	}
	if g := got[0]; g.Group != "alpha" || g.Name != "x" || g.Text != "use the staging db" || g.Redirect {
		t.Errorf("note = %v", g)
	}
	if g := got[1]; g.Name != "x" || g.Text != "stop, rebase first" || !g.Redirect {
		t.Errorf("redirect = %v", g)
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "operator steer sent") || !strings.Contains(v, "redirected") {
		t.Errorf("no confirmations:\n%s", v)
	}
	m.selectTreeRow(treeRow{group: "alpha", session: "goal-x-judge"})
	m.exitTree()
	if strings.Contains(ansi.Strip(m.renderInput()), "steer›") {
		t.Error("judge row carries the steer chip")
	}
	if m.dispatchInput("hello") != nil {
		t.Error("judge: plain enter sent something")
	}
}

// /steer ! from anywhere is a redirect of the named run; a bare "!" is a
// usage error.
func TestSteerBangRedirect(t *testing.T) {
	fk := startLiveFake(t)
	m := treeModel(t, goalGroups(), "alpha", "", "")
	m.exitTree()
	m = liveRun(t, m, m.dispatchInput("/steer name=x !switch to plan b"))
	fk.mu.Lock()
	if len(fk.steers) != 1 || !fk.steers[0].Redirect || fk.steers[0].Name != "x" || fk.steers[0].Text != "switch to plan b" {
		t.Errorf("steers = %v", fk.steers)
	}
	fk.mu.Unlock()
	if m.dispatchInput("/steer !") != nil {
		t.Error("bare ! sent a redirect")
	}
	if last := m.lines[len(m.lines)-1].text; !strings.Contains(last, "usage") {
		t.Errorf("bare !: %q", last)
	}
}

// /amend opens the editor (input prefilled from the running goal), an edit
// sends only the changed field, unchanged prefill is "nothing changed", and
// explicit fields work without the editor.
func TestAmendEditor(t *testing.T) {
	fk := startLiveFake(t)
	m := goalCard(t, 40)
	m = liveRun(t, m, m.dispatchInput("/amend"))
	want := "/amend alpha name=x max=12 ship the thing :: 1. tests pass  2. pushed"
	if got := m.input.Value(); got != want {
		t.Fatalf("editor prefill = %q, want %q", got, want)
	}
	if m.dispatchInput(m.input.Value()) != nil {
		t.Error("unchanged editor line sent an amend")
	}
	if last := m.lines[len(m.lines)-1].text; !strings.Contains(last, "nothing changed") {
		t.Errorf("unchanged: %q", last)
	}
	m = liveRun(t, m, m.dispatchInput(strings.Replace(want, "max=12", "max=15", 1)))
	m = liveRun(t, m, m.dispatchInput("/amend name=x :: 1. tests pass  2. pushed  3. PR open"))
	m = liveRun(t, m, m.dispatchInput("/amend alpha name=x new goal text"))
	fk.mu.Lock()
	a := append([]*pb.GoalAmendReq(nil), fk.amends...)
	fk.mu.Unlock()
	if len(a) != 3 {
		t.Fatalf("amends = %v", a)
	}
	if a[0].Name != "x" || a[0].MaxIterations == nil || *a[0].MaxIterations != 15 || a[0].Text != nil || a[0].Criteria != nil {
		t.Errorf("max edit = %v", a[0])
	}
	if a[1].Criteria == nil || *a[1].Criteria != "1. tests pass  2. pushed  3. PR open" || a[1].Text != nil || a[1].MaxIterations != nil {
		t.Errorf("criteria edit = %v", a[1])
	}
	if a[2].Text == nil || *a[2].Text != "new goal text" || a[2].Criteria != nil {
		t.Errorf("text edit = %v", a[2])
	}
	if v := ansi.Strip(m.View()); !strings.Contains(strings.Join(strings.Fields(v), ""), "amendedtov") {
		t.Errorf("no amend confirmation:\n%s", v)
	}
	if m.dispatchInput("/amend max=0") != nil {
		t.Error("max=0 sent")
	}
	if m.dispatchInput("/amend name=nope") == nil {
		t.Fatal("editor for an unknown run sent nothing")
	}
	sm, _ := m.Update(goalAmendDraftCmd("", "alpha", "nope")())
	m = sm.(Model)
	if last := m.lines[len(m.lines)-1].text; !strings.Contains(last, "no active goal") {
		t.Errorf("unknown run: %q", last)
	}
}

// The steer chip, the /steer and /amend confirmations and the open /amend
// editor render whole and fit at 36 and 40 cols.
func TestGoalLiveRenderNarrow(t *testing.T) {
	startLiveFake(t)
	for _, w := range []int{36, 40} {
		m := goalCard(t, w)
		for _, val := range []string{"", "a long steer that wraps across the input box rows"} {
			m.input.SetValue(val)
			box := m.renderInput()
			assertFits(t, box, w)
			if !strings.Contains(ansi.Strip(box), "steer›") {
				t.Errorf("w=%d val=%q: no steer chip:\n%s", w, val, ansi.Strip(box))
			}
		}
		m.input.SetValue("")
		for _, op := range []string{"steer", "redirect", "amend"} {
			sm, _ := m.Update(goalOpMsg{op: op, group: "alpha", item: goalItemT{ID: "7b68b7d62d4c", Iteration: 3, Version: 2}})
			m = sm.(Model)
		}
		m = liveRun(t, m, m.dispatchInput("/amend"))
		view := m.View()
		assertFits(t, view, w)
		// Chat rows wrap mid-word at these widths: compare with all
		// whitespace (and the wrap gutter) squeezed out.
		v := ansi.Strip(view)
		flat := strings.Join(strings.Fields(v), "")
		for _, s := range []string{"operator steer sent", "redirected", "amended to v2", "amend 7b68b7d62d4c", "steer›"} {
			if !strings.Contains(flat, strings.ReplaceAll(s, " ", "")) {
				t.Errorf("w=%d: view lacks %q:\n%s", w, s, v)
			}
		}
		help := strings.Join(goalHelpLines, "\n")
		for _, s := range []string{"/amend [<group>]", "!<text>"} {
			if !strings.Contains(help, s) {
				t.Errorf("/goals help lacks %q", s)
			}
		}
	}
}
