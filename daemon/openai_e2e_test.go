package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestE2EOpenAIHostHalf is the HOST half of scripts/e2e-openai.sh, not a unit
// test: it skips unless KOTO_E2E_OPENAI_PROXY is set. It serves, for group
// "tg" with provider=openai,
//
//	KOTO_E2E_OPENAI_PROXY  the real proxy handler (serveOpenAI leg) with the
//	                       host API key from KOTO_E2E_OPENAI_KEY and the
//	                       upstream base pointed at KOTO_E2E_OPENAI_UPSTREAM
//	KOTO_E2E_OPENAI_TURN   the real turn sink (fcTurnSink): each accepted
//	                       connection is one guest turn stream on slot 0
//
// It handles KOTO_E2E_OPENAI_TURNS turns (default 1), then prints the host
// slot log and metrics.jsonl so the script can assert on what the daemon
// recorded. The guest half is fcguest TestE2EOpenAIGuestHalf.
func TestE2EOpenAIHostHalf(t *testing.T) {
	proxyAddr := os.Getenv("KOTO_E2E_OPENAI_PROXY")
	if proxyAddr == "" {
		t.Skip("e2e harness: set KOTO_E2E_OPENAI_PROXY (scripts/e2e-openai.sh)")
	}
	turnAddr := os.Getenv("KOTO_E2E_OPENAI_TURN")
	key := os.Getenv("KOTO_E2E_OPENAI_KEY")
	up := os.Getenv("KOTO_E2E_OPENAI_UPSTREAM")
	if turnAddr == "" || key == "" || up == "" {
		t.Fatal("need KOTO_E2E_OPENAI_TURN, KOTO_E2E_OPENAI_KEY, KOTO_E2E_OPENAI_UPSTREAM")
	}
	turns := 1
	if n, err := strconv.Atoi(os.Getenv("KOTO_E2E_OPENAI_TURNS")); err == nil && n > 0 {
		turns = n
	}
	openaiProxyEnv(t) // group tg, provider=openai, temp HERE/METRICS
	openaiAPIUpstream = up
	openaiChatGPTUpstream = up // unused in api-key mode; never the real host
	if err := os.MkdirAll(filepath.Dir(openaiAPIKeyPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openaiAPIKeyPath(), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := groupProvider("tg"); p != providerOpenAI {
		t.Fatalf("groupProvider(tg) = %q, want openai", p)
	}

	pl, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: &handler{group: "tg"}, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(pl) }()
	defer srv.Close()

	tl, err := net.Listen("tcp", turnAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	t.Logf("host half ready: proxy %s turn %s upstream %s turns %d", proxyAddr, turnAddr, up, turns)
	if f := os.Getenv("KOTO_E2E_OPENAI_READY"); f != "" {
		_ = os.WriteFile(f, []byte("ready\n"), 0o644)
	}

	for i := 0; i < turns; i++ {
		fcExpectTurn("tg", 0, "default")
		_ = tl.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Minute))
		c, err := tl.Accept()
		if err != nil {
			t.Fatalf("turn %d: accept: %v", i+1, err)
		}
		fcTurnSink("tg", c) // returns after TurnEnd / close
		t.Logf("turn %d: stream closed", i+1)
	}

	log, err := os.ReadFile(slotLogPath("tg", 0))
	if err != nil {
		t.Fatalf("host slot log: %v", err)
	}
	t.Logf("HOST SLOT LOG (tg slot 0):\n%s", log)
	m, _ := os.ReadFile(METRICS)
	t.Logf("HOST METRICS.JSONL:\n%s", m)
}
