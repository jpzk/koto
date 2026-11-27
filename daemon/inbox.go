package main

// inbox.go — the operator inbox (docs/inbox.md, protocol/koto.proto rpc Inbox).
//
// The inbox is a PROJECTION of state the daemon already keeps, plus one new
// store:
//
//   - approval  — every open entry of the approvals table (approvals.go):
//     tool approvals of confirm sessions and goal decision requests
//     (kind=ask, goal_ask.go). Answered through the existing Approve RPC.
//   - goal_plan — every goal in status awaiting_approval (goals.go).
//     Answered through GoalDecide (below).
//   - notification — the one new store: every notifyDeliver (the single
//     notification funnel, logtail.go) is also appended here, bounded per
//     group and in total, persisted to <KOTO_HOME>/inbox.json so it survives
//     a daemon restart, and removed only by InboxDismiss.
//
// Nothing here decides anything on its own: answers dispatch to the existing
// decision paths. Origin (group/session/kind) always comes from daemon state
// — the approvals key, the goal record, the notifyDeliver caller — never from
// agent-authored text.
//
// ACL: Inbox / WatchInbox are verb-only RPCs whose answer is PROJECTED per
// item through the caller's grant targets for the verb (visibleTargets), the
// List / WatchState treatment. GoalDecide and InboxDismiss are operator-only
// (adminOnlyVerbs), like Approve and GoalAmend.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"koto-protocol/pb"
)

const (
	// inboxNotifyPerGroup / inboxNotifyTotal bound the notification store:
	// the oldest undismissed entry of a group is evicted past the per-group
	// cap, the oldest overall past the total cap.
	inboxNotifyPerGroup = 50
	inboxNotifyTotal    = 500
	// inboxWatchTick is how often watchers are re-checked without a kick.
	inboxWatchTick = time.Second
)

// inboxNote is one stored notification (pb.InboxNotification on the wire).
type inboxNote struct {
	ID       string  `json:"id"`
	Group    string  `json:"group"`
	Session  string  `json:"session,omitempty"`
	Severity string  `json:"severity"`
	Title    string  `json:"title"`
	Body     string  `json:"body,omitempty"`
	Ts       float64 `json:"ts"`
	// Source is who raised it: inboxSourceGuest | inboxSourceDaemon, ""
	// for notes stored before the field existed.
	Source string `json:"source,omitempty"`
}

const (
	inboxSourceGuest  = "guest"  // the group's own cs-notify (ctl notify)
	inboxSourceDaemon = "daemon" // raised by the daemon about Group
)

// inboxSource clamps a source to the known set ("" otherwise).
func inboxSource(s string) string {
	switch s {
	case inboxSourceGuest, inboxSourceDaemon:
		return s
	}
	return ""
}

var (
	inboxMu     sync.Mutex
	inboxNotes  []inboxNote // oldest first
	inboxLoaded string      // path the in-memory store was loaded from ("" = not yet)
	inboxSeq    uint64
	// inboxKick wakes the watch loop early after a change (non-blocking).
	inboxKick = make(chan struct{}, 1)
)

// inboxFile is the store path; "" (memory only) when no state root is set
// (unit tests that never point HERE anywhere).
func inboxFile() string {
	if HERE == "" {
		return ""
	}
	return filepath.Join(HERE, "inbox.json")
}

// inboxLoadLocked (re)loads the store from disk the first time it is used
// for the current state root. A missing or corrupt file starts empty —
// notifications are an operator convenience, never a gate.
func inboxLoadLocked() {
	p := inboxFile()
	if inboxLoaded == "@"+p {
		return
	}
	inboxLoaded = "@" + p // "@" prefix: the memory-only store ("") is a valid root too
	inboxNotes = nil
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var in []inboxNote
	if json.Unmarshal(b, &in) != nil {
		emitLogf("inbox", "warn", "inbox.json unreadable — starting with an empty notification store")
		return
	}
	for _, n := range in {
		if n.ID == "" || !validGroupName(n.Group) {
			continue
		}
		n.Source = inboxSource(n.Source)
		inboxNotes = append(inboxNotes, n)
	}
	inboxEvictLocked()
}

