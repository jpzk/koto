package main

// `koto claude-login` — connect or refresh the Anthropic credentials the
// proxy injects, without going through the whole setup wizard.
//
// The name says "login" and means it for the subscription path, which is
// literally a handoff to `claude auth login`. It also stores an API key
// (--method api-key), and its --status mode reports and verifies whichever
// credential is currently live — both are "connecting the thing that
// authenticates", so they sit here rather than in a second verb. The
// provider is in the name, so there is no provider argument: koto's other
// backend, Venice, is a bare key file the proxy reads (veniceAuth) with
// nothing to log into, and if it ever grows a flow it gets its own verb.
//
// This exists for one moment in particular: a group's turns start coming back
// as `[[err]] … 401 authentication failed` and the fleet goes quiet. The fix
// is a fresh login, and `koto setup --only auth` was the only way to reach it
// — a wizard step that ALSO restarts the daemon, which stops every running
// microVM. Re-authenticating should not cost you your VMs.
//
// It does not, and that is not luck: the proxy resolves both credential
// sources per REQUEST (currentAPIKey re-reads the key file, readCreds
// re-reads the OAuth token), so material written here is picked up by the
// next turn with nothing restarted. The one exception is ANTHROPIC_API_KEY in
// the daemon's own environment, captured once at startup into envAPIKey —
// that one needs a restart, and authReport says so rather than leaving the
// operator to wonder why a fresh login changed nothing.
//
// The other half of the job is the trap the proxy's 401 comment names: an API
// key takes precedence over OAuth in authHeaders, so a stale key file makes a
// successful `claude auth login` look like it did nothing. This command sees
// the whole precedence chain and offers to clear what shadows the credential
// you just connected.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// authEnvFile is the daemon's EnvironmentFile — a var, not the const, only so
// tests can point it at a fixture instead of depending on whether the machine
// running them happens to have koto installed.
var authEnvFile = envFilePath

// authUpstream is the API base the probe calls, same seam and same reason:
// the tests that pin the probe's request shape need somewhere to receive it.
var authUpstream = upstream

func claudeLoginUsage() {
	fmt.Fprintln(os.Stderr, `usage: koto claude-login [flags]

Connects the Anthropic credentials the daemon's proxy injects into every
agent turn. Run it when turns start failing with 401: it logs in fresh,
tells you which credential is actually being sent, and checks it against
the API.

Two ways in, both landing where the daemon reads: a Claude subscription
(a browser login, the default) or an API key from console.anthropic.com.

The daemon re-reads credentials per request, so this does NOT restart the
service and does NOT stop your running microVMs.

The check sends one real /v1/messages request capped at a single output
token — the same call a group makes, so the answer is not a guess. It costs
a fraction of a cent, or one trivial turn against a subscription.

flags:
  --status       report which credential would be sent and check it, change
                 nothing else; exit 0 when the credential works
  --method M     skip the menu: "oauth" (Claude subscription) or "api-key"
  --api-key-stdin  read the API key from stdin (implies --method api-key)
  --no-verify    skip the upstream check (report from local state only)
  --no-color     plain output (also honors NO_COLOR)`)
}

// claudeLoginFatal is ctlFatal's counterpart for this subcommand — same shape, but
// prefixed with the command the operator actually typed.
func claudeLoginFatal(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "koto claude-login: "+format+"\n", a...)
	os.Exit(code)
}

func claudeLoginMain(args []string) {
	fs := flag.NewFlagSet("claude-login", flag.ExitOnError)
	fs.Usage = claudeLoginUsage
	status := fs.Bool("status", false, "report only")
	method := fs.String("method", "", "oauth | api-key")
	keyStdin := fs.Bool("api-key-stdin", false, "read the API key from stdin")
	noVerify := fs.Bool("no-verify", false, "skip the upstream check")
	noColor := fs.Bool("no-color", false, "disable color")
	_ = fs.Parse(args)

	// No positional arguments: the provider is in the verb. Catching a
	// stray one is worth a line, because `koto claude-login anthropic` is
	// the obvious thing to type and silently ignoring it would look like it
	// selected something.
	if fs.NArg() > 0 {
		claudeLoginFatal(2, "unexpected argument %q — this command takes flags only", fs.Arg(0))
	}
	if *keyStdin {
		*method = "api-key"
	}
	if *method != "" && *method != "oauth" && *method != "api-key" {
		claudeLoginFatal(2, "unknown --method %q (oauth | api-key)", *method)
	}

	state, why := authStateDir()
	ac := &authCtx{ui: newSetupUI(false, *noColor), state: state}
	if *status {
		os.Exit(authReport(ac, !*noVerify))
	}
	// Printed up front, not just in the closing report: if this is the wrong
	// system, the operator should see it before handing a browser their
	// login, not after being told it succeeded.
	ac.ui.info("configuring %s (%s)", state, why)
	if err := authConnect(ac, *method, *keyStdin); err != nil {
		if errors.Is(err, errSetupAborted) {
			claudeLoginFatal(1, "aborted")
		}
		claudeLoginFatal(1, "%v", err)
	}
	ac.ui.blank()
	os.Exit(authReport(ac, !*noVerify))
}

type authCtx struct {
	ui    *setupUI
	state string
}

func (ac *authCtx) credsDir() string { return filepath.Join(ac.state, "creds") }

