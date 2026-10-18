package main

// pi.go — the minimax provider's turn path: the pinned pi coding agent
// (earendil-works/pi, Dockerfile.rootfs) in `--mode json` instead of claude.
//
// Before this file existed, provider "minimax" fell through runTurn's switch
// into runClaude: the claude CLI then called ANTHROPIC_BASE_URL + /v1/messages,
// which the daemon's minimax allowlist refuses (only /anthropic/v1/* is
// relayed), so every minimax turn failed (notes: mm-e2e trace).
//
// pi talks to the HOST proxy only. Its provider settings come from
// $PI_CODING_AGENT_DIR/models.json, which this file writes from the read-only
// template the rootfs ships (/usr/local/share/koto/pi/models.<provider>.json):
// baseUrl = the in-guest proxy + /anthropic, apiKey = the placeholder
// "proxied". The proxy drops that key and injects creds/minimax.key, so the
// real key never enters the VM.
//
// The agent dir is a fresh, private temp dir per turn, created by fc-agent
// (root) and only then handed to the worker: pi needs it writable (auth.json,
// models-store.json, their lock dirs), and a dir in the worker-writable
// workspace would let a turn plant models.json for the NEXT turn (an apiKey
// starting with "!" is executed by pi). Conversations persist separately,
// under /workspace/.pi/sessions, one pi session id per koto session
// (sessions/<name>.pi.id, taken from pi's first "session" event), passed with
// --session-id on the turns after.
//
// Event mapping (pi 0.84.4 json mode): message_update text_delta -> text,
// thinking_start/delta/end -> think block, tool_execution_start -> tool,
// tool_execution_end -> tool output. pi exits 0 when the provider fails; the
// failure is only in the assistant message's stopReason "error" +
// errorMessage, so finish() turns the LAST attempt's error into an [[err]]
// (auto_retry_start clears an earlier attempt's).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"koto-protocol/pb"
)

// piTemplateDir holds the rootfs's provider templates (var for tests).
var piTemplateDir = "/usr/local/share/koto/pi"

// piHome is the worker-owned pi state root on the persistent workspace.
var piHome = filepath.Join(wsDir, ".pi")

// piProviders maps a koto provider to its pi provider name and the init-env
// key holding its default model. Only providers listed here run on pi.
var piProviders = map[string]struct{ piName, defaultModelEnv string }{
	"minimax": {"minimax", "KOTO_DEFAULT_MINIMAX_MODEL"},
}

// piThinking: koto effort values pi accepts as --thinking levels.
var piThinking = map[string]bool{"off": true, "minimal": true, "low": true,
	"medium": true, "high": true, "xhigh": true, "max": true}

// piIDDir holds sessions/<name>.pi.id (var for tests).
var piIDDir = filepath.Join(csDir, "sessions")

func piIDFile(sess string) string {
	return filepath.Join(piIDDir, sess+".pi.id")
}

// piSessionID reads the session's pinned pi id ("" on its first turn: pi
// then starts a new session, and the parser stores the id pi announces in
// its first "session" event). Minting the id ourselves and passing it on the
// first turn would make pi print a "creating a new session" warning on
// stderr, which runWorker forwards into the reply.
func piSessionID(sess string) string {
	if b, err := os.ReadFile(piIDFile(sess)); err == nil {
		if s := string(bytes.TrimSpace(b)); validSessionID(s) {
			return s
		}
	}
	return ""
}

// ensureWorkerDir creates dir (0700, worker-owned) if missing; an existing
// directory is kept, and a symlink or file there is refused, never followed.
func ensureWorkerDir(dir string) error {
	err := os.Mkdir(dir, 0o700)
	if err == nil {
		return chownWorker(dir)
	}
	if !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	return nil
}

// chownWorker hands a path fc-agent just created to the worker. Off root
// (unit tests) there is nobody else to hand it to.
func chownWorker(p string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Lchown(p, workerUID, workerGID)
}

// preparePiAgentDir builds this turn's PI_CODING_AGENT_DIR: a new private
// temp dir holding models.json (the provider template, verbatim) and, when
// the group has one, the system prompt as a file (passed by path, so pi
// never has to guess whether the text is a path). Everything is written
// before the dir is handed to the worker.
func preparePiAgentDir(provider string, systemPrompt []byte) (dir, promptFile string, err error) {
	tmpl, err := os.ReadFile(filepath.Join(piTemplateDir, "models."+provider+".json"))
	if err != nil {
		return "", "", fmt.Errorf("pi template: %w", err)
	}
	if !json.Valid(tmpl) {
		return "", "", fmt.Errorf("pi template models.%s.json is not valid JSON", provider)
	}
	dir, err = os.MkdirTemp("", "koto-pi-")
	if err != nil {
		return "", "", err
	}
	fail := func(e error) (string, string, error) {
		os.RemoveAll(dir)
		return "", "", e
	}
	files := []string{filepath.Join(dir, "models.json")}
	if err := os.WriteFile(files[0], tmpl, 0o600); err != nil {
		return fail(err)
	}
	if len(systemPrompt) > 0 {
		promptFile = filepath.Join(dir, "koto-system-prompt.md")
		if err := os.WriteFile(promptFile, systemPrompt, 0o600); err != nil {
			return fail(err)
		}
		files = append(files, promptFile)
	}
	for _, p := range append(files, dir) {
		if err := chownWorker(p); err != nil {
			return fail(err)
		}
	}
	return dir, promptFile, nil
}

