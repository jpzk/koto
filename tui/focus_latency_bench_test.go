package main

// focus_latency_bench_test.go — keystroke latency in the FOCUSED pane under a
// realistic background load. The question these answer is the operator's
// ("wherever focus lies must be flawless"): how long from a key arriving at
// Update until the frame that shows it has been built by View?
//
// go test -run x -bench Focus ./...
//
// Each keystroke is timed on its own (time.Now around exactly the work that
// stands between the key and its visible frame) and the samples are sorted,
// so the benchmark reports the tail — p50-ns and p99-ns — not just the mean
// that ns/op gives. allocs/key is the heap allocations of those timed
// sections only.
//
//   - BenchmarkFocusInput: the message bar is focused and the user types
//     (runes, then backspaces, cycling so the draft stays bounded).
//     Timed: Update(KeyMsg) + View().
//   - BenchmarkFocusShell: the shared terminal pane is open and focused.
//     The guest echoes every key, so the visible latency is
//     Update(KeyMsg) + View() + Update(echo shellFrameMsg) + View().
//
// Sub-benchmarks:
//   - idle:  nothing else arrives between keystrokes (the fleet is still
//     busy — groups mid-turn, so the spinner chain is live).
//   - burst: background messages are interleaved with the typing — 500 per
//     100 keystrokes (5 before every key): streaming deltas for the focused
//     group and other groups, finished turns, spinner ticks, resource polls,
//     state pushes and (shell mode) unrelated pty output. Each background
//     message gets its own Update + View like Bubble Tea gives it, but is NOT
//     timed: only the keystroke is. The criterion is that the burst does not
//     drag the keystroke's tail (burst p99 within 2x of idle p99).
//
// The harness only touches APIs that already existed before the latency
// work, so the same file measures the before and the after.

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"

	"koto-protocol/pb"
)

// focusMinSamples keeps p99 meaningful even when b.N is small (a slow frame
// makes the 1s benchtime settle on a few dozen iterations).
const focusMinSamples = 300

// focusBurstPerKey: 500 background messages per 100 keystrokes.
const focusBurstPerKey = 5

// discardShellStream is a pty stream stub that drops what the pane sends.
type discardShellStream struct{ pb.Koto_AttachShellClient }

func (discardShellStream) Send(*pb.ShellInput) error { return nil }