// authStateDir resolves the state dir this command configures, and returns
// why, because getting it wrong IS the failure mode: a login written to a
// directory the daemon does not read looks like it worked and fixes nothing.
//
// The installed daemon wins over a dev clone, which is the opposite of what
// ctlCredsDir does for the client PKI — and the reversal is the point. A
// clone directory is just a checkout that happens to contain a creds/; the
// installed unit is a daemon that is actually running and actually serving
// the 401. Preferring the clone meant `koto claude-login` run from ~/koto
// silently reconfigured the checkout while the service kept failing. Set
// KOTO_HOME to override, which is how you target a dev daemon deliberately.
func authStateDir() (dir, why string) {
	if h := os.Getenv("KOTO_HOME"); h != "" {
		if abs, err := filepath.Abs(h); err == nil {
			h = abs
		}
		return h, "KOTO_HOME"
	}
	inst, instOK := authInstalledStateDir()
	clone := ""
	if fi, err := os.Stat("creds"); err == nil && fi.IsDir() {
		clone = here()
	}
	switch {
	case instOK && clone != "" && clone != inst:
		// Both exist and disagree. Say so out loud rather than picking in
		// silence — this is the exact shape of the bug above.
		return inst, "the installed daemon (a clone with creds/ is also here: " +
			clone + " — set KOTO_HOME to target it instead)"
	case instOK:
		return inst, "the installed daemon"
	case clone != "":
		return clone, "this clone"
	}
	return defaultStateDir, "the default location"
}

// authInstalledStateDir reports the state dir the INSTALLED daemon actually
// uses — its unit's KOTO_HOME, not the compiled-in default, so an operator
// who relocated the state dir is not sent to the wrong one.
func authInstalledStateDir() (string, bool) {
	if !installed() {
		return "", false
	}
	if v, _ := authDaemonEnv("KOTO_HOME"); v != "" {
		return v, true
	}
	return defaultStateDir, true
}

// --- credential inspection -------------------------------------------------

// authCred is what the proxy would send, resolved the same way authHeaders
// resolves it. Mirroring that precedence is the point: an API key anywhere
// beats a valid OAuth token, and reporting anything else would send the
// operator to fix the credential that is not being used.
type authCred struct {
	kind    string // "API key" | "OAuth token" | ""
	source  string // where it came from, for the report
	headers map[string]string
	detail  string // freshness or shape, one line
	// shadowed names a credential present but outranked, so a fresh login
	// into it would change nothing until the shadow is cleared.
	shadowed string
	// restart is set when the live credential lives in the daemon's startup
	// environment, which no amount of writing to disk can displace.
	restart bool
	// expired marks an OAuth token past its expiry. NOT the same as broken:
	// authHeaders refreshes such a token via `claude` before every request,
	// so a probe 401 here may only mean "not refreshed yet". Reporting it as
	// a dead credential would send people to re-login who do not need to.
	expired bool
}

// authEnvKey reports an ANTHROPIC_API_KEY that would reach the DAEMON — the
// installed daemon's is /etc/koto/koto.env, a dev daemon's is the shell that
// launched it, which is usually this one. Both are checked because the wrong
// answer here is the one that wastes an afternoon.
func authEnvKey() (key, source string) { return authDaemonEnv("ANTHROPIC_API_KEY") }

// authRunningDaemonEnv is a var so tests can neutralize it: a real koto
// daemon on the machine running the tests must not leak its environment into
// fixtures.
var authRunningDaemonEnv = authRunningDaemonEnvReal

// authDaemonEnvCache memoizes the /proc read — authDaemonEnv is called
// several times per run and the answer cannot change mid-command.
var (
	authDaemonEnvOnce  sync.Once
	authDaemonEnvVars  map[string]string
	authDaemonEnvWhere string
)

