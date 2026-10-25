package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"koto-protocol/pb"
)

// fakeGoalSetClient records GoalSet requests; any other RPC panics through
// the embedded nil interface.
type fakeGoalSetClient struct {
	pb.KotoClient
	got *pb.GoalSetReq
}

func (f *fakeGoalSetClient) GoalSet(_ context.Context, r *pb.GoalSetReq, _ ...grpc.CallOption) (*pb.GoalResp, error) {
	f.got = r
	return &pb.GoalResp{Ok: true, Item: &pb.GoalItem{Group: r.Group, OriginSession: r.Session}}, nil
}

// /goals set records the ACTIVE session of the current group as the origin;
// a goal aimed at another group, or typed from a goal session, has none.
// The origin rides GoalSetReq.session end to end through callRPC.
func TestGoalSetPassesActiveSession(t *testing.T) {
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{"work": {Running: true, Sessions: []string{"lab"}}, "other": {Running: true}}
	m.cur = "work"
	m.session = map[string]string{"work": "lab", "other": "x"}
	if o := m.goalOrigin("work"); o != "lab" {
		t.Fatalf("goalOrigin(cur) = %q, want lab", o)
	}
	if o := m.goalOrigin("other"); o != "" {
		t.Fatalf("goalOrigin(other group) = %q, want default", o)
	}
	m.session["work"] = goalSess
	if o := m.goalOrigin("work"); o != "" {
		t.Fatalf("goalOrigin from a goal session = %q, want default", o)
	}
	m.session["work"] = ""
	if o := m.goalOrigin("work"); o != "" {
		t.Fatalf("goalOrigin(default) = %q", o)
	}

	f := &fakeGoalSetClient{}
	extra := map[string]any{"group": "work", "session": "lab", "name": "n", "text": "t", "criteria": "c", "max_iterations": 3, "plan": true}
	resp, err := callRPC(context.Background(), f, "goal_set", extra)
	if err != nil {
		t.Fatal(err)
	}
	if f.got.GetSession() != "lab" || f.got.GetGroup() != "work" {
		t.Fatalf("GoalSetReq = %+v", f.got)
	}
	if r, _ := resp.(*pb.GoalResp); r.GetItem().GetOriginSession() != "lab" {
		t.Fatalf("resp origin = %q", r.GetItem().GetOriginSession())
	}
	if it := goalFromMap(map[string]any{"origin_session": "lab"}); it.OriginSession != "lab" {
		t.Fatalf("goalFromMap origin = %q", it.OriginSession)
	}
}

// state frames carry session_origins into GroupInfo.
func TestStateGroupsCarriesSessionOrigins(t *testing.T) {
	f := &pb.StateFrame{Groups: map[string]*pb.GroupInfo{
		"work": {Sessions: []string{"lab", goalSess}, SessionOrigins: map[string]string{goalSess: "lab"}},
	}}
	if got := stateGroups(f)["work"].SessionOrigins[goalSess]; got != "lab" {
		t.Fatalf("SessionOrigins = %q", got)
	}
}

func rowIDs(rows []treeRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.group+"/"+r.session+"/"+r.job)
	}
	return out
}

func rowIndex(rows []treeRow, g, s, j string) int {
	for i, r := range rows {
		if r.group == g && r.session == s && r.job == j {
			return i
		}
	}
	return -1
}

// A goal set from a named session nests under it: worker row, -judge leaf
// and the goal's jobs all render one level below the origin session row,
// after the origin's own jobs. A default-origin goal stays at group level.
func TestTreeNestsGoalUnderOriginSession(t *testing.T) {
	m := newModel("", 200000)
	judge := goalSess + "-judge"
	jobs := []JobInfo{
		{ID: "labjob01", Session: "lab", Status: "running", Started: 1},
		{ID: "goaljob1", Session: goalSess, Status: "running", Started: 2},
		{ID: "judgjob1", Session: judge, Status: "running", Started: 3},
	}
	m.groups = map[string]GroupInfo{
		"main": {Running: true},
		"work": {Running: true, Sessions: []string{"alpha", "lab", goalSess, judge, "goal-other"}, Jobs: jobs,
			SessionOrigins: map[string]string{goalSess: "lab", judge: "lab"}},
	}
	m.processJobTransitions(m.groups)
	for _, s := range []string{"lab", goalSess, judge} {
		m.jobsOpen[sessKey("work", s)] = true
	}
	rows := m.treeRows()
	ids := strings.Join(rowIDs(rows), "\n")
	branch := map[string]string{}
	for _, r := range rows {
		branch[r.group+"/"+r.session+"/"+r.job] = r.branch
	}
	lab := rowIndex(rows, "work", "lab", "")
	wk := rowIndex(rows, "work", goalSess, "")
	jd := rowIndex(rows, "work", judge, "")
	other := rowIndex(rows, "work", "goal-other", "")
	if lab < 0 || wk < 0 || jd < 0 || other < 0 {
		t.Fatalf("missing rows:\n%s", ids)
	}
	if !(lab < wk && wk < jd && jd < other) {
		t.Fatalf("goal rows not nested after origin:\n%s", ids)
	}
	labDepth := len([]rune(branch["work/lab/"]))
	for _, k := range []string{"work/" + goalSess + "/", "work/" + judge + "/", "work/lab/labjob01"} {
		if d := len([]rune(branch[k])); d != labDepth+3 {
			t.Errorf("%s depth %d (branch %q), want origin depth+3 = %d", k, d, branch[k], labDepth+3)
		}
	}
	for _, k := range []string{"work/" + goalSess + "/goaljob1", "work/" + judge + "/judgjob1"} {
		if d := len([]rune(branch[k])); d != labDepth+6 {
			t.Errorf("%s depth %d (branch %q), want goal depth+3 = %d", k, d, branch[k], labDepth+6)
		}
	}
	// origin's own job first, goal children after it; the last child closes.
	if rowIndex(rows, "work", "lab", "labjob01") != lab+1 {
		t.Errorf("origin job not first under origin:\n%s", ids)
	}
	if !strings.HasSuffix(branch["work/"+judge+"/"], "└─ ") {
		t.Errorf("judge (last child of lab) branch %q", branch["work/"+judge+"/"])
	}
	// default-origin goal: group level, same depth as plain sessions.
	if branch["work/goal-other/"] == "" || len([]rune(branch["work/goal-other/"])) != labDepth {
		t.Errorf("default-origin goal moved: %q vs lab %q", branch["work/goal-other/"], branch["work/lab/"])
	}
}

