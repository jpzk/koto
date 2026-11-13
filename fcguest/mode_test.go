package main

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	pb "koto-protocol/pb"
)

// yolo must stay byte-identical to the pre-modes launch.
func TestClaudeArgsYoloUnchanged(t *testing.T) {
	got, err := claudeBaseArgs(modeYolo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--bare", "--dangerously-skip-permissions",
		"--output-format", "stream-json", "--include-partial-messages", "--verbose"}
	if !slices.Equal(got, want) {
		t.Fatalf("yolo args changed:\n got %q\nwant %q", got, want)
	}
	for _, f := range claudeIsolation {
		if slices.Contains(got, f) {
			t.Fatalf("yolo must not carry %s", f)
		}
	}
}

func argAfter(a []string, flag string) string {
	for i := range a {
		if a[i] == flag && i+1 < len(a) {
			return a[i+1]
		}
	}
	return ""
}

func TestClaudeArgsPlan(t *testing.T) {
	a, err := claudeBaseArgs(modePlan)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(a, "--dangerously-skip-permissions") {
		t.Fatal("plan launch must not skip permissions")
	}
	// bypass-spike fix: no agent-written settings / MCP configs.
	for _, f := range []string{"--bare", "--setting-sources=", "--strict-mcp-config"} {
		if !slices.Contains(a, f) {
			t.Fatalf("plan launch missing %s: %q", f, a)
		}
	}
	if v := argAfter(a, "--permission-mode"); v != "plan" {
		t.Fatalf("--permission-mode = %q", v)
	}
	if v := argAfter(a, "--tools"); v != "Read" {
		t.Fatalf("plan --tools = %q, want read-only Read", v)
	}
	if v := argAfter(a, "--disallowedTools"); !strings.Contains(v, "ExitPlanMode") {
		t.Fatalf("ExitPlanMode not disallowed: %q", v)
	}
	if v := argAfter(a, "--permission-prompts"); v != "none" {
		t.Fatalf("--permission-prompts = %q", v)
	}
	for _, w := range []string{"Bash", "Edit", "Write"} {
		if strings.Contains(argAfter(a, "--tools"), w) {
			t.Fatalf("plan tool set contains %s", w)
		}
	}
}

func TestNormTurnModeFailsClosed(t *testing.T) {
	for in, want := range map[string]string{"": modeYolo, "yolo": modeYolo, " PLAN ": modePlan, "confirm": modeConfirm} {
		got, err := normTurnMode(in)
		if err != nil || got != want {
			t.Fatalf("normTurnMode(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"auto", "bypass", "yolo2", "acceptEdits"} {
		if _, err := normTurnMode(bad); err == nil {
			t.Fatalf("unknown mode %q accepted", bad)
		}
	}
}

// codex / pi / venice must refuse plan and confirm — never a silent yolo.
func TestHarnessRejectsRestrictedModes(t *testing.T) {
	for _, prov := range []string{"openai", "minimax", "venice"} {
		if err := harnessModeErr(prov, modeYolo); err != nil {
			t.Fatalf("%s yolo refused: %v", prov, err)
		}
		for _, m := range []string{modePlan, modeConfirm} {
			err := harnessModeErr(prov, m)
			if err == nil || !strings.Contains(err.Error(), "not supported for this harness yet") {
				t.Fatalf("%s %s: err = %v", prov, m, err)
			}
			if !strings.Contains(err.Error(), turnHarness(prov)) {
				t.Fatalf("error does not name the harness: %v", err)
			}
		}
	}
	for _, prov := range []string{"", "claudesdk"} {
		for _, m := range []string{modeYolo, modePlan, modeConfirm} {
			if err := harnessModeErr(prov, m); err != nil {
				t.Fatalf("claude %q %s refused: %v", prov, m, err)
			}
		}
	}
}

// runClaude end to end through the worker seam: the mode on the wire picks
// the launch; an unimplemented / unknown mode starts NO worker.
func runClaudeMode(t *testing.T, mode string) (argv []string, started bool, errs []string) {
	t.Helper()
	prev := startClaudeWorker
	startClaudeWorker = func(_ string, args, _ []string, _ io.Reader) (*worker, error) {
		argv, started = args, true
		return nil, io.ErrUnexpectedEOF // stop right after the launch decision
	}
	t.Cleanup(func() { startClaudeWorker = prev })
	var buf bytes.Buffer
	tw := &turnConn{c: &buf}
	runClaude(tw, &pb.MsgReq{Msg: []byte("hi"), Mode: mode}, groupConfig{}, t.TempDir()+"/default.id", false, nil)
	for _, f := range decodeFrames(t, &buf) {
		if e := f.GetErr(); e != "" {
			errs = append(errs, e)
		}
	}
	return
}

func TestRunClaudeModeLaunch(t *testing.T) {
	a, ok, _ := runClaudeMode(t, "")
	if !ok || !slices.Contains(a, "--dangerously-skip-permissions") {
		t.Fatalf("legacy empty mode should launch yolo: %q", a)
	}
	a, ok, _ = runClaudeMode(t, "plan")
	if !ok || argAfter(a, "--permission-mode") != "plan" || slices.Contains(a, "--dangerously-skip-permissions") {
		t.Fatalf("plan launch wrong: %q", a)
	}
	for _, m := range []string{"bogus"} {
		_, ok, errs := runClaudeMode(t, m)
		if ok || len(errs) == 0 {
			t.Fatalf("mode %q: started=%v errs=%q — must refuse without launching", m, ok, errs)
		}
	}
}
