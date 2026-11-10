package main

// compact.go — the guest half of koto's `compact` verb (daemon/compact.go).
//
// The daemon decides the mechanism per provider and sends one of two things as
// the turn's message:
//
//   - claude: the literal "/compact [focus]". Claude Code runs it as a built-in
//     on the ordinary -p path and answers with a system/compact_boundary record
//     (rendered by claudeParser.line); nothing to intercept here.
//   - codex / pi: compactPrefix + {"mode","instructions"}. Neither harness's
//     print path knows /compact — `codex exec` has no slash commands and pi -p
//     only dispatches extension commands, which koto disables — so fc-agent
//     intercepts the message BEFORE the provider switch and drives the
//     harness's own JSON-RPC surface instead:
//     codex: `codex app-server` → initialize → thread/resume → thread/compact/start
//     (codex-rs/app-server-protocol/src/protocol/common.rs:500,558,734 at
//     rust-v0.163.0), done on the contextCompaction item or turn/completed.
//     pi: `pi --mode rpc` → {"type":"compact","customInstructions"} → the
//     "response" for command "compact" (pi 1.5.0 docs/rpc-commands.md:365-404).
//
// Either way the outcome also lands in sessions/<name>.compact ("ok …" or
// "err …"), which the daemon reads after the turn: an [[err]] frame does not
// fail a turn host-side, and the transcript note must not say "compacted"
// when nothing was.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"koto-protocol/pb"
)

// compactPrefix — keep in sync with daemon/compact.go. NUL cannot be typed,
// and the daemon refuses user messages that start with it.
const compactPrefix = "\x00koto-compact\x00"

const (
	compactModeCodex = "codex-app-server"
	compactModePi    = "pi-rpc"
)

// compactStatusExt names sessions/<name>.compact (next to the id files, in the
// dir prepareSessDir pins as a real directory).
const compactStatusExt = ".compact"

// compactStatusDir holds the status files (var for tests).
var compactStatusDir = filepath.Join(csDir, "sessions")

func compactStatusFile(sess string) string {
	return filepath.Join(compactStatusDir, sess+compactStatusExt)
}

type compactWire struct {
	Mode         string `json:"mode"`
	Instructions string `json:"instructions,omitempty"`
}

// parseCompactMsg recognises a daemon-authored compaction request.
func parseCompactMsg(msg []byte) (compactWire, bool) {
	var cw compactWire
	if !bytes.HasPrefix(msg, []byte(compactPrefix)) {
		return cw, false
	}
	if json.Unmarshal(msg[len(compactPrefix):], &cw) != nil {
		// Still ours (nobody else can author the prefix): never let it fall
		// through to the model as a prompt.
		cw.Mode = ""
	}
	return cw, true
}

// writeCompactStatus records the outcome for the daemon. Worker-readable, and
// written with writeWorkerFile's temp+rename, so a planted symlink at the
// path is replaced, never followed.
func writeCompactStatus(path string, ok bool, detail string) {
	word := "err"
	if ok {
		word = "ok"
	}
	writeWorkerFile(path, []byte(word+" "+compactSafe(detail)+"\n"))
}

// compactSafe flattens harness-authored text to one bounded line.
func compactSafe(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 512 {
		s = s[:512]
		for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 {
			s = s[:len(s)-1]
		}
		if len(s) > 0 && s[len(s)-1] >= 0xC0 {
			s = s[:len(s)-1]
		}
	}
	return s
}

func compactTokens(n int64) string {
	switch {
	case n <= 0:
		return "?"
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprint(n)
	}
}

// runCompact dispatches a compaction request on the guest.
func runCompact(tw *turnConn, req *pb.MsgReq, cfg groupConfig, sess string, cw compactWire, env []string) {
	status := compactStatusFile(sess)
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		writeCompactStatus(status, false, msg)
		tw.errf("compact: %s", msg)
	}
	tw.tr.workerAttr("compact.mode", cw.Mode)
	switch cw.Mode {
	case compactModeCodex:
		if cfg.Provider != "openai" {
			fail("codex compaction requested but the group's provider is %q now", cfg.Provider)
			return
		}
		runCodexCompact(tw, req, cfg, sess, cw, env, status, fail)
	case compactModePi:
		if _, ok := piProviders[cfg.Provider]; !ok {
			fail("pi compaction requested but the group's provider is %q now", cfg.Provider)
			return
		}
		runPiCompact(tw, req, cfg, sess, cw, env, status, fail)
	default:
		fail("unknown compaction mode %q (host/guest version skew?)", cw.Mode)
	}
}

// ---- codex app-server -----------------------------------------------------------

