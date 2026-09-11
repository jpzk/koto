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

// dropSub is shut() plus the cleanup that used to wait on the handler noticing
// (audit M154): deregister the subscriber and drain its queue.
//
// The handler's select returns on sub.done — BETWEEN sends. A stream.Send that
// is already in flight blocks on HTTP/2 flow control for as long as the client
// keeps the connection open without reading, and closing a channel does not
// interrupt it. So the handler stayed put, and with it the 256 queued events
// and a registration emit() kept walking on every frame.
//
// Cutting the subscriber loose here is what is actually in this side's gift:
// the queued frames are freed, no further frame is queued for it, and destroy()
// no longer waits on a handler that cannot come back. The handler goroutine and
// its stream are NOT reclaimed — returning from a server handler with a Send in
// flight, or calling Send from a second goroutine, are both outside what
// grpc-go permits, and the server cannot cancel a stream context it does not
// own. What bounds those is the per-identity and global stream admission
// (streamAdmit, audit M95); they come back when the transport dies.
func dropSub(g string, s *groupSub) {
	s.shut()
	subsLock.Lock()
	kept := subscribers[g][:0]
	for _, x := range subscribers[g] {
		if x != s {
			kept = append(kept, x)
		}
	}
	subscribers[g] = kept
	subsLock.Unlock()
	drainSub(s)
}

// drainSub frees the frames a shut subscriber will never read. The channel is
// deliberately NOT closed: the handler may still be selecting on it, and a
// closed channel would hand it a nil event to Send.
func drainSub(s *groupSub) {
	for {
		select {
		case <-s.ch:
		default:
			return
		}
	}
}

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
	// ringFloor is the seq below which the ring no longer proves
	// continuity: every seq > floor is either in the ring or a superseded
	// partial (see recordEvent). Advanced only by age trimming.
	ringFloor = map[string]uint64{}
	// ringPartial tracks the one live partial per (group, session) the
	// ring holds, so recordEvent can evict it without scanning.
	ringPartial = map[string]map[string]*pb.Event{}
)

// isPartial reports whether ev is a cumulative in-progress line — the
// tailer re-emits the whole partial on every 50ms poll while a line is
// being streamed, and each one supersedes the last.
func isPartial(ev *pb.Event) bool {
	switch ev.Event {
	case "stream", "thinking_stream", "tool_result_stream":
		return true
	}
	return false
}

// recordEvent assigns the next per-group seq to pbev, appends it to the
// group's replay ring, and snapshots the current subscriber list — one
// atomic step under subsLock, so a concurrent SubscribeGroup either sees
// this event in its replay snapshot or is in the returned subscriber list,
// never neither and never both.
//
// Partials get a seq like everything else but hold AT MOST ONE ring slot
// per session: a newer partial replaces the older one, and the line's
// terminal frame (`done`, `thinking_done`, …) evicts it — a client that
// has the finished line has no use for its drafts. Before this every poll's
// cumulative partial was its own ring entry, so one long streamed
// paragraph (20 polls/s) filled the 1024-slot ring in ~50s with
// near-duplicates, pushing the real done/tool frames out — a since_seq
// resume mid-turn then replayed a ring of drafts and got a `gap` for the
// frames it actually needed. The ring therefore has seq holes where
// superseded partials were; replayFrom tolerates them (ringFloor).
func recordEvent(g string, pbev *pb.Event) []*groupSub {
	subsLock.Lock()
	defer subsLock.Unlock()
	eventSeq[g]++
	pbev.Seq = eventSeq[g]
	ring := eventRing[g]
	if live := ringPartial[g][pbev.Session]; live != nil {
		// Search from the tail: the live partial is the session's newest
		// frame, so it sits within a few entries of the end.
		for i := len(ring) - 1; i >= 0; i-- {
			if ring[i] == live {
				ring = append(ring[:i], ring[i+1:]...)
				break
			}
		}
		delete(ringPartial[g], pbev.Session)
	}
	if isPartial(pbev) {
		if ringPartial[g] == nil {
			ringPartial[g] = map[string]*pb.Event{}
		}
		ringPartial[g][pbev.Session] = pbev
	}
	ring = append(ring, pbev)
	if n := len(ring) - eventRingMax; n > 0 {
		ringFloor[g] = ring[n-1].Seq
		// Reconcile ringPartial with the trim. The index is what keeps a
		// session's live partial FINDABLE for supersession, and it holds a
		// pointer — so an entry whose event has just aged out of the ring
		// pinned that event's payload (a multi-megabyte streamed line) for as
		// long as the map entry survived, which is until another event for
		// that exact session arrives. A session whose turn ended without one
		// — a stopped, wedged or restarted guest — kept it indefinitely, and
		// session names are caller-chosen (audit M94).
		for sess, live := range ringPartial[g] {
			if partialInPrefix(ring[:n], live) {
				delete(ringPartial[g], sess)
			}
		}
		if len(ringPartial[g]) == 0 {
			delete(ringPartial, g)
		}
		// Copy rather than reslice: a reslice keeps the trimmed entries
		// (whole tool/thinking bodies) reachable until the next realloc.
		ring = append(make([]*pb.Event, 0, eventRingMax+eventRingMax/8), ring[n:]...)
	}
	eventRing[g] = ring
	return append([]*groupSub(nil), subscribers[g]...)
}

// partialInPrefix reports whether live is one of the entries about to be
// trimmed. Compared by POINTER, which is how ringPartial indexes it.
func partialInPrefix(prefix []*pb.Event, live *pb.Event) bool {
	for _, e := range prefix {
		if e == live {
			return true
		}
	}
	return false
}

