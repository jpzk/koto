package main

// goal_amend.go — /amend: versioned edits of a goal's spec (goal-live item 4).
//
// The operator may change a non-terminal goal's text, acceptance criteria and
// iteration cap without cancel+recreate. Each amend bumps GoalItem.Version
// and appends a GoalAmend entry (version, who, when, iteration, the fields it
// changed and the values it replaced) to the HOST record. Every consumer reads
// the latest record, never a copy taken before the amend:
//
//   - the worker prompt (goalWorkerTurn refreshes the spec in the same
//     critical section that composes the steer block),
//   - the judge prompt (goalJudgeCheck refreshes before each review attempt),
//   - the cap check at the driver's loop top (reads the record directly).
//
// A running turn is told live (goalLiveNote) so it can adjust before its next
// tool call. The guest's /workspace/goal/<name>/amend.md is a MIRROR rewritten
// from the host record; nothing the guest writes is read back.
//
// OPERATOR-ONLY: the GoalAmend RPC is in adminOnlyVerbs ("goal_amend") and the
// verb does not exist on the ctl plane. An agent that could amend its own
// criteria would be judging itself against a contract it wrote.

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// goalAmendMax bounds the amend log (oldest entries drop first).
const goalAmendMax = 32

// goalAmendMirrorFn, when non-nil, replaces the guest amend.md write. Test seam.
var goalAmendMirrorFn func(g, dir, body string)

// goalVersion is the record's spec version (0 = never amended = 1).
func goalVersion(it goalItem) int {
	if it.Version < 1 {
		return 1
	}
	return it.Version
}

// goalAmendOperator applies one amend. nil pointer = field unchanged.
func goalAmendOperator(g, name string, text, criteria *string, maxIter *int, who string) (goalItem, error) {
	var nt, nc string
	if text != nil {
		nt = strings.TrimSpace(*text)
		if nt == "" {
			return goalItem{}, fmt.Errorf("amended goal text is empty")
		}
		if len(nt) > goalTextMax {
			return goalItem{}, fmt.Errorf("goal text is %d bytes; the limit is %d", len(nt), goalTextMax)
		}
	}
	if criteria != nil {
		nc = strings.TrimSpace(*criteria)
		if nc == "" {
			return goalItem{}, fmt.Errorf("amended acceptance criteria are empty")
		}
		if len(nc) > goalTextMax {
			return goalItem{}, fmt.Errorf("acceptance criteria are %d bytes; the limit is %d", len(nc), goalTextMax)
		}
	}
	if text == nil && criteria == nil && maxIter == nil {
		return goalItem{}, fmt.Errorf("amend needs at least one of text, criteria, max_iterations")
	}

	goalLock.Lock()
	it, err := resolveGoalLocked(g, name,
		[]string{goalStatusPlanning, goalStatusAwaiting, goalStatusRunning, goalStatusPaused})
	if err != nil {
		goalLock.Unlock()
		return goalItem{}, err
	}
	if maxIter != nil {
		n := *maxIter
		switch {
		case n < 1 || n > goalMaxIterCeil:
			goalLock.Unlock()
			return goalItem{}, fmt.Errorf("max_iterations must be within 1..%d", goalMaxIterCeil)
		case n < it.Iteration:
			goalLock.Unlock()
			return goalItem{}, fmt.Errorf("max_iterations %d is below the current iteration %d", n, it.Iteration)
		}
	}
	rec := goalAmendRec{Ts: goalNow(), Iteration: it.Iteration, Who: truncateRunes(flattenInline(who), 80)}
	if text != nil && nt != it.Text {
		rec.Fields, rec.PrevText = append(rec.Fields, "text"), it.Text
	}
	if criteria != nil && nc != it.Criteria {
		rec.Fields, rec.PrevCriteria = append(rec.Fields, "criteria"), it.Criteria
	}
	if maxIter != nil && *maxIter != it.MaxIterations {
		rec.Fields, rec.PrevMax = append(rec.Fields, "max_iterations"), it.MaxIterations
	}
	if len(rec.Fields) == 0 {
		goalLock.Unlock()
		return goalItem{}, fmt.Errorf("amend changes nothing (goal %s is already at that spec)", it.ID)
	}
	for _, f := range rec.Fields {
		switch f {
		case "text":
			it.Text = nt
		case "criteria":
			it.Criteria = nc
		case "max_iterations":
			it.MaxIterations = *maxIter
		}
	}
	it.Version = goalVersion(*it) + 1
	rec.Version = it.Version
	it.AmendLog = append(it.AmendLog, rec)
	if n := len(it.AmendLog); n > goalAmendMax {
		it.AmendLog = append([]goalAmendRec(nil), it.AmendLog[n-goalAmendMax:]...)
	}
	it.UpdatedAt = goalNow()
	out := *it
	sess := goalWorkSessionFor(goalSessionSlug(out))
	dir, body := goalDirFor(out), goalAmendFile(out)
	snap := snapshotGoalsLocked()
	goalLock.Unlock()
	snap.write()

	emitLogfG("goal", g, "info", "[%s] goal %s amended to v%d by %s (%s)", g, out.ID, out.Version, approvalWho(who), strings.Join(rec.Fields, ","))
	emit(g, Event{Event: "goal_amend", ID: out.ID, Text: fmt.Sprintf("v%d %s", out.Version, strings.Join(rec.Fields, ",")), Session: sess})
	goalAmendMirror(g, dir, body)
	if out.Status == goalStatusRunning {
		goalLiveNote(g, sess, "a"+randHex(6), liveAmendText(who, out, rec))
	}
	return goalWithPending(out), nil
}

