package main

import (
	"strings"
	"testing"
)

// TestCodexModelResolution: runCodex must ALWAYS name a model. With no group
// model and no host default it used to pass none, so codex fell back to its
// built-in default (gpt-5.5 on 0.163.0), which the ChatGPT-account upstream
// refuses. Group config beats the host default, which beats koto's own.
func TestCodexModelResolution(t *testing.T) {
	base := []string{"KOTO_OPENAI_BASE_URL=http://127.0.0.1:1/v1", "KOTO_OPENAI_API_KEY=koto-proxied"}
	cases := []struct {
		name string
		cfg  groupConfig
		env  []string
		want string
	}{
		{"unset -> koto default", groupConfig{}, base, codexDefaultModel},
		{"blank values -> koto default", groupConfig{Model: "  "}, append(base, "KOTO_DEFAULT_OPENAI_MODEL= "), codexDefaultModel},
		{"host default wins over koto default", groupConfig{}, append(base, "KOTO_DEFAULT_OPENAI_MODEL=gpt-6-astra"), "gpt-6-astra"},
		{"group model wins over host default", groupConfig{Model: "gpt-6-luna"}, append(base, "KOTO_DEFAULT_OPENAI_MODEL=gpt-6-astra"), "gpt-6-luna"},
		{"group model, no host default", groupConfig{Model: "gpt-5.6-sol"}, base, "gpt-5.6-sol"},
	}
	for _, c := range cases {
		got := codexModel(c.cfg, c.env)
		if got != c.want {
			t.Errorf("%s: model = %q, want %q", c.name, got, c.want)
		}
		args := strings.Join(codexArgs("http://x/v1", got, "", "", ""), "\x00")
		if !strings.Contains(args, "\x00-c\x00model="+tomlString(c.want)+"\x00") {
			t.Errorf("%s: codex argv lacks -c model=%s", c.name, tomlString(c.want))
		}
	}
	if codexDefaultModel == "" || codexDefaultModel == "gpt-5.5" {
		t.Fatalf("codexDefaultModel = %q: must be a model the ChatGPT-account upstream accepts", codexDefaultModel)
	}
}
