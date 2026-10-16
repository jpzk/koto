package main

// Multi-provider onboarding: the provider step's multi-select and default
// question, the credentials step's per-provider loop, and the --providers /
// --provider flags. Same harness as setup_provider_test.go: a temp koto.env,
// a temp state dir, fake logins — nothing touches /etc/koto and no real
// login runs.
//
// KOTO_SETUP_TRANSCRIPT_DIR, when set, receives the rendered provider+auth
// transcripts (<dir>/multi-<scenario>.txt).

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// multiFakes replaces every provider's detect/login: present says which have
// a credential, fail which logins return an error. called records the order
// in which logins ran.
type multiFakes struct {
	present map[string]bool
	fail    map[string]bool
	called  []string
}

func installMultiFakes(t *testing.T, present, fail map[string]bool) *multiFakes {
	t.Helper()
	f := &multiFakes{present: present, fail: fail}
	if f.present == nil {
		f.present = map[string]bool{}
	}
	prev := setupProviderAuth
	t.Cleanup(func() { setupProviderAuth = prev })
	m := map[string]providerAuth{}
	for p, pa := range prev {
		p, pa := p, pa
		pa.detect = func(*setupCtx) (bool, string) {
			if f.present[p] {
				return true, p + " credential present (fake)"
			}
			return false, "no " + p + " credential (fake)"
		}
		pa.login = func(sc *setupCtx) error {
			f.called = append(f.called, p)
			if f.fail[p] {
				return errors.New("(fake) " + p + " login refused: invalid key")
			}
			sc.ui.ok("(fake) %s login completed", p)
			f.present[p] = true
			return nil
		}
		m[p] = pa
	}
	setupProviderAuth = m
	return f
}

// runSteps runs each step's run+verify in order (as setupRun would for
// pending steps), stopping at the first error.
func runSteps(t *testing.T, sc *setupCtx, steps ...setupStep) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		for _, s := range steps {
			sc.ui.printf("%s", sc.ui.bold(s.title))
			if err = s.run(sc); err == nil && s.verify != nil {
				err = s.verify(sc)
			}
			if err != nil {
				sc.ui.fail("step %q: %v", s.id, err)
				return
			}
			sc.ui.ok("%s done", s.id)
			sc.ui.blank()
		}
	})
	return out, err
}

