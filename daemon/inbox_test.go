package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"koto-protocol/pb"
)

// inboxRig: a fresh state root (HERE → tempdir, so the notification store
// starts empty and persists there), no approvals, no goals, an acl.json the
// test can write, and no real tailers for the groups notifications go to.
func inboxRig(t *testing.T) {
	t.Helper()
	fcHarness(t)
	quietLogAlerts(t)
	resetApprovals(t)
	goalTestSetup(t)
	inboxMu.Lock()
	inboxLoaded, inboxNotes = "", nil
	inboxMu.Unlock()
	if err := os.MkdirAll(filepath.Join(HERE, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"main", "tg", "peer"} {
		markTailed(g)
		g := g
		t.Cleanup(func() { dropQueuedNotifies(g, "", true) })
	}
}

func inboxACL(t *testing.T, doc string) {
	t.Helper()
	if err := os.WriteFile(credFile("acl.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func inboxCtx(roles ...string) context.Context {
	return withIdentity(context.Background(), clientIdentity{Name: "probe", Roles: roles})
}

// openTestApproval registers an approvals-table entry directly (the table is
// what the inbox projects; how an entry got there is approvals.go's business).
func openTestApproval(g, sess, id, kind, summary string, opts []string, created time.Time) {
	k := approvalKey{g: g, s: sess, id: id}
	approvalsMu.Lock()
	approvals[k] = &pendingApproval{key: k, tool: "Bash", summary: summary, kind: kind,
		options: opts, created: created}
	approvalsMu.Unlock()
}

func inboxKinds(items []*pb.InboxItem) []string {
	var out []string
	for _, it := range items {
		switch x := it.GetItem().(type) {
		case *pb.InboxItem_Approval:
			k := x.Approval.GetKind()
			if k == "" {
				k = "tool"
			}
			out = append(out, k+"@"+x.Approval.GetGroup())
		case *pb.InboxItem_GoalPlan:
			out = append(out, "plan@"+x.GoalPlan.GetGroup())
		case *pb.InboxItem_Notification:
			out = append(out, "note@"+x.Notification.GetGroup())
		}
	}
	return out
}

func inboxCall(t *testing.T, ctx context.Context) []*pb.InboxItem {
	t.Helper()
	r, err := (&kotoServer{}).Inbox(ctx, &pb.InboxReq{})
	if err != nil {
		t.Fatal(err)
	}
	return r.GetItems()
}

// The inbox is the union of the approvals table (tool + ask), goals in
// awaiting_approval and stored notifications, newest first, and every item is
// projected through the caller's own `inbox` grant.
func TestInboxListsEveryKindNewestFirstAndProjects(t *testing.T) {
	inboxRig(t)
	base := time.Now().Add(-time.Hour)
	openTestApproval("tg", "work", "t-1", "", "rm -rf build", nil, base)
	openTestApproval("peer", "goal-p", "a-1", kindAsk, "which db?", []string{"postgres", "sqlite"}, base.Add(time.Minute))
	goalLock.Lock()
	goals = append(goals, goalItem{ID: "gp1", Group: "tg", Name: "plan", Text: "t", Criteria: "c",
		Status: goalStatusAwaiting, MaxIterations: 5, CreatedAt: goalNow(),
		UpdatedAt: float64(base.Add(2*time.Minute).UnixMilli()) / 1000},
		goalItem{ID: "gr1", Group: "tg", Name: "run", Text: "t", Criteria: "c",
			Status: goalStatusRunning, MaxIterations: 5, CreatedAt: goalNow(), UpdatedAt: goalNow()})
	goalLock.Unlock()
	if !notifyDeliver("main", "high", "", "main alert", "disk") {
		t.Fatal("notifyDeliver main")
	}
	time.Sleep(5 * time.Millisecond)
	if !notifyDeliver("peer", "normal", "goal-p", "peer done", "built") {
		t.Fatal("notifyDeliver peer")
	}

	got := inboxKinds(inboxCall(t, context.Background()))
	want := "note@peer note@main plan@tg ask@peer tool@tg"
	if strings.Join(got, " ") != want {
		t.Fatalf("inbox = %v, want %s", got, want)
	}
	items := inboxCall(t, context.Background())
	if a := items[3].GetApproval(); strings.Join(a.GetOptions(), "|") != "postgres|sqlite" || a.GetSummary() != "which db?" {
		t.Fatalf("ask item = %v", a)
	}
	if n := items[0].GetNotification(); n.GetSession() != "goal-p" || n.GetTitle() != "peer done" || n.GetBody() != "built" || n.GetSeverity() != "normal" || n.GetId() == "" {
		t.Fatalf("notification item = %v", n)
	}

	// A role that sees main + peer only: tg's approval and goal are excluded.
	inboxACL(t, `{"scoped":{"inbox":["main","peer"]},"none":{"list":"*"}}`)
	if got := strings.Join(inboxKinds(inboxCall(t, inboxCtx("scoped"))), " "); got != "note@peer note@main ask@peer" {
		t.Fatalf("scoped role saw %s", got)
	}
	if got := inboxCall(t, inboxCtx("none")); len(got) != 0 {
		t.Fatalf("role without an inbox grant saw %v", inboxKinds(got))
	}
	if got := inboxCall(t, inboxCtx(defaultRole)); len(got) != 5 {
		t.Fatalf("admin saw %v", inboxKinds(got))
	}
}

// ---- WatchInbox ----------------------------------------------------------------

type inboxStreamRec struct {
	grpc.ServerStream
	ctx context.Context
	ch  chan *pb.InboxResp
}

func (s *inboxStreamRec) Context() context.Context { return s.ctx }
func (s *inboxStreamRec) Send(r *pb.InboxResp) error {
	s.ch <- r
	return nil
}

func watchInbox(t *testing.T, ctx context.Context) *inboxStreamRec {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	s := &inboxStreamRec{ctx: ctx, ch: make(chan *pb.InboxResp, 64)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = (&kotoServer{}).WatchInbox(&pb.InboxReq{}, s)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

// nextFrame waits for a frame satisfying ok (frames in between are skipped:
// snapshots are idempotent).
func (s *inboxStreamRec) nextFrame(t *testing.T, what string, ok func([]*pb.InboxItem) bool) []*pb.InboxItem {
	t.Helper()
	dl := time.After(5 * time.Second)
	for {
		select {
		case r := <-s.ch:
			if ok(r.GetItems()) {
				return r.GetItems()
			}
		case <-dl:
			t.Fatalf("no WatchInbox frame: %s", what)
		}
	}
}

func hasApproval(items []*pb.InboxItem, id string) bool {
	for _, it := range items {
		if it.GetApproval().GetId() == id {
			return true
		}
	}
	return false
}

func hasNote(items []*pb.InboxItem, id string) bool {
	for _, it := range items {
		if it.GetNotification().GetId() == id {
			return true
		}
	}
	return false
}

// A goal's ask shows up live, is answered through the EXISTING Approve RPC
// with option + msg, the answer (picked option + note) reaches the blocked ask
// and the steer log, and the next snapshot no longer carries the item.
func TestWatchInboxAskAnsweredViaApproveWithOption(t *testing.T) {
	askRig(t, modeYolo)
	inboxMu.Lock()
	inboxLoaded, inboxNotes = "", nil
	inboxMu.Unlock()
	w := watchInbox(t, context.Background())
	w.nextFrame(t, "initial empty snapshot", func(it []*pb.InboxItem) bool { return len(it) == 0 })

	ch := startAsk(t, "which db?", []string{"postgres", "sqlite"})
	id := askIDT(t)
	items := w.nextFrame(t, "ask appears", func(it []*pb.InboxItem) bool { return hasApproval(it, id) })
	if a := items[0].GetApproval(); a.GetKind() != kindAsk || a.GetSession() != "goal-w" || len(a.GetOptions()) != 2 {
		t.Fatalf("ask item = %v", a)
	}

	s := &kotoServer{}
	ctx := inboxCtx(defaultRole)
	for _, bad := range []*pb.ApproveReq{
		{Group: "tg", Session: "goal-w", Id: id, Allow: true, Option: 3},
		{Group: "tg", Session: "goal-w", Id: id, Allow: true, Option: -1},
		{Group: "tg", Session: "goal-w", Id: id, Allow: false, Option: 1},
	} {
		if r, _ := s.Approve(ctx, bad); r.GetError() == "" {
			t.Fatalf("Approve %v accepted", bad)
		}
	}
	if !hasApproval(listPendingApprovalsAsItems(), id) {
		t.Fatal("a refused option answered the ask")
	}
	r, err := s.Approve(ctx, &pb.ApproveReq{Group: "tg", Session: "goal-w", Id: id, Allow: true, Option: 2, Msg: "smaller ops"})
	if err != nil || r.GetError() != "" || r.GetDecision() != decAllow {
		t.Fatalf("Approve: %v %v", r, err)
	}
	ans := askResult(t, ch)
	if !ans.Answered || ans.Answer != "sqlite — smaller ops" {
		t.Fatalf("ask answer = %+v", ans)
	}
	it := goalByIDT(t, "ga1")
	if n := len(it.SteerLog); n == 0 || !strings.Contains(it.SteerLog[n-1].Text, "A: sqlite — smaller ops") {
		t.Fatalf("steer log = %+v", it.SteerLog)
	}
	w.nextFrame(t, "answered ask gone", func(it []*pb.InboxItem) bool { return !hasApproval(it, id) })
}

func listPendingApprovalsAsItems() []*pb.InboxItem {
	var out []*pb.InboxItem
	for _, a := range listPendingApprovals("") {
		out = append(out, &pb.InboxItem{Item: &pb.InboxItem_Approval{Approval: a}})
	}
	return out
}

// WatchInbox projects per watcher: a scoped watcher never receives another
// group's item, the operator's does.
func TestWatchInboxProjectsPerWatcher(t *testing.T) {
	inboxRig(t)
	inboxACL(t, `{"scoped":{"watch_inbox":["main"]}}`)
	op := watchInbox(t, context.Background())
	sc := watchInbox(t, inboxCtx("scoped"))
	op.nextFrame(t, "op initial", func(it []*pb.InboxItem) bool { return len(it) == 0 })
	sc.nextFrame(t, "scoped initial", func(it []*pb.InboxItem) bool { return len(it) == 0 })
	notifyDeliver("peer", "normal", "", "peer only", "")
	notifyDeliver("main", "normal", "", "main one", "")
	op.nextFrame(t, "op sees both", func(it []*pb.InboxItem) bool { return len(it) == 2 })
	got := sc.nextFrame(t, "scoped sees main", func(it []*pb.InboxItem) bool { return len(it) == 1 })
	if got[0].GetNotification().GetGroup() != "main" {
		t.Fatalf("scoped watcher got %v", inboxKinds(got))
	}
}

// ---- GoalDecide ----------------------------------------------------------------

// deny returns the goal to planning with the reason (quoted into the re-run
// plan turn, recorded as a steer); allow approves it with the message
// recorded as an operator steer. The awaiting goal is an inbox item.
func TestGoalDecideDenyReplansAllowApproves(t *testing.T) {
	inboxRig(t)
	const g = "tg"
	rec := &turnRec{}
	withTurnFn(func(_, session, msg string) error {
		rec.add(session, msg)
		return nil
	}, func() {
		if _, err := goalSet(g, "build the thing", "1. it exists", "", 0, true); err != nil {
			t.Fatalf("goalSet: %v", err)
		}
		it := waitGoal(t, g, goalStatusAwaiting)
		var plan *pb.GoalItem
		for _, x := range inboxCall(t, context.Background()) {
			if x.GetGoalPlan().GetId() == it.ID {
				plan = x.GetGoalPlan()
			}
		}
		if plan == nil || plan.GetStatus() != goalStatusAwaiting {
			t.Fatalf("awaiting goal not in the inbox: %v", plan)
		}
		s := &kotoServer{}
		ctx := inboxCtx(defaultRole)
		r, err := s.GoalDecide(ctx, &pb.GoalDecideReq{Group: g, Name: it.ID, Allow: false, Msg: "too broad, split the DB work"})
		if err != nil || r.GetError() != "" {
			t.Fatalf("deny: %v %v", r, err)
		}
		it = waitGoal(t, g, goalStatusAwaiting) // re-planned
		plans := rec.byRole(roleWork)
		if len(plans) != 2 || !strings.Contains(plans[1], "PLAN REJECTED") || !strings.Contains(plans[1], "> too broad, split the DB work") {
			t.Fatalf("re-plan turn missing the reason: %d plans, last=%q", len(plans), plans[len(plans)-1])
		}
		if n := len(it.SteerLog); n == 0 || it.SteerLog[n-1].Source != "plan rejected" || it.SteerLog[n-1].Text != "too broad, split the DB work" {
			t.Fatalf("deny steer = %+v", it.SteerLog)
		}
		if r, _ := s.GoalDecide(ctx, &pb.GoalDecideReq{Group: g, Name: it.ID, Allow: true, Msg: "ok, keep it small"}); r.GetError() != "" {
			t.Fatalf("allow: %v", r)
		}
		it = goalByIDT(t, it.ID)
		if it.Status != goalStatusRunning || it.PlanRejection != "" {
			t.Fatalf("after allow: status=%s rejection=%q", it.Status, it.PlanRejection)
		}
		if n := len(it.SteerLog); n == 0 || it.SteerLog[n-1].Source != "plan approved" || it.SteerLog[n-1].Text != "ok, keep it small" {
			t.Fatalf("allow steer = %+v", it.SteerLog)
		}
		if !strings.Contains(goalWorkerMsg(it), "ok, keep it small") {
			t.Fatal("approve message missing from the iteration prompt")
		}
		if r, _ := s.GoalDecide(ctx, &pb.GoalDecideReq{Group: g, Name: it.ID, Allow: true}); r.GetError() == "" {
			t.Fatal("deciding a running goal must fail")
		}
		if _, err := goalCancel(g, ""); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		waitGoal(t, g, goalStatusCancelled)
	})
}

// GoalDecide and InboxDismiss are operator-only: no acl.json grant — not even
// "*" — lets an agent or delegated role call them. Inbox/WatchInbox are
// ordinary grantable verbs.
func TestInboxOperatorOnlyVerbs(t *testing.T) {
	inboxRig(t)
	inboxACL(t, `{"agent":{"*":"*","goal_decide":"*","inbox_dismiss":"*"}}`)
	agent := clientIdentity{Name: "agent-tok", Roles: []string{"agent"}}
	admin := clientIdentity{Name: "op", Roles: []string{defaultRole}}
	for m, req := range map[string]any{
		pb.Koto_GoalDecide_FullMethodName:   &pb.GoalDecideReq{Group: "tg", Allow: true},
		pb.Koto_InboxDismiss_FullMethodName: &pb.InboxDismissReq{Ids: []string{"x"}},
	} {
		verb := verbFromMethod(m)
		if !adminOnlyVerbs[verb] {
			t.Fatalf("%s (%s) is not admin-only", m, verb)
		}
		err := aclCheck(context.Background(), agent, verb, req)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s by agent: %v, want PermissionDenied", verb, err)
		}
		if err := aclCheck(context.Background(), admin, verb, req); err != nil {
			t.Fatalf("%s by admin: %v", verb, err)
		}
	}
	for _, m := range []string{pb.Koto_Inbox_FullMethodName, pb.Koto_WatchInbox_FullMethodName} {
		if err := aclCheck(context.Background(), agent, verbFromMethod(m), &pb.InboxReq{}); err != nil {
			t.Fatalf("%s with a grant: %v", m, err)
		}
	}
}

// ---- notification store --------------------------------------------------------

// Per-group cap 50 (oldest evicted), total cap 500, and the store survives a
// daemon restart (in-memory state dropped, reloaded from <HERE>/inbox.json).
func TestInboxNotificationCapsAndRestart(t *testing.T) {
	inboxRig(t)
	for i := 0; i < 60; i++ {
		inboxAddNotification("tg", "normal", "", fmt.Sprintf("tg-%02d", i), "")
	}
	notes := inboxNotesSnapshot()
	if len(notes) != inboxNotifyPerGroup || notes[0].Title != "tg-10" || notes[49].Title != "tg-59" {
		t.Fatalf("per-group cap: %d notes, first=%q", len(notes), notes[0].Title)
	}
	for g := 0; g < 11; g++ {
		for i := 0; i < 50; i++ {
			inboxAddNotification(fmt.Sprintf("cap%02d", g), "high", "", fmt.Sprintf("c%d-%d", g, i), "")
		}
	}
	notes = inboxNotesSnapshot()
	if len(notes) != inboxNotifyTotal || notes[len(notes)-1].Title != "c10-49" {
		t.Fatalf("total cap: %d notes", len(notes))
	}
	for _, n := range notes {
		if n.Group == "tg" {
			t.Fatal("total cap must evict the oldest overall (tg's) first")
		}
	}
	before := inboxNotesSnapshot()

	// "Restart": forget the in-memory store; the next use reloads from disk.
	inboxMu.Lock()
	inboxLoaded, inboxNotes = "", nil
	inboxMu.Unlock()
	after := inboxNotesSnapshot()
	if len(after) != len(before) || after[0] != before[0] || after[len(after)-1] != before[len(before)-1] {
		t.Fatalf("store did not survive restart: %d → %d", len(before), len(after))
	}
	// A corrupt file starts empty instead of failing.
	if err := os.WriteFile(inboxFile(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	inboxMu.Lock()
	inboxLoaded = ""
	inboxMu.Unlock()
	if n := len(inboxNotesSnapshot()); n != 0 {
		t.Fatalf("corrupt store loaded %d notes", n)
	}
}

// InboxDismiss removes a notification for EVERY watcher and on disk.
func TestInboxDismissForAllWatchers(t *testing.T) {
	inboxRig(t)
	notifyDeliver("tg", "high", "", "keep", "")
	notifyDeliver("tg", "normal", "", "drop", "")
	var dropID string
	for _, n := range inboxNotesSnapshot() {
		if n.Title == "drop" {
			dropID = n.ID
		}
	}
	a := watchInbox(t, context.Background())
	b := watchInbox(t, context.Background())
	a.nextFrame(t, "a initial", func(it []*pb.InboxItem) bool { return hasNote(it, dropID) })
	b.nextFrame(t, "b initial", func(it []*pb.InboxItem) bool { return hasNote(it, dropID) })
	r, err := (&kotoServer{}).InboxDismiss(inboxCtx(defaultRole), &pb.InboxDismissReq{Ids: []string{dropID, "nope"}})
	if err != nil || !r.GetOk() || r.GetDismissed() != 1 {
		t.Fatalf("dismiss: %v %v", r, err)
	}
	for name, w := range map[string]*inboxStreamRec{"a": a, "b": b} {
		got := w.nextFrame(t, name+" dismissed", func(it []*pb.InboxItem) bool { return !hasNote(it, dropID) })
		if len(got) != 1 || got[0].GetNotification().GetTitle() != "keep" {
			t.Fatalf("%s after dismiss: %v", name, got)
		}
	}
	inboxMu.Lock()
	inboxLoaded = ""
	inboxMu.Unlock()
	if notes := inboxNotesSnapshot(); len(notes) != 1 || notes[0].Title != "keep" {
		t.Fatalf("dismiss not persisted: %+v", notes)
	}
}
