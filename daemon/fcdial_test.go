package main

// Regression tests for audit p02-001: a VMM that escaped its guest swaps its
// own run/fc/<g>.sock/v for a symlink to another group's socket; the daemon's
// host→guest dial must refuse it and the other group's VMM must never see a
// connection. Uses only fcHostDial + fcSetDialUID so the same file runs
// against the unfixed code (with a no-op fcSetDialUID stub) as mutation proof.

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVMM is a stand-in Firecracker hybrid-vsock base listener: it answers
// "CONNECT <port>\n" with "OK <port>\n" and counts accepted connections.
type fakeVMM struct {
	ln      net.Listener
	accepts atomic.Int32
}

func newFakeVMM(t *testing.T, path string) *fakeVMM {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	f := &fakeVMM{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepts.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				line, err := readLineByte(c)
				if err != nil {
					return
				}
				port := strings.TrimPrefix(strings.TrimSpace(line), "CONNECT ")
				_, _ = c.Write([]byte("OK " + port + "\n"))
				buf := make([]byte, 1)
				_, _ = c.Read(buf) // hold until the client closes
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

// twoGroups lays out run/fc/{a,b}.sock/v exactly like fcSpawn does (0700
// dirs, base socket "v") with a genuine listener in each, and registers both
// groups' expected uid (the test's own uid stands in for the jail uid).
func twoGroups(t *testing.T) (a, b *fakeVMM) {
	t.Helper()
	prev := SOCK_DIR
	// Short base: sun_path is 108 bytes and t.TempDir() can be long.
	dir, err := os.MkdirTemp("", "p02")
	if err != nil {
		t.Fatal(err)
	}
	SOCK_DIR = dir
	t.Cleanup(func() { SOCK_DIR = prev; os.RemoveAll(dir) })
	for _, g := range []string{"p02a", "p02b"} {
		if err := os.MkdirAll(fcSockDir(g), 0o700); err != nil {
			t.Fatal(err)
		}
		fcSetDialUID(g, os.Getuid())
	}
	return newFakeVMM(t, fcUDS("p02a")), newFakeVMM(t, fcUDS("p02b"))
}

func assertNoConn(t *testing.T, who string, f *fakeVMM) {
	t.Helper()
	time.Sleep(200 * time.Millisecond) // let any stray accept land
	if n := f.accepts.Load(); n != 0 {
		t.Fatalf("%s's VMM listener received %d connection(s); the dial was redirected into it", who, n)
	}
}

// The attack: group A's VMM replaces its own "v" with a symlink to group B's
// socket (relative text, as planted from inside the chroot).
func TestP02HostDialRefusesSymlinkedV(t *testing.T) {
	_, b := twoGroups(t)
	v := fcUDS("p02a")
	if err := os.Remove(v); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../p02b.sock/v", v); err != nil {
		t.Fatal(err)
	}
	c, err := fcHostDial("p02a", fcPortAgent, 2*time.Second)
	if err == nil {
		c.Close()
		t.Fatalf("fcHostDial(p02a) followed a symlink to p02b's socket and connected")
	}
	assertNoConn(t, "p02b", b)
}

// Same attack one level up: the sockdir entry itself is a symlink. run/fc is
// daemon-owned so a VMM cannot do this, but the dial must not depend on it.
func TestP02HostDialRefusesSymlinkedSockDir(t *testing.T) {
	_, b := twoGroups(t)
	d := fcSockDir("p02a")
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("p02b.sock", d); err != nil {
		t.Fatal(err)
	}
	c, err := fcHostDial("p02a", fcPortAgent, 2*time.Second)
	if err == nil {
		c.Close()
		t.Fatalf("fcHostDial(p02a) followed a symlinked sockdir into p02b")
	}
	assertNoConn(t, "p02b", b)
}

// Positive path: genuine sockets still connect and complete the handshake,
// each to its own group's listener.
func TestP02HostDialGenuineSocket(t *testing.T) {
	a, b := twoGroups(t)
	for _, tc := range []struct {
		g    string
		self *fakeVMM
		othr *fakeVMM
	}{{"p02a", a, b}, {"p02b", b, a}} {
		before := tc.othr.accepts.Load()
		c, err := fcHostDial(tc.g, fcPortAgent, 2*time.Second)
		if err != nil {
			t.Fatalf("fcHostDial(%s) on its genuine socket: %v", tc.g, err)
		}
		c.Close()
		if n := tc.self.accepts.Load(); n != 1 {
			t.Fatalf("%s's listener accepts = %d, want 1", tc.g, n)
		}
		if tc.othr.accepts.Load() != before {
			t.Fatalf("dial for %s touched the other group's listener", tc.g)
		}
	}
}
