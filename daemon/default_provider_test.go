package main

// The host default provider (KOTO_DEFAULT_PROVIDER): what it resolves to, what
// it is allowed to change, and who is allowed to change it.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Unset, empty or anything off the allowlist falls back to claudesdk — an
// install that never ran the provider step behaves exactly as before.
func TestDefaultProviderFallsBackToClaude(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", "claudesdk"},
		{"gemini", "claudesdk"},
		{"codex", "claudesdk"},
		{"claudesdk", "claudesdk"},
		{"venice", "venice"},
		{"openai", "openai"},
		{" OpenAI\n", "openai"}, // hand-edited koto.env: case and space forgiven
	} {
		t.Setenv(defaultProviderEnv, tc.env)
		if got := defaultProvider(); got != tc.want {
			t.Errorf("KOTO_DEFAULT_PROVIDER=%q: defaultProvider() = %q, want %q", tc.env, got, tc.want)
		}
	}
	os.Unsetenv(defaultProviderEnv) // t.Setenv restores at cleanup
	if got := defaultProvider(); got != "claudesdk" {
		t.Errorf("absent KOTO_DEFAULT_PROVIDER: defaultProvider() = %q, want claudesdk", got)
	}
	if !validProvider(builtinDefaultProvider) {
		t.Fatal("builtinDefaultProvider is not on the allowlist")
	}
}

func readCfgProvider(t *testing.T, g string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	s, _ := cfg["provider"].(string)
	return s
}

// New groups — main included — get the configured default written into their
// config.json, and every reader agrees with it.
func TestNewGroupsGetConfiguredDefault(t *testing.T) {
	for _, p := range knownProviders {
		t.Run(p, func(t *testing.T) {
			fcHarness(t)
			t.Setenv(defaultProviderEnv, p)
			for _, g := range []string{"main", "fresh"} {
				if err := ensureProviderConfig(g); err != nil {
					t.Fatal(err)
				}
				if got := readCfgProvider(t, g); got != p {
					t.Errorf("%s: config.json provider = %q, want %q", g, got, p)
				}
				if got := groupProvider(g); got != p {
					t.Errorf("%s: groupProvider = %q, want %q", g, got, p)
				}
			}
			// A group whose config cannot be read resolves to the same
			// default ensureProviderConfig would write (audit M148).
			if got := groupProvider("never-ensured"); got != p {
				t.Errorf("unreadable config: groupProvider = %q, want %q", got, p)
			}
		})
	}
}

// Changing the default never rewrites a group that already has a valid
// provider — not a byte of its config.json — while a missing or invalid one
// picks the new default up.
func TestExistingValidGroupsNotRewritten(t *testing.T) {
	fcHarness(t)
	write := func(g, body string) string {
		t.Helper()
		d := filepath.Join(vol(g), ".cs")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, "config.json")
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}
	valid := map[string]string{
		"main": `{"model":"claude-opus-5","provider":"claudesdk"}`, // canonical (sorted, compact): what updateGroupConfig writes
		"tg":   `{"provider":"venice"}`,
		"peer": `{"effort":"high","provider":"openai"}`,
	}
	files := map[string]string{}
	inodes := map[string]os.FileInfo{}
	for g, body := range valid {
		files[g] = write(g, body)
		inodes[g], _ = os.Stat(files[g])
	}
	write("noprov", `{"model":"x"}`)
	write("badprov", `{"provider":"gemini"}`)

	for _, def := range knownProviders {
		t.Setenv(defaultProviderEnv, def)
		for g, body := range valid {
			if err := ensureProviderConfig(g); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(files[g])
			if !bytes.Equal(got, []byte(body)) {
				t.Errorf("default=%s rewrote %s: %s, want %s", def, g, got, body)
			}
			// Same bytes is not enough (the canonical form re-marshals
			// identically): updateGroupConfig commits by rename, so an
			// untouched file is the SAME inode.
			if fi, err := os.Stat(files[g]); err != nil || !os.SameFile(fi, inodes[g]) {
				t.Errorf("default=%s replaced %s's config.json (new inode)", def, g)
			}
		}
	}
	t.Setenv(defaultProviderEnv, "openai")
	for _, g := range []string{"noprov", "badprov"} {
		if err := ensureProviderConfig(g); err != nil {
			t.Fatal(err)
		}
		if got := readCfgProvider(t, g); got != "openai" {
			t.Errorf("%s: missing/invalid provider became %q, want the default openai", g, got)
		}
	}
}

// The default is HOST environment only. The ctl plane has no way to name it:
// config_set takes no such key (an unknown JSON field is dropped), nothing
// lands in config.json, and the daemon's default does not move.
func TestDefaultProviderNotSettableViaCtl(t *testing.T) {
	fcHarness(t)
	os.Unsetenv(defaultProviderEnv)
	t.Cleanup(func() { os.Unsetenv(defaultProviderEnv) })
	for _, key := range []string{"default_provider", "defaultProvider", defaultProviderEnv, "KOTO_DEFAULT_PROVIDER"} {
		for _, target := range []string{"main", "tg"} {
			_ = ctlDispatch("main", ctlLine(t, map[string]any{"cmd": "config_set", "group": target, key: "venice"}))
			_ = ctlDispatch(target, ctlLine(t, map[string]any{"cmd": "config_set", key: "venice"}))
		}
	}
	for _, verb := range []string{"default_provider", "set_default_provider", "config_default"} {
		resp := ctlDispatch("main", ctlLine(t, map[string]any{"cmd": verb, "provider": "venice"}))
		if br, ok := resp.(baseResp); !ok || br.OK {
			t.Errorf("verb %q: want refusal, got %+v", verb, resp)
		}
	}
	if v, ok := os.LookupEnv(defaultProviderEnv); ok {
		t.Fatalf("ctl set %s=%q in the daemon environment", defaultProviderEnv, v)
	}
	if got := defaultProvider(); got != "claudesdk" {
		t.Fatalf("defaultProvider() moved to %q via ctl", got)
	}
	for _, g := range []string{"main", "tg"} {
		b, _ := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json"))
		if strings.Contains(strings.ToLower(string(b)), "default") {
			t.Errorf("%s config.json gained a default key: %s", g, b)
		}
	}
	// And no field of the ctl/gRPC config request can carry it.
	rt := reflect.TypeOf(configReq{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.ToLower(rt.Field(i).Tag.Get("json") + rt.Field(i).Name)
		if strings.Contains(tag, "default") {
			t.Errorf("configReq field %s (%s) looks like a default-provider knob", rt.Field(i).Name, rt.Field(i).Tag)
		}
	}
	// A fresh group still gets the built-in default.
	if err := ensureProviderConfig("fresh"); err != nil {
		t.Fatal(err)
	}
	if got := readCfgProvider(t, "fresh"); got != "claudesdk" {
		t.Errorf("fresh group provider = %q after ctl attempts, want claudesdk", got)
	}
}

// Every provider the wizard offers has a description.
func TestProviderBlurbCoversAllowlist(t *testing.T) {
	if len(providerBlurb) != len(knownProviders) {
		t.Fatalf("providerBlurb has %d entries, knownProviders %d", len(providerBlurb), len(knownProviders))
	}
	for _, p := range knownProviders {
		if strings.TrimSpace(providerBlurb[p]) == "" {
			t.Errorf("no blurb for %s", p)
		}
	}
}
