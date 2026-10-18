package main

// The wizard's provider step and the provider-aware credentials step. Every
// test runs against a temp koto.env and a temp state dir through the
// setupEnvRead/setupEnvWrite and setupProviderAuth seams: nothing here reads
// or writes /etc/koto, and no real login (browser, device code) runs.
//
// KOTO_SETUP_TRANSCRIPT_DIR, when set, receives the rendered provider-step
// transcript for each provider (<dir>/provider-<name>.txt) — the evidence
// that the step reads the way it should, captured from the real code path.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// captureStdout runs f with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

// setupEnvHarness points the wizard's koto.env at a temp file holding body
// (absent when body is "") and the state dir at a temp dir.
func setupEnvHarness(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "koto.env")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prevPath, prevRead, prevWrite := setupEnvPath, setupEnvRead, setupEnvWrite
	setupEnvPath = path
	setupEnvRead = func(p string) (string, bool, error) {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return string(b), err == nil, err
	}
	setupEnvWrite = func(u *setupUI, p, b string) (bool, error) {
		return true, os.WriteFile(p, []byte(b), 0o600)
	}
	t.Cleanup(func() { setupEnvPath, setupEnvRead, setupEnvWrite = prevPath, prevRead, prevWrite })
	t.Setenv("KOTO_HOME", filepath.Join(dir, "state"))
	return path
}

// installedEnv is koto.env as a fresh `koto install` renders it.
func installedEnv() string {
	d, o := envFileKeys("/var/lib/koto", "/usr/local/bin/claude")
	return renderEnvFile(nil, d, o)
}

func newTestSC(in string) *setupCtx {
	return &setupCtx{ui: testUI(in), ctx: context.Background()}
}

func readEnvKey(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return parseEnvFile(string(b))[defaultProviderEnv]
}

func runStep(t *testing.T, s setupStep, sc *setupCtx) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		sc.ui.printf("%s", sc.ui.bold(s.title))
		if s.explain != "" {
			sc.ui.prose(s.explain)
		}
		if err = s.run(sc); err == nil && s.verify != nil {
			err = s.verify(sc)
		}
	})
	return out, err
}

var menuLineRE = regexp.MustCompile(`^\s+([* ]) (\d+)\) (\S+)\s+(.*)$`)

// The step offers exactly knownProviders, in order, each with a description,
// and persists the operator's pick into koto.env without disturbing the rest
// of the file.
func TestProviderStepOffersKnownProvidersAndPersists(t *testing.T) {
	if i, j := stepIndex("provider"), stepIndex("auth"); i < 0 || j < 0 || i+1 != j {
		t.Fatalf("provider step at %d, auth at %d: want provider immediately before auth", i, j)
	}
	tdir := os.Getenv("KOTO_SETUP_TRANSCRIPT_DIR")
	for i, want := range knownProviders {
		t.Run(want, func(t *testing.T) {
			path := setupEnvHarness(t, installedEnv())
			before, _ := os.ReadFile(path)
			sc := newTestSC(string(rune('1'+i)) + "\n")
			out, err := runStep(t, stepProvider(), sc)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			var offered []string
			for _, l := range strings.Split(out, "\n") {
				if m := menuLineRE.FindStringSubmatch(l); m != nil {
					offered = append(offered, m[3])
					if strings.TrimSpace(m[4]) != providerBlurb[m[3]] {
						t.Errorf("%s described as %q", m[3], m[4])
					}
					if (m[1] == "*") != (m[3] == "claudesdk") {
						t.Errorf("preselection mark on %s = %q; with nothing chosen it must be claudesdk", m[3], m[1])
					}
				}
			}
			if strings.Join(offered, ",") != strings.Join(knownProviders, ",") {
				t.Fatalf("offered %v, want exactly %v\n%s", offered, knownProviders, out)
			}
			if got := readEnvKey(t, path); got != want {
				t.Fatalf("persisted %s=%q, want %q", defaultProviderEnv, got, want)
			}
			if sc.provider != want || !sc.restartOwed {
				t.Errorf("sc.provider=%q restartOwed=%v", sc.provider, sc.restartOwed)
			}
			// Every other line survives; only the placeholder is filled in.
			after, _ := os.ReadFile(path)
			if got, exp := string(after), strings.Replace(string(before), "#"+defaultProviderEnv+"=\n", defaultProviderEnv+"="+want+"\n", 1); got != exp {
				t.Errorf("koto.env rewritten beyond the one key:\n--- got\n%s--- want\n%s", got, exp)
			}
			if done, detail := stepProvider().detect(newTestSC("")); !done {
				t.Errorf("detect after run: not done (%s)", detail)
			}
			if tdir != "" {
				_ = os.WriteFile(filepath.Join(tdir, "provider-"+want+".txt"),
					[]byte("$ koto setup --only provider   (input: "+string(rune('1'+i))+")\n"+out), 0o644)
			}
		})
	}
}

