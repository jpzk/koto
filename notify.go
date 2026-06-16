package main

// Background-job completion callbacks (`cs-job run --notify`).
//
// A notify-flagged job writes `{"cmd":"job_done","id":...}` to its group's ctl
// FIFO when it finishes (see sidecar/cs-job). ctlDispatch turns that into
// scheduleNotifyFlush(group). We DON'T send one wake-up per job: a fan-out of N
// jobs finishing in a burst would otherwise fire N unprompted turns that all
// contend for the group's single-flight loop. Instead each job_done (re)arms a
// short debounce timer; when it fires we coalesce every completed-but-unreported
// notify job into ONE self-send via enqueueSend — the same queued path used by
// the scheduler and ctl `send`, so sendLock / system-prompt composition / turn
// tracking all stay correct (unlike a raw write to .cs/in).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// notifyDebounce: wait this long after the most recent completion before
	// flushing, so a burst of fan-out jobs collapses into a single wake-up. A
	// straggler that finishes later simply triggers its own later flush — we
	// never block the batch waiting for a job that may have hung.
	notifyDebounce = 3 * time.Second
	// notifyTailBytes bounds how much of each job's output we inline into the
	// callback message (the agent runs `cs-job logs <id>` for the full text).
	notifyTailBytes = 1500
)

var (
	notifyMu     sync.Mutex
	notifyTimers = map[string]*time.Timer{}
)

// scheduleNotifyFlush (re)arms the per-group debounce timer. Called from the
// ctl loop (serialized per group), so map access only races across groups,
// which the mutex covers.
func scheduleNotifyFlush(group string) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if t, ok := notifyTimers[group]; ok {
		t.Stop()
	}
	notifyTimers[group] = time.AfterFunc(notifyDebounce, func() {
		notifyMu.Lock()
		delete(notifyTimers, group)
		notifyMu.Unlock()
		flushNotify(group)
	})
}

// flushNotify scans the group's jobs dir for notify jobs that are done but not
// yet reported, and enqueues one coalesced self-send. Jobs are marked
// `notified` only on a successful enqueue, so a full queue is retried on the
// next job_done rather than silently dropping the result.
func flushNotify(group string) {
	jobsDir := filepath.Join(ROOT, group, ".cs", "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return
	}
	var ready []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(jobsDir, e.Name())
		if !jExists(filepath.Join(d, "notify")) || jExists(filepath.Join(d, "notified")) {
			continue
		}
		if jReadTrim(filepath.Join(d, "status")) != "done" {
			continue
		}
		ready = append(ready, e.Name())
	}
	if len(ready) == 0 {
		return
	}
	sort.Strings(ready)

	var b strings.Builder
	b.WriteString("Background job(s) completed:\n")
	for _, id := range ready {
		d := filepath.Join(jobsDir, id)
		rc := jReadTrim(filepath.Join(d, "rc"))
		if rc == "" {
			rc = "?"
		}
		out, total := jTailBytes(filepath.Join(d, "out"), notifyTailBytes)
		fmt.Fprintf(&b, "\n[job %s rc=%s]\n%s\n", id, rc, strings.TrimRight(out, "\n"))
		if total > int64(len(out)) {
			fmt.Fprintf(&b, "(...truncated; %d bytes total — `cs-job logs %s` for all)\n", total, id)
		}
	}
	b.WriteString("\n(`cs-job logs <id>` for full output; `cs-job clean` to clear finished jobs.)")

	if _, err := enqueueSend(group, b.String()); err != nil {
		emitLogf("warn", "notify[%s]: enqueue failed, will retry on next job_done: %v", group, err)
		return
	}
	for _, id := range ready {
		_ = os.WriteFile(filepath.Join(jobsDir, id, "notified"), []byte("1"), 0o644)
	}
	emitLogf("info", "notify[%s]: reported %d completed job(s)", group, len(ready))
}

func jExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func jReadTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// jTailBytes returns up to max trailing bytes of the file and its total size.
func jTailBytes(p string, max int) (string, int64) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", 0
	}
	total := fi.Size()
	f, err := os.Open(p)
	if err != nil {
		return "", total
	}
	defer f.Close()
	if total > int64(max) {
		_, _ = f.Seek(total-int64(max), io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	return string(b), total
}