// replayFrom returns the ring entries with seq > since, or a single
// synthetic `gap` event when the ring cannot prove continuity: frames aged
// out of the ring (since < ringFloor), or the counter regressed below since
// (daemon restart, destroy+respawn). After a gap the client's view is stale
// beyond replay — it refetches via History. Must be called with subsLock
// held; the returned slice is a copy.
// clearEventRing drops g's in-memory replay state after a transcript clear.
//
// The clear handlers rewrote the persistent logs and deleted the guest's
// conversation, and left the RING alone (audit M64) — so a client reconnecting
// with a pre-clear since_seq was replayed the frames the clear had just
// erased, out of memory, with no trace of them on disk. Destroy already did
// this; clear did not, and clear is the one people run expecting the
// transcript to be gone.
//
// The ring is dropped rather than filtered, and ringFloor is raised to the
// current seq, so any cursor from before the clear replays as `gap` — the
// client's cue to drop its view and refetch History, which is exactly right:
// History now reflects the cleared log. A session-scoped clear takes the same
// route, because a partial frame's session attribution is sticky and a
// half-filtered ring would be a worse answer than a refetch.
func clearEventRing(g string) {
	subsLock.Lock()
	delete(eventRing, g)
	delete(ringPartial, g)
	ringFloor[g] = eventSeq[g]
	subsLock.Unlock()
}

func replayFrom(g string, since uint64) []*pb.Event {
	cur := eventSeq[g]
	if since == cur {
		return nil
	}
	if since > cur || since < ringFloor[g] {
		return []*pb.Event{{Event: "gap", Group: g, Ts: float64(time.Now().UnixNano()) / 1e9}}
	}
	ring := eventRing[g]
	idx := sort.Search(len(ring), func(i int) bool { return ring[i].Seq > since })
	return append([]*pb.Event(nil), ring[idx:]...)
}

func emit(g string, ev Event) {
	ev.Group = g
	if ev.Ts == 0 {
		ev.Ts = float64(time.Now().UnixNano()) / 1e9
	}
	if ev.Event == "turn_end" {
		notifyTurnDone(g, ev.Session)
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
			// Overflow: end the stream AND cut the subscriber loose, rather
			// than waiting for a handler that may be blocked mid-Send to
			// notice (audit M154).
			dropSub(g, s)
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
	// project narrows the snapshot to what THIS watcher may see (M18). nil
	// means "everything" — the common case (admin, or any role whose grant
	// for watch_state is "*"), and the case the shared-frame fast path below
	// is written for. A narrowed watcher pays for its own frame and its own
	// hash, which is right: its view changes on a different schedule.
	project func(map[string]GroupInfo) map[string]GroupInfo
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
		// The pb frame is built lazily, only when some watcher actually
		// needs it — on an idle fleet every tick used to allocate a full
		// StateFrame conversion and then throw it away on the hash check.
		var frame *pb.StateFrame
		stateSubsLock.Lock()
		for _, w := range stateSubs {
			wHash, wFrame := hash, frame
			if w.project != nil {
				view := w.project(gs)
				wHash = stateHash(view)
				if w.lastSent == wHash {
					continue
				}
				wFrame = toStateFrame(view)
			} else {
				if w.lastSent == wHash {
					continue
				}
				if frame == nil {
					frame = toStateFrame(gs)
				}
				wFrame = frame
			}
			select {
			case w.ch <- wFrame:
				w.lastSent = wHash
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
	// dropped counts this subscriber's lost lines — non-zero only while it is
	// behind (audit M156). Guarded by logSubsLock, which logDeliver already
	// holds for the ring append.
	dropped int
}

var (
	logSubsLock sync.Mutex
	logSubs     []*logSub
	logRing     []*pb.LogEvent // recent frames, replayed to fresh subscribers
)

// emitLog formats a pb.LogEvent, mirrors it to stderr (so `make host-run`
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
	// Log lines quote guest-authored bytes (ctl JSON, job_done fields, exec
	// output) and reach stderr and the TUI log pane unframed — scrub them
	// here, the one funnel, rather than at every emit site.
	msg = sanitize(msg)
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
	// Fan out under the lock, because the drop counters are per subscriber and
	// this is the only writer of them. The sends are non-blocking, so the
	// critical section is bounded by the subscriber count.
	for _, s := range logSubs {
		// A subscriber that fell behind gets told so before it gets more
		// lines. The daemon log is the record an operator checks after the
		// fact — a resource alert, an auth rejection, a forwarded error — and
		// dropping from it silently made "nothing was logged" and "you did not
		// receive what was logged" look identical at the one consumer that
		// renders it (audit M156). Unlike the group event stream there are no
		// sequence numbers here to notice a gap with, and the 200-line replay
		// ring only covers a reconnect that happens before the lines age out.
		//
		// The count is never lost, only deferred: if the notice itself does
		// not fit, the counter keeps climbing and the notice goes out when the
		// subscriber catches up.
		if s.dropped > 0 {
			notice := &pb.LogEvent{
				Event: "log", Level: "warn", Subsystem: "log", Ts: pbev.Ts,
				Msg: fmt.Sprintf("log stream fell behind: %d line(s) were not delivered to this subscriber "+
					"(they are in the daemon's own log; `koto ctl logs` re-reads it)", s.dropped),
			}
			select {
			case s.ch <- notice:
				s.dropped = 0
			default:
				s.dropped++
				continue
			}
		}
		select {
		case s.ch <- pbev:
		default:
			s.dropped++
		}
	}
	logSubsLock.Unlock()
}

func emitLogf(subsystem, level, format string, args ...any) {
	emitLog(subsystem, level, fmt.Sprintf(format, args...))
}

func emitLogfG(subsystem, group, level, format string, args ...any) {
	emitLogG(subsystem, group, level, fmt.Sprintf(format, args...))
}
