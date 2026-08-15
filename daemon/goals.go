package main

// goals.go — the goal loop: iterate a group toward a big task until an
// independent judge accepts the acceptance criteria.
//
// Shape (research-informed; rationale + citations in docs/goal-loop.md):
//
//   - Every goal run gets its OWN pair of sessions, named after its generated
//     id: the worker iterates in `goal-<id>`, the judge reviews in
//     `goal-<id>-judge`. The whole "goal-" namespace is reserved and
//     non-interactive (sessions.go) — the operator follows the run from its
//     tree item and never types into it, while the group's default session
//     stays free for chat. Naming per run rather than one fixed `goal-work`
//     keeps successive goals' transcripts apart and puts the run's identity
//     in the session name.
//   - The worker's CONTEXT is reset before every turn — each iteration starts
//     fresh with the filesystem as its only memory (ledger + progress files +
//     git history, all agent-maintained; the daemon never parses them). The
//     TRANSCRIPT is not: clearGoalSession resets the guest conversation and
//     leaves the host log alone, because the transcript is the only way to
//     watch a session nobody may talk to.
//   - The judge runs ONLY when the worker claims completion (`goal_done` on
//     the ctl plane), context-reset per check the same way. Its verdict
//     (`goal_verdict`) either closes the goal or feeds per-criterion feedback
//     into later iteration prompts. The loop can never end on an unverified
//     self-report.
//   - Every goal_* event carries the goal's session, so clients render the
//     lifecycle in the goal's own item rather than in the chat the operator
//     is having. The transitions that need a human (plan ready, cap reached,
//     goal met) also raise an operator notification, which is session-blind.
//   - Plan-first goals (the default) run one planning turn, then park at
//     awaiting_approval until a HUMAN approves (GoalApprove has no ctl-plane
//     counterpart — main can set a peer's goal but not approve one).
//   - max_iterations (execution turns only) bounds a loop that never
//     converges: hitting the cap pauses the goal and notifies the operator.
//   - A group runs SEVERAL goals at once — that is what the multi-session
//     machinery exists for. Each run owns its session pair, driver goroutine
//     and mailbox windows; turns from different goals interleave through the
//     group's slot pool like any other concurrent sessions (sharing one
//     workspace, so goals that fight over the same files are the caller's
//     problem, same as two chat sessions editing one repo). Verbs
//     (approve/pause/interrupt/resume/cancel) take an optional NAME and
//     resolve to the sole matching goal when it is omitted.
//
// One driver goroutine per active goal (keyed by goal ID) is the sole
// producer of that goal's turns: it enqueues onto the group's ordinary send
// queue and waits on the per-job done channel, so goal turns serialize
// naturally with operator chat and each other, and completion attribution is
// exact (the channel is per-job). The driver holds goalLock only for state
// snapshots/mutations, never across a turn.
//
// Verdict/claim delivery is a per-GOAL mailbox with an open/close window:
// the ctl verbs are accepted only while the corresponding turn is in flight
// (first write wins). The claim carries the goal id — the prompt templates
// embed it — and an id-less claim is accepted only while it is unambiguous
// (exactly one window open in the group), so concurrent goals cannot eat each
// other's verdicts.
//
// The window is authorization against ACCIDENT, not against malice: the ctl
// plane's session identity is self-declared by the guest, and every session
// in a group shares one uid, one workspace and one ctl FIFO — any in-flight
// turn could write a claim naming any open goal. That is an accepted risk: a
// worker staging artifacts to fool the same-VM judge was always possible.
// Session separation INSIDE a group is organizational, not enforceable — the
// microVM is the boundary. Use a separate group when the separation has to
// hold against a hostile agent.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	goalStatusPlanning  = "planning"
	goalStatusAwaiting  = "awaiting_approval"
	goalStatusRunning   = "running"
	goalStatusPaused    = "paused"
	goalStatusMet       = "met"
	goalStatusCancelled = "cancelled"

	goalDefaultMaxIter = 20
	goalMaxIterCeil    = 200

	// goalMaxActive caps how many goals may run CONCURRENTLY in one group.
	// Each active goal is one driver goroutine and at most one in-flight
	// turn, so the real thrash limit is the group's turn slot pool; this cap
	// just keeps a looping agent from minting drivers without bound.
	goalMaxActive = 8

	// goalNoteMax caps the decoded goal_done note / goal_verdict reasons —
	// both agent-authored, both persisted into goals.json and (for reasons)
	// embedded into later prompts.
	goalNoteMax = 4000

	// goalSilentJudgeMax pauses the goal after this many consecutive
	// done-claims whose judge check (with one retry each) produced no
	// verdict — a judge that reliably fails to report is a wedged gate, not
	// a rejection.
	goalSilentJudgeMax = 3
)

// goalEnqueueRetries × goalRetrySleep is how long the driver rides out a full
// send queue before pausing the goal. Vars, not consts: tests shrink the sleep.
var (
	goalEnqueueRetries = 3
	goalRetrySleep     = 30 * time.Second
)

var (
	goalLock    sync.Mutex
	goals       []goalItem
	goalDrivers = map[string]bool{} // keyed by goal ID — one driver per run, not per group

	// Mailboxes (guarded by goalLock), keyed by GOAL ID — several goals can
	// have turns in flight in one group at once. *Open maps goal id → owning
	// group while the corresponding turn is in flight; the ctl verbs refuse
	// when closed, and use the group value to authorize the caller and to
	// resolve id-less claims (accepted only while exactly one window is open
	// in the group).
	goalDoneOpen    = map[string]string{} // goal id → group
	goalDoneMail    = map[string]string{} // goal id → worker's evidence note
	goalVerdictOpen = map[string]string{} // goal id → group
	goalVerdictMail = map[string]goalVerdict{}
)

