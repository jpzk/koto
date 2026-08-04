package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// allocPortHarness gives each test its own GROUPS_FILE so they don't stomp
// on the real one and so they're independent of execution order.
func allocPortHarness(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	HERE = dir
	GROUPS_FILE = filepath.Join(dir, "groups.json")
	PORT_BASE = 8787
}

func destroyGroupForTest(g string) {
	groupsLock.Lock()
	defer groupsLock.Unlock()
	m := readGroups()
	delete(m, g)
	writeGroups(m)
}

// TestAllocPortChurn is the regression test for the cross-group routing
// bug: with PORT_BASE+len(m), destroying a middle group caused the next
// allocation to reuse a still-live port. max+1 is collision-free by
// induction; this proves it on the exact sequence that produced the bug.
func TestAllocPortChurn(t *testing.T) {
	allocPortHarness(t)
	a := allocPort("a")
	_ = allocPort("b")
	c := allocPort("c")
	destroyGroupForTest("b")
	d := allocPort("d")
	if d == a || d == c {
		t.Fatalf("allocPort reused port: a=%d b(destroyed) c=%d d=%d", a, c, d)
	}
	if d <= c {
		t.Fatalf("allocPort went backwards: c=%d d=%d (max+1 should be %d)", c, d, c+1)
	}
}

// TestAllocPortIdempotent — same group name returns same port.
func TestAllocPortIdempotent(t *testing.T) {
	allocPortHarness(t)
	p1 := allocPort("foo")
	p2 := allocPort("foo")
	if p1 != p2 {
		t.Fatalf("allocPort not idempotent: %d != %d", p1, p2)
	}
}

// TestAllocPortNoDupesOverChurn — many create/destroy cycles never produce
// a duplicate live port. Stronger than TestAllocPortChurn — exercises the
// invariant across a longer sequence.
func TestAllocPortNoDupesOverChurn(t *testing.T) {
	allocPortHarness(t)
	groups := []string{"g0", "g1", "g2", "g3", "g4"}
	for _, g := range groups {
		allocPort(g)
	}
	// Destroy + recreate the middle three, twice each.
	for round := 0; round < 2; round++ {
		for _, g := range groups[1:4] {
			destroyGroupForTest(g)
			allocPort(g)
		}
	}
	m := readGroups()
	ports := make([]int, 0, len(m))
	for _, p := range m {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for i := 1; i < len(ports); i++ {
		if ports[i] == ports[i-1] {
			t.Fatalf("duplicate live port %d after churn; ports=%v map=%v", ports[i], ports, m)
		}
	}
}

// Reset the real GROUPS_FILE if tests left a stale tempdir reference behind.
// Belt-and-braces: t.TempDir() is per-test so HERE/GROUPS_FILE point at gone
// dirs after each test. Real daemon code re-runs initPaths(), so this only
// matters if another test in this package depends on the package-level vars.
//
// markTailed(main) up front because ANY test that emits a warn/error log
// line now queues an operator notification against main (logalert.go), and
// ensureTail would spawn a real tailLog goroutine that outlives its test,
// chases ROOT into later tests' tempdirs, and races their cleanup (see
// markTailed). Tests that assert on delivery play the tailer themselves via
// deliverNotify.
func TestMain(m *testing.M) {
	markTailed(ctlMainGroup)
	defer func() {
		HERE = ""
		GROUPS_FILE = ""
	}()
	os.Exit(m.Run())
}