func TestParseMultiChoice(t *testing.T) {
	names := knownProviders
	for _, tc := range []struct {
		in   string
		want []int
		bad  bool
	}{
		{"1,3", []int{0, 2}, false},
		{"3, 1", []int{0, 2}, false},
		{"3 1 3", []int{0, 2}, false},
		{"2", []int{1}, false},
		{"all", []int{0, 1, 2, 3}, false},
		{"ALL", []int{0, 1, 2, 3}, false},
		{"openai,claudesdk", []int{0, 2}, false},
		{"1,,2", []int{0, 1}, false},
		{"", nil, false},
		{"   ", nil, false},
		{"0", nil, true},
		{"5", nil, true},
		{"1,9", nil, true},
		{"gemini", nil, true},
		{"1-4", nil, true},
		{",", nil, true},
	} {
		got, err := parseMultiChoice(tc.in, len(names), names)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: accepted as %v, want an error", tc.in, got)
			} else if !strings.Contains(err.Error(), "1-4") {
				t.Errorf("%q: error %q does not say what is valid", tc.in, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

// menuAfter returns the provider names offered in the numbered menu that
// follows question q in out.
func menuLinesAfter(out, q string) []string {
	i := strings.Index(out, q)
	if i < 0 {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(out[i+len(q):], "\n")[1:] {
		if !menuItemRE.MatchString(l) {
			break
		}
		lines = append(lines, l)
	}
	return lines
}

var menuItemRE = regexp.MustCompile(`^\s+[* ] \d+\) (\S+)`)

func menuAfter(out, q string) []string {
	var names []string
	for _, l := range menuLinesAfter(out, q) {
		names = append(names, menuItemRE.FindStringSubmatch(l)[1])
	}
	return names
}

const qWhich = "Which providers do you want to set up?"
const qDefault = "Which one should NEW groups use by default?"

func TestProviderStepMultiSelect(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       string // KOTO_DEFAULT_PROVIDER before ("" = unset)
		present   map[string]bool
		in        string
		sel       []string
		def       string
		offered   []string // default question's options; nil = must not be asked
		reasks    int
		connected []string // menu lines carrying the connected marker
	}{
		{name: "one picked: no default question", in: "2\n", sel: []string{"venice"}, def: "venice"},
		{name: "1,3 then the second", in: "1,3\n2\n", sel: []string{"claudesdk", "openai"}, def: "openai",
			offered: []string{"claudesdk", "openai"}},
		{name: "all, Enter keeps current default", env: "venice", in: "all\n\n", sel: knownProviders, def: "venice",
			offered: knownProviders},
		{name: "invalid re-asked", in: "9\nfoo\n3\n", sel: []string{"openai"}, def: "openai", reasks: 2},
		{name: "empty keeps current set", env: "openai", present: map[string]bool{"venice": true},
			in: "\n\n", sel: []string{"venice", "openai"}, def: "openai", offered: []string{"venice", "openai"},
			connected: []string{"venice"}},
		{name: "empty on a fresh host is claudesdk", in: "\n", sel: []string{"claudesdk"}, def: "claudesdk"},
		{name: "current default not selected: first selected preselected", env: "venice", in: "1,3\n\n",
			sel: []string{"claudesdk", "openai"}, def: "claudesdk", offered: []string{"claudesdk", "openai"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := installedEnv()
			if tc.env != "" {
				body = setEnvKey(body, defaultProviderEnv, tc.env)
			}
			path := setupEnvHarness(t, body)
			installMultiFakes(t, tc.present, nil)
			sc := newTestSC(tc.in)
			out, err := runStep(t, stepProvider(), sc)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := menuAfter(out, qWhich); !reflect.DeepEqual(got, knownProviders) {
				t.Errorf("multi-select offered %v\n%s", got, out)
			}
			if !reflect.DeepEqual(sc.selected, tc.sel) || sc.provider != tc.def {
				t.Errorf("selected %v default %q; want %v %q\n%s", sc.selected, sc.provider, tc.sel, tc.def, out)
			}
			if got := readEnvKey(t, path); got != tc.def {
				t.Errorf("persisted %q, want %q", got, tc.def)
			}
			if tc.offered == nil {
				if strings.Contains(out, qDefault) {
					t.Errorf("default question asked with one provider selected:\n%s", out)
				}
			} else if got := menuAfter(out, qDefault); !reflect.DeepEqual(got, tc.offered) {
				t.Errorf("default question offered %v, want only the selected %v\n%s", got, tc.offered, out)
			}
			if n := strings.Count(out, "is not one of the choices"); n != tc.reasks {
				t.Errorf("%d invalid-answer messages, want %d\n%s", n, tc.reasks, out)
			}
			for _, l := range menuLinesAfter(out, qWhich) {
				name := menuItemRE.FindStringSubmatch(l)[1]
				want := false
				for _, c := range tc.connected {
					want = want || c == name
				}
				if strings.Contains(l, "✓ connected") != want {
					t.Errorf("connected marker on %q: want %v", l, want)
				}
			}
		})
	}
}

// The credentials step runs each selected provider's login in display
// order, keeps going after one fails, and reports every one of them.
func TestAuthStepLoopsSelectedProviders(t *testing.T) {
	setupEnvHarness(t, setEnvKey(installedEnv(), defaultProviderEnv, "claudesdk"))
	f := installMultiFakes(t, nil, map[string]bool{"venice": true})
	sc := newTestSC("y\ny\nn\n")
	sc.provider = "claudesdk"
	sc.selected = []string{"claudesdk", "venice", "openai"}
	out, err := runStep(t, stepAuth(), sc)
	if err != nil {
		t.Fatalf("a non-default failure failed the step: %v\n%s", err, out)
	}
	if strings.Join(f.called, ",") != "claudesdk,venice" {
		t.Errorf("logins ran %v, want claudesdk,venice (openai skipped)", f.called)
	}
	for _, want := range []string{
		"Connect Anthropic credentials for claudesdk (default) now?",
		"Connect Venice credentials for venice now?",
		"Connect OpenAI credentials for openai now?",
		"claudesdk (default): connected",
		"venice: login failed: (fake) venice login refused",
		"openai: skipped",
		"koto openai-login",
		"credentials summary",
		"new groups default to claudesdk",
		"groups set to one of them will fail",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
	// Prompts in display order.
	a, b, c := strings.Index(out, "for claudesdk"), strings.Index(out, "for venice"), strings.Index(out, "for openai")
	if !(a < b && b < c) {
		t.Errorf("not in display order: %d %d %d", a, b, c)
	}
	sum := out[strings.Index(out, "credentials summary"):]
	for _, re := range []string{`claudesdk\s+connected\s+← default`, `venice\s+failed`, `openai\s+skipped`} {
		if !regexp.MustCompile(re).MatchString(sum) {
			t.Errorf("summary lacks %s:\n%s", re, sum)
		}
	}
	if strings.Contains(out, "DEFAULT PROVIDER") {
		t.Errorf("loud warning with the default connected:\n%s", out)
	}
	if !sc.restartOwed || sc.authSkipped {
		t.Errorf("restartOwed=%v authSkipped=%v", sc.restartOwed, sc.authSkipped)
	}

	// Already connected: no login, no question, said so.
	f = installMultiFakes(t, map[string]bool{"venice": true}, nil)
	sc = newTestSC("y\n")
	sc.provider, sc.selected = "openai", []string{"venice", "openai"}
	out, err = runStep(t, stepAuth(), sc)
	if err != nil || strings.Join(f.called, ",") != "openai" {
		t.Fatalf("err=%v called=%v\n%s", err, f.called, out)
	}
	if !strings.Contains(out, "venice: already connected, login skipped") || strings.Contains(out, "credentials for venice now") {
		t.Errorf("already-connected provider not reported / asked anyway:\n%s", out)
	}
	if !regexp.MustCompile(`venice\s+already connected`).MatchString(out) {
		t.Errorf("summary lacks venice already connected:\n%s", out)
	}
}

// A default that ends without a credential is loud; a failed default login
// fails the step, but only after every other provider was handled.
func TestAuthStepDefaultWithoutCredential(t *testing.T) {
	setupEnvHarness(t, installedEnv())
	installMultiFakes(t, nil, nil)
	sc := newTestSC("y\nn\n")
	sc.provider, sc.selected = "openai", []string{"claudesdk", "openai"}
	out, err := runStep(t, stepAuth(), sc)
	if err != nil {
		t.Fatalf("explicit skip of the default must pass: %v", err)
	}
	if !sc.authSkipped || !strings.Contains(out, "DEFAULT PROVIDER openai HAS NO CREDENTIAL") ||
		!strings.Contains(out, "every NEW group, will FAIL") || !strings.Contains(out, "koto openai-login") {
		t.Errorf("no loud warning for the skipped default:\n%s", out)
	}

	f := installMultiFakes(t, nil, map[string]bool{"claudesdk": true})
	sc = newTestSC("y\ny\ny\n")
	sc.provider, sc.selected = "claudesdk", []string{"claudesdk", "venice", "openai"}
	out, err = runStep(t, stepAuth(), sc)
	if err == nil || !strings.Contains(err.Error(), "FAIL until fixed") || !strings.Contains(err.Error(), "koto claude-login") {
		t.Errorf("failed default login: err = %v\n%s", err, out)
	}
	if strings.Join(f.called, ",") != "claudesdk,venice,openai" {
		t.Errorf("did not continue after the failure: %v", f.called)
	}
	if !regexp.MustCompile(`venice\s+connected`).MatchString(out) || !regexp.MustCompile(`claudesdk\s+failed\s+← default`).MatchString(out) {
		t.Errorf("summary wrong:\n%s", out)
	}
}

func TestParseProviderFlags(t *testing.T) {
	for _, tc := range []struct {
		list, def string
		want      []string
		wantDef   string
		errHas    string
	}{
		{"", "", nil, "", ""},
		{"", "venice", nil, "venice", ""},
		{"claudesdk,openai", "", []string{"claudesdk", "openai"}, "claudesdk", ""},
		{"openai,claudesdk", "", []string{"openai", "claudesdk"}, "openai", ""},
		{"claudesdk,openai", "openai", []string{"claudesdk", "openai"}, "openai", ""},
		{"OpenAI, venice,openai", "VENICE", []string{"openai", "venice"}, "venice", ""},
		{"venice", "", []string{"venice"}, "venice", ""},
		{"claudesdk,openai", "venice", nil, "", "--provider venice is not in --providers claudesdk,openai"},
		{"claudesdk,gemini", "", nil, "", `unknown provider "gemini" in --providers`},
		{",", "", nil, "", "lists no provider"},
		{"venice", "gemini", nil, "", `unknown --provider "gemini"`},
	} {
		list, def, err := parseProviderFlags(tc.list, tc.def)
		if tc.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("%q/%q: err = %v, want %q", tc.list, tc.def, err, tc.errHas)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(list, tc.want) || def != tc.wantDef {
			t.Errorf("%q/%q: got %v %q %v; want %v %q", tc.list, tc.def, list, def, err, tc.want, tc.wantDef)
		}
	}
}

// --providers asks nothing; -y connects nothing but passes the providers
// already connected, and fails only when the DEFAULT has no credential.
func TestProvidersFlagNonInteractive(t *testing.T) {
	for _, tc := range []struct {
		name, list, def string
		present         map[string]bool
		wantDef         string
		wantErr         bool
	}{
		{"default first-listed, connected", "claudesdk,openai", "", map[string]bool{"claudesdk": true}, "claudesdk", false},
		{"explicit default missing", "claudesdk,openai", "openai", map[string]bool{"claudesdk": true}, "openai", true},
		{"all connected", "venice,openai", "openai", map[string]bool{"venice": true, "openai": true}, "openai", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := setupEnvHarness(t, installedEnv())
			f := installMultiFakes(t, tc.present, nil)
			sc := newTestSC("") // any read aborts
			sc.ui.yes = true
			var err error
			sc.providersFlag, sc.providerFlag, err = parseProviderFlags(tc.list, tc.def)
			if err != nil {
				t.Fatal(err)
			}
			sc.providerFromList = tc.def == ""
			out, err := runSteps(t, sc, stepProvider(), stepAuth())
			if strings.Contains(out, "choice [") || strings.Contains(out, "now?") {
				t.Errorf("prompted:\n%s", out)
			}
			if got := readEnvKey(t, path); got != tc.wantDef {
				t.Errorf("persisted %q, want %q", got, tc.wantDef)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v\n%s", err, tc.wantErr, out)
			}
			if len(f.called) != 0 {
				t.Errorf("-y ran logins %v", f.called)
			}
			for _, p := range sc.selected {
				if tc.present[p] && !strings.Contains(out, p+": already connected") && !strings.Contains(out, p+" (default): already connected") {
					t.Errorf("%s connected but not passed:\n%s", p, out)
				}
				if !tc.present[p] && !strings.Contains(out, "cannot be set up non-interactively") {
					t.Errorf("%s missing without the non-interactive message:\n%s", p, out)
				}
			}
		})
	}
}

// Rendered end-to-end transcripts of the provider + auth steps, one per
// scenario the operator will meet. Assertions are light (the tests above
// pin the behaviour); the point is the evidence file.
func TestSetupMultiTranscripts(t *testing.T) {
	tdir := os.Getenv("KOTO_SETUP_TRANSCRIPT_DIR")
	for _, tc := range []struct {
		name, cmd, in string
		present, fail map[string]bool
		list, def     string
		yes, wantErr  bool
	}{
		{name: "single", cmd: "koto setup --from provider", in: "3\ny\n"},
		{name: "two-one-skipped", cmd: "koto setup --from provider", in: "1,3\n1\ny\nn\n"},
		{name: "three-one-failing", cmd: "koto setup --from provider", in: "all\n1\ny\ny\ny\n", fail: map[string]bool{"venice": true}},
		{name: "flags-yes", cmd: "koto setup -y --from provider --providers claudesdk,openai", yes: true,
			list: "claudesdk,openai", present: map[string]bool{"claudesdk": true}},
		{name: "flags-yes-default-missing", cmd: "koto setup -y --from provider --providers claudesdk,openai --provider openai", yes: true,
			list: "claudesdk,openai", def: "openai", present: map[string]bool{"claudesdk": true}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupEnvHarness(t, installedEnv())
			installMultiFakes(t, tc.present, tc.fail)
			sc := newTestSC(tc.in)
			sc.ui.yes = tc.yes
			if tc.list != "" {
				var err error
				if sc.providersFlag, sc.providerFlag, err = parseProviderFlags(tc.list, tc.def); err != nil {
					t.Fatal(err)
				}
				sc.providerFromList = tc.def == ""
			}
			out, err := runSteps(t, sc, stepProvider(), stepAuth())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v\n%s", err, out)
			}
			if tdir != "" {
				in := strings.ReplaceAll(strings.TrimSuffix(tc.in, "\n"), "\n", " / ")
				if in == "" {
					in = "none"
				}
				head := "$ " + tc.cmd + "   (typed: " + in + "; fake logins"
				if len(tc.fail) > 0 {
					head += ", venice login fails"
				}
				if len(tc.present) > 0 {
					head += ", claudesdk already connected"
				}
				_ = os.WriteFile(filepath.Join(tdir, "multi-"+tc.name+".txt"), []byte(head+")\n"+out), 0o644)
			}
		})
	}
	if tdir != "" {
		var b strings.Builder
		for _, a := range []struct{ l, d string }{{"claudesdk,openai", "venice"}, {"claudesdk,gemini", ""}} {
			_, _, err := parseProviderFlags(a.l, a.d)
			b.WriteString("$ koto setup --providers " + a.l)
			if a.d != "" {
				b.WriteString(" --provider " + a.d)
			}
			b.WriteString("\nkoto ctl: " + err.Error() + "\n(exit 2)\n\n")
		}
		_ = os.WriteFile(filepath.Join(tdir, "multi-flags-errors.txt"), []byte(b.String()), 0o644)
	}
}
