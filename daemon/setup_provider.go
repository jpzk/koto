package main

// The setup wizard's provider steps: which LLM provider new groups get by
// default (the "provider" step), and connecting THAT provider's credential
// (the "auth" step, which used to be Anthropic-only).
//
// The choice is persisted as KOTO_DEFAULT_PROVIDER in the daemon's
// EnvironmentFile (/etc/koto/koto.env) — the file setup and install already
// own, and a HOST-only channel: nothing a guest or the ctl plane can write
// reaches it (see defaultProvider in providers.go). It applies to groups
// created after the daemon restarts, and to groups whose config.json has no
// valid provider; a group that already names one keeps it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Seams over koto.env so the wizard's tests run against a temp file instead
// of the host's /etc/koto (which is root-owned and must never be touched by
// a test). Production: sudo read/write, exactly as `koto install` does.
var (
	setupEnvPath  = envFilePath
	setupEnvRead  = func(path string) (string, bool, error) { return sudoReadFile(path) }
	setupEnvWrite = func(u *setupUI, path, body string) (bool, error) {
		if err := sudoRun(u, "install", "-d", "-m", "0755", filepath.Dir(path)); err != nil {
			return false, err
		}
		return sudoWriteIfChanged(u, path, body, "0600")
	}
)

// setupProviderSetting reads KOTO_DEFAULT_PROVIDER from koto.env. raw is the
// value as written ("" when absent or commented out); p is what the daemon
// will use for it — the value when valid, else claudesdk.
func setupProviderSetting() (raw, p string, err error) {
	body, _, err := setupEnvRead(setupEnvPath)
	if err != nil {
		return "", builtinDefaultProvider, err
	}
	raw = strings.TrimSpace(parseEnvFile(body)[defaultProviderEnv])
	if v, ok := normProvider(raw); ok {
		return raw, v, nil
	}
	return raw, builtinDefaultProvider, nil
}

