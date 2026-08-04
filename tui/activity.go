package main

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// activity.go — client half of the daemon's per-group phase reporting
// (daemon/activity.go). The daemon sends one `activity` frame per transition
// carrying the phase name, an optional detail, and the phase's START time; the
// elapsed counter is derived here on every render tick, so a turn parked on
// the provider for four minutes costs one frame, not 240.

// activityInfo is a group's live phase as last reported by the daemon.
//
// Two clocks, because they answer different questions. `since` is when the
// current phase started — "how long have we been waiting on the provider".
// `turnSince` is when the group last went from idle to busy and survives every
// phase change until it goes idle again — "how long has this turn been going".
// Only the second one answers "is this stuck?", and it is the only one that
// belongs in the tree: a turn cycling llm→stream→work every two seconds shows
// a phase clock that never counts past 2s no matter how long it grinds.
type activityInfo struct {
	phase     string
	detail    string
	session   string
	since     time.Time
	turnSince time.Time
}

// activityLabel maps a daemon phase to its human-readable label (shown in the
// status bar's progress segment). An unknown phase — a newer daemon than this
// TUI — renders its raw name rather than vanishing, so the operator still sees
// that *something* is happening and for how long.
func activityLabel(phase string) string {
	switch phase {
	case "boot":
		return "booting microVM"
	case "send":
		return "starting turn"
	case "llm":
		return "waiting for model"
	case "retry":
		return "provider retry"
	case "stream":
		return "receiving"
	case "work":
		return "running tools"
	}
	return phase
}

// activityColor picks the accent. Retry is red because it is the one phase
// that means something went wrong upstream and the turn is stalled on a
// backoff, not making progress.
func activityColor(phase string) lipgloss.Color {
	if phase == "retry" {
		return cRed
	}
	return cYellow
}

// fmtElapsed renders a duration for a live counter: seconds under a minute,
// m+s under an hour, h+m beyond. Zero-padded seconds/minutes so the field
// doesn't jitter in width as it counts.
func fmtElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// fmtElapsedShort is fmtElapsed for the tree's badge column, where every
// character costs a character of group name: one unit, never more than three
// glyphs. Resolution past the leading unit doesn't matter there — the question
// the tree answers is "has this been going a suspiciously long time", not
// "exactly how long".
func fmtElapsedShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// activityFor returns the group's phase, if any.
func (m Model) activityFor(g string) (activityInfo, bool) {
	a, ok := m.activity[g]
	if !ok || a.phase == "" {
		return activityInfo{}, false
	}
	return a, true
}

// anyActivity reports whether any group is mid-phase. Gates the render tick
// (isAnimating): the tree paints a spinner for every working group, not just
// the focused one, so the clock has to keep running for background groups too.
func (m Model) anyActivity() bool {
	for _, a := range m.activity {
		if a.phase != "" {
			return true
		}
	}
	return false
}

// applyActivity folds one `activity` frame into the model. An empty phase is
// the daemon saying "idle" (turn over).
//
// since comes off the daemon's clock. Daemon and TUI share a host, so skew is
// not a real concern, but a future timestamp would render as a counter running
// backwards — clamp it to now instead.
func (m *Model) applyActivity(ev Event) {
	if ev.Name == "" {
		delete(m.activity, ev.Group)
		return
	}
	since := time.Now()
	if ev.Ts > 0 {
		if t := time.Unix(0, int64(ev.Ts*float64(time.Second))); t.Before(since) {
			since = t
		}
	}
	// The turn clock starts on the idle→busy edge and rides every subsequent
	// phase. The daemon has no matching field to send: it would have to be a
	// second timestamp on every frame, and the edge is unambiguous from here.
	turnSince := since
	if prev, ok := m.activity[ev.Group]; ok && !prev.turnSince.IsZero() {
		turnSince = prev.turnSince
	}
	m.activity[ev.Group] = activityInfo{
		phase:     ev.Name,
		detail:    ev.Text,
		session:   ev.Session,
		since:     since,
		turnSince: turnSince,
	}
}
