package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"koto-protocol/pb"
)

// ---- streaming: subscribers + tail ----------------------------------------

// A subscriber is a live SubscribeGroup stream handler; emit() pushes pb.Events
// to its buffered channel and the handler goroutine owns stream.Send. The old
// []net.Conn registry + dead-conn pruning is replaced by per-stream context
// cancellation (the handler deregisters itself on stream.Context().Done()).
type groupSub struct {
	ch       chan *pb.Event
	done     chan struct{} // closed via shut() to force the stream handler to return
	shutOnce sync.Once
}

// shut closes the stream handler's done channel exactly once. Callers:
// destroy() (group is gone) and emit() on buffer overflow (the subscriber
// fell too far behind; ending the stream makes the loss visible so the
// client reconnects with since_seq and replays exactly what it missed —
// strictly better than the old silent frame drop).
func (s *groupSub) shut() { s.shutOnce.Do(func() { close(s.done) }) }

// eventRingMax bounds the per-group replay ring. Sized to cover several
// turns of streaming frames — a reconnecting client whose since_seq has
// aged out gets a synthetic `gap` event and refetches history instead.
const eventRingMax = 1024

var (
	subsLock    sync.Mutex
	subscribers = map[string][]*groupSub{}
	tails       = map[string]bool{}
	// eventSeq is the last sequence number assigned per group (starts at 1,
	// in-memory only — resets on daemon restart, which clients observe as a
	// `gap`). eventRing keeps the most recent frames for since_seq replay;
	// both are guarded by subsLock so seq assignment, ring append, and
	// subscriber registration are mutually atomic.
	eventSeq  = map[string]uint64{}
	eventRing = map[string][]*pb.Event{}
)

// recordEvent assigns the next per-group seq to pbev, appends it to the
// group's replay ring, and snapshots the current subscriber list — one
// atomic step under subsLock, so a concurrent SubscribeGroup either sees
// this event in its replay snapshot or is in the returned subscriber list,
// never neither and never both.
func recordEvent(g string, pbev *pb.Event) []*groupSub {
	subsLock.Lock()
	defer subsLock.Unlock()
	eventSeq[g]++
	pbev.Seq = eventSeq[g]
	ring := append(eventRing[g], pbev)
	if len(ring) > eventRingMax {
		ring = ring[len(ring)-eventRingMax:]
	}
	eventRing[g] = ring
	return append([]*groupSub(nil), subscribers[g]...)
}

// replayFrom returns the ring suffix with seq > since, or a single synthetic
// `gap` event when the ring cannot prove continuity: frames aged out of the
// ring, or the counter regressed below since (daemon restart, destroy+respawn).
// After a gap the client's view is stale beyond replay — it refetches via
// History. Must be called with subsLock held; the returned slice is a copy.
func replayFrom(g string, since uint64) []*pb.Event {
	cur := eventSeq[g]
	if since == cur {
		return nil
	}
	ring := eventRing[g]
	if since < cur && len(ring) > 0 && ring[0].Seq <= since+1 {
		idx := int(since + 1 - ring[0].Seq)
		return append([]*pb.Event(nil), ring[idx:]...)
	}
	return []*pb.Event{{Event: "gap", Group: g, Ts: float64(time.Now().UnixNano()) / 1e9}}
}

func emit(g string, ev Event) {
	ev.Group = g
	if ev.Ts == 0 {
		ev.Ts = float64(time.Now().UnixNano()) / 1e9
	}
	if ev.Event == "turn_end" {
		notifyTurnDone(g)
	}
	ev = sanitizeEvent(ev)
	pbev := toPBEvent(ev)
	subs := recordEvent(g, pbev)
	for _, s := range subs {
		// Non-blocking: a slow consumer whose buffer is full must not stall
		// the tailLog goroutine for every other subscriber of the group. But
		// instead of silently dropping the frame (the old behavior — the
		// client had no way to notice), shut the stream: the client sees it
		// close, reconnects with since_seq, and the ring replays exactly the
		// frames it missed.
		select {
		case s.ch <- pbev:
		default:
			s.shut()
		}
	}
}

// ---- state watch: push group snapshots on change ---------------------------
//
// Replaces client-side List polling (the TUI used to call List once per
// second per client; each call shells out to podman per group). The daemon
// recomputes the snapshot once per second — only while at least one watcher
// is attached — and pushes a frame to each watcher whose last-delivered
// snapshot differs. Delivery is non-blocking and lastSent only advances on a
// successful send: a slow watcher simply retries next tick, and because
// frames are idempotent snapshots (not deltas), missing an intermediate one
// is harmless.

type stateSub struct {
	ch       chan *pb.StateFrame
	lastSent string // stateHash of the last frame this watcher took
}

var (
	stateSubsLock sync.Mutex
	stateSubs     []*stateSub
)

