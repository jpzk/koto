package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The 2026-09-04 audit's mechanical fixes, pinned. Each test names the
// finding it covers.

// H2: the unspecified address is control plane at L7 — literal, v6, mapped,
// and via a name that resolves to it — and the dial guard refuses it too.
func TestEgressUnspecifiedIsControlPlane(t *testing.T) {
	orig := egressLookupIP
	egressLookupIP = func(host string) ([]net.IP, error) {
		if host == "any.example" {
			return []net.IP{net.ParseIP("0.0.0.0")}, nil
		}
		return nil, fmt.Errorf("nxdomain")
	}
	defer func() { egressLookupIP = orig }()
	for _, hp := range []string{"0.0.0.0:8788", "[::]:8788", "[::ffff:0.0.0.0]:80", "any.example:8788"} {
		for _, pol := range []string{fcNetWAN, fcNetLAN, fcNetFull} {
			if ok, _ := egressTargetAllowed(hp, pol); ok {
				t.Errorf("egressTargetAllowed(%s, %s) = true; the unspecified address connects locally", hp, pol)
			}
		}
	}
	for _, hp := range []string{"0.0.0.0:1", "127.0.0.1:1", "[::1]:1", "[::]:1", "example.com:443", "garbage"} {
		if ip := egressDialIP(hp); ip != nil && fcClassifyDst(ip) != fcDstCtl {
			t.Errorf("dial guard would let %s through", hp)
		}
	}
	if ip := egressDialIP("1.1.1.1:443"); ip == nil || fcClassifyDst(ip) != fcDstWAN {
		t.Error("dial guard rejects a plain public ip:port")
	}
}

// M9b: model/effort are identifiers, not free text.
func TestApplyConfigValidatesModelAndEffort(t *testing.T) {
	cfg := map[string]any{}
	raw := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	for _, ok := range []string{"claude-sonnet-5", "qwen/qwen3-235b", "kimi-k2.5", "gpt-4.1:free", "high"} {
		applyConfig(cfg, "model", raw(ok))
		if cfg["model"] != ok {
			t.Errorf("model %q rejected", ok)
		}
	}
	for _, bad := range []string{"", " ", "\x1b]0;pwned\x07", "a b", "-leading", strings.Repeat("x", 129), "x\ny"} {
		delete(cfg, "effort")
		applyConfig(cfg, "effort", raw(bad))
		if _, set := cfg["effort"]; set {
			t.Errorf("effort %q accepted", bad)
		}
	}
}

