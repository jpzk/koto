package main

import (
	"os"
	"strings"
	"testing"
)

// renderFromFile renders koto.env the way `koto install` does from the text
// of an existing file. ONE seam so the same assertions ran against the old
// whitelist renderer (which took parseEnvFile's map) to prove they fail there.
func renderFromFile(raw string) string {
	d, o := envFileKeys("/var/lib/koto", "/usr/local/bin/claude")
	return renderEnvFile(raw, d, o)
}

// An upgrade's koto.env, hand-edited by an operator: managed keys, the OTel
// exporter config the daemon reads (otel.go / OTel SDK env), and keys koto
// has never heard of. Values carry '=', quotes and spaces on purpose.
const upgradeEnv = `# koto service configuration — read by the systemd unit.
# Edit and ` + "`sudo systemctl restart koto`" + ` to apply.
# Re-running ` + "`koto install`" + ` preserves the values set here.

KOTO_HOME=/var/lib/koto
KOTO_BIND=127.0.0.1
KOTO_PORT=8443

#KOTO_HOST_CPUS=
KOTO_HOST_MEM_MIB=4096
#ANTHROPIC_API_KEY=
KOTO_CLAUDE_BIN=/usr/local/bin/claude
KOTO_DEFAULT_PROVIDER=venice
# collector on the tailnet (bearer rotated monthly)
OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.net:4318
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer abc==,x-tenant="koto prod"
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://otel.example.net:4318/v1/traces
OTEL_METRIC_EXPORT_INTERVAL=15000
KOTO_OTEL_ENDPOINT=otel.example.net:4317
KOTO_OTEL_CAPTURE_CONTENT=1
KOTO_UPSTREAM=https://upstream.example:8443
KOTO_UPSTREAM_PROVIDERS=claudesdk,openai
FOO_BAR='single quoted = still one value'
`

// preservedKeys: every key the operator set that `koto install` does not
// manage. All of them must come back with the exact value.
var preservedKeys = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_METRIC_EXPORT_INTERVAL", "KOTO_OTEL_ENDPOINT",
	"KOTO_OTEL_CAPTURE_CONTENT", "KOTO_UPSTREAM", "KOTO_UPSTREAM_PROVIDERS",
	"FOO_BAR",
}

func TestRenderEnvFilePreservesUnknownKeys(t *testing.T) {
	in := parseEnvFile(upgradeEnv)
	got := parseEnvFile(renderFromFile(upgradeEnv))
	for _, k := range preservedKeys {
		if got[k] != in[k] {
			t.Errorf("%s: dropped or changed by a reinstall: got %q, want %q", k, got[k], in[k])
		}
	}
	// Managed keys keep their current semantics.
	for _, k := range []string{"KOTO_HOME", "KOTO_BIND", "KOTO_PORT", "KOTO_HOST_MEM_MIB", "KOTO_DEFAULT_PROVIDER", claudeBinEnv} {
		if got[k] != in[k] {
			t.Errorf("managed %s: got %q, want %q", k, got[k], in[k])
		}
	}
	if len(got) != len(in) {
		t.Errorf("key count: got %d, want %d", len(got), len(in))
	}
}

func TestRenderEnvFileValuesWithEqualsAndQuotes(t *testing.T) {
	cases := map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS": `Authorization=Basic dXNlcjpwYXNz==,k="v=1"`,
		"WEIRD":                      `"double = quoted"`,
		"SQ":                         `'it''s'`,
		"TRAIL_EQ":                   `abc==`,
		"SPACES":                     `a b  c`,
	}
	var b strings.Builder
	for _, k := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "WEIRD", "SQ", "TRAIL_EQ", "SPACES"} {
		b.WriteString(k + "=" + cases[k] + "\n")
	}
	got := parseEnvFile(renderFromFile(b.String()))
	for k, want := range cases {
		if got[k] != want {
			t.Errorf("%s: got %q, want %q", k, got[k], want)
		}
	}
}

func TestRenderEnvFileIdempotent(t *testing.T) {
	for name, raw := range map[string]string{"fresh": "", "upgrade": upgradeEnv} {
		once := renderFromFile(raw)
		if twice := renderFromFile(once); twice != once {
			t.Errorf("%s: render(render(x)) != render(x)\n--- once\n%s--- twice\n%s", name, once, twice)
		}
	}
}

// TestWriteEnvFileUpgradePreservesOTel drives the installer's real koto.env
// write (writeEnvFile, as `koto install` calls it) against a temp file through
// the koto.env seams — never the host's /etc/koto/koto.env.
func TestWriteEnvFileUpgradePreservesOTel(t *testing.T) {
	path := setupEnvHarness(t, upgradeEnv)
	o := installOpts{stateDir: "/var/lib/koto", ui: testUI("")}
	if err := writeEnvFile(o, "/usr/local/bin/claude"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	in, got := parseEnvFile(upgradeEnv), parseEnvFile(string(b))
	for _, k := range preservedKeys {
		if got[k] != in[k] {
			t.Errorf("%s after upgrade: got %q, want %q", k, got[k], in[k])
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", fi.Mode().Perm(), err)
	}
}

// The operator's comment travels with its key, once; preserved keys keep the
// order they were written in; a key assigned twice keeps the value systemd
// honoured (the last) and is written once.
func TestRenderEnvFileCommentsOrderDuplicates(t *testing.T) {
	out := renderFromFile(upgradeEnv + "FOO_BAR=second\n")
	if n := strings.Count(out, "# collector on the tailnet"); n != 1 {
		t.Errorf("operator comment: %d copies, want 1\n%s", n, out)
	}
	if !strings.Contains(out, "# collector on the tailnet (bearer rotated monthly)\nOTEL_EXPORTER_OTLP_ENDPOINT=") {
		t.Errorf("comment not directly above its key\n%s", out)
	}
	last := -1
	for _, k := range preservedKeys {
		i := strings.Index(out, "\n"+k+"=")
		if i < last {
			t.Errorf("%s out of original order", k)
		}
		last = i
	}
	if strings.Count(out, "\nFOO_BAR=") != 1 || parseEnvFile(out)["FOO_BAR"] != "second" {
		t.Errorf("duplicate FOO_BAR not collapsed to last value\n%s", out)
	}
	if strings.Count(out, "# koto service configuration") != 1 {
		t.Errorf("header duplicated\n%s", out)
	}
}