// inboxEvictLocked enforces both caps, dropping the oldest first.
func inboxEvictLocked() {
	per := map[string]int{}
	for _, n := range inboxNotes {
		per[n.Group]++
	}
	kept := inboxNotes[:0]
	for _, n := range inboxNotes {
		if per[n.Group] > inboxNotifyPerGroup {
			per[n.Group]--
			continue
		}
		kept = append(kept, n)
	}
	inboxNotes = kept
	if over := len(inboxNotes) - inboxNotifyTotal; over > 0 {
		inboxNotes = append([]inboxNote(nil), inboxNotes[over:]...)
	}
}

// inboxSaveLocked writes the store atomically (temp + rename).
func inboxSaveLocked() {
	b, err := json.Marshal(inboxNotes)
	if err != nil {
		return
	}
	p := inboxFile()
	if p == "" {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		emitLogf("inbox", "warn", "save: %v", err) // warn: error would re-enter notifyDeliver
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		emitLogf("inbox", "warn", "save: %v", err) // warn: error would re-enter notifyDeliver
	}
}

// inboxAddNotification is called by notifyDeliver with the already
// flattened + truncated title/body. g/session/sev/source come from the
// caller (daemon state), never from the text. g is the group the note is
// ABOUT — for a daemon-raised alert that is the subject group, even though
// its banner rides main's stream.
func inboxAddNotification(g, sev, session, title, body, source string) {
	if sev != "high" {
		sev = "normal"
	}
	now := time.Now()
	inboxMu.Lock()
	inboxLoadLocked()
	inboxSeq++
	n := inboxNote{
		ID:    fmt.Sprintf("n%d-%d", now.UnixMilli(), inboxSeq),
		Group: g, Session: session, Severity: sev,
		Title: sanitize(title), Body: sanitize(body),
		Ts:     float64(now.UnixMilli()) / 1000,
		Source: inboxSource(source),
	}
	inboxNotes = append(inboxNotes, n)
	inboxEvictLocked()
	inboxSaveLocked()
	inboxMu.Unlock()
	inboxChanged()
}

// inboxDismiss removes the given notification ids for every client.
func inboxDismiss(ids []string) int {
	want := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return 0
	}
	inboxMu.Lock()
	inboxLoadLocked()
	kept := inboxNotes[:0]
	n := 0
	for _, x := range inboxNotes {
		if want[x.ID] {
			n++
			continue
		}
		kept = append(kept, x)
	}
	inboxNotes = kept
	if n > 0 {
		inboxSaveLocked()
	}
	inboxMu.Unlock()
	if n > 0 {
		inboxChanged()
	}
	return n
}

func inboxNotesSnapshot() []inboxNote {
	inboxMu.Lock()
	defer inboxMu.Unlock()
	inboxLoadLocked()
	return append([]inboxNote(nil), inboxNotes...)
}

// inboxChanged wakes the watch loop (approvals opened/resolved, goal plan
// state, notifications). Never blocks; the 1 s tick covers anything missed.
func inboxChanged() {
	select {
	case inboxKick <- struct{}{}:
	default:
	}
}

// inboxItemGroup is the group an item belongs to (the ACL projection key).
func inboxItemGroup(it *pb.InboxItem) string {
	switch x := it.GetItem().(type) {
	case *pb.InboxItem_Approval:
		return x.Approval.GetGroup()
	case *pb.InboxItem_GoalPlan:
		return x.GoalPlan.GetGroup()
	case *pb.InboxItem_Notification:
		return x.Notification.GetGroup()
	}
	return ""
}

// inboxItemTs is an item's sort time: when it arrived.
func inboxItemTs(it *pb.InboxItem) float64 {
	switch x := it.GetItem().(type) {
	case *pb.InboxItem_Approval:
		return x.Approval.GetCreated()
	case *pb.InboxItem_GoalPlan:
		return x.GoalPlan.GetUpdatedAt()
	case *pb.InboxItem_Notification:
		return x.Notification.GetTs()
	}
	return 0
}

