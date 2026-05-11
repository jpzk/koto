package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Plugin contract — Go port of tui/src/plugins/types.ts.
//
// A plugin is invoked from slash-command dispatch as `/<name> <args>`.
// Only one plugin runs at a time across all groups. The TUI builds a
// pluginRunCtx keyed to the active group at invocation and runs in a
// goroutine; the model holds the pluginHandle for abort + event push.

type plugin struct {
	name string
	desc string
	run  func(pctx *pluginRunCtx)
}

type pluginRunCtx struct {
	ctx    context.Context
	group  string
	args   string
	sock   string
	events chan Event
}

type pluginHandle struct {
	name   string
	cancel context.CancelFunc
	events chan Event
}

func (h *pluginHandle) abort() { h.cancel() }

// push delivers a live event to the plugin's queue, dropping on overflow
// so a runaway producer can't block the model goroutine.
func (h *pluginHandle) push(ev Event) {
	if ev.Group != "" && h != nil && ev.Historical {
		return
	}
	select {
	case h.events <- ev:
	default:
	}
}

func startPlugin(p plugin, args, group, sock string) *pluginHandle {
	ctx, cancel := context.WithCancel(context.Background())
	h := &pluginHandle{
		name:   p.name,
		cancel: cancel,
		events: make(chan Event, 64),
	}
	pctx := &pluginRunCtx{
		ctx: ctx, group: group, args: args, sock: sock, events: h.events,
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				pctx.log(fmt.Sprintf("plugin /%s crashed: %v", p.name, r), "err")
			}
			prog.Send(pluginDoneMsg{name: p.name})
		}()
		p.run(pctx)
	}()
	return h
}

// --- pluginRunCtx methods ----------------------------------------------------

func (p *pluginRunCtx) send(msg string) error {
	_, err := daemonCall(p.sock, "send", map[string]any{"group": p.group, "msg": msg})
	return err
}

func (p *pluginRunCtx) call(cmd string, extra map[string]any) (map[string]any, error) {
	return daemonCall(p.sock, cmd, extra)
}

func (p *pluginRunCtx) metrics() map[string]any {
	resp, err := daemonCall(p.sock, "metrics", map[string]any{"group": p.group})
	if err != nil {
		return nil
	}
	m, _ := resp["metric"].(map[string]any)
	return m
}

func (p *pluginRunCtx) log(text, kind string) {
	if kind == "" {
		kind = "sys"
	}
	prog.Send(pluginLogMsg{group: p.group, kind: kind, text: text})
}

// nextEvent blocks until either an event arrives for this plugin's group,
// the timeout elapses, or the context is cancelled. Returns nil on the
// latter two — used by /burn to detect idle.
func (p *pluginRunCtx) nextEvent(timeoutMs int) *Event {
	t := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return nil
		case ev := <-p.events:
			if ev.Group != p.group {
				continue // event for a different group; ignore
			}
			return &ev
		case <-t.C:
			return nil
		}
	}
}

func (p *pluginRunCtx) sleep(ms int) {
	select {
	case <-p.ctx.Done():
	case <-time.After(time.Duration(ms) * time.Millisecond):
	}
}

// --- registry ----------------------------------------------------------------

var plugins = []plugin{burnPlugin}
var pluginsByName = map[string]plugin{}

func init() {
	for _, p := range plugins {
		pluginsByName[p.name] = p
	}
}

// --- /burn -------------------------------------------------------------------

// Port of tui/src/plugins/burn.ts.
//
// Drives the active group toward <goal>, scripting a continuation prompt
// every time the agent goes idle (10s silence), until the 5h Anthropic
// rate-limit budget is exhausted (≥95% utilization, or rejected overage
// with ≥99%) — or until the user cancels via /stop-plugin or Ctrl+C.
const burnIdleTimeoutMs = 10_000

func util5h(m map[string]any) float64 {
	rl, _ := m["ratelimit"].(map[string]any)
	if rl == nil {
		return 0
	}
	v := rl["anthropic-ratelimit-unified-5h-utilization"]
	if v == nil {
		return 0
	}
	f, err := strconv.ParseFloat(fmt.Sprintf("%v", v), 64)
	if err != nil {
		return 0
	}
	return f
}

func overageStatus(m map[string]any) string {
	rl, _ := m["ratelimit"].(map[string]any)
	if rl == nil {
		return "?"
	}
	v, _ := rl["anthropic-ratelimit-unified-overage-status"].(string)
	if v == "" {
		return "?"
	}
	return v
}

var burnPlugin = plugin{
	name: "burn",
	desc: "pump the agent on a goal until 5h budget is exhausted",
	run: func(pctx *pluginRunCtx) {
		goal := strings.TrimSpace(pctx.args)
		if goal == "" {
			pctx.log("usage: /burn <goal>", "err")
			return
		}
		iter := 1
		pctx.log(fmt.Sprintf("burn: iter %d — goal=%q", iter, goal), "sys")
		if err := pctx.send(goal); err != nil {
			pctx.log(fmt.Sprintf("burn: send failed: %v", err), "err")
			return
		}
		for pctx.ctx.Err() == nil {
			// Drain events until 10s of silence. nextEvent resets the timer
			// naturally each call (it always awaits the NEXT one).
			for pctx.ctx.Err() == nil {
				if pctx.nextEvent(burnIdleTimeoutMs) == nil {
					break
				}
			}
			if pctx.ctx.Err() != nil {
				return
			}

			m := pctx.metrics()
			u := util5h(m)
			ov := overageStatus(m)
			pctx.log(fmt.Sprintf("burn: idle — 5h=%.1f%% overage=%s", u*100, ov), "sys")

			if u >= 0.95 || (ov == "rejected" && u >= 0.99) {
				pctx.log(fmt.Sprintf("burn: stop — limit reached (5h=%.1f%%)", u*100), "sys")
				return
			}
			iter++
			pctx.log(fmt.Sprintf("burn: iter %d", iter), "sys")
			cont := fmt.Sprintf("continue working on the goal. iteration %d.\n\ngoal: %s", iter, goal)
			if err := pctx.send(cont); err != nil {
				pctx.log(fmt.Sprintf("burn: send failed: %v", err), "err")
				return
			}
		}
	},
}