// focusFleet builds the attached-session model: 32 groups (several sessions,
// goal sessions and jobs each), resource metrics for the top/status bars, a
// populated markdown transcript for the focused group, and several groups
// mid-turn so the 80ms spinner chain is running.
func focusFleet(b testing.TB) Model {
	b.Helper()
	const nGroups = 32
	m := newModel("", 200000)
	m.width, m.height = 160, 48
	groups := map[string]GroupInfo{}
	res := map[string]GroupRes{}
	for i := 0; i < nGroups; i++ {
		g := fmt.Sprintf("group%02d", i)
		gi := GroupInfo{
			Running: i%4 != 0, Provider: "claudesdk", Model: "claude-sonnet-5",
			Network: "wan", TokPerSec: float64(i % 7), Queued: i % 3,
		}
		if i%3 == 0 {
			gi.Sessions = []string{"triage", "goal-fix-" + g}
			gi.SessionOrigins = map[string]string{"goal-fix-" + g: "goal"}
		}
		if i%5 == 0 {
			gi.Jobs = []JobInfo{
				{ID: "j" + g + "a", Status: "running", Cmd: "go test ./...", Started: 1786000000},
				{ID: "j" + g + "b", Status: "running", Cmd: "make build", Started: 1786000100},
			}
		}
		groups[g] = gi
		res[g] = GroupRes{
			Running: gi.Running, CPUPct: float64(i * 3), Vcpus: 2, MemMiB: 1024,
			RSSBytes: int64(500+i) << 20, AllocBytes: int64(i+1) << 28,
			DeclaredBytes: 8 << 30, GuestDiskTotal: 8 << 30,
			GuestDiskAvail: 6 << 30, GuestDiskUsed: 1 << 30,
		}
	}
	m.cur = "group01"
	nm, _ := m.Update(listMsg{groups: groups, globalTok: 42, hasGlobalTok: true})
	m = nm.(Model)
	nm, _ = m.Update(resourcesMsg{groups: res})
	m = nm.(Model)

	evs := make([]Event, 0, 120)
	for i := 0; i < 60; i++ {
		ts := float64(1786000000 + 10*i)
		evs = append(evs,
			Event{Event: "prompt", Group: "group01", Msg: fmt.Sprintf("question %d about the daemon's shutdown path", i), Ts: ts},
			Event{Event: "done", Group: "group01", Ts: ts + 1, Text: "**Answer** with `code`, a list:\n" +
				"- one item that is long enough to wrap at a narrow column width, really\n- two\n\n" +
				"```go\nfunc main() {\n\tfor i := range 3 { println(\"x\", i) }\n}\n```\n\nand a closing paragraph."},
		)
	}
	nm, _ = m.Update(historyMsg{group: "group01", events: evs})
	m = nm.(Model)
	// The history load leaves the group's prewarm marked in flight (its cmd
	// never runs here) — clear it so the steady state is the cached one a
	// real session settles into.
	delete(m.prewarming, m.cur)

	// Several turns in flight across the fleet (the cur group included):
	// isAnimating() is true, which is the operator's normal busy fleet.
	for _, g := range []string{"group01", "group05", "group09", "group13"} {
		nm, _ = m.Update(streamEventMsg(Event{Event: "prompt", Group: g, Msg: "keep going", Ts: 1786009000}))
		m = nm.(Model)
	}
	m.focus = focusInput
	nm, _ = m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	m = nm.(Model)
	m.resizePending = false
	nm, _ = m.Update(spinTickMsg{})
	return nm.(Model)
}

// shellScreen is one realistic repaint of a guest tmux pane: colored prompt,
// ls-style output, a status bar — SGR only (what scrubVT keeps).
func shellScreen(rows int) []byte {
	var sb strings.Builder
	sb.WriteString("\x1b[H\x1b[2J")
	for r := 0; r < rows-2; r++ {
		fmt.Fprintf(&sb, "\x1b[32mkoto@vm\x1b[0m:\x1b[34m~/src\x1b[0m$ ls -la \x1b[1;36mdir%02d\x1b[0m  file_%02d.go  \x1b[33mREADME.md\x1b[0m  %d\r\n", r, r, r*1024)
	}
	sb.WriteString("\x1b[42;30m[0] 0:bash*  1:vim-  2:htop                                \x1b[0m\r\n")
	sb.WriteString("\x1b[32mkoto@vm\x1b[0m:\x1b[34m~\x1b[0m$ ")
	return []byte(sb.String())
}

// openShell attaches a stub shared terminal sized like the real pane, paints
// a full screen into it and moves focus there.
func openShell(b testing.TB, m Model) Model {
	b.Helper()
	m.shellOpen = true
	w, h := m.shellPaneSize()
	term := vt.NewEmulator(w, h)
	b.Cleanup(func() { closeEmulator(term) })
	m.shell = &shellSession{id: 7, term: term, stream: discardShellStream{}, group: m.cur, session: "koto-shell", cols: w, rows: h}
	m.preShellFocus = focusInput
	m.focus = focusShell
	nm, _ := m.Update(shellFrameMsg{id: 7, data: shellScreen(h)})
	return nm.(Model)
}