func inboxItemKey(it *pb.InboxItem) string {
	switch x := it.GetItem().(type) {
	case *pb.InboxItem_Approval:
		a := x.Approval
		return "a\x00" + a.GetGroup() + "\x00" + a.GetSession() + "\x00" + a.GetId()
	case *pb.InboxItem_GoalPlan:
		return "g\x00" + x.GoalPlan.GetId()
	case *pb.InboxItem_Notification:
		return "n\x00" + x.Notification.GetId()
	}
	return ""
}

// inboxBuild assembles the full (unprojected) inbox, newest first.
func inboxBuild() []*pb.InboxItem {
	var out []*pb.InboxItem
	for _, a := range listPendingApprovals("") {
		out = append(out, &pb.InboxItem{Item: &pb.InboxItem_Approval{Approval: a}})
	}
	for _, g := range goalList("") {
		if g.Status != goalStatusAwaiting {
			continue
		}
		out = append(out, &pb.InboxItem{Item: &pb.InboxItem_GoalPlan{GoalPlan: toPBGoalItem(g)}})
	}
	for _, n := range inboxNotesSnapshot() {
		out = append(out, &pb.InboxItem{Item: &pb.InboxItem_Notification{Notification: &pb.InboxNotification{
			Id: n.ID, Group: n.Group, Session: n.Session, Severity: n.Severity,
			Title: n.Title, Body: n.Body, Ts: n.Ts, Source: n.Source,
		}}})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := inboxItemTs(out[i]), inboxItemTs(out[j])
		if ti != tj {
			return ti > tj
		}
		return inboxItemKey(out[i]) < inboxItemKey(out[j])
	})
	return out
}

// inboxProject keeps the items of groups vis covers.
func inboxProject(items []*pb.InboxItem, vis targetSet) []*pb.InboxItem {
	if vis.any {
		return items
	}
	out := make([]*pb.InboxItem, 0, len(items))
	for _, it := range items {
		if vis.covers(inboxItemGroup(it)) {
			out = append(out, it)
		}
	}
	return out
}

// inboxVisible is the caller's view for verb: everything for an in-process
// caller (no identity), else the union of its roles' grant targets.
func inboxVisible(ctx context.Context, verb string) targetSet {
	id := identityOf(ctx)
	if id.Name == "" {
		return targetSet{any: true}
	}
	return visibleTargets(loadACL(), id.Roles, verb)
}

// inboxHash is the canonical comparison form of a snapshot (no timestamp).
func inboxHash(items []*pb.InboxItem) string {
	var b strings.Builder
	for _, it := range items {
		k := inboxItemKey(it)
		fmt.Fprintf(&b, "%d:%s|", len(k), k)
		if g := it.GetGoalPlan(); g != nil {
			fmt.Fprintf(&b, "%s|%d|", g.GetStatus(), g.GetVersion())
		}
	}
	return b.String()
}

func inboxResp(items []*pb.InboxItem) *pb.InboxResp {
	return &pb.InboxResp{Items: items, Ts: float64(time.Now().UnixMilli()) / 1000}
}

func (s *kotoServer) Inbox(ctx context.Context, _ *pb.InboxReq) (*pb.InboxResp, error) {
	return inboxResp(inboxProject(inboxBuild(), inboxVisible(ctx, "inbox"))), nil
}

type inboxSub struct {
	ch       chan *pb.InboxResp
	vis      targetSet
	lastSent string
}

var (
	inboxSubsMu   sync.Mutex
	inboxSubs     []*inboxSub
	inboxLoopOnce sync.Once
)