// authRunningDaemonEnvReal reads the RUNNING daemon's own environment from
// /proc/<pid>/environ. This is ground truth — better than /etc/koto/koto.env
// in two ways: it reflects what the process actually got (unit Environment=,
// EnvironmentFile, and anything the operator exported for a dev run alike),
// and it is readable by the user who owns the process, whereas koto.env is
// root-owned 0600 and silently unreadable to the operator running this
// command. That silence was a real blind spot: an ANTHROPIC_API_KEY sitting
// in koto.env outranks every credential on disk, and a report that cannot
// see it would never say so.
func authRunningDaemonEnvReal() (map[string]string, string) {
	out, err := exec.Command("systemctl", "show", "koto", "-p", "MainPID", "--value").Output()
	if err != nil {
		return nil, ""
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" || pid == "0" {
		return nil, ""
	}
	b, err := os.ReadFile("/proc/" + pid + "/environ")
	if err != nil {
		return nil, ""
	}
	vars := map[string]string{}
	for _, kv := range strings.Split(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	return vars, "the running daemon (pid " + pid + ")"
}

// authEnvFileHidden reports a koto.env that exists but this user cannot read
// — so the report can say "I cannot see whether a key is hiding here"
// instead of implying there is none.
func authEnvFileHidden() bool {
	if _, err := os.Stat(authEnvFile); err != nil {
		return false
	}
	f, err := os.Open(authEnvFile)
	if err != nil {
		return true
	}
	_ = f.Close()
	return false
}

// authDaemonEnv reads one variable as the DAEMON sees it. Order matters: the
// running process first (ground truth), then its EnvironmentFile, then the
// shell that launched this command — which is the right answer for a dev
// daemon started from the same shell.
func authDaemonEnv(name string) (val, source string) {
	authDaemonEnvOnce.Do(func() {
		authDaemonEnvVars, authDaemonEnvWhere = authRunningDaemonEnv()
	})
	if v, ok := authDaemonEnvVars[name]; ok {
		if v = strings.TrimSpace(v); v != "" {
			return v, authDaemonEnvWhere
		}
	}
	if b, err := os.ReadFile(authEnvFile); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == name {
				if v = strings.TrimSpace(v); v != "" {
					return v, authEnvFile
				}
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, name + " in this environment"
	}
	return "", ""
}

func authKeyPath(ac *authCtx) string { return filepath.Join(ac.credsDir(), "anthropic-api-key") }

// authOAuthPath resolves the OAuth credentials file the DAEMON reads, by the
// same rules proxyInitPaths uses: CRED_PATH when set, else
// $HOME/.claude/.credentials.json — and the daemon's HOME is the state dir.
//
// It used to hardcode <state>/creds/.credentials.json, on the assumption that
// <state>/.claude is always the installer's symlink to creds/. That is a real
// bug wherever it is not: a dev clone whose repo has its own project-local
// .claude/ directory (Claude Code puts skills and settings there) keeps that
// directory, `claude auth login` writes the token INTO it, and the creds/
// path is then a different — usually stale — file. The login succeeded, this
// command verified the stale file and called it good, and the daemon went on
// sending the old token. Resolving the path the same way the proxy does is
// the only version of this that cannot drift.
func authOAuthPath(ac *authCtx) string {
	if v, _ := authDaemonEnv("CRED_PATH"); v != "" {
		return v
	}
	return filepath.Join(ac.state, ".claude", ".credentials.json")
}

// authClaudeDirCheck decides whether <state>/.claude is a place koto is willing
// to have a credential written (audit M139).
//
// `claude auth login` writes to $HOME/.claude/.credentials.json, and HOME here
// is the state dir — so whatever that path resolves to IS where the OAuth
// bearer token lands. The login used to accept anything already sitting there,
// for a real reason (a dev clone routinely has its own project-local .claude/
// directory holding skills and settings, and replacing that would destroy the
// operator's files) — but "leave it alone" silently included a SYMLINK, which
// is not the operator's files, it is a redirection of the write. Anyone able to
// create that entry before the operator logs in — the daemon, which shares the
// operator's uid and has ReadWritePaths over the state dir, is the interesting
// one — chooses where the token is written and where it can be read from.
//
// Three outcomes, and only the first two continue:
//
//   - absent: koto creates the symlink to creds/ itself, which is the state the
//     installer leaves behind and the one this whole arrangement assumes;
//   - a real directory, or a symlink that resolves to <state>/creds: accepted,
//     which covers both the dev clone and every installed host;
//   - anything else — a symlink somewhere else, a regular file, a socket:
//     refused, naming what it found. The operator can remove it; koto must not
//     quietly write a bearer token through it.
func authClaudeDirCheck(ac *authCtx) error {
	link := filepath.Join(ac.state, ".claude")
	fi, err := os.Lstat(link)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("%s: %w", link, err)
		}
		if err := os.Symlink("creds", link); err != nil {
			return fmt.Errorf("link .claude -> creds: %w", err)
		}
		return nil
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		if fi.IsDir() {
			return nil // a clone's own .claude/ — the operator's, and untouched
		}
		return fmt.Errorf("%s is a %s, not a directory — refusing to log in through it "+
			"(remove it, or point it at %s)", link, fi.Mode().Type(), ac.credsDir())
	}
	// A symlink: judge it by where it RESOLVES, not by the text of the link,
	// so "creds", an absolute path and a chain of links are all decided the
	// same way. A dangling link resolves to nothing and is refused with it.
	got, gerr := filepath.EvalSymlinks(link)
	want, werr := filepath.EvalSymlinks(ac.credsDir())
	if gerr != nil || werr != nil || got != want {
		target, _ := os.Readlink(link)
		return fmt.Errorf("%s is a symlink to %q, not to %s — refusing to log in through it, "+
			"because that is where the OAuth token would be written and read from "+
			"(remove the link and re-run; koto will recreate it)",
			link, target, ac.credsDir())
	}
	return nil
}

// authClaudeDirCheckReadOnly is authClaudeDirCheck's judgement without its one
// side effect, for --status: an absent link is not a problem to report (the
// login creates it), it is just not yet there.
func authClaudeDirCheckReadOnly(ac *authCtx) error {
	link := filepath.Join(ac.state, ".claude")
	if _, err := os.Lstat(link); err != nil {
		return nil
	}
	return authClaudeDirCheck(ac)
}

