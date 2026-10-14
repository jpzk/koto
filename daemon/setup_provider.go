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

func stepProvider() setupStep {
	return setupStep{
		id:    "provider",
		title: "Default provider",
		explain: `Every group talks to one LLM provider. This picks the one NEW groups get
— main included — unless you choose another when you create the group or
change it later with /config. Groups that already have a provider keep it.

The choice is stored as ` + defaultProviderEnv + ` in ` + envFilePath + `, which only
you (the host) can edit; agents cannot change it. The next step connects
that provider's credential.`,
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
			var p string
			switch {
			case sc.providerFlag != "":
				p = sc.providerFlag
				u.info("default provider: %s %s", u.bold(p), u.dim("(--provider)"))
			default:
				def := 0
				for i, k := range knownProviders {
					if k == cur {
						def = i
					}
				}
				p = knownProviders[u.choice("Which provider should new groups use?", providerMenu(), def)]
			}
			changed, err := setupPersistProvider(u, p)
			if err != nil {
				return err
			}
			sc.provider = p
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
		remedy: "koto setup --only auth   (or write the key to <state>/creds/venice.key, 0600)",
		batch:  "install -m 0600 keyfile <state>/creds/venice.key",
	},
	providerOpenAI: {
		label:  "OpenAI",
		detect: openaiCredDetect,
		login:  openaiCredLogin,
		remedy: "koto openai-login",
		batch:  "koto openai-login --api-key-stdin < keyfile",
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

func veniceCredLogin(sc *setupCtx) error {
	ac := &authCtx{ui: sc.ui, state: sc.stateDir()}
	if err := os.MkdirAll(ac.credsDir(), 0o700); err != nil {
		return fmt.Errorf("creds dir: %w", err)
	}
	if err := authTrustedCredsDir(ac); err != nil {
		return err
	}
	sc.ui.info("create a key at https://venice.ai/settings/api")
	s, err := sc.ui.secret("Venice API key")
	if err != nil {
		return err
	}
	key := strings.TrimSpace(s)
	if key == "" {
		return errors.New("no key given")
	}
	path := filepath.Join(ac.credsDir(), "venice.key")
	if err := pkiWriteFile(path, []byte(key), 0o600); err != nil {
		return err
	}
	sc.ui.ok("stored in %s (0600)", path)
	return nil
}

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

func stepAuth() setupStep {
	return setupStep{
		id:    "auth",
		title: "Provider credentials",
		explain: `Agents need to reach their model. koto never hands credentials to an agent:
the daemon runs a proxy that injects them per request, so a compromised
agent can talk to the model but cannot read the key.

This connects the credential for the default provider chosen above:
  claudesdk  a Claude subscription (browser OAuth) or an Anthropic API key
  venice     a Venice API key
  openai     a ChatGPT subscription (device code) or an OpenAI API key
Check each provider's terms for which fits your use — the README has the
details. Groups on another provider need that provider's credential too.

The result lands in the installed system's creds directory, which is where
the daemon already looks. Nothing is written to your personal ~/.claude.`,
		detect: func(sc *setupCtx) (bool, string) {
			p := sc.effectiveProvider()
			ok, detail := setupProviderAuth[p].detect(sc)
			return ok, p + ": " + detail
		},
		run: func(sc *setupCtx) error {
			u := sc.ui
			p := sc.effectiveProvider()
			pa := setupProviderAuth[p]
			// Credentials need a real person: a browser, a device code or a
			// typed secret. Under -y there is nobody to ask, so say so
			// instead of handing a tty that isn't there to a login and
			// hanging.
			if u.yes {
				return fmt.Errorf("%s credentials are required and cannot be set up non-interactively\n"+
					"  run `%s` from a terminal, or provide one first:\n"+
					"    %s", pa.label, pa.remedy, pa.batch)
			}
			if !u.yesno(fmt.Sprintf("Connect %s credentials for %s now?", pa.label, p), true) {
				sc.authSkipped = true
				u.warn("skipped: every turn of a %s group will FAIL until a credential exists", p)
				u.hint("connect it later with:\n  " + pa.remedy)
				return nil
			}
			if err := pa.login(sc); err != nil {
				return err
			}
			// The wizard is already going to start (or restart) the service
			// in the next step, so a restart owed is its business, not the
			// login's.
			sc.restartOwed = true
			return nil
		},
		verify: func(sc *setupCtx) error {
			if sc.authSkipped {
				return nil
			}
			p := sc.effectiveProvider()
			if ok, detail := setupProviderAuth[p].detect(sc); !ok {
				return fmt.Errorf("still not satisfied after running: %s", detail)
			}
			return nil
		},
	}
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
