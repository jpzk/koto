package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestParseJobsTSV: well-formed lines parse with session normalization and
// numeric fields; junk lines (bad id, too few fields) are dropped; output is
// sorted by start time.
func TestParseJobsTSV(t *testing.T) {
	out := "zB9k2Xa1\trunning\t\tdefault\t200\t512\tsh -c sleep 99\n" +
		"aQ3fT7cD\tdone\t0\ttriage\t100\t2048\tcs-subagent summarise\n" +
		"../evil\tdone\t0\tx\t1\t1\tnope\n" + // bad id → dropped
		"short\tline\n" + // too few fields → dropped
		"bR4gU8dE\torphaned\t\tnot a session!!\t300\t0\tmake test\n"
	jobs := parseJobsTSV(out)
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs: %+v", len(jobs), jobs)
	}
	if jobs[0].ID != "aQ3fT7cD" || jobs[0].Session != "triage" || jobs[0].RC != "0" || jobs[0].OutSize != 2048 {
		t.Fatalf("job[0] = %+v", jobs[0])
	}
	if jobs[1].ID != "zB9k2Xa1" || jobs[1].Session != "" || jobs[1].Status != "running" {
		t.Fatalf("job[1] = %+v", jobs[1])
	}
	// Invalid session name degrades to default attribution, not an error.
	if jobs[2].ID != "bR4gU8dE" || jobs[2].Session != "" || jobs[2].Status != "orphaned" {
		t.Fatalf("job[2] = %+v", jobs[2])
	}
}

// 2026-10-03: jobsListScript forked ~8 processes per job directory, so a group
// with hundreds of finished jobs could never finish the walk inside the exec
// timeout and its mirror stayed empty — a running job included. The walk must
// now run with NOTHING on PATH but stat: any per-directory head/tr/basename
// fails here rather than in a loaded guest. It must also produce the same
// records the old one did, sizes included, across the 512-path stat batch.
func TestJobsListScriptForksNothingPerDirectory(t *testing.T) {
	statBin, err := exec.LookPath("stat")
	if err != nil {
		t.Skip("no stat on this host")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on this host")
	}
	dir := t.TempDir()
	bin := t.TempDir()
	if err := os.Symlink(statBin, filepath.Join(bin, "stat")); err != nil {
		t.Fatal(err)
	}
	write := func(d, name, v string) {
		if err := os.WriteFile(filepath.Join(d, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const n = 700 // > one 512-path stat batch
	for i := 0; i < n; i++ {
		d := filepath.Join(dir, fmt.Sprintf("job%05d", i))
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		write(d, "status", "done\n")
		write(d, "rc", "0\n")
		write(d, "started", fmt.Sprintf("%d\n", 1000+i))
		write(d, "cmd", fmt.Sprintf("cmd %d", i))
		write(d, "out", strings.Repeat("x", i))
	}
	// The interesting one: multi-line cmd with tabs, a session, a running
	// status, a FIFO where a field file should be (must not block), no rc.
	d := filepath.Join(dir, "liveJob1")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	write(d, "status", "running")
	write(d, "session", "triage\n")
	write(d, "started", "99999")
	write(d, "cmd", "sh /w/goal-run.sh\tvault\nsecond line"+strings.Repeat("y", 300))
	write(d, "out", "12345")
	if err := syscall.Mkfifo(filepath.Join(d, "rc"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dir with nothing in it at all.
	if err := os.Mkdir(filepath.Join(dir, "emptyJob"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := strings.ReplaceAll(jobsListScript, "/workspace/.cs/jobs", dir)
	cmd := exec.Command(bash, "--posix", "-c", script) // the guest runs it as /bin/sh = bash
	cmd.Env = []string{"PATH=" + bin}
	done := make(chan struct{})
	var out []byte
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the walk blocked (FIFO?)")
	}
	if err != nil {
		t.Fatalf("walk: %v\n%s", err, out)
	}
	jobs := parseJobsTSV(string(out))
	if len(jobs) != jobsMaxPerGroup {
		t.Fatalf("got %d jobs, want the newest %d", len(jobs), jobsMaxPerGroup)
	}
	byID := map[string]JobInfo{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	live, ok := byID["liveJob1"]
	if !ok {
		t.Fatal("the running job is missing")
	}
	if live.Status != "running" || live.Session != "triage" || live.RC != "" || live.OutSize != 5 || live.Started != 99999 {
		t.Errorf("live job = %+v", live)
	}
	if !strings.HasPrefix(live.Cmd, "sh /w/goal-run.sh vault second line") || len(live.Cmd) > 200 {
		t.Errorf("cmd not flattened/bounded: %q (%d)", live.Cmd, len(live.Cmd))
	}
	// Sizes from both stat batches: job00699 is in the second one.
	last := byID[fmt.Sprintf("job%05d", n-1)]
	if last.OutSize != int64(n-1) || last.Status != "done" || last.RC != "0" || last.Cmd != fmt.Sprintf("cmd %d", n-1) {
		t.Errorf("last job = %+v", last)
	}
	// The empty dir falls outside the newest jobsMaxPerGroup (started=0), so
	// check its raw record.
	if !strings.Contains(string(out), "emptyJob\tunknown\t\t\t0\t\t\n") {
		t.Error("an empty job dir did not degrade to status=unknown, started=0")
	}
}

// A forged size line cannot invent a job, and cannot name an id that fails
// jobIDRE; the first size for an id wins.
func TestJobsSizeLinesCannotForge(t *testing.T) {
	out := "aaaaaaaa\trunning\t\t\t5\t\tmake\n" +
		"#sz\t42\t/w/jobs/aaaaaaaa//out\n" +
		"#sz\t99\t/w/jobs/aaaaaaaa//out\n" +
		"#sz\t7\t/w/jobs/bbbbbbbb//out\n" + // no such record: no job
		"#sz\t7\t/w/jobs/../out\n" +
		"#sz\t7\t/w/jobs/cccccccc//status\n"
	jobs := parseJobsTSV(out)
	if len(jobs) != 1 || jobs[0].ID != "aaaaaaaa" || jobs[0].OutSize != 42 {
		t.Fatalf("jobs = %+v", jobs)
	}
}

// A failed read backs off the watcher-driven refresh for jobsFailBackoff (the
// guest keeps running an abandoned exec until its own cap), and a streak of
// failures is counted so it can be reported once at warn.
func TestJobsRefreshFailureBacksOff(t *testing.T) {
	fcHarness(t)
	const g = "jobsbackoff"
	t.Cleanup(func() { dropJobsCache(g) })
	jobsMu.Lock()
	jobsCache[g] = jobsCacheEntry{jobs: []JobInfo{{ID: "kept"}}}
	jobsRefreshing[g] = true // refreshJobsNow's precondition
	jobsMu.Unlock()
	for i := 0; i < jobsFailWarnAfter; i++ {
		if got := refreshJobsNow(g); len(got) != 1 || got[0].ID != "kept" {
			t.Fatalf("a failed read did not serve the last good list: %+v", got)
		}
	}
	jobsMu.Lock()
	e := jobsCache[g]
	delete(jobsRefreshing, g)
	jobsMu.Unlock()
	if e.fails != jobsFailWarnAfter || e.failedAt.IsZero() {
		t.Fatalf("failure streak not recorded: %+v", e)
	}
	kickJobsRefresh(g, false)
	jobsMu.Lock()
	kicked := jobsRefreshing[g]
	jobsMu.Unlock()
	if kicked {
		t.Fatal("a refresh was kicked inside the failure backoff")
	}
}
