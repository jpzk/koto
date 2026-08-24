package main

// render_bench_test.go — what one frame costs, and therefore what the 80ms
// spinner tick costs. isAnimating() is true whenever ANY group in the fleet is
// mid-turn, so on a busy fleet the tick chain runs continuously and every one
// of those ticks repaints the whole frame whether or not anything visible
// changed. These benchmarks say how much CPU that is.

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// benchModel builds a model shaped like a real attached session: a fleet-sized
// group list, a populated chat log for the focused group, and the tree open.
func benchModel(b *testing.B, nGroups, nEvents int) Model {
	b.Helper()
	m := newModel("", 200000)
	m.width, m.height = 150, 44
	m.groups = map[string]GroupInfo{}
	m.resources = map[string]GroupRes{}
	for i := 0; i < nGroups; i++ {
		g := fmt.Sprintf("group%02d", i)
		m.groups[g] = GroupInfo{
			Running: i%3 != 0, Model: "claude-sonnet-5",
			Network: "wan", TokPerSec: float64(i),
		}
		m.resources[g] = GroupRes{
			Running: i%3 != 0, CPUPct: float64(i * 3), Vcpus: 2, MemMiB: 1024,
			RSSBytes: int64(500+i) << 20, AllocBytes: int64(i+1) << 30,
			DeclaredBytes: 8 << 30, GuestDiskTotal: 8 << 30,
			GuestDiskAvail: 6 << 30, GuestDiskUsed: 1 << 30,
		}
	}
	m.cur = "group01"
	m.groups["group01"] = GroupInfo{Running: true, Model: "claude-sonnet-5"}

	evs := make([]Event, 0, nEvents)
	for i := 0; i < nEvents; i++ {
		ts := float64(1786000000 + i)
		evs = append(evs,
			Event{Event: "prompt", Group: "group01", Msg: "a question about the code", Ts: ts},
			Event{Event: "done", Group: "group01", Ts: ts + 1, Text: "**Answer** with `code`, a list:\n" +
				"- one\n- two\n\n```go\nfunc main() { println(\"x\") }\n```\n"},
		)
	}
	nm, _ := m.Update(historyMsg{group: "group01", events: evs})
	m = nm.(Model)
	return m
}

// One full frame with the tree open — what every spinner tick repaints.
func BenchmarkViewFullFrame(b *testing.B) {
	m := benchModel(b, 29, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// The same frame with the chat log EMPTY, to separate the cost of the
// transcript (markdown) from the cost of the chrome (tree + bars).
func BenchmarkViewEmptyLog(b *testing.B) {
	m := benchModel(b, 29, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// A single group, populated log — isolates per-group tree cost by comparison
// with BenchmarkViewFullFrame.
func BenchmarkViewOneGroup(b *testing.B) {
	m := benchModel(b, 1, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// buildLogContent is the markdown path refreshLog falls into whenever the
// viewport cache is bypassed — which is the whole time a live overlay is up,
// i.e. the whole time the focused group is streaming.
func BenchmarkBuildLogContent(b *testing.B) {
	m := benchModel(b, 29, 40)
	cols := m.logContentCols()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.buildLogContent(cols, false)
	}
}

// The tree alone, at fleet size.
func BenchmarkRenderTree(b *testing.B) {
	m := benchModel(b, 29, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.renderTree(m.height - 3)
	}
}

// The same frame under a theme: themeFrame's post-pass (ground re-assertion
// and padding on every line) is on top of everything above.
func BenchmarkViewFullFrameThemed(b *testing.B) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	b.Cleanup(func() { lipgloss.SetColorProfile(prev); resetTheme() })
	p, err := loadTheme("", "apollo")
	if err != nil {
		b.Fatal(err)
	}
	applyTheme(p)
	m := benchModel(b, 29, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

func BenchmarkThemeFrameOnly(b *testing.B) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	b.Cleanup(func() { lipgloss.SetColorProfile(prev); resetTheme() })
	p, _ := loadTheme("", "apollo")
	applyTheme(p)
	m := benchModel(b, 29, 40)
	frame := m.view()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = themeFrame(frame, m.width)
	}
}

// Streaming: what one contentFlush costs while the focused group has a live
// overlay (the vpCache is bypassed, buildLogContent runs every flush).
func BenchmarkRefreshLogStreaming(b *testing.B) {
	m := benchModel(b, 29, 40)
	m.streamBuf[m.curKey()] = "partial answer being streamed right now, a few words"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.refreshLog()
	}
}
