package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Persistent cron-style schedules. State lives in SCHED_FILE; the in-memory
// `sched` slice is the source of truth at runtime. Parsed bitmasks live
// alongside in `parsed` keyed by ID — rebuilt at load, not serialized.
// cronLoop() wakes on minute boundaries and fires anything due.

var (
	schedLock sync.Mutex
	sched     []scheduleItem
	parsed    = map[string]parsedCron{}
)

func loadSched() {
	schedLock.Lock()
	defer schedLock.Unlock()
	b, err := os.ReadFile(SCHED_FILE)
	if err != nil {
		return
	}
	var items []scheduleItem
	if err := json.Unmarshal(b, &items); err != nil {
		emitLogf("warn", "loadSched: %v", err)
		return
	}
	sched = sched[:0]
	for _, it := range items {
		p, perr := parseCron(it.Cron)
		if perr != nil {
			emitLogf("warn", "loadSched: drop %s (bad cron %q: %v)", it.ID, it.Cron, perr)
			continue
		}
		// Recompute NextDueAt from now so a daemon restart doesn't re-fire
		// the same minute that was just fired before the crash.
		if nx, ok := p.next(time.Now()); ok {
			it.NextDueAt = float64(nx.Unix())
		} else {
			it.NextDueAt = 0
		}
		parsed[it.ID] = p
		sched = append(sched, it)
	}
}

// saveSched serializes the in-memory schedule slice. Caller MUST hold
// schedLock (the file write is cheap so we keep it under the lock to avoid
// torn states when two mutating commands race).
func saveSched() {
	b, err := json.MarshalIndent(sched, "", "  ")
	if err != nil {
		emitLogf("error", "saveSched marshal: %v", err)
		return
	}
	if err := os.WriteFile(SCHED_FILE, b, 0o644); err != nil {
		emitLogf("error", "saveSched write: %v", err)
	}
}

func newSchedID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func addSched(group, cronExpr, msg string) (scheduleItem, error) {
	if group == "" {
		return scheduleItem{}, fmt.Errorf("group is required")
	}
	if msg == "" {
		return scheduleItem{}, fmt.Errorf("msg is required")
	}
	p, err := parseCron(cronExpr)
	if err != nil {
		return scheduleItem{}, err
	}
	now := time.Now()
	nx, ok := p.next(now)
	if !ok {
		return scheduleItem{}, fmt.Errorf("cron %q has no fire within 4 years", cronExpr)
	}
	it := scheduleItem{
		ID:        newSchedID(),
		Group:     group,
		Cron:      cronExpr,
		Msg:       msg,
		Enabled:   true,
		CreatedAt: float64(now.Unix()),
		NextDueAt: float64(nx.Unix()),
	}
	schedLock.Lock()
	parsed[it.ID] = p
	sched = append(sched, it)
	saveSched()
	schedLock.Unlock()
	emitLogf("info", "sched add id=%s group=%s cron=%q next=%s", it.ID, group, cronExpr, nx.Format(time.RFC3339))
	return it, nil
}

func listSched(filter string) []scheduleItem {
	schedLock.Lock()
	defer schedLock.Unlock()
	out := make([]scheduleItem, 0, len(sched))
	for _, it := range sched {
		if filter != "" && it.Group != filter {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

func delSched(id string) error {
	schedLock.Lock()
	defer schedLock.Unlock()
	for i, it := range sched {
		if it.ID == id {
			sched = append(sched[:i], sched[i+1:]...)
			delete(parsed, id)
			saveSched()
			emitLogf("info", "sched del id=%s", id)
			return nil
		}
	}
	return fmt.Errorf("no schedule with id %q", id)
}

func toggleSched(id string, enabled bool) (scheduleItem, error) {
	schedLock.Lock()
	defer schedLock.Unlock()
	for i := range sched {
		if sched[i].ID == id {
			sched[i].Enabled = enabled
			// On re-enable, recompute NextDueAt so a paused-then-resumed
			// schedule doesn't fire for a moment that has passed.
			if enabled {
				if p, ok := parsed[id]; ok {
					if nx, ok := p.next(time.Now()); ok {
						sched[i].NextDueAt = float64(nx.Unix())
					}
				}
			}
			saveSched()
			emitLogf("info", "sched toggle id=%s enabled=%t", id, enabled)
			return sched[i], nil
		}
	}
	return scheduleItem{}, fmt.Errorf("no schedule with id %q", id)
}

// runSchedNow fires a schedule immediately, out of band. Used for testing
// and for the TUI `/sched run <id>` ergonomic. Does NOT update LastFiredAt
// or NextDueAt — those track the cron loop's view of "due".
func runSchedNow(id string) error {
	schedLock.Lock()
	var it scheduleItem
	found := false
	for _, s := range sched {
		if s.ID == id {
			it = s
			found = true
			break
		}
	}
	schedLock.Unlock()
	if !found {
		return fmt.Errorf("no schedule with id %q", id)
	}
	go fireSchedule(it, true)
	return nil
}

// fireSchedule enqueues the scheduled message onto the group's send queue.
// The per-group worker serializes it (in arrival order) against concurrent
// manual sends. We emit a sched_fired event so subscribers (e.g. the TUI) can
// render a marker before the prompt echo sendNow writes into .cs/log. The
// enqueue is non-blocking; we surface only an overflow error (queue full) —
// the turn result flows to the group's log, not back here.
func fireSchedule(it scheduleItem, manual bool) {
	ev := Event{Event: "sched_fired", Msg: it.Msg, ID: it.ID}
	if manual {
		ev.Event = "sched_run"
	}
	emit(it.Group, ev)
	if _, err := enqueueSend(it.Group, it.Msg); err != nil {
		emitLogf("error", "sched fire id=%s group=%s: %v", it.ID, it.Group, err)
	}
}

// cronLoop wakes at each wall-clock minute boundary and fires every enabled
// schedule whose NextDueAt has arrived. No catch-up: if the daemon was off
// when a fire was due, that fire is skipped (LastFiredAt + NextDueAt are
// reset to the next future occurrence on load).
func cronLoop() {
	for {
		// Sleep until the next minute boundary so ticks stay drift-free even
		// after long pauses (e.g. host suspend/resume).
		now := time.Now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		time.Sleep(time.Until(next))

		now = time.Now()
		schedLock.Lock()
		// Snapshot fires we need to trigger; mutate state under the same lock.
		var toFire []scheduleItem
		for i := range sched {
			s := &sched[i]
			if !s.Enabled {
				continue
			}
			p, ok := parsed[s.ID]
			if !ok {
				continue
			}
			due := int64(s.NextDueAt)
			if due == 0 || due > now.Unix() {
				continue
			}
			toFire = append(toFire, *s)
			s.LastFiredAt = float64(now.Unix())
			if nx, ok := p.next(now); ok {
				s.NextDueAt = float64(nx.Unix())
			} else {
				s.NextDueAt = 0
				s.Enabled = false
			}
		}
		if len(toFire) > 0 {
			saveSched()
		}
		schedLock.Unlock()

		for _, it := range toFire {
			emitLogf("info", "sched fire id=%s group=%s cron=%q", it.ID, it.Group, it.Cron)
			go fireSchedule(it, false)
		}
	}
}
