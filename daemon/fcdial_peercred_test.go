package main

// Branch-only tests for the ownership / SO_PEERCRED legs of fcDialVMM. Both
// fake listeners run as the test's uid, so a foreign peer is simulated via
// the fcPeerUID seam and a foreign owner via the registered jail uid.

import (
	"net"
	"os"
	"testing"
	"time"
)

func TestP02RealPeerCredIsOwnUID(t *testing.T) {
	twoGroups(t)
	c, err := fcDialVMM("p02a", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uid, err := fcPeerUID(c)
	if err != nil || uid != os.Getuid() {
		t.Fatalf("SO_PEERCRED uid=%d err=%v, want %d", uid, err, os.Getuid())
	}
}

func TestP02HostDialRefusesForeignPeer(t *testing.T) {
	twoGroups(t)
	prev := fcPeerUID
	fcPeerUID = func(*net.UnixConn) (int, error) { return os.Getuid() + 4242, nil }
	t.Cleanup(func() { fcPeerUID = prev })
	if c, err := fcHostDial("p02a", fcPortAgent, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("dial accepted a peer whose SO_PEERCRED uid is not the group's jail uid")
	}
}

func TestP02HostDialRefusesForeignOwner(t *testing.T) {
	a, _ := twoGroups(t)
	fcSetDialUID("p02a", os.Getuid()+4242)
	if c, err := fcHostDial("p02a", fcPortAgent, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("dial accepted a socket inode not owned by the group's jail uid")
	}
	assertNoConn(t, "p02a (wrong owner)", a)
}

func TestP02HostDialRefusesUnregisteredGroup(t *testing.T) {
	twoGroups(t)
	fcDialUIDMu.Lock()
	delete(fcDialUIDs, "p02a")
	fcDialUIDMu.Unlock()
	if c, err := fcHostDial("p02a", fcPortAgent, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("dial accepted a group with no registered jail uid")
	}
}