// authResolve walks the proxy's precedence and returns what it would send.
func authResolve(ac *authCtx) authCred {
	fileKey := ""
	if b, err := os.ReadFile(authKeyPath(ac)); err == nil {
		fileKey = strings.TrimSpace(string(b))
	}
	envKey, envSrc := authEnvKey()
	oauth, oauthErr := authReadOAuth(ac)

	switch {
	case envKey != "":
		c := authCred{
			kind: "API key", source: envSrc, detail: authRedact(envKey),
			headers: map[string]string{"x-api-key": envKey, "anthropic-version": "2023-06-01"},
			restart: true,
		}
		c.shadowed = authShadowed(fileKey != "", oauthErr == nil)
		return c
	case fileKey != "":
		c := authCred{
			kind: "API key", source: authKeyPath(ac), detail: authRedact(fileKey),
			headers: map[string]string{"x-api-key": fileKey, "anthropic-version": "2023-06-01"},
		}
		c.shadowed = authShadowed(false, oauthErr == nil)
		return c
	case oauthErr == nil:
		ttl := credTTL(oauth)
		detail := fmt.Sprintf("expires in %s", authDuration(ttl))
		if ttl <= 0 {
			detail = "EXPIRED " + authDuration(-ttl) + " ago"
		}
		return authCred{
			kind: "OAuth token", source: authOAuthPath(ac), detail: detail,
			expired: ttl <= 0,
			headers: map[string]string{
				"authorization":     "Bearer " + oauth.ClaudeAiOauth.AccessToken,
				"anthropic-beta":    "oauth-2025-04-20",
				"anthropic-version": "2023-06-01",
			},
		}
	}
	return authCred{}
}

func authShadowed(fileKey, oauth bool) string {
	var s []string
	if fileKey {
		s = append(s, "an API key file")
	}
	if oauth {
		s = append(s, "an OAuth token")
	}
	return strings.Join(s, " and ")
}

func authReadOAuth(ac *authCtx) (credsFile, error) {
	var c credsFile
	b, err := os.ReadFile(authOAuthPath(ac))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	// A file with no accessToken is treated as no credential at all. That
	// diverges from readCreds, which would hand authHeaders an empty Bearer
	// — but "no credential, run `koto claude-login`" is both the accurate
	// description and the same remediation, where "OAuth token" plus an
	// unexplained 401 is neither.
	if c.ClaudeAiOauth.AccessToken == "" {
		return c, errors.New("no accessToken in credentials file")
	}
	return c, nil
}

// authRedact shows enough of a secret to tell two keys apart and not enough
// to use one. Short strings are reported by length only.
func authRedact(s string) string {
	if len(s) < 16 {
		return fmt.Sprintf("%d characters", len(s))
	}
	return s[:11] + "…" + s[len(s)-4:]
}

