package main

// claude_compact.go — claude's /compact, driven so the compaction PERSISTS.
//
// Root cause (/workspace/goal/claude-compact/rootcause.md): Claude Code 2.1.280
// answers `claude -p --resume <id> "/compact"` with system/compact_boundary on
// stdout as soon as the summary call returns, but writes the compact pair
// (boundary + summary) to <id>.jsonl asynchronously (insertMessageChain →
// trackWrite → enqueueWrite, drained on a timer). A one-shot -p run starts
// shutting down right after `result`, and sealAppendsForShutdown() turns every
// later appendEntry/enqueueWrite into a silent return — so whether the
// compaction survived was a race against process exit, and koto, trusting the
// stdout boundary, reported success either way. Lost, the next --resume loads
// the full uncompacted history.
//
// So for a resumed /compact koto feeds the prompt over --input-format
// stream-json and keeps stdin OPEN after `result`, until a new
// compact_boundary record is in the transcript koto resumes next (or
// claudeCompactPersistTimeout passes); only then does EOF let claude exit. The
// status the daemon reads says "ok … persisted" only when that check passed.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"koto-protocol/pb"
)

// claudeProjectsDir is where Claude Code keeps <cwd-slug>/<id>.jsonl
// (HOME=/workspace for the worker). A var for tests.
var claudeProjectsDir = filepath.Join(wsDir, ".claude", "projects")

// claudeCompactPersistTimeout bounds the wait, after claude's `result`, for
// the compact pair to reach the transcript. The write is queued already; it
// lands in milliseconds when the process is allowed to live.
var claudeCompactPersistTimeout = 60 * time.Second

var claudeCompactPoll = 200 * time.Millisecond

// isClaudeCompactPrompt reports whether msg is Claude Code's /compact
// built-in ("/compact" or "/compact <focus>"), the only message the daemon's
// claude-slash mechanism sends.
func isClaudeCompactPrompt(msg []byte) bool {
	s := strings.TrimSpace(string(msg))
	return s == "/compact" || strings.HasPrefix(s, "/compact ")
}

// compactBoundaryRecord is the transcript shape of a real boundary: a top-level
// system record (message text that merely mentions one is JSON-escaped).
var compactBoundaryRecord = []byte(`"subtype":"compact_boundary"`)

// claudeTranscriptBoundaries counts compact_boundary records in the
// transcript of session id. fc-agent is root and the file is worker-owned, so
// it is opened O_NOFOLLOW and only ever COUNTED — a planted link yields an
// error, never another file's bytes. -1 when there is no such transcript.
func claudeTranscriptBoundaries(id string) (int, error) {
	if !validSessionID(id) {
		return -1, fmt.Errorf("invalid session id %q", id)
	}
	ms, _ := filepath.Glob(filepath.Join(claudeProjectsDir, "*", id+".jsonl"))
	if len(ms) == 0 {
		return -1, nil
	}
	n := 0
	for _, m := range ms {
		f, err := os.OpenFile(m, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return -1, err
		}
		r := bufio.NewReaderSize(f, 1<<16)
		for {
			line, err := r.ReadSlice('\n')
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) && bytes.Contains(line, compactBoundaryRecord) {
				var rec struct {
					Type    string `json:"type"`
					Subtype string `json:"subtype"`
				}
				if json.Unmarshal(line, &rec) == nil && rec.Type == "system" && rec.Subtype == "compact_boundary" {
					n++
				}
			}
			if err == bufio.ErrBufferFull {
				// a record longer than the buffer: skip the rest of it
				for err == bufio.ErrBufferFull {
					_, err = r.ReadSlice('\n')
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	return n, nil
}

// claudeCompactor holds claude's stdin open until the compaction is on disk.
type claudeCompactor struct {
	in     io.WriteCloser
	idf    string
	id     string // the session being compacted (koto's id file at start)
	before int

	mu       sync.Mutex
	boundary string // stdout compact_boundary detail, "" if none yet
	polling  bool
	closed   bool
	stop     chan struct{}
}

func newClaudeCompactor(in io.WriteCloser, idf, id string, before int) *claudeCompactor {
	return &claudeCompactor{in: in, idf: idf, id: id, before: before, stop: make(chan struct{})}
}

// send writes the one user message, as stream-json.
func (c *claudeCompactor) send(msg []byte) error {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": strings.TrimSpace(string(msg))},
	})
	_, err := c.in.Write(append(b, '\n'))
	return err
}

func (c *claudeCompactor) closeIn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.in.Close()
	}
}