// WatchInbox: first frame = the full projected set, then a full snapshot on
// every change (WatchState's model: idempotent snapshots, no seq/cursor).
func (s *kotoServer) WatchInbox(_ *pb.InboxReq, stream pb.Koto_WatchInboxServer) error {
	ctx := stream.Context()
	sub := &inboxSub{ch: make(chan *pb.InboxResp, 4), vis: inboxVisible(ctx, "watch_inbox")}
	items := inboxProject(inboxBuild(), sub.vis)
	sub.lastSent = inboxHash(items)
	inboxSubsMu.Lock()
	inboxSubs = append(inboxSubs, sub)
	inboxSubsMu.Unlock()
	inboxLoopOnce.Do(func() { go inboxWatchLoop() })
	defer func() {
		inboxSubsMu.Lock()
		kept := inboxSubs[:0]
		for _, x := range inboxSubs {
			if x != sub {
				kept = append(kept, x)
			}
		}
		inboxSubs = kept
		inboxSubsMu.Unlock()
	}()
	if err := stream.Send(inboxResp(items)); err != nil {
		return err
	}
	for {
		select {
		case f := <-sub.ch:
			if err := stream.Send(f); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// inboxWatchLoop recomputes the inbox on a kick or every tick while someone
// watches, and pushes each watcher whose projected view changed. Delivery
// is non-blocking; lastSent advances only on a successful hand-off, so a
// slow watcher simply gets the newer snapshot on a later round.
func inboxWatchLoop() {
	t := time.NewTicker(inboxWatchTick)
	defer t.Stop()
	for {
		select {
		case <-inboxKick:
		case <-t.C:
		}
		inboxSubsMu.Lock()
		n := len(inboxSubs)
		inboxSubsMu.Unlock()
		if n == 0 {
			continue
		}
		all := inboxBuild()
		inboxSubsMu.Lock()
		for _, w := range inboxSubs {
			view := inboxProject(all, w.vis)
			h := inboxHash(view)
			if h == w.lastSent {
				continue
			}
			select {
			case w.ch <- inboxResp(view):
				w.lastSent = h
			default:
			}
		}
		inboxSubsMu.Unlock()
	}
}

// ---- answers ------------------------------------------------------------------

// GoalDecide answers a goal plan awaiting approval. Operator-only
// (adminOnlyVerbs goal_decide).
func (s *kotoServer) GoalDecide(ctx context.Context, r *pb.GoalDecideReq) (*pb.GoalResp, error) {
	who := identityOf(ctx).Name
	return goalGroupRPC(r.Group, r.Name, func(g, name string) (goalItem, error) {
		return goalDecide(g, name, r.Allow, r.Msg, who)
	})
}

// goalDecide: allow = goalApprove + the message recorded as an operator steer
// (source "plan approved"); deny = the goal returns to planning with the
// message as the re-plan reason (recorded as a steer too, so it outlives the
// plan turn), and the driver re-runs the plan turn.
func goalDecide(g, name string, allow bool, msg, who string) (goalItem, error) {
	msg = strings.TrimSpace(truncateRunes(flattenInline(msg), goalSteerTextMax))
	if allow {
		it, err := goalApprove(g, name)
		if err != nil {
			return it, err
		}
		if msg != "" {
			goalAddSteer(g, goalWorkSessionFor(goalSessionSlug(it)), "plan approved", who, msg)
		}
		inboxChanged()
		return it, nil
	}
	reason := msg
	if reason == "" {
		reason = "the operator rejected the plan"
	}
	it, err := goalTransition(g, name, []string{goalStatusAwaiting}, func(it *goalItem) {
		it.Status = goalStatusPlanning
		it.PlanRejection = reason
	})
	if err != nil {
		return it, err
	}
	goalAddSteer(g, goalWorkSessionFor(goalSessionSlug(it)), "plan rejected", who, reason)
	emitLogfG("goal", g, "info", "plan rejected id=%s by %s — re-planning", it.ID, approvalWho(who))
	emit(g, Event{Event: "goal_plan_rejected", ID: it.ID, Text: reason, Session: goalWorkSessionFor(goalSessionSlug(it))})
	startGoalDriver(g, it.ID)
	inboxChanged()
	return it, nil
}

// InboxDismiss removes notifications for every client. Operator-only
// (adminOnlyVerbs inbox_dismiss).
func (s *kotoServer) InboxDismiss(_ context.Context, r *pb.InboxDismissReq) (*pb.InboxDismissResp, error) {
	if len(r.Ids) > inboxNotifyTotal {
		return &pb.InboxDismissResp{Error: fmt.Sprintf("at most %d ids per call", inboxNotifyTotal)}, nil
	}
	n := inboxDismiss(r.Ids)
	return &pb.InboxDismissResp{Ok: true, Dismissed: int32(n)}, nil
}
