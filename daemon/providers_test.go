package main

import (
	"net/http"
	"os"
	"strings"
	"testing"

	pb "koto-protocol/pb"
)

// The single allowlist accepts exactly the three providers and nothing else,
// and every admission point agrees with it.
func TestProviderAllowlist(t *testing.T) {
	for _, p := range []string{"claudesdk", "venice", "openai", "minimax"} {
		if !validProvider(p) {
			t.Errorf("validProvider(%q) = false, want true", p)
		}
		if got, ok := normProvider("  " + strings.ToUpper(p) + "\n"); !ok || got != p {
			t.Errorf("normProvider(%q) = %q,%v", p, got, ok)
		}
	}
	for _, p := range []string{"", "gemini", "OpenAI", "openai ", "chatgpt", "codex", "anthropic", "venice\x00"} {
		if validProvider(p) {
			t.Errorf("validProvider(%q) = true, want false", p)
		}
	}
	if _, ok := normProvider("gemini"); ok {
		t.Error("normProvider accepted gemini")
	}
	if !validProvider(defaultProvider()) {
		t.Fatal("defaultProvider() is not on the allowlist")
	}
	if got := providerListText(); got != "claudesdk, venice, openai or minimax" {
		t.Errorf("providerListText = %q", got)
	}
	// applyConfig: accepts openai, rejects unknown without clobbering.
	cfg := map[string]any{"provider": "venice"}
	applyConfig(cfg, "provider", jsonStr(t, "gemini"))
	if cfg["provider"] != "venice" {
		t.Errorf("unknown provider clobbered config: %v", cfg["provider"])
	}
	applyConfig(cfg, "provider", jsonStr(t, " OpenAI "))
	if cfg["provider"] != "openai" {
		t.Errorf("openai not accepted by applyConfig: %v", cfg["provider"])
	}
}

func jsonStr(t *testing.T, s string) []byte {
	t.Helper()
	return []byte(`"` + s + `"`)
}

// config_set provider:"openai" over the ctl plane persists to config.json
// and groupProvider (what the proxy reads per request) returns it; an
// unknown provider leaves it untouched.
func TestCtlConfigSetProviderOpenAI(t *testing.T) {
	fcHarness(t)
	resp := ctlDispatch("main", ctlLine(t, map[string]any{"cmd": "config_set", "group": "tg", "provider": "openai"}))
	if cr, ok := resp.(configResp); !ok || !cr.OK {
		t.Fatalf("config_set provider openai: %+v", resp)
	}
	if got := groupProvider("tg"); got != "openai" {
		t.Fatalf("groupProvider = %q after config_set, want openai", got)
	}
	if got := loadGroupConfig("tg").str("provider"); got != "openai" {
		t.Fatalf("config.json provider = %q, want openai", got)
	}
	if got := groupModelName("tg"); got != defaultOpenAIModel {
		t.Errorf("effective model = %q, want %q", got, defaultOpenAIModel)
	}
	// ensure() must not rewrite a valid openai back to the default.
	if err := ensureProviderConfig("tg"); err != nil {
		t.Fatal(err)
	}
	if got := groupProvider("tg"); got != "openai" {
		t.Fatalf("ensureProviderConfig clobbered openai -> %q", got)
	}
	ctlDispatch("main", ctlLine(t, map[string]any{"cmd": "config_set", "group": "tg", "provider": "gemini"}))
	if got := groupProvider("tg"); got != "openai" {
		t.Fatalf("unknown provider changed the group: %q", got)
	}
}

// Spawn's own admission check uses the same list.
func TestSpawnProviderAllowlist(t *testing.T) {
	s := &kotoServer{}
	r, err := s.Spawn(t.Context(), &pb.SpawnReq{Group: "okgroup", Provider: "gemini"})
	if err != nil || !strings.Contains(r.Error, "provider must be claudesdk, venice, openai or minimax") {
		t.Fatalf("gemini must be refused: %+v %v", r, err)
	}
}

// A provider without a wired leg must never reach the Anthropic leg: the
// gate refuses every path, and the belt in ServeHTTP refuses anything that
// slips past. Unknown providers get no routes at all.
func TestUnwiredProviderFailsClosed(t *testing.T) {
	if r := llmRoutesFor("gemini"); r != nil {
		t.Fatalf("unknown provider got routes: %v", r)
	}
	if llmPathAllowed("gemini", http.MethodPost, "/v1/messages") {
		t.Fatal("unknown provider allowed onto /v1/messages")
	}
}

// The guest's fallback (fcguest codexDefaultModel, used when the host sends no
// KOTO_DEFAULT_OPENAI_MODEL) must be the same model the daemon defaults to and
// shows in the picker, or an old/new host+rootfs pair would disagree.
func TestOpenAIDefaultMatchesGuestFallback(t *testing.T) {
	b, err := os.ReadFile("../fcguest/codex.go")
	if err != nil {
		t.Skipf("fcguest source not beside daemon: %v", err)
	}
	want := `const codexDefaultModel = "` + defaultOpenAIModel + `"`
	if !strings.Contains(string(b), want) {
		t.Errorf("fcguest/codex.go lacks %s", want)
	}
	if l := modelsFallback["openai"]; len(l) == 0 || l[0].ID != defaultOpenAIModel {
		t.Errorf("openai fallback list must lead with %q: %+v", defaultOpenAIModel, l)
	}
}