// liveAmendText is the live note telling a running turn the spec changed.
func liveAmendText(who string, it goalItem, rec goalAmendRec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OPERATOR AMEND (live, from %s, authoritative): the goal spec is now version %d (changed: %s).",
		approvalWho(who), rec.Version, strings.Join(rec.Fields, ", "))
	for _, f := range rec.Fields {
		switch f {
		case "text":
			b.WriteString(" NEW GOAL: " + truncateRunes(flattenInline(it.Text), 1500))
		case "criteria":
			b.WriteString(" NEW ACCEPTANCE CRITERIA: " + truncateRunes(flattenInline(it.Criteria), 1500))
		case "max_iterations":
			fmt.Fprintf(&b, " NEW ITERATION CAP: %d.", it.MaxIterations)
		}
	}
	b.WriteString(" The reviewer judges against this version only.")
	return b.String()
}

// goalSpecLabel names the spec version in worker/judge prompts ("" = v1).
func goalSpecLabel(it goalItem) string {
	if goalVersion(it) <= 1 || len(it.AmendLog) == 0 {
		return ""
	}
	last := it.AmendLog[len(it.AmendLog)-1]
	return fmt.Sprintf(" (version %d — amended by the operator at %s, iteration %d: %s; this is the CURRENT spec, earlier versions no longer apply)",
		goalVersion(it), time.Unix(int64(last.Ts), 0).UTC().Format("2006-01-02 15:04Z"), last.Iteration, strings.Join(last.Fields, ", "))
}

// goalRefreshSpecLocked copies the record's CURRENT spec into a snapshot taken
// earlier (the driver's loop-top copy). Caller holds goalLock.
func goalRefreshSpecLocked(snap *goalItem) {
	cur := findGoalByIDLocked(snap.ID)
	if cur == nil {
		return
	}
	snap.Text, snap.Criteria, snap.MaxIterations = cur.Text, cur.Criteria, cur.MaxIterations
	snap.Version = cur.Version
	snap.AmendLog = append([]goalAmendRec(nil), cur.AmendLog...)
}

// goalAmendFile is the guest mirror's content.
func goalAmendFile(it goalItem) string {
	var b strings.Builder
	b.WriteString("# goal spec versions — mirror written by koto from the host record.\n")
	b.WriteString("# Edits here are ignored: prompts and the reviewer read the host copy.\n\n")
	fmt.Fprintf(&b, "current: version %d\n\n", goalVersion(it))
	for _, a := range it.AmendLog {
		fmt.Fprintf(&b, "- v%d · %s · iteration %d · by %s · changed %s\n", a.Version,
			time.Unix(int64(a.Ts), 0).UTC().Format("2006-01-02 15:04Z"), a.Iteration, approvalWho(a.Who), strings.Join(a.Fields, ", "))
	}
	fmt.Fprintf(&b, "\n## GOAL (v%d)\n%s\n\n## ACCEPTANCE CRITERIA (v%d)\n%s\n\n## CAP\n%d iterations\n",
		goalVersion(it), it.Text, goalVersion(it), it.Criteria, it.MaxIterations)
	return b.String()
}

// goalAmendMirror rewrites the guest's amend.md in the background (best-effort).
func goalAmendMirror(g, dir, body string) {
	if goalAmendMirrorFn != nil {
		goalAmendMirrorFn(g, dir, body)
		return
	}
	d := compactShellQuote(dir)
	script := "mkdir -p " + d + " && printf %s '" + base64.StdEncoding.EncodeToString([]byte(body)) +
		"' | base64 -d > " + d + "/.amend.md.tmp && mv -f " + d + "/.amend.md.tmp " + d + "/amend.md"
	go func() {
		if _, rc, err := fcExec(g, script, 15*time.Second); err != nil || rc != 0 {
			emitLogfG("goal", g, "warn", "[%s] amend.md mirror failed (rc=%d err=%v)", g, rc, err)
		}
	}()
}
