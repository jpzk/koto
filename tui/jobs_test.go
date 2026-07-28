package main

import "testing"

// gj builds a one-group state map with the given job list.
func gj(g string, jobs ...JobInfo) map[string]GroupInfo {
	return map[string]GroupInfo{g: {Running: true, Jobs: jobs}}
}

// A frame carrying no jobs is the daemon's cold/dropped mirror, not proof the
// group has none: priming off it made the whole finished backlog look freshly
// completed the moment the real list arrived, so every done/orphaned job
// blinked back into the tree on attach.
func TestColdMirrorDoesNotResurrectFinishedJobs(t *testing.T) {
	m := newModel("", 200000)
	backlog := []JobInfo{
		{ID: "aaaa1111", Status: "done", RC: "0", Cmd: "make test"},
		{ID: "bbbb2222", Status: "done", RC: "1", Cmd: "make lint"},
		{ID: "cccc3333", Status: "orphaned", Cmd: "sleep 999"},
	}

	m.processJobTransitions(gj("main"))             // cold mirror: jobs=[]
	m.processJobTransitions(gj("main", backlog...)) // real list lands

	for _, j := range backlog {
		if m.jobVisible("main", j) {
			t.Errorf("job %s (%s) re-appeared in the tree after the mirror warmed up", j.ID, j.Status)
		}
	}
}

// The backlog must stay hidden across a group restart too: while the group is
// down the daemon drops the mirror, and the entrypoint relabels nothing that
// was already finished.
func TestRestartDoesNotResurrectFinishedJobs(t *testing.T) {
	m := newModel("", 200000)
	done := JobInfo{ID: "aaaa1111", Status: "done", RC: "1", Cmd: "make test"}

	m.processJobTransitions(gj("main", done))
	m.processJobTransitions(map[string]GroupInfo{"main": {Running: false}}) // stopped: mirror dropped
	m.processJobTransitions(gj("main", done))                               // back up

	if m.jobVisible("main", done) {
		t.Error("finished job re-appeared after a group restart")
	}
}

// A job we actually watched finish still gets its linger window.
func TestObservedCompletionLingers(t *testing.T) {
	m := newModel("", 200000)
	run := JobInfo{ID: "aaaa1111", Status: "running", Cmd: "make test"}
	m.processJobTransitions(gj("main", run))
	if !m.jobVisible("main", run) {
		t.Fatal("running job not visible")
	}
	fin := run
	fin.Status, fin.RC = "done", "1"
	m.processJobTransitions(gj("main", fin))
	if !m.jobVisible("main", fin) || !m.jobBlinking("main", fin.ID) {
		t.Error("job observed finishing should linger and blink")
	}
}

// A job that appears already-finished in a primed group (completed inside the
// refresh gap) still counts as a fresh completion.
func TestUnseenCompletionInPrimedGroupLingers(t *testing.T) {
	m := newModel("", 200000)
	other := JobInfo{ID: "aaaa1111", Status: "running", Cmd: "make test"}
	m.processJobTransitions(gj("main", other)) // primes off a non-empty list
	fast := JobInfo{ID: "bbbb2222", Status: "done", RC: "0", Cmd: "echo hi"}
	m.processJobTransitions(gj("main", other, fast))
	if !m.jobVisible("main", fast) {
		t.Error("job that finished inside the refresh gap should linger")
	}
}

// Tracking state is still pruned when the job really goes away (cs-job rm) or
// the group is destroyed — the empty-list guard must not leak entries forever.
func TestTrackingPruned(t *testing.T) {
	m := newModel("", 200000)
	a := JobInfo{ID: "aaaa1111", Status: "running", Cmd: "a"}
	b := JobInfo{ID: "bbbb2222", Status: "running", Cmd: "b"}
	m.processJobTransitions(gj("main", a, b))
	m.processJobTransitions(gj("main", a)) // b removed, list still authoritative
	if _, ok := m.jobStatusSeen[jobKey("main", b.ID)]; ok {
		t.Error("removed job still tracked")
	}
	m.processJobTransitions(map[string]GroupInfo{}) // group destroyed
	if len(m.jobStatusSeen) != 0 {
		t.Errorf("tracking not pruned after group vanished: %v", m.jobStatusSeen)
	}
}