func authDuration(sec float64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// --- report ----------------------------------------------------------------

// authReport prints the resolved credential and, when probe is set, checks it
// against the API. Exit code is the doctor's verdict: 0 only when a
// credential exists and (if probed) upstream accepted it.
func authReport(ac *authCtx, probe bool) int {
	u := ac.ui
	u.printf("%s", u.bold("anthropic credentials"))
	u.info("state dir %s", ac.state)
	u.info("credentials file %s", authOAuthPath(ac))
	// The doctor reports what the login would refuse, rather than refusing:
	// --status mutates nothing, and an operator whose login is about to fail
	// should hear why here (audit M139).
	if err := authClaudeDirCheckReadOnly(ac); err != nil {
		u.warn("%v", err)
	}
	c := authResolve(ac)
	if c.kind == "" {
		u.fail("no credential — the proxy has nothing to inject")
		u.hint("run `koto claude-login` to connect one")
		return 1
	}
	u.ok("%s from %s (%s)", c.kind, c.source, c.detail)
	if c.shadowed != "" {
		u.warn("this outranks %s, which the proxy will not use", c.shadowed)
	}
	if c.restart {
		u.warn("this key comes from the daemon's environment, read once at startup")
		u.hint("writing a new credential will not displace it — clear it from " + c.source + "\n" +
			"and `sudo systemctl restart koto` (that stops running microVMs)")
	}
	// A koto.env this user cannot read may hold an ANTHROPIC_API_KEY that
	// outranks everything above. Only worth saying when the running daemon's
	// environment was not available — that read already answered the question.
	if authDaemonEnvWhere == "" && authEnvFileHidden() {
		u.warn("cannot read %s (root-owned) and the daemon is not running", authEnvFile)
		// `grep` prints the matching LINE, i.e. the key (audit 2026-09-11
		// L97). The question here is only whether the variable is SET, and
		// answering it by putting a live credential into the operator's
		// scrollback — and from there into session recordings, tmux buffers
		// and pasted diagnostics — defeats the 0600 the file is carrying.
		// `grep -c` answers the same question with a count.
		u.hint("an ANTHROPIC_API_KEY there would outrank everything above and is invisible from here\n" +
			"check it with: sudo grep -c '^ANTHROPIC_API_KEY=' " + authEnvFile + "   (a count, not the key)")
	}
	// Only for OAuth: an API key is never refreshed, so the claude CLI is
	// irrelevant to it. "installed" = this report targets the state dir the
	// systemd unit serves, which is the only daemon whose ProtectHome applies.
	refreshBroken := false
	if c.kind == "OAuth token" {
		inst, ok := authInstalledStateDir()
		refreshBroken = !authRefreshReport(u, ok && inst == ac.state)
	}
	if !probe {
		if refreshBroken {
			return 1
		}
		return 0
	}
	pr := authProbe(c.headers)
	status, body := pr.status, pr.msg
	switch {
	case status == 200:
		u.ok("api.anthropic.com accepted it")
		return 0
	case status == 429 && pr.rateLimited:
		// A real quota answer, and one the credential had to be accepted to
		// receive. Not a failure — the daemon's own retry path rides this
		// out — so it is reported as what it is.
		u.warn("api.anthropic.com rate-limited the check (429) — the credential works; the quota is what is short")
		if body != "" && body != "Error" {
			u.info("%s", u.dim(body))
		}
		return 0
	case status == 429:
		// 429 with no anthropic-ratelimit-* header is not a quota answer at
		// all: it is the gate on subscription tokens used outside Claude
		// Code, and it means this REQUEST was refused, not that the
		// credential is bad. Since the probe now sends the same system block
		// a group's turn does (claudeCodeSystem), reaching here means that
		// shape stopped being enough — which the operator can do nothing
		// about, and which must not be reported as either pass or fail.
		u.warn("api.anthropic.com answered 429 with no rate-limit headers — the check's request shape was refused")
		u.info("this says nothing about the credential: the gate is on the request, not the token")
		u.hint("the daemon's own turns may well work — try one, and re-run with --no-verify to skip this check")
		return 0
	case (status == 401 || status == 403) && c.expired:
		// The proxy would have refreshed this token before sending it, and
		// this probe does not — so a rejection here is expected and says
		// nothing on its own. What settles it is whether turns are still
		// failing after the daemon has tried.
		u.warn("api.anthropic.com rejected the expired token (%d %s) — expected, since this "+
			"check does not refresh", status, http.StatusText(status))
		if refreshBroken {
			u.info("and the daemon CANNOT refresh it (see above) — that is why turns 401; a fresh")
			u.info("login only buys one token lifetime (~8h) until the refresh path is fixed")
		} else {
			u.info("the daemon refreshes it via `claude` before every request; if turns are")
			u.info("still 401ing, that refresh is failing and a fresh login is the fix")
		}
		u.hint("run `koto claude-login` if the 401s continue")
		return 1
	case status == 401 || status == 403:
		u.fail("api.anthropic.com rejected it (%d %s)", status, http.StatusText(status))
		if body != "" {
			u.info("%s", u.dim(body))
		}
		u.hint("run `koto claude-login` to connect a fresh credential")
		return 1
	case status == 0:
		u.warn("could not reach api.anthropic.com: %s", body)
		u.info("the credential looks well-formed; this says nothing about whether it works")
		return 0
	default:
		u.warn("api.anthropic.com answered %d — not an auth failure, so the credential is accepted", status)
		if body != "" {
			u.info("%s", u.dim(body))
		}
		return 0
	}
}

// authRefreshReport checks the OAuth refresh path from the DAEMON's point of
// view and reports it; false means the daemon cannot run claude, so the token
// will expire (~8h) and every turn will 401 until someone logs in by hand.
//
// This is the check preflight never made. `koto setup --check` looks for
// claude on the OPERATOR's PATH, where it is fine; the daemon has systemd's
// PATH and ProtectHome, and on a native-installer host (~/.local/bin/claude)
// it could not exec the binary at all. refresh() used to discard that error,
// so the only symptom was a 401 storm eight hours after every login.
func authRefreshReport(u *setupUI, installed bool) bool {
	bin, src := authDaemonEnv(claudeBinEnv)
	how := ""
	if bin != "" {
		how = claudeBinEnv + " from " + src
	} else {
		path, psrc := authDaemonEnv("PATH")
		if psrc == "" {
			path, psrc = os.Getenv("PATH"), "this shell's PATH (no daemon environment to read)"
		}
		for _, d := range strings.Split(path, ":") {
			if d == "" {
				continue
			}
			p := filepath.Join(d, "claude")
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				bin, how = p, "PATH lookup in "+psrc
				break
			}
		}
		if bin == "" {
			u.fail("token refresh: no `claude` on the daemon's PATH (%s) and %s is unset", path, claudeBinEnv)
			u.hint("the daemon execs claude to rotate the token, so without it the token expires every\n" +
				"~8h and every turn 401s. Install claude (`npm i -g @anthropic-ai/claude-code`) and\n" +
				"re-run `koto install`, which records its path as " + claudeBinEnv)
			return false
		}
	}
	if _, err := os.Stat(bin); err != nil {
		u.fail("token refresh: %s (%s) does not exist", bin, how)
		u.hint("re-run `koto install` to re-resolve it, or fix " + claudeBinEnv + " in " + authEnvFile)
		return false
	}
	if installed && protectHomeHides(bin) {
		switch ok, why := authUnitShows(unitPath, bin); {
		case ok:
		case why == "":
			u.fail("token refresh: %s is under a home directory the unit's ProtectHome hides", bin)
			u.hint("re-run `koto install` — it switches the unit to ProtectHome=tmpfs and binds the\n" +
				"claude directories read-only, so the daemon can exec it")
			return false
		default:
			u.fail("token refresh: %s", why)
			u.hint("re-run `koto install` to restore the read-only bind, or fix the unit by hand")
			return false
		}
	}
	u.ok("token refresh via %s (%s)", bin, how)
	return true
}