// sawBoundary records the stdout boundary (the parser's hook in compact mode).
func (c *claudeCompactor) sawBoundary(detail string) {
	c.mu.Lock()
	c.boundary = detail
	c.mu.Unlock()
}

// line watches for `result`: the summary call is done, the pair is queued —
// wait for it to land, then let claude exit.
func (c *claudeCompactor) line(b []byte) {
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &ev) != nil || ev.Type != "result" {
		return
	}
	c.mu.Lock()
	start := !c.polling
	c.polling = true
	saw := c.boundary != ""
	c.mu.Unlock()
	if !start {
		return
	}
	if !saw { // answered as a prompt: nothing will land
		c.closeIn()
		return
	}
	go func() {
		defer c.closeIn()
		deadline := time.NewTimer(claudeCompactPersistTimeout)
		defer deadline.Stop()
		tick := time.NewTicker(claudeCompactPoll)
		defer tick.Stop()
		for {
			if n, _ := claudeTranscriptBoundaries(c.id); n > c.before {
				return
			}
			select {
			case <-c.stop:
				return
			case <-deadline.C:
				return
			case <-tick.C:
			}
		}
	}()
}

// finish runs once the worker has exited: the verdict is the transcript koto
// resumes NEXT, not claude's stdout.
func (c *claudeCompactor) finish(tw *turnConn) {
	close(c.stop)
	c.closeIn()
	c.mu.Lock()
	detail := c.boundary
	c.mu.Unlock()
	status := strings.TrimSuffix(c.idf, ".id") + compactStatusExt
	next := c.id
	if b, err := os.ReadFile(c.idf); err == nil && validSessionID(string(bytes.TrimSpace(b))) {
		next = string(bytes.TrimSpace(b))
	}
	if detail == "" {
		writeCompactStatus(status, false, "claude emitted no compact_boundary — /compact was likely answered as a prompt (nothing to compact yet?)")
		return
	}
	before := c.before
	if next != c.id {
		before = 0 // a fork: any boundary in the new transcript is this one
	}
	n, err := claudeTranscriptBoundaries(next)
	if err == nil && n > before {
		ok := fmt.Sprintf("%s; persisted to %s.jsonl (%d boundaries)", detail, next, n)
		tw.text([]byte("[koto] " + ok + "\n"))
		writeCompactStatus(status, true, ok)
		return
	}
	why := fmt.Sprintf("%d boundaries before, %d after", c.before, n)
	if err != nil {
		why = err.Error()
	}
	msg := fmt.Sprintf("claude reported compact_boundary but it was not persisted to the transcript koto resumes next (%s.jsonl: %s) — the session is NOT compacted", next, why)
	writeCompactStatus(status, false, msg)
	tw.errf("compact: %s", msg)
}

// runClaudeCompact drives one resumed /compact (args already carry --resume).
func runClaudeCompact(tw *turnConn, req *pb.MsgReq, args []string, env []string, idf, id string) {
	before, err := claudeTranscriptBoundaries(id)
	if err != nil {
		before = -1
		logf("compact: count boundaries in %s: %v", id, err)
	}
	if before < 0 {
		before = 0
	}
	args = append(args, "--input-format", "stream-json")
	inR, inW, err := os.Pipe()
	if err != nil {
		tw.errf("claude: stdin pipe: %v", err)
		return
	}
	w, err := startClaudeWorker("claude", args, env, inR)
	inR.Close()
	if err != nil {
		inW.Close()
		tw.errf("claude: start: %v", err)
		return
	}
	c := newClaudeCompactor(inW, idf, id, before)
	if err := c.send(req.Msg); err != nil {
		logf("compact: write /compact to claude: %v", err)
	}
	p := newClaudeParser(tw, idf)
	p.compact = c
	tw.tr.workerAttr("compact.mode", "claude-stream")
	runWorker(tw, w, func(b []byte) {
		p.line(b)
		c.line(b)
	})
	c.finish(tw)
}
