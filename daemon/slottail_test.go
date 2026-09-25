package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A slot's tailer must be positioned BEFORE ensureSlotTail returns. sendNow
// calls ensureSlotTail and then immediately writes the turn's [[session]]
// marker into that slot's log; when the tailer opened the file (and seeked to
// EOF) inside its own goroutine, it usually did so after the marker was
// written, never saw it, and attributed the whole turn — [[turn_end]]
// included — to the default session. The named session's sendNow then waited
// on a completion that had gone to the wrong lane: its queue froze, and 25
// minutes later the turn was declared STALLED and self-heal restarted the VM
// under every other session's running turn. It needed only two sessions
// running at once, since the second one is what gets a fresh slot.
func TestSlotTailerSeesTheFirstTurnsSessionMarker(t *testing.T) {
	prevRoot := ROOT
	ROOT = t.TempDir()
	t.Cleanup(func() { ROOT = prevRoot })

	// Many fresh slots: a goroutine-side open loses this race almost always,
	// but "almost" is not what a regression test should rest on.
	for i := 0; i < 20; i++ {
		g := fmt.Sprintf("slottail%d", i)
		sess := "s2"
		const slot = 1
		t.Cleanup(func() { dropGroupTailState(g) })

		named := turnDoneCh(g, sess)
		def := turnDoneCh(g, "")
		// A live group's .cs/ exists long before its first concurrent turn.
		if err := os.MkdirAll(filepath.Dir(slotLogPath(g, slot)), 0o700); err != nil {
			t.Fatal(err)
		}

		ensureSlotTail(g, slot)
		// Exactly what sendNow writes next, with no pause in between...
		appendLog(t, slotLogPath(g, slot), fmt.Sprintf("%s\n[ts:%d]\n>>> hi\n", sessionMarker(sess), time.Now().UnixMilli()))
		// ...and the guest's response, which arrives only after delivery.
		time.Sleep(150 * time.Millisecond)
		appendLog(t, slotLogPath(g, slot), "hello\n[[turn_end]]\n")

		select {
		case <-named:
		case <-def:
			t.Fatalf("%s: the first turn on a fresh slot was attributed to the default session — the tailer missed its [[session]] marker", g)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no turn_end reached any session", g)
		}
	}
}

func appendLog(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}
