package main

import "testing"

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