// authUnitShows reads the installed unit and says whether bin, which lives
// under a ProtectHome'd directory, is bound through READ-ONLY. An unreadable or
// absent unit answers true: the report accuses only on evidence. The second
// return value is a specific complaint when there is one; "" means the plain
// "not bound at all" case, which the caller words itself. The unit PATH is a
// parameter rather than the package constant so this is testable against a
// rendered unit without an installed system.
//
// The two bind directives are tracked SEPARATELY (audit 2026-09-11 L68). They
// used to be appended to one list, so a writable `BindPaths=` covering the
// claude directory satisfied a check whose entire subject is that the daemon
// can execute the binary WITHOUT being able to modify it — tier 2 handed write
// access into the operator's home, reported as correctly hardened. A writable
// bind is worse than a missing one, so it is named rather than waved through.
func authUnitShows(unit, bin string) (bool, string) {
	b, err := os.ReadFile(unit)
	if err != nil {
		return true, ""
	}
	protect := ""
	var ro, rw []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ProtectHome="); ok {
			protect = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "BindReadOnlyPaths="); ok {
			ro = append(ro, unitBindSources(v)...)
		}
		if v, ok := strings.CutPrefix(line, "BindPaths="); ok {
			rw = append(rw, unitBindSources(v)...)
		}
	}
	if protect == "" || protect == "no" || protect == "read-only" {
		return true, ""
	}
	covers := func(list []string, need string) bool {
		for _, have := range list {
			if need == have || strings.HasPrefix(need, have+"/") {
				return true
			}
		}
		return false
	}
	for _, need := range claudeBindDirs(bin, protectHomeHides) {
		if covers(rw, need) {
			return false, fmt.Sprintf("%s is bound into the unit WRITABLE (BindPaths=), not read-only — "+
				"the daemon can modify the claude installation in your home", need)
		}
		if !covers(ro, need) {
			return false, ""
		}
	}
	return true, ""
}

