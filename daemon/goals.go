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
//
// One driver goroutine per active goal is the sole producer of goal turns: it
// enqueues onto the group's ordinary send queue and waits on the per-job done
// channel, so goal turns serialize naturally with operator chat on the same
// group and completion attribution is exact (the channel is per-job). The
// driver holds goalLock only for state snapshots/mutations, never across a
// turn.
//
// Verdict/claim delivery is a per-group mailbox with an open/close window:
// the ctl verbs are accepted only while the corresponding turn is in flight
// (first write wins).
//
// That window used to be sufficient BY ITSELF, because a group ran one turn at
// a time: only the goal's own turn could be running while a window was open.
// Concurrent turns (queue.go's slot pool) removed that argument. Any session
// running at the same time can now write a claim the window accepts — the ctl
// plane's session field is self-declared by the guest, and every session in a
// group shares one uid, one workspace and one ctl FIFO.
//
// This widens an already-accepted risk rather than opening a new class: a
// worker staging artifacts to fool the same-VM judge was always possible.
// The rule it makes explicit is that session separation INSIDE a group is
// organizational, not enforceable — the microVM is the boundary. Use a
// separate group when the separation has to hold against a hostile agent.

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
	goalDrivers = map[string]bool{}

	// Mailboxes (guarded by goalLock). *Open maps group → goal id while the
	// corresponding turn is in flight; the ctl verbs refuse when closed.
	goalDoneOpen    = map[string]string{}
	goalDoneMail    = map[string]string{} // worker's evidence note
	goalVerdictOpen = map[string]string{}
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

// findGoalLocked returns a pointer into the goals slice for g's goal, or nil.
// Caller holds goalLock; the pointer is invalid once the lock is released.
func findGoalLocked(g string) *goalItem {
	for i := range goals {
		if goals[i].Group == g {
			return &goals[i]
		}
	}
	return nil
}

func goalTerminal(status string) bool {
	return status == goalStatusMet || status == goalStatusCancelled
}

// goalLiveSessions returns the goal loop's reserved sessions that should be
// SHOWN for g — the worker session while a non-terminal goal exists, so
// clients get a navigable tree leaf to follow the work from (the sessions
// stay out of the on-disk registry: they are not sendable, and the leaf
// should vanish when the goal ends, not linger like a chat session). The
// judge session is deliberately not listed — its verdicts surface as
// goal_verdict events; the transcript stays reachable via /session
// goal-judge for the curious.
func goalLiveSessions(g string) []string {
	goalLock.Lock()
	defer goalLock.Unlock()
	if it := findGoalLocked(g); it != nil && !goalTerminal(it.Status) {
		return []string{goalWorkSessionFor(goalSessionSlug(*it))}
	}
	return nil
}

