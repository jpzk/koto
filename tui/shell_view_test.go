package main

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// 2026-09-11 L84: the guest emits a frame per non-empty pty read and the TUI
// handed each one straight to the terminal emulator, synchronously, on the
// single Bubble Tea update loop — so a guest that simply keeps writing kept the
// whole UI busy. The 16 MiB protocol limit bounds one FRAME, not the stream,
// and transport backpressure slows the producer without ever being a policy.
func TestShellPaneHasAnOutputBudget(t *testing.T) {
	s := &shellSession{group: "g", cols: 80, rows: 24, term: vt.NewEmulator(80, 24)}

	// The burst absorbs an honest full repaint without throttling anything.
	if !s.budget(1 << 20) {
		t.Fatal("a one-megabyte repaint was throttled")
	}
	// A flood is not absorbed.
	spent, dropped := 0, 0
	for i := 0; i < 64; i++ {
		if s.budget(1 << 20) {
			spent++
		} else {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatalf("64 MiB in one instant was admitted whole (%d chunks spent)", spent)
	}
	if s.outDropped == 0 {
		t.Fatal("dropped bytes were not counted, so the pane cannot say it is incomplete")
	}
	// Refilling lets it through again — this is a throttle, not a disconnect.
	s.outTokens, s.outLast = shellBytesBurst, time.Now()
	if !s.budget(1 << 20) {
		t.Fatal("a refilled budget still refused")
	}
	// And the pane says so, rather than silently missing output.
	before := s.outDropped
	s.feed([]byte("x"))
	if s.outDropped != 0 {
		t.Errorf("the dropped count (%d) was not reported and cleared", before)
	}
}
