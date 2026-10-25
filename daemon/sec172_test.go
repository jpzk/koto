package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// 1.7.2 fix(1), audit proxy/chat-completions-budget-bypass: the Anthropic
// leg relayed POST /v1/chat/completions, whose OpenAI-shaped usage
// (prompt_tokens/completion_tokens) budgetRecordSpend never counted, so a
// guest could spend past every budget cap through it. The route is gone:
// the gate must refuse it with 403 BEFORE any credential is read or any
// upstream is contacted, while the Venice leg (which meters its own usage)
// keeps it.
func TestSec172AnthropicLegRefusesChatCompletions(t *testing.T) {
	for _, p := range []string{providerClaude, ""} {
		if llmPathAllowed(p, http.MethodPost, "/v1/chat/completions") {
			t.Errorf("provider %q: POST /v1/chat/completions must be outside the Anthropic allowlist", p)
		}
		if llmPathAllowed(p, http.MethodPost, "/v1/messages/../chat/completions") {
			t.Errorf("provider %q: traversal onto /v1/chat/completions must be refused", p)
		}
	}
	if !llmPathAllowed(providerVenice, http.MethodPost, "/v1/chat/completions") {
		t.Error("the venice leg still needs /v1/chat/completions (pi)")
	}

	prevHere := HERE
	HERE = t.TempDir()
	t.Cleanup(func() { HERE = prevHere; initPaths() })
	initPaths()
	h := &handler{group: "sec172g"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer proxied")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("claudesdk POST /v1/chat/completions: status %d, want 403 (refused at the gate); body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside the endpoint allowlist") {
		t.Errorf("refusal must name the allowlist: %q", rec.Body.String())
	}
}

// sec172Zeros yields zero bytes forever without holding them, so a test can
// push large bodies through readRequestBody without a large source buffer.
type sec172Zeros struct{}

func (sec172Zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// 1.7.2 fix(3), audit proxy/global-body-budget-starvation: every group's
// request bodies were charged to ONE fleet-wide 512 MiB counter, so a single
// guest holding enough in-flight maximum-size bodies spent the whole pool and
// every OTHER group's next request answered 503. A group must be refused at
// its own share while another group is still admitted; the global ceiling
// still applies (TestProxyBodyBudgetBounded).
//
// The hog drops each body as soon as it is read and keeps only the release,
// so the CHARGE stays held (exactly what an in-flight request does) while the
// test's heap stays small even on the unfixed base.
func TestSec172BodyBudgetPerGroup(t *testing.T) {
	if n := proxyBodyCharged.Load(); n != 0 {
		t.Fatalf("budget starts at %d, not 0", n)
	}
	t.Cleanup(func() { proxyBodyCharged.Store(0) })

	read := func(group string, size int) (int, func()) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			io.LimitReader(sec172Zeros{}, int64(size)))
		rec := httptest.NewRecorder()
		_, rel, ok := readRequestBody(rec, req, group)
		if !ok {
			return rec.Code, func() {}
		}
		return http.StatusOK, rel
	}

	var held []func()
	releaseAll := func() {
		for _, r := range held {
			r()
		}
		held = nil
	}
	t.Cleanup(releaseAll)

	// The hog fills as much as it is allowed to, one maximum-size body at a
	// time, up to the whole fleet budget.
	var admitted int64
	refused := false
	for admitted < proxyBodyBudget {
		code, rel := read("sec172-hog", proxyMaxBody)
		if code != http.StatusOK {
			if code != http.StatusServiceUnavailable {
				t.Fatalf("hog refused with %d, want 503", code)
			}
			refused = true
			break
		}
		held = append(held, rel)
		admitted += proxyMaxBody
	}
	if !refused {
		t.Errorf("one group was admitted the entire fleet budget (%d MiB) — no per-group share", admitted>>20)
	}
	if admitted == 0 {
		t.Fatal("hog was refused its very first maximum-size body; the share is below proxyMaxBody")
	}

	// Another group is still served while the hog sits at its share.
	if code, rel := read("sec172-victim", 1<<20); code != http.StatusOK {
		t.Errorf("victim group refused with %d while another group held %d MiB — fleet starved by one peer", code, admitted>>20)
	} else {
		rel()
	}

	// Releasing returns the hog's share too, not just the global bytes.
	releaseAll()
	if got := proxyBodyCharged.Load(); got != 0 {
		t.Fatalf("after release charged %d, want 0", got)
	}
	if code, rel := read("sec172-hog", proxyMaxBody); code != http.StatusOK {
		t.Errorf("hog refused (%d) after releasing everything — its share leaked", code)
	} else {
		rel()
	}
}

// sec172FakeClone builds a release-shaped clone that ALSO carries attacker-
// committed creds and daemon state (audit supply/release-clone-seeds-creds-
// state), plus the prompts/fcassets a real install needs so seedStateDir can
// run end to end.
func sec172FakeClone(t *testing.T) (root, state string, planted []string) {
	t.Helper()
	root, state = t.TempDir(), t.TempDir()
	if err := os.Chmod(state, 0o750); err != nil {
		t.Fatal(err)
	}
	w := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("prompts/global.md", "policy\n")
	for _, a := range []string{"firecracker", "vmlinux", "rootfs.img"} {
		w("fcassets/"+a, a)
	}
	planted = []string{
		"creds/ca.crt", "creds/ca.key", "creds/server.crt", "creds/server.key",
		"creds/acl.json", "creds/.credentials.json", "creds/anthropic-api-key",
		"creds/venice.key", "creds/minimax.key",
		"creds/client-evil.crt", "creds/client-evil.key", "creds/token-evil",
		"groups/main/prompt.md", "groups.json", "schedules.json", "goals.json", "metrics.jsonl",
	}
	for _, p := range planted {
		w(p, "attacker\n")
	}
	w("creds/clients.allow", "SHA256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff evil\n")
	w("creds/tokens.json", `{"evil":{"hash":"x","roles":["admin"]}}`)
	planted = append(planted, "creds/clients.allow", "creds/tokens.json")
	return root, state, planted
}

// 1.7.2 fix(5): a default install (what install.sh runs from the unsigned
// tag clone) must not carry ANY clone creds or daemon state into the state
// dir, and must leave the clone's files where they are.
func TestSec172InstallIgnoresCloneState(t *testing.T) {
	root, state, planted := sec172FakeClone(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	o := installOpts{root: root, stateDir: state, ui: newSetupUI(true, true)}
	if err := seedStateDir(o, me); err != nil {
		t.Fatalf("seedStateDir: %v", err)
	}
	for _, p := range planted {
		if _, err := os.Lstat(filepath.Join(state, p)); err == nil {
			t.Errorf("clone %s was imported into the state dir", p)
		}
	}
	// Top-level registries must not exist at all; groups/ and creds/ may
	// exist (seedStateDir creates them) but must be empty.
	for _, d := range []string{"groups", "creds"} {
		ents, _ := os.ReadDir(filepath.Join(state, d))
		for _, e := range ents {
			t.Errorf("state %s/ contains %s after a default install", d, e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(state, "prompts", "global.md")); err != nil {
		t.Errorf("prompts must still be installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "schedules.json")); err != nil {
		t.Errorf("clone schedules.json moved away (should be left alone): %v", err)
	}
}
