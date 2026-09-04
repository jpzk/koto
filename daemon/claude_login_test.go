package main

// The property worth pinning here is that authResolve agrees with
// authHeaders about which credential is live. They are two readers of the
// same precedence, and the whole command is worthless if they disagree —
// reporting the credential the proxy is NOT sending is exactly the failure
// that made a fresh login look like a no-op.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// authTestCtx builds an authCtx over a temp state dir with the env-file seam
// pointed at a path that does not exist, so the host's own install cannot
// leak into the result.
func authTestCtx(t *testing.T) *authCtx {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := authEnvFile
	authEnvFile = filepath.Join(dir, "no-such-koto.env")
	t.Cleanup(func() { authEnvFile = old })
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CRED_PATH", "")
	t.Setenv("KOTO_HOME", "")
	// A real koto daemon on this machine must not leak its environment into
	// the fixtures — the whole point of these tests is a controlled one.
	oldEnvFn := authRunningDaemonEnv
	authRunningDaemonEnv = func() (map[string]string, string) { return nil, "" }
	authDaemonEnvOnce = sync.Once{}
	t.Cleanup(func() {
		authRunningDaemonEnv = oldEnvFn
		authDaemonEnvOnce = sync.Once{}
	})
	return &authCtx{ui: newSetupUI(false, true), state: dir}
}