// setEnvKey sets KEY=val in an EnvironmentFile body, preserving every other
// line — comments, ordering, values the operator set. An active assignment is
// replaced in place (later duplicates dropped: systemd honours the last one,
// and two answers in one file is how an edit silently does nothing); else a
// commented-out "#KEY=" placeholder is filled in; else the line is appended.
func setEnvKey(body, key, val string) string {
	if body == "" {
		return key + "=" + val + "\n"
	}
	lines := strings.Split(body, "\n")
	trailingNL := strings.HasSuffix(body, "\n")
	if trailingNL {
		lines = lines[:len(lines)-1]
	}
	assign := key + "=" + val
	active := func(l string) bool {
		k, _, ok := strings.Cut(strings.TrimSpace(l), "=")
		return ok && strings.TrimSpace(k) == key
	}
	placeholder := func(l string) bool {
		t := strings.TrimSpace(l)
		return strings.HasPrefix(t, "#") && active(strings.TrimPrefix(t, "#"))
	}
	out := make([]string, 0, len(lines)+1)
	done := false
	for _, l := range lines {
		if active(l) {
			if !done {
				out = append(out, assign)
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	if !done {
		for i, l := range out {
			if placeholder(l) {
				out[i] = assign
				done = true
				break
			}
		}
	}
	if !done {
		out = append(out, assign)
	}
	return strings.Join(out, "\n") + "\n"
}

// setupPersistProvider writes the choice into koto.env. Reports whether the
// file changed (a restart is owed when it did: the daemon reads its
// environment once, at start).
func setupPersistProvider(u *setupUI, p string) (bool, error) {
	if !validProvider(p) {
		return false, fmt.Errorf("unknown provider %q (want %s)", p, providerListText())
	}
	body, _, err := setupEnvRead(setupEnvPath)
	if err != nil {
		return false, fmt.Errorf("%w\n  refusing to rewrite it: the values set there must be preserved", err)
	}
	next := setEnvKey(body, defaultProviderEnv, p)
	if next == body {
		return false, nil
	}
	return setupEnvWrite(u, setupEnvPath, next)
}

// effectiveProvider is the provider the auth and smoke steps act on: the one
// chosen in this run, else the persisted setting, else claudesdk.
func (sc *setupCtx) effectiveProvider() string {
	if sc.provider != "" {
		return sc.provider
	}
	if sc.providerFlag != "" {
		return sc.providerFlag
	}
	_, p, _ := setupProviderSetting()
	return p
}

func providerMenu() []string {
	out := make([]string, len(knownProviders))
	w := 0
	for _, p := range knownProviders {
		w = max(w, len(p))
	}
	for i, p := range knownProviders {
		out[i] = p + strings.Repeat(" ", w-len(p)) + "  " + providerBlurb[p]
	}
	return out
}

// parseProviderFlags validates --providers (a comma-separated list) and
// --provider (the default). The list keeps the order given, duplicates
// dropped. Without --provider the default is the first one listed; a
// --provider that is not in the list is an error, because the default is
// one of the providers being set up.
func parseProviderFlags(list, def string) ([]string, string, error) {
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		p, ok := normProvider(tok)
		if !ok {
			return nil, "", fmt.Errorf("unknown provider %q in --providers (want %s)", truncateRunes(tok, 20), providerListText())
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if strings.TrimSpace(list) != "" && len(out) == 0 {
		return nil, "", fmt.Errorf("--providers lists no provider (want %s)", providerListText())
	}
	var p string
	if def != "" {
		v, ok := normProvider(def)
		if !ok {
			return nil, "", fmt.Errorf("unknown --provider %q (want %s)", def, providerListText())
		}
		p = v
	}
	switch {
	case out == nil:
	case p == "":
		p = out[0]
	case !seen[p]:
		return nil, "", fmt.Errorf("--provider %s is not in --providers %s: the default must be one of the providers being set up",
			p, strings.Join(out, ","))
	}
	return out, p, nil
}

// inDisplayOrder returns the members of set in knownProviders order.
func inDisplayOrder(set map[string]bool) []string {
	var out []string
	for _, p := range knownProviders {
		if set[p] {
			out = append(out, p)
		}
	}
	return out
}

// connectedProviders reports, per provider, whether a credential is in
// place (local check, no API call).
func connectedProviders(sc *setupCtx) map[string]bool {
	out := map[string]bool{}
	for _, p := range knownProviders {
		if pa, ok := setupProviderAuth[p]; ok {
			if done, _ := pa.detect(sc); done {
				out[p] = true
			}
		}
	}
	return out
}

// selectedProviders is the set the auth step connects, in display order:
// the one chosen in this run's provider step, else --providers, else
// --provider alone (the PR #25 behaviour), else what is derivable from the
// host: the default plus every provider that already has a credential. The
// default is always a member.
func (sc *setupCtx) selectedProviders() []string {
	def := sc.effectiveProvider()
	set := map[string]bool{def: true}
	switch {
	case sc.selected != nil:
		for _, p := range sc.selected {
			set[p] = true
		}
	case sc.providersFlag != nil:
		for _, p := range sc.providersFlag {
			set[p] = true
		}
	case sc.providerFlag != "":
	default:
		for p := range connectedProviders(sc) {
			set[p] = true
		}
	}
	return inDisplayOrder(set)
}

func stepProvider() setupStep {
	return setupStep{
		id:    "provider",
		title: "Providers",
		explain: `Every group talks to one LLM provider. First pick the providers you want to
use (one or several); the next step connects a credential for each. Then,
if you picked more than one, pick the one NEW groups get by default — main
included — unless you choose another when you create the group or change it
later with /config. Groups that already have a provider keep it.

The default is stored as ` + defaultProviderEnv + ` in ` + envFilePath + `, which only
you (the host) can edit; agents cannot change it.`,
		advisory: true,
		detect: func(sc *setupCtx) (bool, string) {
			raw, p, err := setupProviderSetting()
			if err != nil {
				return false, "cannot read " + setupEnvPath + ": " + err.Error()
			}
			if sc.providerFlag != "" && sc.providerFlag != p {
				return false, "--provider " + sc.providerFlag + " (currently " + p + ")"
			}
			if raw == "" {
				return false, "not chosen (new groups default to " + builtinDefaultProvider + ")"
			}
			if _, ok := normProvider(raw); !ok {
				return false, fmt.Sprintf("%s=%q is not a known provider (daemon falls back to %s)",
					defaultProviderEnv, truncateRunes(raw, 20), builtinDefaultProvider)
			}
			sc.provider = p
			return true, "new groups use " + p
		},
		run: func(sc *setupCtx) error {
			u := sc.ui
			_, cur, err := setupProviderSetting()
			if err != nil {
				return err
			}
			var sel []string
			var p string
			switch {
			case sc.providersFlag != nil:
				sel, p = sc.providersFlag, sc.providerFlag
				how := "--provider"
				if sc.providerFromList {
					how = "first in --providers"
				}
				u.info("providers: %s %s", u.bold(strings.Join(inDisplayOrder(setOf(sel)), ", ")), u.dim("(--providers)"))
				u.info("default provider: %s %s", u.bold(p), u.dim("("+how+")"))
			case sc.providerFlag != "":
				p = sc.providerFlag
				sel = []string{p}
				u.info("default provider: %s %s", u.bold(p), u.dim("(--provider)"))
			default:
				sel, p = askProviders(sc, cur)
			}
			changed, err := setupPersistProvider(u, p)
			if err != nil {
				return err
			}
			sc.provider = p
			sc.selected = inDisplayOrder(setOf(sel))
			if changed {
				sc.restartOwed = true
				u.info("%s=%s written to %s", defaultProviderEnv, p, setupEnvPath)
				u.info("applies to groups created after the daemon restarts (the service step does that)")
			}
			return nil
		},
		verify: func(sc *setupCtx) error {
			_, p, err := setupProviderSetting()
			if err != nil {
				return err
			}
			if p != sc.provider {
				return fmt.Errorf("%s reads back as %s, want %s", setupEnvPath, p, sc.provider)
			}
			return nil
		},
	}
}

func setOf(ps []string) map[string]bool {
	m := make(map[string]bool, len(ps))
	for _, p := range ps {
		m[p] = true
	}
	return m
}

// askProviders is the interactive half of the provider step: which
// providers (multi-select; Enter keeps the current set — the default plus
// every provider already connected), then, only when more than one was
// picked, which of THOSE is the default. Under -y both answers are the
// current ones.
func askProviders(sc *setupCtx, cur string) ([]string, string) {
	u := sc.ui
	connected := connectedProviders(sc)
	var def []int
	marks := make([]string, len(knownProviders))
	for i, k := range knownProviders {
		if k == cur || connected[k] {
			def = append(def, i)
		}
		if connected[k] {
			marks[i] = "✓ connected"
		}
	}
	idx := u.multiChoice("Which providers do you want to set up?", providerMenu(), def, knownProviders, marks)
	sel := make([]string, len(idx))
	for k, i := range idx {
		sel[k] = knownProviders[i]
	}
	if len(sel) == 1 {
		if sel[0] != cur {
			u.info("default provider: %s %s", u.bold(sel[0]), u.dim("(the only one selected)"))
		}
		return sel, sel[0]
	}
	d := 0
	for i, k := range sel {
		if k == cur {
			d = i
		}
	}
	opts := make([]string, len(sel))
	for i, k := range sel {
		opts[i] = k
	}
	return sel, sel[u.choice("Which one should NEW groups use by default?", opts, d)]
}

// ---- per-provider credentials ---------------------------------------------

// providerAuth is what the auth step needs to know about one provider: how
// to tell (locally, no API call) whether a credential is in place, how to
// connect one, and what to tell an operator who skips it.
type providerAuth struct {
	label  string // "Anthropic", "Venice", "OpenAI"
	detect func(sc *setupCtx) (bool, string)
	login  func(sc *setupCtx) error
	// remedy is how to connect it later, outside the wizard.
	remedy string
	// batch is how to provide it with no terminal (the -y path).
	batch string
}

// setupProviderAuth is the dispatch table, a var so tests can substitute
// fakes for the real logins (a browser, a device code, a typed secret).
var setupProviderAuth = map[string]providerAuth{
	providerClaude: {
		label:  "Anthropic",
		detect: claudeCredDetect,
		login:  claudeCredLogin,
		remedy: "koto claude-login",
		batch:  "koto claude-login --api-key-stdin < keyfile",
	},
	providerVenice: {
		label:  "Venice",
		detect: veniceCredDetect,
		login:  veniceCredLogin,
		remedy: "koto venice-login",
		batch:  "koto venice-login --key-file keyfile",
	},
	providerOpenAI: {
		label:  "OpenAI",
		detect: openaiCredDetect,
		login:  openaiCredLogin,
		remedy: "koto openai-login",
		batch:  "koto openai-login --api-key-stdin < keyfile",
	},
	providerMinimax: {
		label:  "MiniMax",
		detect: minimaxCredDetect,
		login:  minimaxCredLogin,
		remedy: "koto minimax-login",
		batch:  "koto minimax-login --key-file keyfile",
	},
}

func nonEmptyFile(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) != ""
}

func claudeCredDetect(sc *setupCtx) (bool, string) {
	if exists(filepath.Join(sc.credsDir(), ".credentials.json")) {
		return true, "OAuth credentials present"
	}
	if exists(filepath.Join(sc.credsDir(), "anthropic-api-key")) {
		return true, "API key stored"
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return true, "ANTHROPIC_API_KEY set in the environment"
	}
	return false, "no Anthropic credentials"
}

func claudeCredLogin(sc *setupCtx) error {
	// The flow itself lives in claude_login.go, shared with the `koto
	// claude-login` subcommand — the wizard is one caller of it, not a second
	// implementation.
	return authConnect(&authCtx{ui: sc.ui, state: sc.stateDir()}, "", false)
}

func veniceCredDetect(sc *setupCtx) (bool, string) {
	if nonEmptyFile(filepath.Join(sc.credsDir(), "venice.key")) {
		return true, "Venice API key stored"
	}
	return false, "no Venice API key (creds/venice.key)"
}

func veniceCredLogin(sc *setupCtx) error { return keyCredLogin(sc, providerVenice) }

func openaiCredDetect(sc *setupCtx) (bool, string) {
	if exists(filepath.Join(sc.credsDir(), "openai-auth.json")) {
		return true, "ChatGPT subscription (OAuth) present"
	}
	if nonEmptyFile(filepath.Join(sc.credsDir(), "openai-api-key")) {
		return true, "OpenAI API key stored"
	}
	return false, "no OpenAI credential"
}

// openaiCredLogin runs `koto openai-login`'s flows in-process (not by
// re-executing this binary): device code for a ChatGPT subscription, or a
// platform API key.
func openaiCredLogin(sc *setupCtx) error {
	ac := &authCtx{ui: sc.ui, state: sc.stateDir()}
	// openai_auth.go resolves paths from HERE, as the daemon does; point it
	// at the state dir this wizard configures (as openai-login does).
	HERE = sc.stateDir()
	if err := os.MkdirAll(ac.credsDir(), 0o700); err != nil {
		return fmt.Errorf("creds dir: %w", err)
	}
	if err := authTrustedCredsDir(ac); err != nil {
		return err
	}
	var err error
	if sc.ui.choice("How do you want to connect OpenAI?", []string{
		"ChatGPT subscription (device code: a URL and a one-time code)",
		"OpenAI platform API key (billed per token)",
	}, 0) == 1 {
		err = openaiStoreKey(ac, false)
	} else {
		ctx := sc.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		err = openaiStoreDevice(ctx, ac)
	}
	if err != nil {
		return err
	}
	openaiReport(sc.ui)
	return nil
}

// authOutcome is what the credentials step did for one provider.
type authOutcome string

const (
	authConnected authOutcome = "connected"
	authAlready   authOutcome = "already connected"
	authSkippedP  authOutcome = "skipped"
	authFailed    authOutcome = "failed"
	authNoTTY     authOutcome = "not connected (-y)"
)

func stepAuth() setupStep {
	return setupStep{
		id:    "auth",
		title: "Provider credentials",
		explain: `Agents need to reach their model. koto never hands credentials to an agent:
the daemon runs a proxy that injects them per request, so a compromised
agent can talk to the model but cannot read the key.

This connects a credential for each provider selected above, one at a time
(each can be skipped):
  claudesdk  a Claude subscription (browser OAuth) or an Anthropic API key
  venice     a Venice API key
  openai     a ChatGPT subscription (device code) or an OpenAI API key
  minimax    a MiniMax M Plan subscription key or API key
Check each provider's terms for which fits your use — the README has the
details. A provider already connected is not logged in again.

The result lands in the installed system's creds directory, which is where
the daemon already looks. Nothing is written to your personal ~/.claude.`,
		detect: func(sc *setupCtx) (bool, string) {
			def := sc.effectiveProvider()
			sel := sc.selectedProviders()
			// The default first: it is the one that decides.
			order := append([]string{def}, without(sel, def)...)
			var parts []string
			done := true
			for _, p := range order {
				ok, detail := setupProviderAuth[p].detect(sc)
				done = done && ok
				parts = append(parts, p+": "+detail)
			}
			return done, strings.Join(parts, "; ")
		},
		run: func(sc *setupCtx) error {
			u := sc.ui
			def := sc.effectiveProvider()
			sel := sc.selectedProviders()
			res := map[string]authOutcome{}
			var defErr error
			for _, p := range sel {
				pa := setupProviderAuth[p]
				tag := p
				if p == def {
					tag += " (default)"
				}
				if ok, detail := pa.detect(sc); ok {
					res[p] = authAlready
					u.ok("%s: already connected, login skipped %s", tag, u.dim("("+detail+")"))
					continue
				}
				// Credentials need a real person: a browser, a device code
				// or a typed secret. Under -y there is nobody to ask, so say
				// so instead of handing a tty that isn't there to a login
				// and hanging.
				if u.yes {
					res[p] = authNoTTY
					u.fail("%s: %s credentials cannot be set up non-interactively", tag, pa.label)
					u.hint("run `" + pa.remedy + "` from a terminal, or provide one first:\n  " + pa.batch)
					continue
				}
				if !u.yesno(fmt.Sprintf("Connect %s credentials for %s now?", pa.label, tag), true) {
					res[p] = authSkippedP
					u.warn("%s: skipped", tag)
					u.hint("connect it later with:\n  " + pa.remedy)
					continue
				}
				if err := pa.login(sc); err != nil {
					res[p] = authFailed
					u.fail("%s: login failed: %v", tag, err)
					u.hint("retry later with:\n  " + pa.remedy)
					if p == def {
						defErr = err
					}
					continue
				}
				// The wizard is already going to start (or restart) the
				// service in the next step, so a restart owed is its
				// business, not the login's.
				sc.restartOwed = true
				if ok, detail := pa.detect(sc); !ok {
					res[p] = authFailed
					u.fail("%s: login finished but no credential is in place (%s)", tag, detail)
					if p == def {
						defErr = errors.New(detail)
					}
					continue
				}
				res[p] = authConnected
				u.ok("%s: connected", tag)
			}

			u.blank()
			u.printf("%s", u.bold("credentials summary"))
			for _, p := range sel {
				tag := ""
				if p == def {
					tag = "  " + u.bold("← default for new groups")
				}
				switch res[p] {
				case authConnected, authAlready:
					u.ok("%-10s %s%s", p, res[p], tag)
				case authSkippedP:
					u.warn("%-10s %s%s", p, res[p], tag)
				default:
					u.fail("%-10s %s%s", p, res[p], tag)
				}
			}
			u.info("new groups default to %s", u.bold(def))
			var soft []string
			for _, p := range sel {
				if p != def && res[p] != authConnected && res[p] != authAlready {
					soft = append(soft, p)
				}
			}
			if len(soft) > 0 {
				u.warn("no credential yet for %s: groups set to %s will fail until it is connected",
					strings.Join(soft, ", "), map[bool]string{true: "it", false: "one of them"}[len(soft) == 1])
			}
			pa := setupProviderAuth[def]
			switch res[def] {
			case authConnected, authAlready:
				return nil
			case authSkippedP:
				sc.authSkipped = true
				u.warn("%s", u.red(fmt.Sprintf("DEFAULT PROVIDER %s HAS NO CREDENTIAL: every turn of a %s group, and of every NEW group, will FAIL until it is fixed", def, def)))
				u.hint("connect it with:\n  " + pa.remedy)
				return nil
			case authNoTTY:
				return fmt.Errorf("default provider %s: %s credentials are required and cannot be set up non-interactively\n"+
					"  every turn of new groups will FAIL until fixed; run `%s` from a terminal, or provide one first:\n"+
					"    %s", def, pa.label, pa.remedy, pa.batch)
			default:
				return fmt.Errorf("default provider %s: login failed (%v); every turn of new groups will FAIL until fixed\n"+
					"  retry with `%s`", def, defErr, pa.remedy)
			}
		},
		verify: func(sc *setupCtx) error {
			if sc.authSkipped {
				return nil
			}
			// Only the default decides; other providers are advisory (the
			// run already reported them).
			p := sc.effectiveProvider()
			if ok, detail := setupProviderAuth[p].detect(sc); !ok {
				return fmt.Errorf("still not satisfied after running: %s", detail)
			}
			return nil
		},
	}
}

func without(ps []string, x string) []string {
	var out []string
	for _, p := range ps {
		if p != x {
			out = append(out, p)
		}
	}
	return out
}

// smokeProviderCheck reports the default provider's credential after the
// daemon answers. Local only — like the claude check before it, the smoke
// test spends no LLM request.
func smokeProviderCheck(sc *setupCtx) bool {
	u := sc.ui
	p := sc.effectiveProvider()
	pa := setupProviderAuth[p]
	if ok, detail := pa.detect(sc); ok {
		u.ok("default provider %s: %s", p, detail)
		return true
	}
	u.warn("default provider %s has no credential — its turns will fail", p)
	u.hint("connect it with:\n  " + pa.remedy)
	return false
}

func minimaxCredDetect(sc *setupCtx) (bool, string) {
	if nonEmptyFile(filepath.Join(sc.credsDir(), "minimax.key")) {
		return true, "MiniMax key stored"
	}
	return false, "no MiniMax key (creds/minimax.key)"
}

func minimaxCredLogin(sc *setupCtx) error { return keyCredLogin(sc, providerMinimax) }

// keyCredLogin is the wizard side of `koto venice-login` / `koto
// minimax-login`: same prompt, same key check before anything is written.
func keyCredLogin(sc *setupCtx, provider string) error {
	ac := &authCtx{ui: sc.ui, state: sc.stateDir()}
	if err := os.MkdirAll(ac.credsDir(), 0o700); err != nil {
		return fmt.Errorf("creds dir: %w", err)
	}
	if err := authTrustedCredsDir(ac); err != nil {
		return err
	}
	key, err := providerPromptKey(ac, provider)
	if err != nil {
		return err
	}
	return providerStoreKey(sc.ctx, ac, provider, key, true)
}