// piArgs builds the argv for one turn; the prompt goes on stdin (so a
// leading "-" or "@file" in it is just text).
func piArgs(piName, model, effort, sessionDir, sessionID, promptFile string) []string {
	args := []string{"--mode", "json", "-p",
		"--provider", piName,
		"--session-dir", sessionDir,
		"--offline",
		"--no-extensions", "--no-skills", "--no-prompt-templates",
		"--no-themes", "--no-approve"}
	if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if piThinking[effort] {
		args = append(args, "--thinking", effort)
	}
	if promptFile != "" {
		args = append(args, "--append-system-prompt", promptFile)
	}
	return args
}

// piEnv returns env with PI_CODING_AGENT_DIR set to agentDir (any inherited
// value is dropped).
func piEnv(env []string, agentDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PI_CODING_AGENT_DIR=") {
			out = append(out, kv)
		}
	}
	return append(out, "PI_CODING_AGENT_DIR="+agentDir)
}

func runPi(tw *turnConn, req *pb.MsgReq, cfg groupConfig, sess string, env []string) {
	pp, ok := piProviders[cfg.Provider]
	if !ok {
		tw.errf("pi: provider %q has no pi mapping", cfg.Provider)
		return
	}
	model := cfg.Model
	if model == "" {
		model = envValue(env, pp.defaultModelEnv)
	}
	sessDir := filepath.Join(piHome, "sessions")
	for _, d := range []string{piHome, sessDir} {
		if err := ensureWorkerDir(d); err != nil {
			tw.errf("pi: %v", err)
			return
		}
	}
	id := piSessionID(sess)
	if id != "" {
		tw.tr.workerAttr("session.resumed", "true")
	}
	agentDir, promptFile, err := preparePiAgentDir(cfg.Provider, req.SystemPrompt)
	if err != nil {
		tw.errf("pi: %v", err)
		return
	}
	defer os.RemoveAll(agentDir)
	w, err := workerCmd("pi", piArgs(pp.piName, model, cfg.Effort, sessDir, id, promptFile),
		piEnv(env, agentDir), req.Msg)
	if err != nil {
		tw.errf("pi: start: %v", err)
		return
	}
	p := newPiParser(tw, piIDFile(sess))
	p.wroteID = id != ""
	runWorker(tw, w, p.line)
	p.finish()
}

// piToolOutMax bounds one tool output frame (the daemon cuts markers to
// 64 KiB for display anyway).
const piToolOutMax = 256 << 10

type piParser struct {
	tw       *turnConn
	idf      string
	wroteID  bool
	think    *thinkState
	lastErr  string
	sawReply bool
}

func newPiParser(tw *turnConn, idf string) *piParser { return &piParser{tw: tw, idf: idf} }

type piEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	AME  *struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	Message *struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
	ToolName string          `json:"toolName"`
	Args     json.RawMessage `json:"args"`
	Result   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"result"`
}

func (p *piParser) line(b []byte) {
	var ev piEvent
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	switch ev.Type {
	case "session":
		// The id becomes a path component on the clear path: validated.
		if !p.wroteID && validSessionID(ev.ID) {
			writeWorkerFile(p.idf, []byte(ev.ID))
			p.wroteID = true
		}
	case "message_update":
		if ev.AME == nil {
			return
		}
		switch ev.AME.Type {
		case "text_delta":
			p.tw.text([]byte(ev.AME.Delta))
		case "text_end":
			p.tw.text([]byte("\n"))
		case "thinking_start":
			p.think = &thinkState{}
			p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkBegin{ThinkBegin: true}})
		case "thinking_delta":
			if p.think != nil {
				p.think.count(ev.AME.Delta)
			}
			p.tw.text([]byte(ev.AME.Delta))
		case "thinking_end":
			if p.think != nil {
				p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkEnd{ThinkEnd: int32(p.think.words)}})
				p.think = nil
			}
		}
	case "message_end":
		if ev.Message == nil || ev.Message.Role != "assistant" {
			return
		}
		if ev.Message.StopReason == "error" {
			p.lastErr = ev.Message.ErrorMessage
			if p.lastErr == "" {
				p.lastErr = "provider error"
			}
		} else {
			p.lastErr = ""
			p.sawReply = true
		}
	case "auto_retry_start":
		p.lastErr = ""
	case "tool_execution_start":
		in := string(ev.Args)
		if in == "" || in == "null" {
			in = "{}"
		}
		if len(in) > claudeToolInputMax {
			in = in[:claudeToolInputMax]
		}
		p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Tool{Tool: &pb.ToolUse{Name: ev.ToolName, Input: in}}})
	case "tool_execution_end":
		out := ""
		if ev.Result != nil {
			out = toolResultText(ev.Result.Content)
		}
		if len(out) > piToolOutMax {
			out = out[:piToolOutMax]
		}
		p.tw.toolOut(out)
	}
}

// finish reports a turn whose final model call failed: pi's exit code is 0
// either way.
func (p *piParser) finish() {
	if p.think != nil {
		p.tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_ThinkEnd{ThinkEnd: int32(p.think.words)}})
		p.think = nil
	}
	if p.lastErr != "" {
		p.tw.errf("pi: %s", p.lastErr)
	}
}