func codexAppServerArgs(baseURL, model, effort, systemPrompt string) []string {
	// -c is a root flag of the codex CLI; placing it before the subcommand
	// works whether or not app-server re-declares it.
	args := codexConfigArgs(baseURL, model, effort, systemPrompt)
	return append(args, "app-server")
}

func runCodexCompact(tw *turnConn, req *pb.MsgReq, cfg groupConfig, sess string, cw compactWire, env []string, status string, fail func(string, ...any)) {
	baseURL := envValue(env, "KOTO_OPENAI_BASE_URL")
	if baseURL == "" || envValue(env, "KOTO_OPENAI_API_KEY") == "" {
		fail("codex: KOTO_OPENAI_BASE_URL / KOTO_OPENAI_API_KEY not set")
		return
	}
	thread := ""
	if id, err := os.ReadFile(codexIDFile(sess)); err == nil {
		if s := string(bytes.TrimSpace(id)); validSessionID(s) {
			thread = s
		}
	}
	if thread == "" {
		fail("codex: this session has no thread yet — nothing to compact")
		return
	}
	if err := ensureCodexHome(); err != nil {
		fail("codex: CODEX_HOME: %v", err)
		return
	}
	env = append(env, "CODEX_HOME="+codexHome)
	w, in, err := workerCmdPipe("codex", codexAppServerArgs(baseURL, codexModel(cfg, env), cfg.Effort, string(req.SystemPrompt)), env)
	if err != nil {
		fail("codex: start app-server: %v", err)
		return
	}
	c := newCodexCompactor(tw, in, thread, status)
	c.instr = cw.Instructions
	c.start()
	runWorker(tw, w, c.line)
	c.end()
}

// codexCompactor drives one app-server conversation. All writes happen on the
// stdout pump goroutine (line) or before it starts (start), never both at once.
type codexCompactor struct {
	tw     *turnConn
	in     io.WriteCloser
	thread string
	status string
	instr  string
	done   bool
}

func newCodexCompactor(tw *turnConn, in io.WriteCloser, thread, status string) *codexCompactor {
	return &codexCompactor{tw: tw, in: in, thread: thread, status: status}
}

// JSON-RPC ids of our three requests.
const (
	codexRPCInit    = 1
	codexRPCResume  = 2
	codexRPCCompact = 3
)

func (c *codexCompactor) send(v any) {
	b, _ := json.Marshal(v)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.finish(false, fmt.Sprintf("codex: write to app-server: %v", err))
	}
}

func (c *codexCompactor) start() {
	c.send(map[string]any{"id": codexRPCInit, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]any{"name": "koto", "title": nil, "version": "1"},
	}})
}

type codexRPCMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Params json.RawMessage `json:"params"`
}

func (c *codexCompactor) line(b []byte) {
	if c.done {
		return
	}
	var m codexRPCMsg
	if json.Unmarshal(b, &m) != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case hasID && m.Method != "":
		// A server→client request (an approval, an elicitation). Nothing in a
		// compaction should ask; refuse rather than leave it hanging until
		// the turn budget kills the worker.
		c.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "koto compaction client: not supported"}})
	case hasID:
		c.response(string(m.ID), m)
	default:
		c.notification(m.Method, m.Params)
	}
}

func (c *codexCompactor) response(id string, m codexRPCMsg) {
	step := map[string]string{"1": "initialize", "2": "thread/resume", "3": "thread/compact/start"}[id]
	if step == "" {
		return
	}
	if m.Error != nil {
		c.finish(false, fmt.Sprintf("codex: %s: %s", step, m.Error.Message))
		return
	}
	switch id {
	case "1":
		c.send(map[string]any{"method": "initialized"})
		c.send(map[string]any{"id": codexRPCResume, "method": "thread/resume", "params": map[string]any{"threadId": c.thread}})
	case "2":
		c.send(map[string]any{"id": codexRPCCompact, "method": "thread/compact/start", "params": map[string]any{"threadId": c.thread}})
	}
	// "3": accepted; the work is reported by notifications.
}