type goalVerdict struct {
	Met     bool
	Reasons string
}

// clearGoalSessionFn, when non-nil, replaces clearSessionContext as the
// pre-turn session reset. Solely a test seam (the real one boots VMs); nil in
// production. Not initialized to a closure over clearSessionContext directly
// — that forms a static initialization cycle (→ ensure → fcSpawn →
// ctlDispatch → goalSet → … → this var), same trap turnFn documents.
var clearGoalSessionFn func(g, sess string)

// clearGoalSession gives the next goal turn fresh context — and NOTHING more.
// It resets the guest's conversation state only (clearSessionContext), leaving
// the host log intact.
//
// It used to call clearSession, which also rewrites the log to drop the
// session's segments. That erased the goal's own history one iteration at a
// time: observed 2026-08-14 on group ALPHA, a plan-first goal past approval
// whose PLAN transcript existed in no log on the host — iteration 1's
// pre-turn clear had deleted the very turn the operator was asked to approve.
// A session nobody may type into is observable or it is nothing, so the
// transcript is the one thing the reset must not touch.
func clearGoalSession(g, sess string) {
	if clearGoalSessionFn != nil {
		clearGoalSessionFn(g, sess)
		return
	}
	if r := clearSessionContext(g, sess); !r.OK {
		// Non-fatal: the subsequent turn still runs, just without the fresh-
		// context guarantee (e.g. first iteration, where the session doesn't
		// exist yet — the reset is a no-op wrapped in an ensure()).
		emitLogfG("goal", g, "warn", "[%s] clear session %s: %s", g, sess, r.Error)
	}
}

func goalNow() float64 { return float64(time.Now().Unix()) }

// ---- store ------------------------------------------------------------------

func loadGoals() {
	goalLock.Lock()
	defer goalLock.Unlock()
	b, err := os.ReadFile(GOALS_FILE)
	if err != nil {
		return
	}
	var items []goalItem
	if err := json.Unmarshal(b, &items); err != nil {
		emitLogf("goal", "warn", "load: %v", err)
		return
	}
	goals = items
}

// saveGoalsLocked serializes the goal slice. Caller MUST hold goalLock (same
// contract as saveSched).
func saveGoalsLocked() {
	b, err := json.MarshalIndent(goals, "", "  ")
	if err != nil {
		emitLogf("goal", "error", "save marshal: %v", err)
		return
	}
	if err := os.WriteFile(GOALS_FILE, b, 0o644); err != nil {
		emitLogf("goal", "error", "save write: %v", err)
	}
}

// findGoalByIDLocked returns a pointer into the goals slice for the goal with
// this id, or nil. Caller holds goalLock; the pointer is invalid once the
// lock is released.
func findGoalByIDLocked(id string) *goalItem {
	for i := range goals {
		if goals[i].ID == id {
			return &goals[i]
		}
	}
	return nil
}

// activeGoalsLocked returns pointers to g's non-terminal goals, in slice
// (creation) order. Caller holds goalLock.
func activeGoalsLocked(g string) []*goalItem {
	var out []*goalItem
	for i := range goals {
		if goals[i].Group == g && !goalTerminal(goals[i].Status) {
			out = append(out, &goals[i])
		}
	}
	return out
}

// resolveGoalLocked picks the goal a group-scoped verb addresses. `name`
// matches the run's Name or ID; empty means "the obvious one": the sole goal
// in one of the `from` statuses (nil from = any status). Ambiguity is an
// error naming the candidates — with concurrent goals, guessing would steer
// the wrong run. Caller holds goalLock.
func resolveGoalLocked(g, name string, from []string) (*goalItem, error) {
	inFrom := func(s string) bool {
		if from == nil {
			return true
		}
		for _, f := range from {
			if s == f {
				return true
			}
		}
		return false
	}
	if name != "" {
		for i := range goals {
			if goals[i].Group == g && (goals[i].Name == name || goals[i].ID == name) {
				if !inFrom(goals[i].Status) {
					return nil, fmt.Errorf("goal %s is %s", goals[i].ID, goals[i].Status)
				}
				return &goals[i], nil
			}
		}
		return nil, fmt.Errorf("group %q has no goal named %q", g, name)
	}
	var cand []*goalItem
	var nonTerminal []*goalItem
	for i := range goals {
		if goals[i].Group != g {
			continue
		}
		if !goalTerminal(goals[i].Status) {
			nonTerminal = append(nonTerminal, &goals[i])
		}
		if inFrom(goals[i].Status) {
			cand = append(cand, &goals[i])
		}
	}
	switch len(cand) {
	case 1:
		return cand[0], nil
	case 0:
		// The old single-goal error texts, kept for the common shapes: a
		// group with exactly one goal in the wrong state reports that state.
		if len(nonTerminal) == 1 {
			return nil, fmt.Errorf("goal %s is %s", nonTerminal[0].ID, nonTerminal[0].Status)
		}
		return nil, fmt.Errorf("group %q has no goal", g)
	default:
		names := make([]string, len(cand))
		for i, it := range cand {
			names[i] = goalSessionSlug(*it)
		}
		return nil, fmt.Errorf("group %q has %d goals — name one of: %s", g, len(cand), strings.Join(names, ", "))
	}
}

func goalTerminal(status string) bool {
	return status == goalStatusMet || status == goalStatusCancelled
}