// background returns the i-th message of the interleaved load.
func background(i int, shell bool) tea.Msg {
	others := []string{"group05", "group09", "group13"}
	g := others[i%len(others)]
	switch i % 10 {
	case 0, 1:
		// Streaming delta for the FOCUSED group — the transcript is live.
		return streamEventMsg(Event{Event: "stream", Group: "group01",
			Text: strings.Repeat("streaming answer text ", 1+i%40)})
	case 2, 3:
		return streamEventMsg(Event{Event: "stream", Group: g,
			Text: strings.Repeat("other group stream ", 1+i%30)})
	case 4:
		return spinTickMsg{}
	case 5:
		// Another group finishes a turn and starts the next one.
		if i%20 == 5 {
			return streamEventMsg(Event{Event: "done", Group: g, Text: "done: **ok**", Ts: float64(1786010000 + i)})
		}
		return streamEventMsg(Event{Event: "prompt", Group: g, Msg: "next", Ts: float64(1786010000 + i)})
	case 6:
		return contentFlushMsg{}
	case 7:
		if shell {
			// Unrelated pty output (a status-bar clock repaint).
			return shellFrameMsg{id: 7, data: []byte(fmt.Sprintf("\x1b7\x1b[1;60H\x1b[7m %02d:%02d \x1b[0m\x1b8", i/60%24, i%60))}
		}
		return spinTickMsg{}
	case 8:
		return metricsRespMsg{group: "group01", metric: map[string]any{"usage": map[string]any{"in": float64(i)}}}
	default:
		return spinTickMsg{}
	}
}

func step(m Model, msg tea.Msg) Model {
	nm, _ := m.Update(msg)
	return nm.(Model)
}

// focusKey is the i-th keystroke of the typing pattern: 24 runes, then 24
// backspaces, so the draft (and its wrap) stays bounded however long it runs.
func focusKey(i int) tea.KeyMsg {
	if (i/24)%2 == 1 {
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune('a' + i%26)}}
}

func runFocus(b *testing.B, shell, burst bool) {
	m := focusFleet(b)
	if shell {
		m = openShell(b, m)
		if m.focus != focusShell || !m.shellViewActive() {
			b.Fatalf("shell pane not focused/visible (focus=%v)", m.focus)
		}
	} else if m.focus != focusInput {
		b.Fatalf("input not focused (focus=%v)", m.focus)
	}
	// Settle: a few untimed rounds so first-render caches are warm.
	for i := 0; i < 50; i++ {
		m = step(m, focusKey(i))
		_ = m.View()
	}
	n := max(b.N, focusMinSamples)
	samples := make([]time.Duration, 0, n)
	var ms runtime.MemStats
	var allocs uint64
	bg := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < n; i++ {
		if burst {
			for j := 0; j < focusBurstPerKey; j++ {
				m = step(m, background(bg, shell))
				bg++
				_ = m.View()
			}
		}
		key := focusKey(i)
		var echo tea.Msg
		if shell {
			r := byte('a' + i%26)
			if key.Type == tea.KeyBackspace {
				echo = shellFrameMsg{id: 7, data: []byte("\b \b")}
			} else {
				echo = shellFrameMsg{id: 7, data: []byte{r}}
			}
		}
		runtime.ReadMemStats(&ms)
		a0 := ms.Mallocs
		t0 := time.Now()
		m = step(m, key)
		_ = m.View()
		if echo != nil {
			m = step(m, echo)
			_ = m.View()
		}
		d := time.Since(t0)
		runtime.ReadMemStats(&ms)
		allocs += ms.Mallocs - a0
		samples = append(samples, d)
	}
	b.StopTimer()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	pct := func(p float64) float64 {
		k := int(p*float64(len(samples))+0.5) - 1
		k = min(max(k, 0), len(samples)-1)
		return float64(samples[k].Nanoseconds())
	}
	b.ReportMetric(pct(0.50), "p50-ns")
	b.ReportMetric(pct(0.99), "p99-ns")
	b.ReportMetric(float64(allocs)/float64(len(samples)), "allocs/key")
}

func BenchmarkFocusInput(b *testing.B) {
	b.Run("idle", func(b *testing.B) { runFocus(b, false, false) })
	b.Run("burst", func(b *testing.B) { runFocus(b, false, true) })
}

func BenchmarkFocusShell(b *testing.B) {
	b.Run("idle", func(b *testing.B) { runFocus(b, true, false) })
	b.Run("burst", func(b *testing.B) { runFocus(b, true, true) })
}