// goalTurnShouldRun is the delivery-layer guard on queued goal turns
// (sendWorker consults it before running a reserved-session job): the driver
// enqueues while the goal is active, but a pause/interrupt/cancel can land
// while the turn still sits queued behind operator chat — without this check
// the dead goal grinds one full stray iteration anyway. Best-effort by
// design: a status change after delivery starts doesn't abort the turn
// (that's goalInterrupt's SIGINT path).
func goalTurnShouldRun(g string) bool {
	goalLock.Lock()
	defer goalLock.Unlock()
	it := findGoalLocked(g)
	return it != nil && (it.Status == goalStatusRunning || it.Status == goalStatusPlanning)
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
	if cur := findGoalLocked(group); cur != nil {
		if !goalTerminal(cur.Status) {
			goalLock.Unlock()
			return goalItem{}, fmt.Errorf("group %q already has a goal (%s, %s); cancel it first", group, cur.ID, cur.Status)
		}
		// Replace the terminal goal in place.
		*cur = it
	} else {
		goals = append(goals, it)
	}
	saveGoalsLocked()
	goalLock.Unlock()
	emitLogfG("goal", group, "info", "set id=%s name=%s group=%s plan=%t max=%d", it.ID, it.Name, group, plan, maxIter)
	emit(group, Event{Event: "goal_set", ID: it.ID, Text: text, Session: goalWorkSessionFor(it.Name)})
	startGoalDriver(group)
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
// updated item. `from` lists the statuses the change is valid from.
func goalTransition(g string, from []string, apply func(*goalItem)) (goalItem, error) {
	goalLock.Lock()
	defer goalLock.Unlock()
	it := findGoalLocked(g)
	if it == nil {
		return goalItem{}, fmt.Errorf("group %q has no goal", g)
	}
	ok := false
	for _, s := range from {
		if it.Status == s {
			ok = true
			break
		}
	}
	if !ok {
		return goalItem{}, fmt.Errorf("goal %s is %s", it.ID, it.Status)
	}
	apply(it)
	it.UpdatedAt = goalNow()
	saveGoalsLocked()
	return *it, nil
}

func goalApprove(g string) (goalItem, error) {
	it, err := goalTransition(g, []string{goalStatusAwaiting}, func(it *goalItem) {
		it.Status = goalStatusRunning
	})
	if err != nil {
		return it, err
	}
	emitLogfG("goal", g, "info", "approve id=%s", it.ID)
	emit(g, Event{Event: "goal_resumed", ID: it.ID, Text: "approved", Session: goalWorkSessionFor(goalSessionSlug(it))})
	startGoalDriver(g)
	return it, nil
}

func goalPause(g string) (goalItem, error) {
	it, err := goalTransition(g, []string{goalStatusRunning}, func(it *goalItem) {
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
func goalInterrupt(g string) (goalItem, error) {
	it, err := goalTransition(g, []string{goalStatusRunning}, func(it *goalItem) {
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
func goalResume(g string) (goalItem, error) {
	it, err := goalTransition(g, []string{goalStatusPaused}, func(it *goalItem) {
		it.Status = goalStatusRunning
		it.PausedReason = ""
		it.Iteration = 0
	})
	if err != nil {
		return it, err
	}
	emitLogfG("goal", g, "info", "resume id=%s", it.ID)
	emit(g, Event{Event: "goal_resumed", ID: it.ID, Session: goalWorkSessionFor(goalSessionSlug(it))})
	startGoalDriver(g)
	return it, nil
}

func goalCancel(g string) (goalItem, error) {
	it, err := goalTransition(g,
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

// goalCancelOnDestroy silently cancels any non-terminal goal when its group is
// destroyed. Unlike goalCancel it is a no-op (not an error) without one.
func goalCancelOnDestroy(g string) {
	if _, err := goalCancel(g); err == nil {
		emitLogfG("goal", g, "info", "cancelled by destroy group=%s", g)
	}
}

// goalPauseOnStop pauses a running goal when the operator stops its group —
// otherwise the driver's next enqueue would silently re-boot the VM the
// operator just powered off. No-op for any other status (a terminal or
// already-paused goal, or a planning turn — the human approval gate already
// stands between a truncated plan and execution).
func goalPauseOnStop(g string) {
	it, err := goalTransition(g, []string{goalStatusRunning}, func(it *goalItem) {
		it.Status = goalStatusPaused
		it.PausedReason = "stopped"
	})
	if err != nil {
		return
	}
	emitLogfG("goal", g, "warn", "pause id=%s (group stopped)", it.ID)
	emit(g, Event{Event: "goal_paused", ID: it.ID, Text: "stopped", Session: goalWorkSessionFor(goalSessionSlug(it))})
	goalNotify(g, "high", "goal paused (group stopped)",
		fmt.Sprintf("goal %s paused at iteration %d/%d; /goal resume after restarting the group", it.ID, it.Iteration, it.MaxIterations))
}

// ---- mailboxes (ctl plane → driver) ----------------------------------------

// recordGoalDone accepts the worker's completion claim, only while its turn
// is in flight (first write wins; a duplicate in the same window is ignored).
func recordGoalDone(owner, note string) error {
	goalLock.Lock()
	defer goalLock.Unlock()
	if goalDoneOpen[owner] == "" {
		return fmt.Errorf("no goal turn in flight for %q", owner)
	}
	if _, dup := goalDoneMail[owner]; !dup {
		goalDoneMail[owner] = note
	}
	return nil
}

// recordGoalVerdict accepts the judge's verdict, only while a judge turn is
// in flight (first write wins).
func recordGoalVerdict(owner string, met bool, reasons string) error {
	goalLock.Lock()
	defer goalLock.Unlock()
	if goalVerdictOpen[owner] == "" {
		return fmt.Errorf("no goal review in flight for %q", owner)
	}
	if _, dup := goalVerdictMail[owner]; !dup {
		goalVerdictMail[owner] = goalVerdict{Met: met, Reasons: reasons}
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
	var resume []string
	for _, it := range goals {
		if it.Status == goalStatusRunning || it.Status == goalStatusPlanning {
			resume = append(resume, it.Group)
		}
	}
	goalLock.Unlock()
	for _, g := range resume {
		emitLogfG("goal", g, "info", "resuming driver group=%s after daemon start", g)
		startGoalDriver(g)
	}
}

// startGoalDriver spawns g's driver goroutine unless one is already live.
// The driver map is the single-flight guard: the driver is the sole producer
// of goal turns, so no queue-level coalescing is needed.
func startGoalDriver(g string) {
	goalLock.Lock()
	if goalDrivers[g] {
		goalLock.Unlock()
		return
	}
	it := findGoalLocked(g)
	if it == nil || (it.Status != goalStatusRunning && it.Status != goalStatusPlanning) {
		goalLock.Unlock()
		return
	}
	goalDrivers[g] = true
	goalLock.Unlock()
	go goalDriver(g)
}

func goalDriver(g string) {
	defer func() {
		goalLock.Lock()
		delete(goalDrivers, g)
		goalLock.Unlock()
	}()
	for {
		goalDriveOnce(g)
		// The driver is per GROUP, but every exit path inside goalDriveOnce
		// reasons about one goal — and a goalSet can REPLACE a terminal goal
		// while the driver is parked in a turn of the previous one. That
		// goalSet's startGoalDriver saw this driver alive and did nothing;
		// exiting here would strand the new goal at running/iteration 0 with
		// nothing driving it (observed 2026-08-14: ALPHA's agent test-fired a
		// junk goal, cancelled it mid-plan-turn, and set the real one — which
		// then sat idle). So before dying, re-check for a driveable goal; a
		// hot loop is impossible because every other exit leaves the status
		// non-driveable (paused/awaiting/met/cancelled/absent).
		goalLock.Lock()
		it := findGoalLocked(g)
		again := it != nil && (it.Status == goalStatusRunning || it.Status == goalStatusPlanning)
		goalLock.Unlock()
		if !again {
			return
		}
		emitLogfG("goal", g, "info", "driver re-driving replacement goal id=%s", it.ID)
	}
}

// goalDriveOnce runs the plan phase and iteration loop for the group's
// CURRENT goal until an exit condition (see goalDriver for why exits are
// re-checked rather than final).
func goalDriveOnce(g string) {
	if !goalPlanPhase(g) {
		return
	}
	silentJudge := 0
	for {
		// Loop top: snapshot state, honor pause/cancel, enforce the cap.
		goalLock.Lock()
		it := findGoalLocked(g)
		if it == nil || it.Status != goalStatusRunning {
			goalLock.Unlock()
			return
		}
		if it.Iteration >= it.MaxIterations {
			goalLock.Unlock()
			goalPauseWith(g, "cap", fmt.Sprintf("no accepted completion after %d iterations", it.MaxIterations))
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
				goalPauseWith(g, "judge", fmt.Sprintf("%d consecutive reviews produced no verdict", silentJudge))
				return
			}
			goalSetFeedback(g, "the reviewer produced no verdict; treat the previous completion claim as unverified and continue")
			continue
		}
		silentJudge = 0
		if !verdict.Met {
			goalSetFeedback(g, verdict.Reasons)
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
func goalPlanPhase(g string) bool {
	goalLock.Lock()
	it := findGoalLocked(g)
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
		goalPauseWith(g, "stalled", "plan turn could not be enqueued: "+err.Error())
		return false
	}
	if terr := <-done; terr != nil {
		goalPauseWith(g, "stalled", "plan turn failed: "+terr.Error())
		return false
	}
	if isStalled(g, work) {
		goalPauseWith(g, "stalled", "plan turn produced no turn_end (session stalled)")
		return false
	}
	if _, err := goalTransition(g, []string{goalStatusPlanning}, func(it *goalItem) {
		it.Status = goalStatusAwaiting
	}); err != nil {
		return false // cancelled mid-turn
	}
	emit(g, Event{Event: "goal_awaiting", ID: snap.ID, Session: goalWorkSessionFor(goalSessionSlug(snap))})
	emitLogfG("goal", g, "info", "plan ready id=%s — awaiting approval", snap.ID)
	goalNotify(g, "normal", "goal plan ready for review",
		fmt.Sprintf("goal %s: review the plan in the goal-work session, then /goal approve %s (or /goal cancel)", snap.ID, g))
	return false
}

// goalWorkerTurn runs one execution iteration. Returns the worker's claim
// (claimed + note) and ok=false when the driver must exit (pause applied).
func goalWorkerTurn(g string, snap goalItem) (claimed bool, note string, ok bool) {
	work := goalWorkSessionFor(goalSessionSlug(snap))
	clearGoalSession(g, work)

	goalLock.Lock()
	goalDoneOpen[g] = snap.ID
	delete(goalDoneMail, g)
	goalLock.Unlock()
	defer func() {
		goalLock.Lock()
		delete(goalDoneOpen, g)
		if n, has := goalDoneMail[g]; has {
			claimed, note = true, n
		}
		delete(goalDoneMail, g)
		goalLock.Unlock()
	}()

	emit(g, Event{Event: "goal_iter", ID: snap.ID, Text: fmt.Sprintf("%d/%d", snap.Iteration, snap.MaxIterations), Session: work})
	emitLogfG("goal", g, "info", "iteration %d/%d id=%s", snap.Iteration, snap.MaxIterations, snap.ID)

	done, err := goalEnqueue(g, work, goalWorkerMsg(snap))
	if err != nil {
		goalPauseWith(g, "stalled", "iteration could not be enqueued: "+err.Error())
		return false, "", false
	}
	if terr := <-done; terr != nil {
		goalPauseWith(g, "stalled", "iteration failed: "+terr.Error())
		return false, "", false
	}
	if isStalled(g, work) {
		goalPauseWith(g, "stalled", "no turn_end within the wait window (session stalled; self-heal owns the restart)")
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
		it := findGoalLocked(g)
		if it == nil || it.Status != goalStatusRunning || it.ID != snap.ID {
			goalLock.Unlock()
			return goalVerdict{}, false, false
		}
		goalLock.Unlock()

		clearGoalSession(g, judge)
		goalLock.Lock()
		goalVerdictOpen[g] = snap.ID
		delete(goalVerdictMail, g)
		goalLock.Unlock()

		emit(g, Event{Event: "goal_judge", ID: snap.ID, Session: goalWorkSessionFor(goalSessionSlug(snap))})
		emitLogfG("goal", g, "info", "judge check id=%s (attempt %d)", snap.ID, attempt+1)

		done, err := goalEnqueue(g, judge, goalJudgeMsg(snap))
		var terr error
		if err == nil {
			terr = <-done
		}
		goalLock.Lock()
		delete(goalVerdictOpen, g)
		verdict, has := goalVerdictMail[g]
		delete(goalVerdictMail, g)
		goalLock.Unlock()
		if err != nil {
			goalPauseWith(g, "stalled", "judge turn could not be enqueued: "+err.Error())
			return goalVerdict{}, false, false
		}
		if terr != nil {
			goalPauseWith(g, "stalled", "judge turn failed: "+terr.Error())
			return goalVerdict{}, false, false
		}
		if isStalled(g, judge) {
			goalPauseWith(g, "stalled", "judge turn produced no turn_end (session stalled)")
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

func goalSetFeedback(g, feedback string) {
	goalLock.Lock()
	if it := findGoalLocked(g); it != nil {
		it.LastFeedback = truncateRunes(feedback, goalNoteMax)
		it.UpdatedAt = goalNow()
		saveGoalsLocked()
	}
	goalLock.Unlock()
}

func goalMarkMet(g string, snap goalItem, note string) {
	if _, err := goalTransition(g, []string{goalStatusRunning}, func(it *goalItem) {
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
func goalPauseWith(g, reason, detail string) {
	it, err := goalTransition(g, []string{goalStatusRunning, goalStatusPlanning}, func(it *goalItem) {
		it.Status = goalStatusPaused
		it.PausedReason = reason
	})
	if err != nil {
		return
	}
	emit(g, Event{Event: "goal_paused", ID: it.ID, Text: reason, Session: goalWorkSessionFor(goalSessionSlug(it))})
	emitLogfG("goal", g, "warn", "paused id=%s reason=%s: %s", it.ID, reason, detail)
	goalNotify(g, "high", "goal paused ("+reason+")",
		fmt.Sprintf("goal %s at iteration %d/%d: %s — /goal resume %s to continue", it.ID, it.Iteration, it.MaxIterations, detail, g))
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
  printf '{"cmd":"goal_done","note":"%%s"}\n' \
    "$(printf '%%s' "$R" | base64 -w 0)" > /workspace/.cs/ctl
Otherwise just end your turn; you will be re-invoked.`,
		it.ID, it.Iteration, it.MaxIterations, feedback, it.Text, it.Criteria)
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
  printf '{"cmd":"goal_verdict","met":true,"reasons":""}\n' > /workspace/.cs/ctl
  # or, unmet — put the per-criterion failures in R first:
  R="criterion 2 FAIL: observed ...; acceptable would be ..."
  printf '{"cmd":"goal_verdict","met":false,"reasons":"%%s"}\n' \
    "$(printf '%%s' "$R" | base64 -w 0)" > /workspace/.cs/ctl`,
		it.ID, it.Iteration, it.MaxIterations, it.Text, it.Criteria)
}