// goalLiveSessions returns the goal loop's reserved sessions that should be
// SHOWN for g — tree leaves clients can follow each run from (the sessions
// stay out of the on-disk registry: they are not sendable, and the leaves
// should vanish when a run ends, not linger like chat sessions). The worker
// is listed for the whole run; the judge only from its FIRST review turn
// (Judged) — before that the session has no transcript, and a ⚖ leaf for a
// review that hasn't happened reads as if one had. The judge used to be
// unlisted entirely on the theory that its verdicts (goal_verdict events)
// were all that mattered — but a verdict without its reasoning is exactly
// the review process the operator most wants to audit, and a session
// reachable only by hand-typing /session goal-<name>-judge is not visible.
// Once judging starts the leaf stays for the rest of the run, not just while
// turns are in flight: a rejection's transcript matters most AFTER the judge
// turn ends.
func goalLiveSessions(g string) []string {
	goalLock.Lock()
	defer goalLock.Unlock()
	var out []string
	for _, it := range activeGoalsLocked(g) {
		slug := goalSessionSlug(*it)
		out = append(out, goalWorkSessionFor(slug))
		if it.Judged {
			out = append(out, goalJudgeSessionFor(slug))
		}
	}
	return out
}

// goalTurnShouldRun is the delivery-layer guard on queued goal turns
// (sendWorker consults it before running a reserved-session job): the driver
// enqueues while its goal is active, but a pause/interrupt/cancel can land
// while the turn still sits queued behind operator chat — without this check
// the dead goal grinds one full stray iteration anyway. The session names
// the run, so the check is against THAT goal — a peer goal's state is
// irrelevant. Best-effort by design: a status change after delivery starts
// doesn't abort the turn (that's goalInterrupt's SIGINT path).
func goalTurnShouldRun(g, session string) bool {
	goalLock.Lock()
	defer goalLock.Unlock()
	for i := range goals {
		if goals[i].Group != g {
			continue
		}
		slug := goalSessionSlug(goals[i])
		if session == goalWorkSessionFor(slug) || session == goalJudgeSessionFor(slug) {
			return goals[i].Status == goalStatusRunning || goals[i].Status == goalStatusPlanning
		}
	}
	return false
}

// ---- verbs ------------------------------------------------------------------

// goalNameMax keeps a run's name short. The name is a SESSION name, so it
// must leave room for the "goal-" prefix and the judge's "-judge" suffix
// inside the 32-char session charset — but the real reason it is this short is
// that the name is what you fuzzy-jump to (ctrl+t) and read in a tree column
// barely twenty cells wide. A name you have to squint at is a name nobody uses.
const goalNameMax = 16

// slugifyGoalName turns free text into a short session-safe handle:
// lowercase, words joined by "-", cut at a word boundary when it can be. It is
// the FALLBACK — a caller (agent or human) that supplies a name gets that name
// — because a mechanical slug of a sentence is rarely the word you would
// reach for. Agents setting goals are told to pass one (prompts/global.md).
func slugifyGoalName(text string) string {
	var words []string
	cur := make([]rune, 0, 16)
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	// Drop leading filler so "find a signal in weather data" reads as
	// "signal-weather", not "find-a-signal".
	filler := map[string]bool{
		"a": true, "an": true, "the": true, "to": true, "of": true, "in": true,
		"on": true, "for": true, "and": true, "or": true, "is": true, "be": true,
		"make": true, "find": true, "get": true, "do": true, "it": true, "we": true,
	}
	kept := words[:0]
	for _, w := range words {
		if filler[w] {
			continue
		}
		kept = append(kept, w)
	}
	if len(kept) == 0 {
		kept = words
	}
	out := ""
	for _, w := range kept {
		next := w
		if out != "" {
			next = out + "-" + w
		}
		if len(next) > goalNameMax {
			break
		}
		out = next
	}
	if out == "" && len(kept) > 0 {
		out = kept[0] // single long word: cut it rather than give up
	}
	if len(out) > goalNameMax {
		out = out[:goalNameMax]
	}
	out = strings.Trim(out, "-")
	if out == "" {
		out = "goal"
	}
	return out
}

// resolveGoalName settles a run's name: the caller's if given, else a slug of
// the goal text, then uniquified within the group. Uniqueness spans both the
// group's chat sessions and its past goals, since the name becomes a session
// name and two runs sharing one would merge their transcripts.
func resolveGoalName(group, name, text string) (string, error) {
	if name == "" {
		name = slugifyGoalName(text)
	}
	if len(name) > goalNameMax {
		return "", fmt.Errorf("goal name must be at most %d characters", goalNameMax)
	}
	if !sessionNameRE.MatchString(goalWorkSessionFor(name)) ||
		!sessionNameRE.MatchString(goalJudgeSessionFor(name)) {
		return "", fmt.Errorf("goal name %q is not usable as a session name", name)
	}
	taken := map[string]bool{}
	for _, s := range listSessions(group) {
		taken[s] = true
	}
	goalLock.Lock()
	for _, it := range goals {
		if it.Group == group && it.Name != "" {
			taken[goalWorkSessionFor(it.Name)] = true
		}
	}
	goalLock.Unlock()
	base := name
	for n := 2; taken[goalWorkSessionFor(name)]; n++ {
		suffix := fmt.Sprintf("-%d", n)
		trimmed := base
		if len(trimmed)+len(suffix) > goalNameMax {
			trimmed = trimmed[:goalNameMax-len(suffix)]
		}
		name = trimmed + suffix
		if n > 99 {
			return "", fmt.Errorf("could not find a free name near %q", base)
		}
	}
	return name, nil
}

// goalSessionSlug is the token a goal's sessions are named after: its Name,
// or its ID for records written before names existed (their sessions are
// already on disk as goal-<id> and must keep resolving there).
func goalSessionSlug(it goalItem) string {
	if it.Name != "" {
		return it.Name
	}
	return it.ID
}

