package main

// codex.go — the openai provider's turn path: OpenAI's Codex CLI
// (`codex exec --json`, pinned in Dockerfile.rootfs) instead of claude.
//
// Codex talks to the HOST proxy, never to OpenAI: every model call goes to
// KOTO_OPENAI_BASE_URL (the in-guest proxy bridge, /v1) as a custom
// Responses-API provider whose API key is the placeholder in
// KOTO_OPENAI_API_KEY. The host proxy (daemon/proxy_openai.go) drops that
// placeholder and injects the real ChatGPT/API credential, which therefore
// never exists inside the VM. Codex's own login (auth.json) is never used.
//
// All provider settings ride on `-c` overrides, which beat any
// $CODEX_HOME/config.toml: that file lives in the worker-writable workspace,
// so a turn must not depend on (or be redirectable by) what is in it.
//
// The JSONL stream (codex-rs/exec/src/exec_events.rs at rust-v0.152.1) maps
// onto the same frames the claude path emits: agent_message -> text,
// reasoning -> think block, command_execution / file_change / mcp_tool_call /
// web_search -> tool + tool output, turn.failed -> [[err]]. Token usage is
// not taken from here: the host proxy reads it off the Responses stream it
// relays (proxy_openai.go openaiScanUsage), the same place it reads claude's.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"koto-protocol/pb"
)

// codexProviderID names the custom model provider the -c overrides define.
const codexProviderID = "koto"

// codexHome is where codex keeps its thread history; HOME is /workspace for
// the worker, so this is also codex's default and survives restarts.
var codexHome = filepath.Join(wsDir, ".codex")

// codexIDFile is the per-session thread pointer, beside claude's
// sessions/<name>.id but distinct from it: a group that switches provider
// must not hand a claude session id to `codex exec resume` (or vice versa).
// The daemon's session clear removes both.
func codexIDFile(sess string) string {
	return filepath.Join(csDir, "sessions", sess+".codex.id")
}

// tomlString renders s as a TOML basic string, so a -c value is parsed as
// exactly s (codex parses the value part of key=value as TOML).
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexArgs builds the argv for one turn. resumeID != "" continues that
// thread. The prompt always comes on stdin ("-").
func codexArgs(baseURL, model, effort, systemPrompt, resumeID string) []string {
	args := []string{"exec"}
	if resumeID != "" {
		args = append(args, "resume")
	}
	args = append(args,
		"--json",
		"--skip-git-repo-check",
		// The VM is the sandbox (as with claude's --dangerously-skip-permissions):
		// no interactive approver exists, and bwrap is not in the rootfs.
		"--dangerously-bypass-approvals-and-sandbox",
		"-c", "model_provider="+tomlString(codexProviderID),
		"-c", "model_providers."+codexProviderID+".name="+tomlString("koto host proxy"),
		"-c", "model_providers."+codexProviderID+".base_url="+tomlString(baseURL),
		"-c", "model_providers."+codexProviderID+".env_key="+tomlString("KOTO_OPENAI_API_KEY"),
		"-c", "model_providers."+codexProviderID+".wire_api="+tomlString("responses"),
		// The proxy relays HTTP request/response only (no websocket upgrade).
		"-c", "model_providers."+codexProviderID+".supports_websockets=false",
		"-c", "model_providers."+codexProviderID+".requires_openai_auth=false",
		// No update check (the guest has no egress, and the binary is pinned).
		"-c", "check_for_update_on_startup=false",
		"-c", "analytics.enabled=false",
	)
	if model != "" {
		args = append(args, "-c", "model="+tomlString(model))
	}
	if effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(effort))
	}
	if systemPrompt != "" {
		args = append(args, "-c", "developer_instructions="+tomlString(systemPrompt))
	}
	if resumeID != "" {
		args = append(args, resumeID)
	}
	return append(args, "-")
}

func runCodex(tw *turnConn, req *pb.MsgReq, cfg groupConfig, sess string, env []string) {
	baseURL := envValue(env, "KOTO_OPENAI_BASE_URL")
	if baseURL == "" || envValue(env, "KOTO_OPENAI_API_KEY") == "" {
		tw.errf("codex: KOTO_OPENAI_BASE_URL / KOTO_OPENAI_API_KEY not set — host too old for provider openai?")
		return
	}
	model := cfg.Model
	if model == "" {
		model = envValue(env, "KOTO_DEFAULT_OPENAI_MODEL")
	}
	idf := codexIDFile(sess)
	resume := ""
	if id, err := os.ReadFile(idf); err == nil {
		if s := string(bytes.TrimSpace(id)); validSessionID(s) {
			resume = s
			tw.tr.workerAttr("session.resumed", "true")
		}
	}
	env = append(env, "CODEX_HOME="+codexHome)
	w, err := workerCmd("codex", codexArgs(baseURL, model, cfg.Effort, string(req.SystemPrompt), resume), env, req.Msg)
	if err != nil {
		tw.errf("codex: start: %v", err)
		return
	}
	runWorker(tw, w, newCodexParser(tw, idf).line)
}

// codexToolOutMax bounds one tool output frame; the daemon cuts markers to
// 64 KiB for display anyway (fcMarkerMaxBody), so this only keeps a huge
// aggregated_output from being queued whole.
const codexToolOutMax = 256 << 10