// unitBindSources splits one bind directive's value into the SOURCE paths it
// names. systemd accepts a space-separated list whose entries are
// `[-]source[:destination[:options]]`, so neither the optional leading `-` nor
// anything after the first colon is part of the path being exposed.
func unitBindSources(v string) []string {
	var out []string
	for _, f := range strings.Fields(v) {
		f = strings.TrimPrefix(f, "-")
		if i := strings.IndexByte(f, ':'); i >= 0 {
			f = f[:i]
		}
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// claudeCodeSystem is the first system block every turn koto proxies carries,
// because every turn is `claude` running in a guest. The probe sends it too,
// and that is not cosmetic: a SUBSCRIPTION OAuth token is only honored for
// Claude Code traffic, and Anthropic identifies that traffic by this line.
// Without it a perfectly good token comes back
//
//	429 {"type":"rate_limit_error","message":"Error"}
//
// with no anthropic-ratelimit-* headers at all — a gate wearing a rate
// limit's clothes. Measured 2026-09-04 on a freshly minted token with 2% of
// its 5h window used: bare request 429, same request plus this line 200. The
// discriminator is the system block alone — user-agent, x-app and the
// claude-code beta header make no difference either way (all four
// combinations tested).
const claudeCodeSystem = "You are Claude Code, Anthropic's official CLI for Claude."

// authProbeResult is what one probe established. rateLimited distinguishes
// the two things a 429 can mean, since they are opposites here: with
// anthropic-ratelimit-* headers it is a real quota answer and the credential
// is good; without them the request SHAPE was refused and the credential was
// never judged at all.
type authProbeResult struct {
	status      int
	msg         string
	rateLimited bool
}

// authProbe validates a credential by making the request a group actually
// makes: POST /v1/messages, same endpoint, same headers, same default model,
// same leading system block, capped at one output token so it costs
// essentially nothing. Deliberately not a lighter endpoint like GET
// /v1/models — the two credential shapes (x-api-key and an OAuth bearer +
// its beta header) are not guaranteed to be accepted identically everywhere,
// and a probe that says "rejected" about a credential the proxy would have
// used successfully is worse than no probe. "The same call a group makes"
// has to be literally true or the probe answers a question nobody asked;
// claudeCodeSystem is the part that was missing.
//
// Returns status 0 with a reason when the request never reached Anthropic, so
// a broken network is never reported as a bad credential. Only 401/403 is
// read as an auth failure; every other status means the credential got past
// auth and something else answered, which is all this needs to establish.
func authProbe(headers map[string]string) authProbeResult {
	body, err := json.Marshal(map[string]any{
		"model":      defaultClaudeModel,
		"max_tokens": 1,
		"system":     []map[string]string{{"type": "text", "text": claudeCodeSystem}},
		"messages":   []map[string]string{{"role": "user", "content": "ok"}},
	})
	if err != nil {
		return authProbeResult{msg: err.Error()}
	}
	req, err := http.NewRequest("POST", authUpstream+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return authProbeResult{msg: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return authProbeResult{msg: err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return authProbeResult{status: resp.StatusCode, msg: authErrText(b), rateLimited: authHasRateLimitHeaders(resp.Header)}
}

// authHasRateLimitHeaders reports whether Anthropic answered as the rate
// limiter — the tell that separates a real 429 from the shape gate.
func authHasRateLimitHeaders(h http.Header) bool {
	if h.Get("retry-after") != "" {
		return true
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-ratelimit-") {
			return true
		}
	}
	return false
}

// authErrText pulls the human half out of an Anthropic error body, falling
// back to a trimmed snippet when the shape is unfamiliar.
func authErrText(b []byte) string {
	var e struct {
		Error struct{ Message string } `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// --- connecting ------------------------------------------------------------

// authTrustedCredsDir requires <state>/creds to be a real, privately-owned
// directory — the same rule seedStateDir applies to the state dir at install
// time (stateDirTrusted), applied where the credentials are actually written.
func authTrustedCredsDir(ac *authCtx) error {
	dir := ac.credsDir()
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("creds dir %s: %w", dir, err)
	}
	me, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve the current user: %w", err)
	}
	if err := stateDirTrusted(fi, me); err != nil {
		return fmt.Errorf("%s: %v — refusing to write credentials there "+
			"(remove it, or point -state somewhere you own)", dir, err)
	}
	return nil
}

// authConnect is the shared credential flow: `koto claude-login` runs it
// directly and the setup wizard's auth step calls it too, so there is one
// implementation of "how a credential gets connected" rather than two that
// drift. method may be "" (ask), "oauth" or "api-key".
func authConnect(ac *authCtx, method string, keyStdin bool) error {
	u := ac.ui
	if err := os.MkdirAll(ac.credsDir(), 0o700); err != nil {
		return fmt.Errorf("creds dir: %w", err)
	}
	// ...and then check what is actually there (audit M147). MkdirAll is happy
	// with an existing SYMLINK in the path, os.Stat follows one, and
	// os.WriteFile follows a pre-existing destination link — so every
	// credential this flow writes (an OAuth refresh token, an API key, and via
	// the wizard a CA private key and a bearer token) could be planted into a
	// directory of somebody else's choosing by anyone who can write the state
	// tree before the operator runs this. `koto install` already refuses that
	// shape for the state dir; the check was simply never applied to the
	// standalone credential paths, which are the ones that write the secrets.
	//
	// Checking the creds CHILD is what matters and is enough: a symlink there
	// is caught as a symlink, and swapping in a directory of one's own is
	// caught by the owner test — both regardless of what the parent allows.
	// The state dir itself is deliberately not held to the mode rule, because
	// it is the operator's chosen location (a 0755 checkout on a shared dev
	// box is ordinary) and it holds no secrets outside this child.
	if err := authTrustedCredsDir(ac); err != nil {
		return err
	}
	if method == "" {
		if !isTTY(os.Stdin) {
			return errors.New("no terminal to prompt on — pass --method oauth, or --api-key-stdin and pipe the key")
		}
		switch u.choice("How do you want to authenticate?",
			[]string{"Claude subscription (OAuth login in a browser)", "Anthropic API key"}, 0) {
		case 0:
			method = "oauth"
		default:
			method = "api-key"
		}
	}
	if method == "oauth" {
		return authOAuthLogin(ac)
	}
	return authStoreKey(ac, keyStdin)
}

// authOAuthLogin hands the terminal to `claude auth login` with HOME pointed
// at the STATE dir, not creds/ — claude writes $HOME/.claude, and the state
// dir has a .claude -> creds symlink for exactly this. Pointing HOME at
// creds/ would bury the token in creds/.claude/ where nothing looks; pointing
// it at the operator's home would put koto's token in their personal profile.
func authOAuthLogin(ac *authCtx) error {
	u := ac.ui
	// Resolve ONCE and keep the path. LookPath-then-exec.Command("claude")
	// does the lookup twice, and the second one happens when the command is
	// built — so the binary that was checked and the binary that runs need not
	// be the same file (audit M126). PATH being attacker-writable is tier 1
	// and not koto's boundary (M5, M8), but a check followed by a separate
	// name-based execution is a defect on its own terms: it makes the check
	// mean less than it reads as, and this child inherits the operator's
	// terminal for an interactive credential flow.
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("`claude` not found on PATH — install it with `npm i -g @anthropic-ai/claude-code`")
	}
	// <state>/.claude is where `claude` will write, so it is where the OAuth
	// token lands. Establish that boundary rather than inheriting whatever is
	// there — see authClaudeDirCheck (audit M139).
	if err := authClaudeDirCheck(ac); err != nil {
		return err
	}
	// Record what the credentials file looked like going in, so "did this
	// login actually write anything" is answerable afterwards. A stale file
	// that already parses is the failure mode that fooled the old check.
	before := authOAuthStamp(ac)
	u.info("handing over to `claude auth login` — follow its prompts")
	u.blank()
	// The login writes a credential, so koto decides WHERE — not whatever the
	// operator's shell happens to export (audit M165). HOME was overridden and
	// everything else inherited, which leaves the destination decided by any
	// variable the CLI honours ahead of it: CLAUDE_CONFIG_DIR and
	// XDG_CONFIG_HOME are config-root overrides, and an operator who has one
	// set for their personal use would have had `koto claude-login` write the
	// koto token into their personal config — the precise arrangement the trust
	// model rules out ("creds/ is dedicated, not ~/.claude … compromise can
	// only steal the koto token, not your personal claude session").
	//
	// CRED_PATH goes too. It is koto's own variable rather than the CLI's, so
	// it does not steer the write — but it steers where authOAuthPath then
	// LOOKS, and an inherited one differing from the daemon's is exactly the
	// "verified a different file than was written" trap this command exists to
	// close. authDaemonEnv reads the DAEMON's value, which is the one that
	// matters, and the check below reports a disagreement rather than
	// discovering it after the token is on disk.
	env := setEnv(os.Environ(), "HOME", ac.state)
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "CRED_PATH"} {
		env = unsetEnv(env, k)
	}
	if want := filepath.Join(ac.state, ".claude", ".credentials.json"); authOAuthPath(ac) != want {
		u.warn("the daemon reads its OAuth token from %s, but this login writes %s",
			authOAuthPath(ac), want)
		u.hint("that is a CRED_PATH in the daemon's environment; the login will succeed and the daemon will not see it")
	}
	cmd := exec.Command(claudeBin, "auth", "login")
	cmd.Dir = ac.state
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The child owns the tty and handles its own ^C; a signal that killed us
	// mid-login would leave a half-written credentials file behind.
	signal.Ignore(os.Interrupt)
	// `claude auth login` draws a bubbletea UI, so it puts the terminal into
	// raw mode. If it exits abnormally it does not put it back, and the
	// damage outlives this process: every prompt afterwards — including the
	// first prompt of the NEXT `koto claude-login` — echoes ^M and never
	// returns a line, because Enter arrives as \r with ICRNL cleared. Restore
	// what we handed over.
	restoreTTY := ttyGuard()
	err = cmd.Run()
	restoreTTY()
	signal.Reset(os.Interrupt)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if _, err := authReadOAuth(ac); err != nil {
		return fmt.Errorf("login reported success but %s has no usable token: %w", authOAuthPath(ac), err)
	}
	// Parsing is not enough: the file may be one `claude` never touched.
	// Unchanged mtime+size after a successful login means the token was
	// written somewhere this command is not looking, and reporting success
	// on the strength of a stale file is precisely the bug that let a fresh
	// login sit unused while every turn kept 401ing.
	if after := authOAuthStamp(ac); after == before {
		return fmt.Errorf("login succeeded but %s was not written\n"+
			"  the token went somewhere else — check $HOME/.claude in %s,\n"+
			"  and that CRED_PATH (if set) names the file the daemon reads",
			authOAuthPath(ac), ac.state)
	}
	// The trap this command exists to close: authHeaders short-circuits on a
	// non-empty API key, so a leftover key file means every request keeps
	// using it and the login just performed is dead weight.
	return authClearShadowingKey(ac)
}

// authOAuthStamp identifies a version of the credentials file without reading
// its secret: mtime and size are enough to tell "rewritten" from "untouched".
// A missing file stamps as "", which no written file can collide with.
func authOAuthStamp(ac *authCtx) string {
	fi, err := os.Stat(authOAuthPath(ac))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
}

// authClearShadowingKey offers to set aside an API key that would outrank the
// OAuth token just minted. Declining is fine — the report will keep saying
// which credential is live — but it is asked because the alternative is a
// successful login that changes nothing.
//
// It RENAMES rather than deletes. The file is a secret the operator may hold
// nowhere else, and this prompt defaults to yes; losing a key to a reflexive
// Enter is a much worse outcome than an extra file in creds/. authResolve
// reads only the exact name, so the moved-aside key stops shadowing either
// way, and restoring it is a `mv`.
func authClearShadowingKey(ac *authCtx) error {
	u := ac.ui
	p := authKeyPath(ac)
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
		u.blank()
		u.warn("%s still holds an API key, and an API key outranks OAuth", p)
		if u.yesno("Set it aside so the new login is what gets used?", true) {
			aside := p + ".disabled"
			if err := os.Rename(p, aside); err != nil {
				return fmt.Errorf("move %s aside: %w", p, err)
			}
			u.ok("moved to %s — `mv` it back to re-enable the key", aside)
		}
	}
	if _, src := authEnvKey(); src != "" {
		u.blank()
		u.warn("%s also carries an ANTHROPIC_API_KEY, which outranks everything on disk", src)
		u.hint("clear it there and `sudo systemctl restart koto`, or the new login stays unused")
	}
	return nil
}

// authStoreKey writes an API key into the state dir at 0600. It is read per
// request by currentAPIKey, so the next turn picks it up with no restart.
func authStoreKey(ac *authCtx, fromStdin bool) error {
	u := ac.ui
	var key string
	if fromStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
		if err != nil {
			return fmt.Errorf("read key from stdin: %w", err)
		}
		key = strings.TrimSpace(string(b))
	} else {
		s, err := u.secret("Anthropic API key (sk-ant-…)")
		if err != nil {
			return err
		}
		key = strings.TrimSpace(s)
	}
	if key == "" {
		return errors.New("no key given")
	}
	if !strings.HasPrefix(key, "sk-ant-") {
		u.warn("that doesn't look like an Anthropic key, storing it anyway")
	}
	p := authKeyPath(ac)
	// pkiWriteFile, not os.WriteFile: O_NOFOLLOW, so a link planted at
	// creds/anthropic-api-key cannot redirect the key (audit M146/M147).
	if err := pkiWriteFile(p, []byte(key), 0o600); err != nil {
		return err
	}
	u.ok("stored in %s (0600); the daemon reads it from there per request", p)
	return nil
}

// unsetEnv removes a variable from an environment slice. The mirror of setEnv
// (userns.go), for the variables a child must NOT inherit.
func unsetEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, e := range env {
		if k, _, ok := strings.Cut(e, "="); !ok || k != key {
			out = append(out, e)
		}
	}
	return out
}
