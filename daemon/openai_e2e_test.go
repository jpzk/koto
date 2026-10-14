package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	if d := os.Getenv("KOTO_E2E_OPENAI_BUDGET_DIR"); d != "" {
		e2eOpenAIBudget(t, d, proxyAddr, os.Getenv("KOTO_E2E_OPENAI_UPLOG"))
	}
}

// e2eOpenAIBudget is the budget phase of scripts/e2e-openai.sh, run after the
// guest turns. It writes what the script asserts on into dir:
//
//	budget-before.json  the operator budget view (same data `koto budget
//	                    --json` serves) after the real turns: x-codex windows,
//	                    credits and token spend for openai/apikey
//	budget-render.txt   the `koto budget` text rendering of that view
//	context.txt         the koto-context block group tg would get next turn
//	cap.txt             status, x-koto-budget-cap and body of one request sent
//	                    through the proxy AFTER a tokens_per_day cap below the
//	                    day's spend was set via the admin path (budgetAdmin)
//	upstream-hits.txt   upstream request count (lines "=== " in uplog) right
//	                    before and after that request
//	budget-after.json   the view after the refusal (refused counter)
func e2eOpenAIBudget(t *testing.T, dir, proxyAddr, uplog string) {
	t.Helper()
	writeJSON := func(name string, v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	upHits := func() int {
		b, err := os.ReadFile(uplog)
		if err != nil {
			t.Fatalf("upstream log: %v", err)
		}
		n := 0
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "=== ") {
				n++
			}
		}
		return n
	}
	v, err := budgetAdmin("show", "", "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON("budget-before.json", v)
	_ = os.WriteFile(filepath.Join(dir, "budget-render.txt"), []byte(budgetRender(v)), 0o644)
	key := budgetKeyFor(groupProvider("tg"))
	_ = os.WriteFile(filepath.Join(dir, "context.txt"), []byte(contextBlock(key, latestMetric("tg"))), 0o644)

	// The day's spend is already > 1000 tokens, so a 1k cap is exceeded.
	if _, err := budgetAdmin("set", "openai", "apikey", "tokens_per_day", "1k", "", nil); err != nil {
		t.Fatalf("budget set: %v", err)
	}
	before := upHits()
	req, _ := http.NewRequest(http.MethodPost, "http://"+proxyAddr+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5-codex","input":"over the cap","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer koto-proxied")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("capped request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	time.Sleep(500 * time.Millisecond) // any stray upstream hit would land by now
	after := upHits()
	_ = os.WriteFile(filepath.Join(dir, "cap.txt"), []byte(fmt.Sprintf("status=%d\ncap=%s\nretry=%s\nbody=%s\n",
		resp.StatusCode, resp.Header.Get("x-koto-budget-cap"), resp.Header.Get("x-should-retry"), body)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "upstream-hits.txt"), []byte(fmt.Sprintf("before=%d after=%d\n", before, after)), 0o644)
	v, err = budgetAdmin("show", "", "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON("budget-after.json", v)
	t.Logf("budget phase: capped request status %d, upstream hits %d -> %d", resp.StatusCode, before, after)
}