func goalSet(group, text, criteria, name string, maxIter int, plan bool) (goalItem, error) {
	if group == "" {
		return goalItem{}, fmt.Errorf("group is required")
	}
	if text == "" {
		return goalItem{}, fmt.Errorf("goal text is required")
	}
	if criteria == "" {
		return goalItem{}, fmt.Errorf("acceptance criteria are required")
	}
	if maxIter <= 0 {
		maxIter = goalDefaultMaxIter
	}
	if maxIter > goalMaxIterCeil {
		maxIter = goalMaxIterCeil
	}
	status := goalStatusRunning
	if plan {
		status = goalStatusPlanning
	}
	name, err := resolveGoalName(group, strings.TrimSpace(name), text)
	if err != nil {
		return goalItem{}, err
	}
	it := goalItem{
		ID:            newSchedID(),
		Group:         group,
		Name:          name,
		Text:          text,
		Criteria:      criteria,
		Plan:          plan,
		Status:        status,
		MaxIterations: maxIter,
		CreatedAt:     goalNow(),
	}
	goalLock.Lock()
	if n := len(activeGoalsLocked(group)); n >= goalMaxActive {
		goalLock.Unlock()
		return goalItem{}, fmt.Errorf("group %q already has %d active goals; finish or cancel one first", group, n)
	}
	// Setting a new goal retires the group's terminal records (the
	// generalization of the old replace-in-place): outcomes stay listed
	// until fresh work starts, and goals.json stays bounded at the actives
	// plus the last batch of results.
	kept := goals[:0]
	for _, old := range goals {
		if old.Group == group && goalTerminal(old.Status) {
			continue
		}
		kept = append(kept, old)
	}
	goals = append(kept, it)
	saveGoalsLocked()
	goalLock.Unlock()
	emitLogfG("goal", group, "info", "set id=%s name=%s group=%s plan=%t max=%d", it.ID, it.Name, group, plan, maxIter)
	emit(group, Event{Event: "goal_set", ID: it.ID, Text: text, Session: goalWorkSessionFor(it.Name)})
	startGoalDriver(group, it.ID)
	return it, nil
}