// Origin row absent (cleared/destroyed) -> the goal falls back to the group.
func TestTreeGoalFallsBackWhenOriginMissing(t *testing.T) {
	m := newModel("", 200000)
	m.groups = map[string]GroupInfo{
		"main": {Running: true},
		"work": {Running: true, Sessions: []string{"alpha", goalSess}, SessionOrigins: map[string]string{goalSess: "gone"}},
	}
	rows := m.treeRows()
	a := rowIndex(rows, "work", "alpha", "")
	g := rowIndex(rows, "work", goalSess, "")
	if a < 0 || g < 0 || rows[a].branch[:len(rows[a].branch)-len("├─ ")] != rows[g].branch[:len(rows[g].branch)-len("└─ ")] {
		t.Fatalf("goal with missing origin not at group level:\n%s", strings.Join(rowIDs(rows), "\n"))
	}
	// A goal session can never be another goal's origin.
	m.groups["work"] = GroupInfo{Running: true, Sessions: []string{"goal-a", "goal-b"}, SessionOrigins: map[string]string{"goal-b": "goal-a"}}
	top, kids := m.sessionLayout("work")
	if len(top) != 2 || len(kids) != 0 {
		t.Fatalf("goal nested under goal: top=%v kids=%v", top, kids)
	}
}

// Jobs whose session has no row (finished goal, cleared session) stay
// visible under the group's default conversation, tagged with their session,
// and count toward its folded badge.
func TestOrphanJobsStayVisibleUnderGroup(t *testing.T) {
	m := newModel("", 200000)
	m.width, m.height = 200, 30
	jobs := []JobInfo{
		{ID: "orphan01", Session: "goal-done", Status: "running", Started: 1},
		{ID: "orphan02", Session: "cleared", Status: "running", Started: 2},
		{ID: "livejob1", Session: "alive", Status: "running", Started: 3},
		{ID: "defjob01", Session: "", Status: "running", Started: 4},
	}
	m.groups = map[string]GroupInfo{"main": {Running: true}, "work": {Running: true, Sessions: []string{"alive"}, Jobs: jobs}}
	m.processJobTransitions(m.groups)
	if n := m.foldedJobs("work", ""); n != 3 {
		t.Fatalf("folded default count = %d, want 3 (default + 2 orphans)", n)
	}
	if n := m.foldedJobs("work", "alive"); n != 1 {
		t.Fatalf("folded alive count = %d", n)
	}
	m.jobsOpen[sessKey("work", "")] = true
	rows := m.treeRows()
	for _, id := range []string{"orphan01", "orphan02", "defjob01"} {
		i := rowIndex(rows, "work", "", id)
		if i < 0 {
			t.Fatalf("job %s not under the group row:\n%s", id, strings.Join(rowIDs(rows), "\n"))
		}
		txt := stripANSI(m.renderTreeRow(rows[i], false, false, false, func(s string, n int) string { return s }))
		if id == "orphan01" && !strings.Contains(txt, "orphan01 @") {
			t.Errorf("orphan row not tagged with its session: %q", txt)
		}
		if id == "defjob01" && strings.Contains(txt, "@") {
			t.Errorf("default job wrongly tagged: %q", txt)
		}
	}
	if rowIndex(rows, "work", "", "livejob1") >= 0 {
		t.Fatal("a job with a live session row was adopted by the default conversation")
	}
	// Linger window over -> the orphan disappears like any finished job.
	fin := jobs
	fin[0].Status, fin[0].RC = "done", "0"
	m.groups["work"] = GroupInfo{Running: true, Sessions: []string{"alive"}, Jobs: fin}
	m.processJobTransitions(m.groups)
	m.jobDoneAt[jobKey("work", "orphan01")] = time.Now().Add(-time.Hour)
	if rowIndex(m.treeRows(), "work", "", "orphan01") >= 0 {
		t.Fatal("finished orphan outlived its linger window")
	}
}