func (c *codexCompactor) notification(method string, params json.RawMessage) {
	switch method {
	case "item/completed":
		var p struct {
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal(params, &p) == nil && p.Item.Type == "contextCompaction" {
			c.finish(true, "codex compacted thread "+c.thread)
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		if p.Turn.Status == "completed" {
			c.finish(true, "codex compacted thread "+c.thread)
			return
		}
		msg := "compaction turn " + p.Turn.Status
		if p.Turn.Error != nil && p.Turn.Error.Message != "" {
			msg += ": " + p.Turn.Error.Message
		}
		c.finish(false, "codex: "+msg)
	case "error":
		var p struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(params, &p) == nil && !p.WillRetry {
			c.finish(false, "codex: "+p.Error.Message)
		}
	}
}

func (c *codexCompactor) finish(ok bool, detail string) {
	if c.done {
		return
	}
	c.done = true
	writeCompactStatus(c.status, ok, detail)
	if ok {
		c.tw.text([]byte("[koto] " + compactSafe(detail) + "\n"))
	} else {
		c.tw.errf("compact: %s", compactSafe(detail))
	}
	// EOF on stdin is app-server's shutdown signal.
	c.in.Close()
}

// end runs after the worker exited: an app-server that died mid-way must not
// leave the outcome unreported.
func (c *codexCompactor) end() {
	c.finish(false, "codex: app-server exited before the compaction finished")
}

// ---- pi --mode rpc ---------------------------------------------------------------

// piRPCArgs is piArgs for RPC mode: same provider, session, offline and
// no-extensions flags, minus print mode.
func piRPCArgs(piName, model, effort, sessionDir, sessionID, promptFile string) []string {
	src := piArgs(piName, model, effort, sessionDir, sessionID, promptFile)
	out := make([]string, 0, len(src))
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == "-p":
			continue
		case src[i] == "--mode" && i+1 < len(src):
			out = append(out, "--mode", "rpc")
			i++
		default:
			out = append(out, src[i])
		}
	}
	return out
}

func runPiCompact(tw *turnConn, req *pb.MsgReq, cfg groupConfig, sess string, cw compactWire, env []string, status string, fail func(string, ...any)) {
	pp := piProviders[cfg.Provider]
	model := cfg.Model
	if model == "" {
		model = envValue(env, pp.defaultModelEnv)
	}
	id := piSessionID(sess)
	if id == "" {
		fail("pi: this session has no pi session yet — nothing to compact")
		return
	}
	sessDir := filepath.Join(piHome, "sessions")
	for _, d := range []string{piHome, sessDir} {
		if err := ensureWorkerDir(d); err != nil {
			fail("pi: %v", err)
			return
		}
	}
	agentDir, promptFile, err := preparePiAgentDir(cfg.Provider, req.SystemPrompt)
	if err != nil {
		fail("pi: %v", err)
		return
	}
	defer os.RemoveAll(agentDir)
	w, in, err := workerCmdPipe("pi", piRPCArgs(pp.piName, model, cfg.Effort, sessDir, id, promptFile), piEnv(env, agentDir))
	if err != nil {
		fail("pi: start rpc: %v", err)
		return
	}
	c := newPiCompactor(tw, in, status)
	c.start(cw.Instructions)
	runWorker(tw, w, c.line)
	c.end()
}

type piCompactor struct {
	tw     *turnConn
	in     io.WriteCloser
	status string
	done   bool
}

func newPiCompactor(tw *turnConn, in io.WriteCloser, status string) *piCompactor {
	return &piCompactor{tw: tw, in: in, status: status}
}

const piCompactReqID = "koto-compact"

func (c *piCompactor) start(instr string) {
	cmd := map[string]any{"id": piCompactReqID, "type": "compact"}
	if instr != "" {
		cmd["customInstructions"] = instr
	}
	b, _ := json.Marshal(cmd)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.finish(false, fmt.Sprintf("pi: write to rpc: %v", err))
	}
}

func (c *piCompactor) line(b []byte) {
	if c.done {
		return
	}
	var r struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Data    *struct {
			TokensBefore         int64 `json:"tokensBefore"`
			EstimatedTokensAfter int64 `json:"estimatedTokensAfter"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &r) != nil || r.Type != "response" {
		return
	}
	// A parse error is answered with command "parse"; anything else is not
	// the reply to our one request.
	if r.Command != "compact" && r.Command != "parse" {
		return
	}
	if !r.Success {
		msg := r.Error
		if msg == "" {
			msg = "compaction failed"
		}
		c.finish(false, "pi: "+msg)
		return
	}
	detail := "pi compacted the session"
	if r.Data != nil {
		detail += fmt.Sprintf(" (%s → ~%s tokens)", compactTokens(r.Data.TokensBefore), compactTokens(r.Data.EstimatedTokensAfter))
	}
	c.finish(true, detail)
}

func (c *piCompactor) finish(ok bool, detail string) {
	if c.done {
		return
	}
	c.done = true
	writeCompactStatus(c.status, ok, detail)
	if ok {
		c.tw.text([]byte("[koto] " + compactSafe(detail) + "\n"))
	} else {
		c.tw.errf("compact: %s", compactSafe(detail))
	}
	c.in.Close() // pi --mode rpc shuts down on stdin EOF
}

func (c *piCompactor) end() {
	c.finish(false, "pi: rpc exited before the compaction finished")
}