// M5: schedules are bounded per group, and messages in length.
func TestAddSchedCaps(t *testing.T) {
	prev := SCHED_FILE
	SCHED_FILE = filepath.Join(t.TempDir(), "schedules.json")
	t.Cleanup(func() { SCHED_FILE = prev })
	schedLock.Lock()
	sched, parsed = nil, map[string]parsedCron{}
	schedLock.Unlock()
	if _, err := addSched("g", "* * * * *", strings.Repeat("m", schedMaxMsg+1)); err == nil {
		t.Fatal("oversized msg accepted")
	}
	for i := 0; i < schedMaxPerGroup; i++ {
		if _, err := addSched("g", "* * * * *", "m"); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := addSched("g", "* * * * *", "m"); err == nil {
		t.Fatal("cap not enforced")
	}
	if _, err := addSched("other", "* * * * *", "m"); err != nil {
		t.Fatalf("cap leaked across groups: %v", err)
	}
}

// M4: buffered job results per (group, session) are bounded.
func TestRecordJobDoneBounded(t *testing.T) {
	notifyMu.Lock()
	notifyPending = map[string][]jobResult{}
	notifyMu.Unlock()
	for i := 0; i < notifyMaxPending*3; i++ {
		recordJobDone("g", jobResult{ID: fmt.Sprint(i), RC: "0"})
	}
	notifyMu.Lock()
	n := len(notifyPending[notifyKey("g", "")])
	for _, tm := range notifyTimers {
		tm.Stop()
	}
	notifyMu.Unlock()
	if n > notifyMaxPending {
		t.Fatalf("pending grew to %d (cap %d)", n, notifyMaxPending)
	}
}

// L5: marker bodies are cut on a rune boundary with a visible mark.
func TestTruncateBytes(t *testing.T) {
	if got := truncateBytes("short", 10); got != "short" {
		t.Errorf("short string changed: %q", got)
	}
	got := truncateBytes(strings.Repeat("é", 100), 11) // 2-byte runes; 11 is mid-rune
	if !strings.HasSuffix(got, "…[truncated]") || strings.Count(strings.TrimSuffix(got, "…[truncated]"), "é") != 5 {
		t.Errorf("truncateBytes cut mid-rune or lost the mark: %q", got)
	}
}

// L9: a pre-existing state dir must be ours, a real directory, and private.
func TestStateDirTrusted(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o750)
	fi, _ := os.Lstat(dir)
	if err := stateDirTrusted(fi, me); err != nil {
		t.Fatalf("own private dir refused: %v", err)
	}
	os.Chmod(dir, 0o777)
	fi, _ = os.Lstat(dir)
	if err := stateDirTrusted(fi, me); err == nil {
		t.Fatal("world-writable dir accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(dir, link)
	fi, _ = os.Lstat(link)
	if err := stateDirTrusted(fi, me); err == nil {
		t.Fatal("symlink accepted")
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	fi, _ = os.Lstat(f)
	if err := stateDirTrusted(fi, me); err == nil {
		t.Fatal("regular file accepted")
	}
}

// L4: a turn stream may open only on a slot the daemon handed out.
func TestExpectedTurnSlots(t *testing.T) {
	if fcConsumeExpectedTurn("g", 3) {
		t.Fatal("slot with no outstanding turn accepted")
	}
	fcExpectTurn("g", 3)
	if !fcConsumeExpectedTurn("g", 3) {
		t.Fatal("handed-out slot refused")
	}
	if fcConsumeExpectedTurn("g", 3) {
		t.Fatal("slot accepted twice")
	}
}

// H1: posture is the operator's. The ctl plane refuses every posture key,
// on main itself and on a peer, and still takes model/effort/provider.
func TestCtlConfigSetRefusesPosture(t *testing.T) {
	fcHarness(t)
	for _, key := range []string{"network", "internet", "root", "ports", "size", "autostart"} {
		for _, target := range []string{"main", "tg"} {
			val := any("full")
			if key == "ports" {
				val = []int{8080}
			}
			resp := ctlDispatch("main", ctlLine(t, map[string]any{"cmd": "config_set", "group": target, key: val}))
			br, ok := resp.(baseResp)
			if !ok || br.OK || !strings.Contains(br.Error, "operator-only") {
				t.Fatalf("config_set %s on %s: want operator-only refusal, got %+v", key, target, resp)
			}
		}
	}
	resp := ctlDispatch("main", ctlLine(t, map[string]any{"cmd": "config_set", "group": "tg", "model": "claude-opus-5", "effort": "high"}))
	if cr, ok := resp.(configResp); !ok || !cr.OK {
		t.Fatalf("model/effort should still be settable: %+v", resp)
	}
	if groupNetwork("tg") != fcNetNone {
		t.Fatal("a refused network key must not have landed in config.json")
	}
}

// H1: the L7 gate takes the stricter of the booted profile and the live
// config — a live raise is not live egress; a live lower is.
func TestEgressGateHonoursBootProfile(t *testing.T) {
	fcHarness(t)
	d := filepath.Join(vol("tg"), ".cs")
	os.MkdirAll(d, 0o755)
	h := &handler{group: "tg"}
	t.Cleanup(func() { proxySetBootNetwork("tg", fcNetNone) })
	connect := func() int {
		req := &http.Request{Method: http.MethodConnect, Host: "192.168.1.10:443", URL: &url.URL{Host: "192.168.1.10:443"}}
		rec := httptest.NewRecorder()
		h.serveEgress(rec, req)
		return rec.Code
	}
	// Config says full, but no VM has booted with it → denied.
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"network":"full"}`), 0o644)
	if code := connect(); code != http.StatusForbidden {
		t.Fatalf("config=full, booted=none: want 403, got %d", code)
	}
	// Booted as wan, config raised to full → the LAN target stays blocked.
	proxySetBootNetwork("tg", fcNetWAN)
	if code := connect(); code != http.StatusForbidden {
		t.Fatalf("config=full, booted=wan: LAN target should be blocked, got %d", code)
	}
	// Booted as full, config lowered to none → denied at once.
	proxySetBootNetwork("tg", fcNetFull)
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"network":"none"}`), 0o644)
	if code := connect(); code != http.StatusForbidden {
		t.Fatalf("config=none, booted=full: want 403, got %d", code)
	}
	// Stop clears the snapshot.
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"network":"full"}`), 0o644)
	proxySetBootNetwork("tg", fcNetNone)
	if code := connect(); code != http.StatusForbidden {
		t.Fatalf("after stop: want 403, got %d", code)
	}
}

// M1: the per-group proxy listens on a unix socket in an owner-only dir, not
// on loopback TCP; (port, group) stays one-to-one; unlisten removes the file.
func TestProxyListenIsUnixSocket(t *testing.T) {
	fcHarness(t)
	const port = 4343
	if err := proxyListen(port, "tg"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxyUnlisten(port) })
	fi, err := os.Lstat(proxySockPath(port))
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a unix socket at %s, got %v %v", proxySockPath(port), fi, err)
	}
	if di, err := os.Stat(proxySockDir()); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("run/proxy should be 0700, got %v %v", di, err)
	}
	if err := proxyListen(port, "tg"); err != nil {
		t.Fatalf("re-listen for the same group should be a no-op: %v", err)
	}
	if err := proxyListen(port, "other"); err == nil {
		t.Fatal("same port for a different group must be refused")
	}
	// The server actually serves on it: a CONNECT for a none-profile group
	// comes back 403 from serveEgress, which proves the request reached the
	// handler through the socket.
	c, err := net.Dial("unix", proxySockPath(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), " 403 ") {
		t.Fatalf("expected a 403 through the unix listener, got %q", buf[:n])
	}
	proxyUnlisten(port)
	if _, err := os.Lstat(proxySockPath(port)); err == nil {
		t.Fatal("unlisten should remove the socket file")
	}
}

// M11: the wizard's default `koto ctl` identity is least-privilege, and the
// doctor can read an identity's roles out of tokens.json in every shape the
// file has had.
func TestIdentityRoles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tokens.json"), []byte(`{
	  "agent": {"hash":"aa", "roles":["agent"]},
	  "ops":   {"hash":"bb", "role":"operator"},
	  "old":   "cc",
	  "bare":  {"hash":"dd"}
	}`), 0o600)
	for name, want := range map[string][]string{
		"agent": {"agent"}, "ops": {"operator"}, "old": {"admin"}, "bare": {"admin"},
	} {
		got := identityRoles(dir, name)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("identityRoles(%s) = %v, want %v", name, got, want)
		}
	}
	if identityRoles(dir, "nobody") != nil || identityRoles(t.TempDir(), "agent") != nil {
		t.Error("absent name / absent file should be nil")
	}
	// The wizard source itself: the agent identity is minted with the agent role.
	src, err := os.ReadFile("setup_steps.go")
	if err != nil {
		t.Skip("source not available")
	}
	if strings.Contains(string(src), `pkiClient(sc.credsDir(), "agent", []string{"admin"})`) {
		t.Fatal("the wizard mints the default koto ctl identity as admin again (audit M11)")
	}
	if !strings.Contains(string(src), `pkiClient(sc.credsDir(), "agent", []string{"agent"})`) {
		t.Fatal("the wizard no longer mints the agent identity with the agent role")
	}
}