func goalList(filter string) []goalItem {
	goalLock.Lock()
	defer goalLock.Unlock()
	out := make([]goalItem, 0, len(goals))
	for _, it := range goals {
		if filter != "" && it.Group != filter {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

// goalTransition applies a status change under goalLock and returns the
// updated item. `name` addresses the goal (Name/ID; "" = the sole candidate
// — see resolveGoalLocked); `from` lists the statuses the change is valid
// from.
func goalTransition(g, name string, from []string, apply func(*goalItem)) (goalItem, error) {
	goalLock.Lock()
	defer goalLock.Unlock()
	it, err := resolveGoalLocked(g, name, from)
	if err != nil {
		return goalItem{}, err
	}
	apply(it)
	it.UpdatedAt = goalNow()
	saveGoalsLocked()
	return *it, nil
}

func goalApprove(g, name string) (goalItem, error) {
	it, err := goalTransition(g, name, []string{goalStatusAwaiting}, func(it *goalItem) {
		it.Status = goalStatusRunning
	})
	if err != nil {
		return it, err
	}
	emitLogfG("goal", g, "info", "approve id=%s", it.ID)
	emit(g, Event{Event: "goal_resumed", ID: it.ID, Text: "approved", Session: goalWorkSessionFor(goalSessionSlug(it))})
	startGoalDriver(g, it.ID)
	return it, nil
}

func goalPause(g, name string) (goalItem, error) {
	it, err := goalTransition(g, name, []string{goalStatusRunning}, func(it *goalItem) {
		it.Status = goalStatusPaused
		it.PausedReason = "operator"
	})
	if err != nil {
		return it, err
	}
	emitLogfG("goal", g, "info", "pause id=%s (operator)", it.ID)
	emit(g, Event{Event: "goal_paused", ID: it.ID, Text: "operator", Session: goalWorkSessionFor(goalSessionSlug(it))})
	return it, nil
}

// goalInterruptTurnFn, when non-nil, replaces interruptAgent for tests
// (interruptAgent execs into a live VM). Same seam pattern as
// clearGoalSessionFn.
var goalInterruptTurnFn func(g, sess string) error

// goalInterrupt is goalPause NOW: pause the goal and abort the in-flight
// worker/judge turn instead of letting it finish the iteration. The SIGINT
// is sent only when the turn currently RUNNING belongs to a reserved goal
// session — the goal's mailbox windows open before enqueue, so "goal turn in
// flight" per the mailbox can still mean "queued behind operator chat", and
// interruptAgent kills whatever agent process is running. In that queued
// case the pause alone suffices: the stray iteration runs, then the driver
// sees paused at the loop top and exits (its claim, if any, is discarded by
// the same status check in goalJudgeCheck).
func goalInterrupt(g, name string) (goalItem, error) {
	it, err := goalTransition(g, name, []string{goalStatusRunning}, func(it *goalItem) {
		it.Status = goalStatusPaused
		it.PausedReason = "interrupted"
	})
	if err != nil {
		return it, err
	}
	emitLogfG("goal", g, "info", "interrupt id=%s", it.ID)
	emit(g, Event{Event: "goal_paused", ID: it.ID, Text: "interrupted", Session: goalWorkSessionFor(goalSessionSlug(it))})
	// Aimed at the goal's own worker session alone: up to groupSlots turns run
	// at once, so a group-wide signal would abort conversations that have
	// nothing to do with this goal.
	if work := goalWorkSessionFor(goalSessionSlug(it)); sessionBusy(g, work) {
		fn := goalInterruptTurnFn
		if fn == nil {
			fn = interruptAgent
		}
		if ierr := fn(g, work); ierr != nil {
			// Non-fatal: the pause already holds; the turn just runs out.
			emitLogfG("goal", g, "warn", "interrupt turn: %v", ierr)
		}
	}
	return it, nil
}

// goalResume restarts a paused goal with a fresh iteration budget.
func goalResume(g, name string) (goalItem, error) {
	it, err := goalTransition(g, name, []string{goalStatusPaused}, func(it *goalItem) {
		it.Status = goalStatusRunning
		it.PausedReason = ""
		it.Iteration = 0
	})
	if err != nil {
		return it, err
	}
	emitLogfG("goal", g, "info", "resume id=%s", it.ID)
	emit(g, Event{Event: "goal_resumed", ID: it.ID, Session: goalWorkSessionFor(goalSessionSlug(it))})
	startGoalDriver(g, it.ID)
	return it, nil
}

func goalCancel(g, name string) (goalItem, error) {
	it, err := goalTransition(g, name,
		[]string{goalStatusPlanning, goalStatusAwaiting, goalStatusRunning, goalStatusPaused},
		func(it *goalItem) {
			it.Status = goalStatusCancelled
			it.CompletedAt = goalNow()
		})
	if err != nil {
		return it, err
	}
	emitLogfG("goal", g, "info", "cancel id=%s", it.ID)
	emit(g, Event{Event: "goal_cancelled", ID: it.ID, Session: goalWorkSessionFor(goalSessionSlug(it))})
	return it, nil
}

// goalCancelOnDestroy silently cancels every non-terminal goal when its
// group is destroyed. Unlike goalCancel it is a no-op (not an error) when
// there are none.
func goalCancelOnDestroy(g string) {
	goalLock.Lock()
	var cancelled []goalItem
	for i := range goals {
		if goals[i].Group == g && !goalTerminal(goals[i].Status) {
			goals[i].Status = goalStatusCancelled
			goals[i].CompletedAt = goalNow()
			goals[i].UpdatedAt = goalNow()
			cancelled = append(cancelled, goals[i])
		}
	}
	if len(cancelled) > 0 {
		saveGoalsLocked()
	}
	goalLock.Unlock()
	for _, it := range cancelled {
		emit(g, Event{Event: "goal_cancelled", ID: it.ID, Session: goalWorkSessionFor(goalSessionSlug(it))})
		emitLogfG("goal", g, "info", "cancelled by destroy id=%s group=%s", it.ID, g)
	}
}

// goalPauseOnStop pauses every running goal when the operator stops its
// group — otherwise a driver's next enqueue would silently re-boot the VM
// the operator just powered off. Other statuses are untouched (a terminal or
// already-paused goal, or a planning turn — the human approval gate already
// stands between a truncated plan and execution).
func goalPauseOnStop(g string) {
	goalLock.Lock()
	var paused []goalItem
	for i := range goals {
		if goals[i].Group == g && goals[i].Status == goalStatusRunning {
			goals[i].Status = goalStatusPaused
			goals[i].PausedReason = "stopped"
			goals[i].UpdatedAt = goalNow()
			paused = append(paused, goals[i])
		}
	}
	if len(paused) > 0 {
		saveGoalsLocked()
	}
	goalLock.Unlock()
	for _, it := range paused {
		emitLogfG("goal", g, "warn", "pause id=%s (group stopped)", it.ID)
		emit(g, Event{Event: "goal_paused", ID: it.ID, Text: "stopped", Session: goalWorkSessionFor(goalSessionSlug(it))})
		goalNotify(g, "high", "goal paused (group stopped)",
			fmt.Sprintf("goal %s (%s) paused at iteration %d/%d; /goals resume after restarting the group",
				it.ID, goalSessionSlug(it), it.Iteration, it.MaxIterations))
	}
}

// ---- mailboxes (ctl plane → driver) ----------------------------------------

// resolveGoalWindowLocked maps a ctl claim to its open window. The claim's
// `id` (embedded in the prompt templates) names the goal directly; an id-less
// claim — older prompts, or an agent that trimmed the template — is accepted
// only while it is unambiguous, i.e. exactly one window is open in the owning
// group. Caller holds goalLock.
func resolveGoalWindowLocked(open map[string]string, owner, id, what string) (string, error) {
	if id != "" {
		if open[id] != owner {
			return "", fmt.Errorf("no %s in flight for %q (goal %s)", what, owner, id)
		}
		return id, nil
	}
	var ids []string
	for gid, grp := range open {
		if grp == owner {
			ids = append(ids, gid)
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no %s in flight for %q", what, owner)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%d %ss in flight for %q — include the goal \"id\"", len(ids), what, owner)
	}
}

// recordGoalDone accepts the worker's completion claim, only while its turn
// is in flight (first write wins; a duplicate in the same window is ignored).
func recordGoalDone(owner, id, note string) error {
	goalLock.Lock()
	defer goalLock.Unlock()
	gid, err := resolveGoalWindowLocked(goalDoneOpen, owner, id, "goal turn")
	if err != nil {
		return err
	}
	if _, dup := goalDoneMail[gid]; !dup {
		goalDoneMail[gid] = note
	}
	return nil
}

// recordGoalVerdict accepts the judge's verdict, only while a judge turn is
// in flight (first write wins).
func recordGoalVerdict(owner, id string, met bool, reasons string) error {
	goalLock.Lock()
	defer goalLock.Unlock()
	gid, err := resolveGoalWindowLocked(goalVerdictOpen, owner, id, "goal review")
	if err != nil {
		return err
	}
	if _, dup := goalVerdictMail[gid]; !dup {
		goalVerdictMail[gid] = goalVerdict{Met: met, Reasons: reasons}
	}
	return nil
}

// ---- driver -----------------------------------------------------------------

// resumeGoalDrivers restarts drivers after a daemon restart: running goals
// continue their loop (the iteration count was persisted before the lost
// turn, so the cap still bounds everything), planning goals re-run their plan
// turn, awaiting_approval keeps waiting for the human.
func resumeGoalDrivers() {
	goalLock.Lock()
	var resume []goalItem
	for _, it := range goals {
		if it.Status == goalStatusRunning || it.Status == goalStatusPlanning {
			resume = append(resume, it)
		}
	}
	goalLock.Unlock()
	for _, it := range resume {
		emitLogfG("goal", it.Group, "info", "resuming driver id=%s group=%s after daemon start", it.ID, it.Group)
		startGoalDriver(it.Group, it.ID)
	}
}

// startGoalDriver spawns the goal's driver goroutine unless one is already
// live. Keyed by goal ID — a group runs one driver PER ACTIVE GOAL, and the
// per-id key is also what killed the replaced-goal hazard (a per-group key
// let a new goal's startGoalDriver see the old goal's driver alive and do
// nothing, stranding the new goal — observed 2026-08-14 on ALPHA). The
// driver map is the single-flight guard: the driver is the sole producer of
// its goal's turns, so no queue-level coalescing is needed.
func startGoalDriver(g, id string) {
	goalLock.Lock()
	if goalDrivers[id] {
		goalLock.Unlock()
		return
	}
	it := findGoalByIDLocked(id)
	if it == nil || it.Group != g ||
		(it.Status != goalStatusRunning && it.Status != goalStatusPlanning) {
		goalLock.Unlock()
		return
	}
	goalDrivers[id] = true
	goalLock.Unlock()
	go goalDriver(g, id)
}

func goalDriver(g, id string) {
	defer func() {
		goalLock.Lock()
		delete(goalDrivers, id)
		goalLock.Unlock()
	}()
	goalDriveOnce(g, id)
}

// goalDriveOnce runs the plan phase and iteration loop for ONE goal until an
// exit condition (pause, cancel, cap, met, record gone).
func goalDriveOnce(g, id string) {
	if !goalPlanPhase(g, id) {
		return
	}
	silentJudge := 0
	for {
		// Loop top: snapshot state, honor pause/cancel, enforce the cap.
		goalLock.Lock()
		it := findGoalByIDLocked(id)
		if it == nil || it.Status != goalStatusRunning {
			goalLock.Unlock()
			return
		}
		if it.Iteration >= it.MaxIterations {
			goalLock.Unlock()
			goalPauseWith(g, id, "cap", fmt.Sprintf("no accepted completion after %d iterations", it.MaxIterations))
			return
		}
		// iteration++ persists BEFORE the enqueue: a daemon crash mid-turn
		// must not reset the count — the monotonic iteration is what bounds
		// crash-restart loops at the cap.
		it.Iteration++
		it.UpdatedAt = goalNow()
		saveGoalsLocked()
		snap := *it
		goalLock.Unlock()

		claimed, note, ok := goalWorkerTurn(g, snap)
		if !ok {
			return // pause already applied
		}
		if !claimed {
			continue // next iteration; the judge runs only on a done-claim
		}

		verdict, got, ok := goalJudgeCheck(g, snap)
		if !ok {
			return
		}
		if !got {
			silentJudge++
			if silentJudge >= goalSilentJudgeMax {
				goalPauseWith(g, id, "judge", fmt.Sprintf("%d consecutive reviews produced no verdict", silentJudge))
				return
			}
			goalSetFeedback(id, "the reviewer produced no verdict; treat the previous completion claim as unverified and continue")
			continue
		}
		silentJudge = 0
		if !verdict.Met {
			goalSetFeedback(id, verdict.Reasons)
			emit(g, Event{Event: "goal_verdict", ID: snap.ID, Name: "unmet", Text: verdict.Reasons, Session: goalWorkSessionFor(goalSessionSlug(snap))})
			emitLogfG("goal", g, "info", "verdict id=%s unmet (iteration %d/%d)", snap.ID, snap.Iteration, snap.MaxIterations)
			continue
		}
		goalMarkMet(g, snap, note)
		return
	}
}

// goalPlanPhase runs the plan-first turn when the goal is in `planning`.
// Returns true when the driver should continue into the execution loop
// (either no plan phase was needed, or — never — a plan phase flows straight
// through: approval always goes through a fresh driver).
func goalPlanPhase(g, id string) bool {
	goalLock.Lock()
	it := findGoalByIDLocked(id)
	if it == nil {
		goalLock.Unlock()
		return false
	}
	if it.Status != goalStatusPlanning {
		cont := it.Status == goalStatusRunning
		goalLock.Unlock()
		return cont
	}
	snap := *it
	goalLock.Unlock()

	work := goalWorkSessionFor(goalSessionSlug(snap))
	clearGoalSession(g, work)
	emit(g, Event{Event: "goal_plan", ID: snap.ID, Session: work})
	emitLogfG("goal", g, "info", "plan turn id=%s", snap.ID)
	done, err := enqueueSend(g, work, goalPlanMsg(snap))
	if err != nil {
		goalPauseWith(g, id, "stalled", "plan turn could not be enqueued: "+err.Error())
		return false
	}
	if terr := <-done; terr != nil {
		goalPauseWith(g, id, "stalled", "plan turn failed: "+terr.Error())
		return false
	}
	if isStalled(g, work) {
		goalPauseWith(g, id, "stalled", "plan turn produced no turn_end (session stalled)")
		return false
	}
	if _, err := goalTransition(g, id, []string{goalStatusPlanning}, func(it *goalItem) {
		it.Status = goalStatusAwaiting
	}); err != nil {
		return false // cancelled mid-turn
	}
	emit(g, Event{Event: "goal_awaiting", ID: snap.ID, Session: goalWorkSessionFor(goalSessionSlug(snap))})
	emitLogfG("goal", g, "info", "plan ready id=%s — awaiting approval", snap.ID)
	goalNotify(g, "normal", "goal plan ready for review",
		fmt.Sprintf("goal %s (%s): review the plan in its goal session, then /goals approve %s %s (or /goals cancel)",
			snap.ID, goalSessionSlug(snap), g, goalSessionSlug(snap)))
	return false
}

// goalWorkerTurn runs one execution iteration. Returns the worker's claim
// (claimed + note) and ok=false when the driver must exit (pause applied).
func goalWorkerTurn(g string, snap goalItem) (claimed bool, note string, ok bool) {
	work := goalWorkSessionFor(goalSessionSlug(snap))
	clearGoalSession(g, work)

	goalLock.Lock()
	goalDoneOpen[snap.ID] = g
	delete(goalDoneMail, snap.ID)
	goalLock.Unlock()
	defer func() {
		goalLock.Lock()
		delete(goalDoneOpen, snap.ID)
		if n, has := goalDoneMail[snap.ID]; has {
			claimed, note = true, n
		}
		delete(goalDoneMail, snap.ID)
		goalLock.Unlock()
	}()

	emit(g, Event{Event: "goal_iter", ID: snap.ID, Text: fmt.Sprintf("%d/%d", snap.Iteration, snap.MaxIterations), Session: work})
	emitLogfG("goal", g, "info", "iteration %d/%d id=%s", snap.Iteration, snap.MaxIterations, snap.ID)

	done, err := goalEnqueue(g, work, goalWorkerMsg(snap))
	if err != nil {
		goalPauseWith(g, snap.ID, "stalled", "iteration could not be enqueued: "+err.Error())
		return false, "", false
	}
	if terr := <-done; terr != nil {
		goalPauseWith(g, snap.ID, "stalled", "iteration failed: "+terr.Error())
		return false, "", false
	}
	if isStalled(g, work) {
		goalPauseWith(g, snap.ID, "stalled", "no turn_end within the wait window (session stalled; self-heal owns the restart)")
		return false, "", false
	}
	return false, "", true // claim, if any, is filled in by the deferred mailbox read
}

// goalJudgeCheck runs the acceptance review for a done-claim: up to two judge
// turns (one retry for a silent judge). got=false means both stayed silent;
// ok=false means the driver must exit (pause applied or goal gone).
func goalJudgeCheck(g string, snap goalItem) (v goalVerdict, got bool, ok bool) {
	judge := goalJudgeSessionFor(goalSessionSlug(snap))
	for attempt := 0; attempt < 2; attempt++ {
		goalLock.Lock()
		it := findGoalByIDLocked(snap.ID)
		if it == nil || it.Status != goalStatusRunning {
			goalLock.Unlock()
			return goalVerdict{}, false, false
		}
		// First review of this run: from here the judge session exists as a
		// tree leaf (goalLiveSessions). Persisted so the leaf survives a
		// daemon restart for as long as the run does.
		if !it.Judged {
			it.Judged = true
			it.UpdatedAt = goalNow()
			saveGoalsLocked()
		}
		goalLock.Unlock()

		clearGoalSession(g, judge)
		goalLock.Lock()
		goalVerdictOpen[snap.ID] = g
		delete(goalVerdictMail, snap.ID)
		goalLock.Unlock()

		emit(g, Event{Event: "goal_judge", ID: snap.ID, Session: goalWorkSessionFor(goalSessionSlug(snap))})
		emitLogfG("goal", g, "info", "judge check id=%s (attempt %d)", snap.ID, attempt+1)

		done, err := goalEnqueue(g, judge, goalJudgeMsg(snap))
		var terr error
		if err == nil {
			terr = <-done
		}
		goalLock.Lock()
		delete(goalVerdictOpen, snap.ID)
		verdict, has := goalVerdictMail[snap.ID]
		delete(goalVerdictMail, snap.ID)
		goalLock.Unlock()
		if err != nil {
			goalPauseWith(g, snap.ID, "stalled", "judge turn could not be enqueued: "+err.Error())
			return goalVerdict{}, false, false
		}
		if terr != nil {
			goalPauseWith(g, snap.ID, "stalled", "judge turn failed: "+terr.Error())
			return goalVerdict{}, false, false
		}
		if isStalled(g, judge) {
			goalPauseWith(g, snap.ID, "stalled", "judge turn produced no turn_end (session stalled)")
			return goalVerdict{}, false, false
		}
		if has {
			return verdict, true, true
		}
	}
	return goalVerdict{}, false, true
}

// goalEnqueue is enqueueSend with a bounded ride-out of a full queue (the
// group may be busy with operator chat; the goal can wait a few beats).
func goalEnqueue(g, session, msg string) (<-chan error, error) {
	var lastErr error
	for i := 0; i < goalEnqueueRetries; i++ {
		done, err := enqueueSend(g, session, msg)
		if err == nil {
			return done, nil
		}
		lastErr = err
		time.Sleep(goalRetrySleep)
	}
	return nil, lastErr
}

func goalSetFeedback(id, feedback string) {
	goalLock.Lock()
	if it := findGoalByIDLocked(id); it != nil {
		it.LastFeedback = truncateRunes(feedback, goalNoteMax)
		it.UpdatedAt = goalNow()
		saveGoalsLocked()
	}
	goalLock.Unlock()
}

func goalMarkMet(g string, snap goalItem, note string) {
	if _, err := goalTransition(g, snap.ID, []string{goalStatusRunning}, func(it *goalItem) {
		it.Status = goalStatusMet
		it.DoneNote = note
		it.LastFeedback = ""
		it.CompletedAt = goalNow()
	}); err != nil {
		return
	}
	emit(g, Event{Event: "goal_verdict", ID: snap.ID, Name: "met", Session: goalWorkSessionFor(goalSessionSlug(snap))})
	emit(g, Event{Event: "goal_met", ID: snap.ID, Text: note, Session: goalWorkSessionFor(goalSessionSlug(snap))})
	emitLogfG("goal", g, "info", "MET id=%s after %d iteration(s)", snap.ID, snap.Iteration)
	goalNotify(g, "normal", "goal met",
		fmt.Sprintf("goal %s accepted by the reviewer after %d iteration(s)", snap.ID, snap.Iteration))
}

// goalPauseWith pauses an active goal with a reason and alerts the operator.
// No-op if the goal moved to a terminal/paused state meanwhile.
func goalPauseWith(g, id, reason, detail string) {
	it, err := goalTransition(g, id, []string{goalStatusRunning, goalStatusPlanning}, func(it *goalItem) {
		it.Status = goalStatusPaused
		it.PausedReason = reason
	})
	if err != nil {
		return
	}
	emit(g, Event{Event: "goal_paused", ID: it.ID, Text: reason, Session: goalWorkSessionFor(goalSessionSlug(it))})
	emitLogfG("goal", g, "warn", "paused id=%s reason=%s: %s", it.ID, reason, detail)
	goalNotify(g, "high", "goal paused ("+reason+")",
		fmt.Sprintf("goal %s (%s) at iteration %d/%d: %s — /goals resume %s %s to continue",
			it.ID, goalSessionSlug(it), it.Iteration, it.MaxIterations, detail, g, goalSessionSlug(it)))
}

// goalNotify raises an operator notification against the goal's group. A var
// as a test seam — notifyDeliver arms the group's log tailer.
var goalNotify = func(g, sev, title, msg string) {
	notifyDeliver(g, sev, "", title, msg)
}

// ---- prompts ----------------------------------------------------------------

// The prompts are the harness contract with the worker/judge. Design notes
// (evidence-before-verdict, artifacts-not-claims, one item per iteration,
// feedback shape) are research-backed — see docs/goal-loop.md.

func goalPlanMsg(it goalItem) string {
	return fmt.Sprintf(`[koto goal %s — PLAN]
You have been given a goal. This turn, plan only — do not start implementing:
1. mkdir -p /workspace/goal. Decompose the goal into concrete work items in
   /workspace/goal/ledger.json: [{"id":1,"item":"...","done":false}, ...].
   Items must be individually verifiable.
2. Note key decisions and risks in /workspace/goal/progress.md.
3. If /workspace should be a git repo and is not one yet, git init and commit.
4. End your reply with a concise summary of the plan — a human reviews it and
   must approve before execution starts.

GOAL:
%s

ACCEPTANCE CRITERIA (an independent reviewer will verify these against
artifacts before the goal can close):
%s`, it.ID, it.Text, it.Criteria)
}

func goalWorkerMsg(it goalItem) string {
	feedback := ""
	if it.LastFeedback != "" {
		feedback = fmt.Sprintf(`
REVIEWER FEEDBACK (your last completion claim was rejected — address this first):
%s
`, it.LastFeedback)
	}
	return fmt.Sprintf(`[koto goal %s — iteration %d/%d]
You are working toward a goal. Your context is fresh; your memory is the
filesystem. Startup ritual, in order:
1. Read /workspace/goal/ledger.json and /workspace/goal/progress.md; run
   git log --oneline -20. If the ledger does not exist yet, create it now
   (mkdir -p /workspace/goal; decompose the goal into individually verifiable
   items with "done": false) before doing anything else.
2. Verify the current state still works before building on it.
3. Pick ONE unfinished ledger item (prioritize the reviewer feedback, if any)
   and complete it.
4. Verify what you did, mark the item done in the ledger, append what you did
   and decided to progress.md, and git commit at a good state.
%s
GOAL:
%s

ACCEPTANCE CRITERIA:
%s

When — and only when — EVERY acceptance criterion is met, re-verify each one
by actually running the relevant commands (claims are not verification), then
report completion (an independent reviewer will verify before the goal
closes; put a short evidence summary in R):
  R="criterion 1: <command> passed; criterion 2: ..."
  printf '{"cmd":"goal_done","id":"%s","note":"%%s"}\n' \
    "$(printf '%%s' "$R" | base64 -w 0)" > /workspace/.cs/ctl
Otherwise just end your turn; you will be re-invoked.`,
		it.ID, it.Iteration, it.MaxIterations, feedback, it.Text, it.Criteria, it.ID)
}

func goalJudgeMsg(it goalItem) string {
	return fmt.Sprintf(`[koto goal review — goal %s, after iteration %d/%d]
You are an independent acceptance reviewer with shell access to /workspace.
A worker agent claims this goal is complete. Judge STRICTLY whether each
acceptance criterion is met:
- For EACH criterion, FIRST write down what concrete evidence (command
  output, file content) would prove it — before inspecting anything.
- Then verify by running commands and reading artifacts yourself.
- The worker's notes, ledger, and claims are orientation, NEVER evidence.
  Unproven = FAIL. Do not fix anything.
- Verdict per criterion: PASS with the evidence you observed, or FAIL with
  three parts: what failed, what you observed, and what would be acceptable.
- Overall: met only if EVERY criterion passes.

GOAL:
%s

ACCEPTANCE CRITERIA:
%s

Report the verdict by running EXACTLY one of these (mandatory — a review
without a verdict is discarded):
  printf '{"cmd":"goal_verdict","id":"%s","met":true,"reasons":""}\n' > /workspace/.cs/ctl
  # or, unmet — put the per-criterion failures in R first:
  R="criterion 2 FAIL: observed ...; acceptable would be ..."
  printf '{"cmd":"goal_verdict","id":"%s","met":false,"reasons":"%%s"}\n' \
    "$(printf '%%s' "$R" | base64 -w 0)" > /workspace/.cs/ctl`,
		it.ID, it.Iteration, it.MaxIterations, it.Text, it.Criteria, it.ID, it.ID)
}