type codexParser struct {
	tw      *turnConn
	idf     string
	wroteID bool
	// started remembers items whose tool frame went out on item.started, so
	// item.completed only adds the output. Bounded like claude's open blocks.
	started map[string]bool
}

func newCodexParser(tw *turnConn, idf string) *codexParser {
	return &codexParser{tw: tw, idf: idf, started: map[string]bool{}}
}

type codexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Message          string          `json:"message"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Changes          json.RawMessage `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query string `json:"query"`
	Items []struct {
		Text      string `json:"text"`
		Completed bool   `json:"completed"`
	} `json:"items"`
}

func (p *codexParser) line(b []byte) {
	var ev struct {
		Type     string     `json:"type"`
		ThreadID string     `json:"thread_id"`
		Message  string     `json:"message"`
		Item     *codexItem `json:"item"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	switch ev.Type {
	case "thread.started":
		// Same trust rule as claude's session_id (audit M33): the value comes
		// off the worker's stdout and is later used by the daemon's clear.
		if p.idf != "" && !p.wroteID && validSessionID(ev.ThreadID) {
			writeWorkerFile(p.idf, []byte(ev.ThreadID))
			p.wroteID = true
		}
	case "turn.failed":
		msg := "turn failed"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		p.tw.errf("codex: %s", msg)
	case "error":
		// Stream-level notices ("Reconnecting...") are transient retries;
		// a fatal one is followed by turn.failed, which is what surfaces.
		logf("codex: %s", ev.Message)
	case "item.started":
		if ev.Item != nil {
			p.itemStarted(ev.Item)
		}
	case "item.completed":
		if ev.Item != nil {
			p.itemCompleted(ev.Item)
		}
	}
}

func (p *codexParser) tool(name, input string) {
	if input == "" {
		input = "{}"
	}
	p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Tool{Tool: &pb.ToolUse{Name: name, Input: input}}})
}

func (p *codexParser) toolOut(s string) {
	if len(s) > codexToolOutMax {
		s = s[:codexToolOutMax] + "…[truncated]"
	}
	p.tw.toolOut(s)
}

func jsonInput(kv map[string]any) string {
	b, err := json.Marshal(kv)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (it *codexItem) toolCall() (name, input string, ok bool) {
	switch it.Type {
	case "command_execution":
		return "Bash", jsonInput(map[string]any{"command": it.Command}), true
	case "file_change":
		var ch any = []any{}
		if len(it.Changes) > 0 {
			ch = it.Changes
		}
		return "apply_patch", jsonInput(map[string]any{"changes": ch}), true
	case "mcp_tool_call":
		var args any = map[string]any{}
		if len(it.Arguments) > 0 && string(it.Arguments) != "null" {
			args = it.Arguments
		}
		return "mcp__" + it.Server + "__" + it.Tool, jsonInput(map[string]any{"arguments": args}), true
	case "web_search":
		return "web_search", jsonInput(map[string]any{"query": it.Query}), true
	}
	return "", "", false
}

func (p *codexParser) itemStarted(it *codexItem) {
	name, input, ok := it.toolCall()
	if !ok || it.ID == "" || p.started[it.ID] {
		return
	}
	if len(p.started) >= claudeMaxOpenBlocks {
		return // the completed event still emits the tool frame
	}
	p.started[it.ID] = true
	p.tool(name, input)
}

func (p *codexParser) itemCompleted(it *codexItem) {
	switch it.Type {
	case "agent_message":
		if it.Text != "" {
			p.tw.text([]byte(it.Text))
			p.tw.text([]byte("\n"))
		}
		return
	case "reasoning":
		if it.Text == "" {
			return
		}
		var t thinkState
		t.count(it.Text)
		p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkBegin{ThinkBegin: true}})
		p.tw.text([]byte(it.Text))
		p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkEnd{ThinkEnd: int32(t.words)}})
		return
	case "error":
		// Non-fatal codex notices (e.g. unknown model metadata): visible,
		// but not an [[err]] — the turn carries on.
		if it.Message != "" {
			p.tw.text([]byte("[codex] " + it.Message + "\n"))
		}
		return
	case "todo_list":
		var b strings.Builder
		for _, t := range it.Items {
			mark := "[ ]"
			if t.Completed {
				mark = "[x]"
			}
			b.WriteString(mark + " " + t.Text + "\n")
		}
		p.tool("todo_list", "{}")
		p.toolOut(b.String())
		return
	}
	name, input, ok := it.toolCall()
	if !ok {
		return
	}
	if it.ID == "" || !p.started[it.ID] {
		p.tool(name, input)
	}
	delete(p.started, it.ID)
	switch it.Type {
	case "command_execution":
		out := it.AggregatedOutput
		if it.ExitCode != nil && *it.ExitCode != 0 {
			out += fmt.Sprintf("\n[exit %d]", *it.ExitCode)
		}
		p.toolOut(out)
	case "file_change":
		p.toolOut(it.Status)
	case "mcp_tool_call":
		var b strings.Builder
		if it.Result != nil {
			for _, c := range it.Result.Content {
				if c.Type == "text" {
					b.WriteString(c.Text)
				}
			}
		}
		if it.Error != nil && it.Error.Message != "" {
			b.WriteString("error: " + it.Error.Message)
		}
		p.toolOut(b.String())
	case "web_search":
		p.toolOut("")
	}
}