func stepIndex(id string) int {
	for i, s := range setupSteps() {
		if s.id == id {
			return i
		}
	}
	return -1
}

// The current setting is preselected, and Enter keeps it.
func TestProviderStepPreselectsCurrent(t *testing.T) {
	path := setupEnvHarness(t, setEnvKey(installedEnv(), defaultProviderEnv, "openai"))
	sc := newTestSC("\n")
	out, err := runStep(t, stepProvider(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s+\* 3\) openai`).MatchString(out) {
		t.Errorf("openai not preselected:\n%s", out)
	}
	if got := readEnvKey(t, path); got != "openai" || sc.provider != "openai" || sc.restartOwed {
		t.Errorf("Enter changed something: env=%q provider=%q restartOwed=%v", got, sc.provider, sc.restartOwed)
	}
}

// --provider sets it with no prompt at all; -y alone keeps the existing value
// (claudesdk when none). Neither reads stdin — an empty reader would abort.
func TestProviderStepNonInteractive(t *testing.T) {
	path := setupEnvHarness(t, installedEnv())
	sc := newTestSC("") // any read would hit EOF and abort
	sc.providerFlag = "venice"
	if done, _ := stepProvider().detect(sc); done {
		t.Fatal("detect: done, though --provider differs from the file")
	}
	out, err := runStep(t, stepProvider(), sc)
	if err != nil {
		t.Fatalf("--provider: %v\n%s", err, out)
	}
	if strings.Contains(out, "choice [") || strings.Contains(out, "Which provider") {
		t.Errorf("--provider prompted:\n%s", out)
	}
	if got := readEnvKey(t, path); got != "venice" {
		t.Fatalf("--provider venice persisted %q", got)
	}
	if done, _ := stepProvider().detect(sc); !done {
		t.Error("detect: not done after --provider was applied")
	}
	if tdir := os.Getenv("KOTO_SETUP_TRANSCRIPT_DIR"); tdir != "" {
		_ = os.WriteFile(filepath.Join(tdir, "provider-flag-venice.txt"),
			[]byte("$ koto setup --only provider --provider venice   (no stdin)\n"+out), 0o644)
	}

	// -y, nothing set: keeps (and records) claudesdk; with a value set,
	// keeps that one.
	for _, tc := range []struct{ body, want string }{
		{installedEnv(), "claudesdk"},
		{setEnvKey(installedEnv(), defaultProviderEnv, "openai"), "openai"},
	} {
		path := setupEnvHarness(t, tc.body)
		sc := &setupCtx{ui: testUI(""), ctx: context.Background()}
		sc.ui.yes = true
		if out, err := runStep(t, stepProvider(), sc); err != nil {
			t.Fatalf("-y: %v\n%s", err, out)
		}
		if got := readEnvKey(t, path); got != tc.want {
			t.Errorf("-y persisted %q, want %q", got, tc.want)
		}
	}
}

// Absent, commented-out or unknown values read as claudesdk, and the step
// reports itself pending (so the wizard offers the choice).
func TestProviderSettingFallsBackToClaude(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no koto.env", ""},
		{"placeholder only", installedEnv()},
		{"unknown value", setEnvKey(installedEnv(), defaultProviderEnv, "gemini")},
		{"empty value", setEnvKey(installedEnv(), defaultProviderEnv, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupEnvHarness(t, tc.body)
			if _, p, err := setupProviderSetting(); err != nil || p != "claudesdk" {
				t.Errorf("setting = %q, %v; want claudesdk", p, err)
			}
			sc := newTestSC("")
			if done, detail := stepProvider().detect(sc); done {
				t.Errorf("detect: done (%s), want pending", detail)
			}
			if p := sc.effectiveProvider(); p != "claudesdk" {
				t.Errorf("effectiveProvider = %q", p)
			}
		})
	}
	if !stepProvider().advisory {
		t.Error("provider step must be advisory: an install that never chose still runs")
	}
}

type fakeLogins struct{ called []string }

func (f *fakeLogins) install(t *testing.T, present map[string]bool) {
	prev := setupProviderAuth
	t.Cleanup(func() { setupProviderAuth = prev })
	m := map[string]providerAuth{}
	for p, pa := range prev {
		p, pa := p, pa
		pa.detect = func(*setupCtx) (bool, string) {
			if present[p] {
				return true, p + " credential present (fake)"
			}
			return false, "no " + p + " credential (fake)"
		}
		pa.login = func(sc *setupCtx) error {
			f.called = append(f.called, p)
			sc.ui.ok("(fake) %s login completed", p)
			present[p] = true
			return nil
		}
		m[p] = pa
	}
	setupProviderAuth = m
}

// The credentials step runs the chosen provider's login and no other.
func TestAuthStepDispatchesToChosenProvider(t *testing.T) {
	if len(setupProviderAuth) != len(knownProviders) {
		t.Fatalf("setupProviderAuth covers %d providers, knownProviders has %d", len(setupProviderAuth), len(knownProviders))
	}
	for _, p := range knownProviders {
		t.Run(p, func(t *testing.T) {
			setupEnvHarness(t, setEnvKey(installedEnv(), defaultProviderEnv, p))
			f := &fakeLogins{}
			f.install(t, map[string]bool{})
			sc := newTestSC("y\n")
			if done, detail := stepAuth().detect(sc); done || !strings.HasPrefix(detail, p+":") {
				t.Fatalf("detect = %v %q before login", done, detail)
			}
			out, err := runStep(t, stepAuth(), sc)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if strings.Join(f.called, ",") != p {
				t.Fatalf("logins run: %v, want only %s", f.called, p)
			}
			if !sc.restartOwed {
				t.Error("a login must owe a restart")
			}
			// The choice made in this run wins over the file.
			f.called = nil
			sc2 := newTestSC("y\n")
			other := knownProviders[(indexOf(p)+1)%len(knownProviders)]
			sc2.provider = other
			if _, err := runStep(t, stepAuth(), sc2); err != nil {
				t.Fatal(err)
			}
			if strings.Join(f.called, ",") != other {
				t.Errorf("with sc.provider=%s ran %v", other, f.called)
			}
		})
	}
}

func indexOf(p string) int {
	for i, k := range knownProviders {
		if k == p {
			return i
		}
	}
	return -1
}

// Declining is allowed and loud; -y fails with the provider's own remedy.
func TestAuthStepSkipAndBatch(t *testing.T) {
	remedy := map[string]string{"claudesdk": "koto claude-login", "venice": "koto venice-login", "minimax": "koto minimax-login", "openai": "koto openai-login"}
	for _, p := range knownProviders {
		setupEnvHarness(t, setEnvKey(installedEnv(), defaultProviderEnv, p))
		f := &fakeLogins{}
		f.install(t, map[string]bool{})
		sc := newTestSC("n\n")
		out, err := runStep(t, stepAuth(), sc)
		if err != nil {
			t.Fatalf("%s skip: %v", p, err)
		}
		if len(f.called) != 0 || !sc.authSkipped {
			t.Errorf("%s skip ran %v (skipped=%v)", p, f.called, sc.authSkipped)
		}
		if !strings.Contains(out, "will FAIL") || !strings.Contains(out, remedy[p]) {
			t.Errorf("%s skip message does not say turns fail and how to fix:\n%s", p, out)
		}

		sc = newTestSC("")
		sc.ui.yes = true
		_, err = runStep(t, stepAuth(), sc)
		if err == nil || !strings.Contains(err.Error(), remedy[p]) {
			t.Errorf("%s -y: err = %v, want the %q remedy", p, err, remedy[p])
		}
		if len(f.called) != 0 {
			t.Errorf("%s -y ran a login", p)
		}
	}
}

// The real per-provider detectors look at the provider's own files only.
func TestProviderCredDetectors(t *testing.T) {
	setupEnvHarness(t, installedEnv())
	t.Setenv("ANTHROPIC_API_KEY", "")
	sc := newTestSC("")
	creds := sc.credsDir()
	if err := os.MkdirAll(creds, 0o700); err != nil {
		t.Fatal(err)
	}
	check := func(want map[string]bool) {
		t.Helper()
		for p, w := range want {
			if got, d := setupProviderAuth[p].detect(sc); got != w {
				t.Errorf("%s detect = %v (%s), want %v", p, got, d, w)
			}
		}
	}
	check(map[string]bool{"claudesdk": false, "venice": false, "openai": false})
	os.WriteFile(filepath.Join(creds, "venice.key"), []byte("  \n"), 0o600)
	check(map[string]bool{"venice": false}) // whitespace is not a key
	os.WriteFile(filepath.Join(creds, "venice.key"), []byte("vk-123\n"), 0o600)
	check(map[string]bool{"claudesdk": false, "venice": true, "openai": false})
	os.WriteFile(filepath.Join(creds, "openai-api-key"), []byte("sk-x"), 0o600)
	check(map[string]bool{"claudesdk": false, "openai": true})
	os.WriteFile(filepath.Join(creds, "anthropic-api-key"), []byte("sk-ant-x"), 0o600)
	check(map[string]bool{"claudesdk": true})
}

// The smoke step's provider check is a warning with a remedy, not a failure.
func TestSmokeProviderCheck(t *testing.T) {
	setupEnvHarness(t, setEnvKey(installedEnv(), defaultProviderEnv, "openai"))
	f := &fakeLogins{}
	f.install(t, map[string]bool{})
	sc := newTestSC("")
	var ok bool
	out := captureStdout(t, func() { ok = smokeProviderCheck(sc) })
	if ok || !strings.Contains(out, "openai") || !strings.Contains(out, "koto openai-login") {
		t.Errorf("missing credential: ok=%v\n%s", ok, out)
	}
	f.install(t, map[string]bool{"openai": true})
	out = captureStdout(t, func() { ok = smokeProviderCheck(sc) })
	if !ok {
		t.Errorf("present credential reported missing:\n%s", out)
	}
}

func TestSetEnvKey(t *testing.T) {
	k := defaultProviderEnv
	for _, tc := range []struct{ name, in, want string }{
		{"empty file", "", k + "=venice\n"},
		{"placeholder filled", "A=1\n#" + k + "=\nB=2\n", "A=1\n" + k + "=venice\nB=2\n"},
		{"active replaced", "A=1\n" + k + "=openai\n", "A=1\n" + k + "=venice\n"},
		{"duplicates collapsed", k + "=openai\nA=1\n" + k + "=claudesdk\n", k + "=venice\nA=1\n"},
		{"appended", "# header\nA=1\n", "# header\nA=1\n" + k + "=venice\n"},
		{"no trailing newline", "A=1", "A=1\n" + k + "=venice\n"},
		{"similar key untouched", "X" + k + "=a\n", "X" + k + "=a\n" + k + "=venice\n"},
	} {
		if got := setEnvKey(tc.in, k, "venice"); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	// Persisting refuses anything off the allowlist.
	setupEnvHarness(t, installedEnv())
	if _, err := setupPersistProvider(testUI(""), "gemini"); err == nil {
		t.Error("persisted an unknown provider")
	}
}

// `koto install` re-renders koto.env from a fixed key list; the default
// provider must be on it or a reinstall silently drops the operator's choice.
func TestInstallPreservesDefaultProvider(t *testing.T) {
	d, o := envFileKeys("/var/lib/koto", "")
	body := renderEnvFile(map[string]string{defaultProviderEnv: "venice"}, d, o)
	if parseEnvFile(body)[defaultProviderEnv] != "venice" {
		t.Fatalf("reinstall dropped %s:\n%s", defaultProviderEnv, body)
	}
	if !strings.Contains(renderEnvFile(nil, d, o), "#"+defaultProviderEnv+"=\n") {
		t.Error("fresh koto.env does not carry the commented placeholder")
	}
}