// stateHash renders the snapshot into a canonical comparable string (sorted
// by group name; excludes the frame timestamp so identical states compare
// equal across ticks).
func stateHash(gs map[string]GroupInfo) string {
	names := make([]string, 0, len(gs))
	for g := range gs {
		names = append(names, g)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, g := range names {
		gi := gs[g]
		// tok/s is hashed at integer resolution: enough for the display,
		// while sub-token jitter doesn't push a frame every tick forever.
		fmt.Fprintf(&b, "%s|%d|%t|%s|%s|%s|%t|%d|%.0f|%s|%s|%t", g, gi.Port, gi.Running, gi.Provider, gi.Model, gi.Effort, gi.Stalled, gi.Queued, gi.TokPerSec, strings.Join(gi.Sessions, ","), gi.Network, gi.Root)
		for _, j := range gi.Jobs {
			// id/status/rc/size cover every observable transition (a running
			// job's growing output bumps size, so watchers see progress).
			fmt.Fprintf(&b, "|%s:%s:%s:%s:%d", j.ID, j.Session, j.Status, j.RC, j.OutSize)
		}
		b.WriteString(";")
	}
	return b.String()
}

func toStateFrame(gs map[string]GroupInfo) *pb.StateFrame {
	groups := map[string]*pb.GroupInfo{}
	for g, gi := range gs {
		groups[g] = toPBGroupInfo(gi)
	}
	_, global := tokRates()
	return &pb.StateFrame{Groups: groups, Ts: float64(time.Now().UnixNano()) / 1e9, GlobalTokPerSec: global}
}

func stateWatchLoop() {
	for {
		time.Sleep(time.Second)
		stateSubsLock.Lock()
		n := len(stateSubs)
		stateSubsLock.Unlock()
		if n == 0 {
			continue
		}
		gs := listGroups()
		// Keep the job mirrors warm while (and only while) someone watches:
		// stale running-group mirrors refresh in the background and surface
		// on a later tick via the hash change.
		for g, gi := range gs {
			if gi.Running {
				kickJobsRefresh(g, false)
			}
		}
		hash := stateHash(gs)
		frame := toStateFrame(gs)
		stateSubsLock.Lock()
		for _, w := range stateSubs {
			if w.lastSent == hash {
				continue
			}
			select {
			case w.ch <- frame:
				w.lastSent = hash
			default:
			}
		}
		stateSubsLock.Unlock()
	}
}

// ---- daemon log: ring buffer + subscriber fan-out -------------------------
//
// Separate from the per-group `subscribers` map: daemon logs are global
// (no group key) and carry a level. The ring buffer (logRing) holds the
// most recent logRingMax lines so a fresh `cmd:"logs"` subscriber gets
// some immediate context instead of an empty pane until something happens.

const logRingMax = 200

type logSub struct {
	ch chan *pb.LogEvent
}

var (
	logSubsLock sync.Mutex
	logSubs     []*logSub
	logRing     []*pb.LogEvent // recent frames, replayed to fresh subscribers
)

// emitLog formats a LogEvent, mirrors it to stderr (so `make host-run`
// stays useful for tail -F debugging), appends to the ring, and broadcasts
// to all log subscribers. Dead subscribers are pruned in a second pass —
// same dead-conn pattern as emit(). subsystem names the emitting area
// (acl, auth, fc, egress, sched, ...) so clients can filter/label lines;
// it replaces the old ad-hoc "acl:" / "fc[g]:" message prefixes.
func emitLog(subsystem, level, msg string) {
	emitLogG(subsystem, "", level, msg)
}

// emitLogG is emitLog with group attribution: group names the one group a
// line is about (fc/egress/send/shell/... lines), "" for daemon-wide lines.
// The message text keeps its own "[g]"/"group=g" wording — the field is
// structured metadata so clients can *scope* the log view to the group the
// user is looking at without parsing prose.
//
// error lines are additionally forwarded to the operator as a
// high-severity notification (logalert.go; warn stays log-only); lines
// emitted BY the notification path itself must use emitLogfQuiet or
// forwarding would double-banner or recurse.
func emitLogG(subsystem, group, level, msg string) {
	logDeliver(subsystem, group, level, msg)
	forwardLogAlert(subsystem, group, level, msg)
}

// emitLogfQuiet is emitLogf minus the error→notification forwarding —
// only for log lines the notification machinery emits about itself
// (resNotifyOperator pairs its log line with its own queueNotify; delivery
// failures must not re-enter the forwarder).
func emitLogfQuiet(subsystem, level, format string, args ...any) {
	logDeliver(subsystem, "", level, fmt.Sprintf(format, args...))
}

// logDeliver is the delivery half of emitLogG: stderr mirror, ring append,
// subscriber fan-out.
func logDeliver(subsystem, group, level, msg string) {
	pbev := &pb.LogEvent{
		Event:     "log",
		Level:     level,
		Msg:       msg,
		Ts:        float64(time.Now().UnixNano()) / 1e9,
		Subsystem: subsystem,
		Group:     group,
	}

	fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", level, subsystem, msg)

	logSubsLock.Lock()
	logRing = append(logRing, pbev)
	if len(logRing) > logRingMax {
		logRing = logRing[len(logRing)-logRingMax:]
	}
	subs := append([]*logSub(nil), logSubs...)
	logSubsLock.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- pbev:
		default:
		}
	}
}

func emitLogf(subsystem, level, format string, args ...any) {
	emitLog(subsystem, level, fmt.Sprintf(format, args...))
}

func emitLogfG(subsystem, group, level, format string, args ...any) {
	emitLogG(subsystem, group, level, fmt.Sprintf(format, args...))
}
