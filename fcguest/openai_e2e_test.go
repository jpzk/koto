package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// TestE2EOpenAIGuestHalf is the GUEST half of scripts/e2e-openai.sh, not a
// unit test: it skips unless KOTO_E2E_OPENAI_TURN is set. It must run inside
// the built rootfs image (root, with the pinned /usr/local/bin/codex) because
// it drives the real turn path: readGroupConfig -> runCodex -> workerCmd
// (uid 1000) -> codex exec, frames streamed to the host turn sink at
// KOTO_E2E_OPENAI_TURN exactly as runTurn streams them over vsock 9004.
// The worker env carries ONLY what the daemon gives a guest
// (fcGuestInitEnv): the proxy base URL and the placeholder key.
// KOTO_E2E_OPENAI_PROMPTS is a "|"-separated list, one turn each, in the
// same session (so turn 2 exercises codex exec resume).
func TestE2EOpenAIGuestHalf(t *testing.T) {
	turnAddr := os.Getenv("KOTO_E2E_OPENAI_TURN")
	if turnAddr == "" {
		t.Skip("e2e harness: set KOTO_E2E_OPENAI_TURN (scripts/e2e-openai.sh)")
	}
	base := os.Getenv("KOTO_E2E_OPENAI_BASE_URL")
	if base == "" {
		t.Fatal("need KOTO_E2E_OPENAI_BASE_URL (the host proxy, guest-facing)")
	}
	// fc-agent is PID 1 and runs the central wait4(-1) reaper that
	// startTracked relies on; a test binary has to start it itself.
	go reaper()
	for _, d := range []string{wsDir, csDir, filepath.Join(csDir, "sessions")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.Chown(d, workerUID, workerGID)
	}
	// What handleInit materialises from the daemon's ConfigJson.
	writeWorkerFile(filepath.Join(csDir, "config.json"), []byte(`{"provider":"openai"}`))
	cfg := readGroupConfig()
	if cfg.Provider != "openai" {
		t.Fatalf("provider = %q", cfg.Provider)
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + wsDir,
		"KOTO_OPENAI_BASE_URL=" + base,
		"KOTO_OPENAI_API_KEY=koto-proxied",
		"KOTO_DEFAULT_OPENAI_MODEL=gpt-5.5",
		"KOTO_SESSION=default",
	}
	prompts := strings.Split(os.Getenv("KOTO_E2E_OPENAI_PROMPTS"), "|")
	for i, p := range prompts {
		c, err := net.DialTimeout("tcp", turnAddr, 10*time.Second)
		if err != nil {
			t.Fatalf("turn %d: dial host: %v", i+1, err)
		}
		tw := newTurnConn(c)
		tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_Open{Open: &pb.TurnOpen{Slot: 0}}})
		runCodex(tw, &pb.MsgReq{Msg: []byte(p), Session: "default"}, cfg, "default", env)
		tw.send(&pb.TurnFrame{Kind: &pb.TurnFrame_TurnEnd{TurnEnd: true}})
		tw.close()
		c.Close()
		id, _ := os.ReadFile(codexIDFile("default"))
		t.Logf("turn %d done; codex thread id file: %q", i+1, strings.TrimSpace(string(id)))
	}
}