func writeOAuth(t *testing.T, ac *authCtx, token string, ttl time.Duration) {
	t.Helper()
	c := credsFile{}
	c.ClaudeAiOauth.AccessToken = token
	c.ClaudeAiOauth.ExpiresAt = float64(time.Now().Add(ttl).UnixMilli())
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := authOAuthPath(ac)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAuthResolveNothing(t *testing.T) {
	ac := authTestCtx(t)
	if c := authResolve(ac); c.kind != "" {
		t.Fatalf("empty creds dir resolved to %q from %s", c.kind, c.source)
	}
}

func TestAuthResolveOAuth(t *testing.T) {
	ac := authTestCtx(t)
	writeOAuth(t, ac, "tok-abc", 8*time.Hour)
	c := authResolve(ac)
	if c.kind != "OAuth token" {
		t.Fatalf("kind = %q, want OAuth token", c.kind)
	}
	if got := c.headers["authorization"]; got != "Bearer tok-abc" {
		t.Fatalf("authorization = %q", got)
	}
	// The beta header is not decoration: without it the OAuth token is
	// rejected, so a probe would report a working credential as broken.
	if c.headers["anthropic-beta"] != "oauth-2025-04-20" {
		t.Fatalf("missing oauth beta header: %v", c.headers)
	}
	if c.shadowed != "" || c.restart {
		t.Fatalf("unexpected shadowed=%q restart=%v", c.shadowed, c.restart)
	}
}

func TestAuthResolveExpiredOAuthStillReported(t *testing.T) {
	ac := authTestCtx(t)
	writeOAuth(t, ac, "tok-old", -2*time.Hour)
	c := authResolve(ac)
	if c.kind != "OAuth token" {
		t.Fatalf("kind = %q", c.kind)
	}
	// An expired token is still the credential being sent — the proxy tries
	// to refresh it. Reporting "no credential" would send the operator
	// looking for a missing file that is right there.
	if want := "EXPIRED"; len(c.detail) < len(want) || c.detail[:len(want)] != want {
		t.Fatalf("detail = %q, want it to lead with EXPIRED", c.detail)
	}
}

// The trap the command exists to close: a key file outranks OAuth, so a
// successful `claude auth login` changes nothing until the file is gone.
func TestAuthResolveKeyFileShadowsOAuth(t *testing.T) {
	ac := authTestCtx(t)
	writeOAuth(t, ac, "tok-abc", 8*time.Hour)
	if err := os.WriteFile(authKeyPath(ac), []byte("sk-ant-api03-deadbeefcafe1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := authResolve(ac)
	if c.kind != "API key" || c.source != authKeyPath(ac) {
		t.Fatalf("kind=%q source=%q, want the key file to win", c.kind, c.source)
	}
	if c.headers["x-api-key"] != "sk-ant-api03-deadbeefcafe1234" {
		t.Fatalf("key not trimmed: %q", c.headers["x-api-key"])
	}
	if c.shadowed != "an OAuth token" {
		t.Fatalf("shadowed = %q, want the OAuth token named", c.shadowed)
	}
	if c.restart {
		t.Fatal("a key file needs no restart — the proxy re-reads it per request")
	}
}

// An env key outranks both, and unlike the two on disk it cannot be displaced
// by writing a file: the proxy captures it once at startup.
func TestAuthResolveEnvKeyWinsAndNeedsRestart(t *testing.T) {
	ac := authTestCtx(t)
	writeOAuth(t, ac, "tok-abc", 8*time.Hour)
	if err := os.WriteFile(authKeyPath(ac), []byte("sk-ant-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-fromtheenvironment")
	c := authResolve(ac)
	if c.headers["x-api-key"] != "sk-ant-api03-fromtheenvironment" {
		t.Fatalf("x-api-key = %q, want the environment's", c.headers["x-api-key"])
	}
	if !c.restart {
		t.Fatal("an env key must be reported as needing a restart")
	}
	if c.shadowed != "an API key file and an OAuth token" {
		t.Fatalf("shadowed = %q", c.shadowed)
	}
}

// The daemon's EnvironmentFile is what an INSTALLED daemon reads; this
// process's own environment says nothing about it.
func TestAuthEnvKeyFromEnvFile(t *testing.T) {
	ac := authTestCtx(t)
	p := filepath.Join(ac.state, "koto.env")
	body := "# comment\nKOTO_PORT=8443\n#ANTHROPIC_API_KEY=\nANTHROPIC_API_KEY= sk-ant-unit \n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	authEnvFile = p
	key, src := authEnvKey()
	if key != "sk-ant-unit" || src != p {
		t.Fatalf("authEnvKey() = %q, %q", key, src)
	}
}

func TestAuthEnvKeyIgnoresCommentedOut(t *testing.T) {
	ac := authTestCtx(t)
	p := filepath.Join(ac.state, "koto.env")
	if err := os.WriteFile(p, []byte("#ANTHROPIC_API_KEY=\nANTHROPIC_API_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authEnvFile = p
	if key, src := authEnvKey(); key != "" || src != "" {
		t.Fatalf("commented/empty key resolved to %q from %q", key, src)
	}
}

// authResolve must agree with the proxy's own authHeaders — the report is
// only useful if it names the credential that actually goes on the wire.
func TestAuthResolveMatchesAuthHeaders(t *testing.T) {
	ac := authTestCtx(t)
	writeOAuth(t, ac, "tok-live", 8*time.Hour)

	// Point the proxy's globals at the same fixture.
	oldCred, oldKeyPath, oldEnv := credPath, apiKeyPath, envAPIKey
	t.Cleanup(func() { credPath, apiKeyPath, envAPIKey = oldCred, oldKeyPath, oldEnv })
	credPath = authOAuthPath(ac)
	apiKeyPath = authKeyPath(ac)
	envAPIKey = ""

	for _, tc := range []struct{ name, key string }{
		{"oauth", ""},
		{"key file", "sk-ant-api03-onthefile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.key == "" {
				_ = os.Remove(authKeyPath(ac))
			} else if err := os.WriteFile(authKeyPath(ac), []byte(tc.key), 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := authHeaders()
			if err != nil {
				t.Fatalf("authHeaders: %v", err)
			}
			got := authResolve(ac).headers
			if len(got) != len(want) {
				t.Fatalf("header sets differ:\n got %v\nwant %v", got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("header %q = %q, authHeaders says %q", k, got[k], v)
				}
			}
		})
	}
}

func TestAuthStateDirPrefersKotoHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KOTO_HOME", dir)
	got, why := authStateDir()
	if got != dir || why != "KOTO_HOME" {
		t.Fatalf("authStateDir() = %q, %q; want %q, KOTO_HOME", got, why, dir)
	}
}

func TestAuthStateDirDevClone(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOTO_HOME", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// here() resolves symlinks the way the daemon's own path setup does not,
	// so compare against the cwd as the process sees it.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Only meaningful on a host with no install; where koto IS installed the
	// installed daemon wins by design and this case cannot arise.
	if installed() {
		t.Skip("koto is installed on this host — the installed daemon wins by design")
	}
	got, why := authStateDir()
	if got != cwd || why != "this clone" {
		t.Fatalf("authStateDir() = %q, %q; want the clone %q", got, why, cwd)
	}
}

func TestAuthRedactKeepsSecrecy(t *testing.T) {
	key := "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123"
	got := authRedact(key)
	if got == key {
		t.Fatal("authRedact returned the key verbatim")
	}
	if len(got) > 20 {
		t.Fatalf("authRedact leaks too much: %q", got)
	}
	// Different keys must still be distinguishable, or the report cannot
	// answer "is this the key I think it is".
	if authRedact(key) == authRedact("sk-ant-api03-zyxwvutsrqponmlkjihgfedcba9876") {
		t.Fatal("two different keys redact identically")
	}
}

func TestAuthErrTextUnwrapsAnthropicShape(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	if got := authErrText(body); got != "invalid x-api-key" {
		t.Fatalf("authErrText = %q", got)
	}
	if got := authErrText([]byte("not json at all")); got != "not json at all" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestAuthDuration(t *testing.T) {
	for _, tc := range []struct {
		sec  float64
		want string
	}{
		{90, "1m"},
		{3 * 3600, "3h0m"},
		{50 * 3600, "2d2h"},
	} {
		if got := authDuration(tc.sec); got != tc.want {
			t.Fatalf("authDuration(%v) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}

// An expired OAuth token is flagged so the report does not call it dead: the
// proxy refreshes such a token before every request, and a probe that skips
// the refresh has no standing to declare it broken.
func TestAuthResolveMarksExpiredOAuth(t *testing.T) {
	ac := authTestCtx(t)
	writeOAuth(t, ac, "tok-old", -2*time.Hour)
	if c := authResolve(ac); !c.expired {
		t.Fatal("an expired token must be marked expired")
	}
	writeOAuth(t, ac, "tok-new", 8*time.Hour)
	if c := authResolve(ac); c.expired {
		t.Fatal("a live token must not be marked expired")
	}
}

// A credentials file with no accessToken is no credential — the proxy would
// send an empty Bearer, and "no credential" is both truer and actionable.
func TestAuthResolveEmptyOAuthFileIsNoCredential(t *testing.T) {
	ac := authTestCtx(t)
	if err := os.MkdirAll(filepath.Dir(authOAuthPath(ac)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authOAuthPath(ac), []byte(`{"claudeAiOauth":{"expiresAt":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := authResolve(ac); c.kind != "" {
		t.Fatalf("kind = %q, want no credential", c.kind)
	}
}

// Setting a shadowing key aside must not destroy it: the file is a secret the
// operator may hold nowhere else, and the prompt defaults to yes.
func TestAuthClearShadowingKeyPreservesTheSecret(t *testing.T) {
	ac := authTestCtx(t)
	p := authKeyPath(ac)
	if err := os.WriteFile(p, []byte("sk-ant-api03-irreplaceable"), 0o600); err != nil {
		t.Fatal(err)
	}
	// yesno with no tty falls through to its default, which is yes — the
	// same path a reflexive Enter takes.
	if err := authClearShadowingKey(ac); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the key still shadows: it was not moved aside")
	}
	b, err := os.ReadFile(p + ".disabled")
	if err != nil {
		t.Fatalf("the key was destroyed, not set aside: %v", err)
	}
	if string(b) != "sk-ant-api03-irreplaceable" {
		t.Fatalf("set-aside key = %q", b)
	}
	if c := authResolve(ac); c.kind != "" {
		t.Fatalf("a .disabled key still resolves as %q", c.kind)
	}
}

// --- regression: the two bugs that let a successful login keep 401ing ------

// Bug 1. `claude auth login` writes $HOME/.claude/.credentials.json. When
// <state>/.claude is a real project-local directory rather than the
// installer's symlink to creds/, that is NOT <state>/creds/.credentials.json
// — and this command must read the file the daemon reads, not the one it
// wishes existed.
func TestAuthOAuthPathFollowsTheDaemonNotCredsDir(t *testing.T) {
	ac := authTestCtx(t)
	claudeDir := filepath.Join(ac.state, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil { // a real dir, no symlink
		t.Fatal(err)
	}
	want := filepath.Join(claudeDir, ".credentials.json")
	if got := authOAuthPath(ac); got != want {
		t.Fatalf("authOAuthPath() = %q, want the daemon's %q", got, want)
	}

	// The trap in full: a STALE but perfectly parseable file at the creds/
	// path must not be mistaken for the fresh login.
	stale := credsFile{}
	stale.ClaudeAiOauth.AccessToken = "tok-stale"
	stale.ClaudeAiOauth.ExpiresAt = float64(time.Now().Add(-72 * time.Hour).UnixMilli())
	b, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ac.credsDir(), ".credentials.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeOAuth(t, ac, "tok-fresh", 8*time.Hour) // lands at the .claude path
	c := authResolve(ac)
	if c.headers["authorization"] != "Bearer tok-fresh" {
		t.Fatalf("resolved %q — the stale creds/ file won", c.headers["authorization"])
	}
}

// CRED_PATH is the daemon's explicit override; ignoring it sends this command
// at a file nothing reads.
func TestAuthOAuthPathHonorsCredPath(t *testing.T) {
	ac := authTestCtx(t)
	want := filepath.Join(ac.state, "elsewhere.json")
	t.Setenv("CRED_PATH", want)
	if got := authOAuthPath(ac); got != want {
		t.Fatalf("authOAuthPath() = %q, want CRED_PATH's %q", got, want)
	}
}

// Bug 2. A clone directory that merely contains creds/ must not outrank the
// daemon that is actually running and actually serving the 401.
func TestAuthStateDirPrefersInstalledOverClone(t *testing.T) {
	if !installed() {
		t.Skip("no installed koto on this host to prefer")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOTO_HOME", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	inst, ok := authInstalledStateDir()
	if !ok {
		t.Fatal("installed() is true but authInstalledStateDir disagrees")
	}
	got, why := authStateDir()
	if got != inst {
		t.Fatalf("authStateDir() = %q from a clone dir, want the installed %q", got, inst)
	}
	// And it must say the clone was seen, not pick in silence.
	if !strings.Contains(why, "clone") {
		t.Fatalf("why = %q, want it to name the clone it passed over", why)
	}
}

// authOAuthStamp is what distinguishes "the login wrote the file" from "a
// stale file happened to parse".
func TestAuthOAuthStampDetectsAnUntouchedFile(t *testing.T) {
	ac := authTestCtx(t)
	if s := authOAuthStamp(ac); s != "" {
		t.Fatalf("a missing file stamped as %q, want empty", s)
	}
	writeOAuth(t, ac, "tok-a", 8*time.Hour)
	first := authOAuthStamp(ac)
	if first == "" {
		t.Fatal("a written file stamped as empty")
	}
	if authOAuthStamp(ac) != first {
		t.Fatal("stamp changed with no write — it cannot detect an untouched file")
	}
	time.Sleep(10 * time.Millisecond)
	writeOAuth(t, ac, "tok-b-longer-than-the-first", 8*time.Hour)
	if authOAuthStamp(ac) == first {
		t.Fatal("stamp unchanged after a rewrite — a fresh login would read as a no-op")
	}
}

// The running daemon's environment outranks its EnvironmentFile: the process
// is what is actually serving the 401, and koto.env is root-owned 0600 and
// often unreadable from here.
func TestAuthDaemonEnvPrefersRunningProcess(t *testing.T) {
	ac := authTestCtx(t)
	p := filepath.Join(ac.state, "koto.env")
	if err := os.WriteFile(p, []byte("ANTHROPIC_API_KEY=sk-ant-from-envfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authEnvFile = p

	authRunningDaemonEnv = func() (map[string]string, string) {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-ant-from-process"}, "the running daemon (pid 42)"
	}
	authDaemonEnvOnce = sync.Once{}

	key, src := authEnvKey()
	if key != "sk-ant-from-process" {
		t.Fatalf("authEnvKey() = %q, want the running process's value", key)
	}
	if !strings.Contains(src, "pid 42") {
		t.Fatalf("source = %q, want it to name the running daemon", src)
	}
	// And that value must reach the resolved credential, restart flag and all.
	c := authResolve(ac)
	if c.headers["x-api-key"] != "sk-ant-from-process" || !c.restart {
		t.Fatalf("authResolve did not use the daemon's live env: %v restart=%v", c.headers, c.restart)
	}
}

// With no daemon running, the EnvironmentFile is the next best answer.
func TestAuthDaemonEnvFallsBackToEnvFile(t *testing.T) {
	ac := authTestCtx(t)
	p := filepath.Join(ac.state, "koto.env")
	if err := os.WriteFile(p, []byte("CRED_PATH=/somewhere/creds.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authEnvFile = p
	if got, _ := authDaemonEnv("CRED_PATH"); got != "/somewhere/creds.json" {
		t.Fatalf("authDaemonEnv(CRED_PATH) = %q", got)
	}
}

// A koto.env that exists but cannot be read is reported, not silently
// treated as "no key here" — it can hide the credential that outranks all.
func TestAuthEnvFileHidden(t *testing.T) {
	ac := authTestCtx(t)
	p := filepath.Join(ac.state, "koto.env")
	authEnvFile = p
	if authEnvFileHidden() {
		t.Fatal("a missing file must not count as hidden")
	}
	if err := os.WriteFile(p, []byte("ANTHROPIC_API_KEY=sk-ant-secret\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root — mode 000 is still readable")
	}
	if !authEnvFileHidden() {
		t.Fatal("an unreadable koto.env must be reported as hidden")
	}
}

// The probe must carry the Claude Code system block. Without it a valid
// subscription token comes back 429 with a "rate_limit_error" body and no
// rate-limit headers — the gate on non-Claude-Code traffic — and the report
// then either fails a working credential or, as it did, waves it through on
// evidence that meant nothing. Measured against the real API 2026-09-04:
// bare request 429, same request plus this block 200.
func TestAuthProbeSendsTheClaudeCodeSystemBlock(t *testing.T) {
	var got struct {
		System []struct{ Type, Text string } `json:"system"`
		Model  string                        `json:"model"`
		Max    int                           `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	withUpstream(t, srv.URL)

	if pr := authProbe(map[string]string{"x-api-key": "k"}); pr.status != 200 {
		t.Fatalf("status = %d, want 200", pr.status)
	}
	if len(got.System) != 1 || got.System[0].Text != claudeCodeSystem {
		t.Fatalf("system = %+v, want one block %q", got.System, claudeCodeSystem)
	}
	if got.Max != 1 {
		t.Fatalf("max_tokens = %d, want 1 — the probe must stay ~free", got.Max)
	}
}

// A 429 means opposite things depending on one header family, so the two are
// pinned apart: with anthropic-ratelimit-* the credential was accepted and
// merely throttled; without it the request shape was refused and the
// credential was never judged.
func TestAuthProbeSeparatesThrottleFromShapeGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"gate: no headers at all", nil, false},
		{"throttle: unified status", map[string]string{"anthropic-ratelimit-unified-status": "allowed"}, true},
		{"throttle: retry-after", map[string]string{"retry-after": "30"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))
			}))
			defer srv.Close()
			withUpstream(t, srv.URL)

			pr := authProbe(map[string]string{"x-api-key": "k"})
			if pr.status != 429 {
				t.Fatalf("status = %d, want 429", pr.status)
			}
			if pr.rateLimited != tc.want {
				t.Fatalf("rateLimited = %v, want %v", pr.rateLimited, tc.want)
			}
		})
	}
}

// withUpstream points the probe at a test server for the duration of a test,
// restoring the real base so a later test cannot end up calling a dead
// listener — or, worse, the real API.
func withUpstream(t *testing.T, url string) {
	t.Helper()
	old := authUpstream
	authUpstream = url
	t.Cleanup(func() { authUpstream = old })
}
