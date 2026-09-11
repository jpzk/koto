package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"koto-protocol/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
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

// M9a: RunScript output is sanitized by default, judged on whole lines so an
// escape split across two frames cannot slip through; raw is a pass-through.
func TestChunkSanitizer(t *testing.T) {
	c := newChunkSanitizer(true)
	// An OSC 52 clipboard write split mid-sequence across frames, then a
	// line that carries legitimate SGR styling and a real newline.
	part1 := []byte("hello \x1b]52;c;cG9")
	part2 := []byte("3duZWQ=\x07 world\n\x1b[31mred\x1b[0m")
	var got []byte
	got = append(got, c.write(part1)...)
	if len(got) != 0 {
		t.Fatalf("partial line must be held back, got %q", got)
	}
	got = append(got, c.write(part2)...)
	got = append(got, c.flush()...)
	s := string(got)
	if strings.Contains(s, "\x1b]52") || strings.Contains(s, "\x07") || strings.Contains(s, "cG93") {
		t.Fatalf("OSC clipboard write survived: %q", s)
	}
	if !strings.Contains(s, "hello  world\n") && !strings.Contains(s, "hello world\n") {
		t.Fatalf("text or newline lost: %q", s)
	}
	if !strings.Contains(s, "\x1b[31mred\x1b[0m") {
		t.Fatalf("SGR styling should pass: %q", s)
	}
	// Mouse-mode switch and cursor motion dropped; text kept.
	c2 := newChunkSanitizer(true)
	out := string(append(c2.write([]byte("\x1b[?1000h\x1b[2Jline\n")), c2.flush()...))
	if strings.Contains(out, "\x1b[?1000h") || strings.Contains(out, "\x1b[2J") || !strings.Contains(out, "line\n") {
		t.Fatalf("mode/cursor escapes should drop, text stay: %q", out)
	}
	// raw: byte-exact pass-through, frame by frame.
	r := newChunkSanitizer(false)
	if string(r.write([]byte("\x1b]0;x\x07 partial"))) != "\x1b]0;x\x07 partial" || r.flush() != nil {
		t.Fatal("raw must pass every byte through unchanged and hold nothing back")
	}
	// A newline-free firehose is not held forever.
	c3 := newChunkSanitizer(true)
	big := bytes.Repeat([]byte("x"), chunkSanitizerMaxPartial+1)
	if len(c3.write(big)) == 0 {
		t.Fatal("over-cap partial should be flushed")
	}
}

// L6: the client cert and the bearer token must name the same identity.
// Two allowlisted identities are minted into a temp creds dir and driven
// through the REAL serverTLSConfig + interceptors over a loopback gRPC
// connection, because that is the only place the binding can be observed:
// the cert reaches the interceptor through the peer's TLS state, not through
// anything a unit test can synthesize honestly.
func TestCertAndTokenMustNameOneIdentity(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds")
	if err := pkiInit(creds, nil); err != nil {
		t.Fatalf("pkiInit: %v", err)
	}
	tokTUI, err := pkiClient(creds, "tui", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	tokRO, err := pkiClient(creds, "readonly", []string{"reader"})
	if err != nil {
		t.Fatal(err)
	}
	oldHere := HERE
	HERE = dir
	t.Cleanup(func() { HERE = oldHere })

	tlsCfg, err := serverTLSConfig()
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(authUnary),
	)
	pb.RegisterKotoServer(srv, &kotoServer{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Stop()

	caPEM, _ := os.ReadFile(filepath.Join(creds, "ca.crt"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	call := func(certName, token string) error {
		cert, err := tls.LoadX509KeyPair(
			filepath.Join(creds, "client-"+certName+".crt"),
			filepath.Join(creds, "client-"+certName+".key"))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(l.Addr().String(),
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
				ServerName:   "koto-daemon",
				MinVersion:   tls.VersionTLS13,
			})),
			grpc.WithPerRPCCredentials(testToken{token}))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = pb.NewKotoClient(conn).List(ctx, &pb.ListReq{})
		return err
	}

	if err := call("tui", tokTUI); err != nil {
		t.Fatalf("matched cert+token must be accepted: %v", err)
	}
	// The attack L6 describes: an allowlisted cert of a low-privilege
	// identity plus a leaked admin token.
	if err := call("readonly", tokTUI); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("readonly cert + tui token: want Unauthenticated, got %v", err)
	}
	// And the mirror: the admin's own cert may not carry someone else's token.
	if err := call("tui", tokRO); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("tui cert + readonly token: want Unauthenticated, got %v", err)
	}
}

// M3: the LLM leg relays a LIST of endpoints, not whatever the guest asks
// for. The allowed set is what koto's own clients were observed to call; the
// refused set is drawn from the same 7 weeks of recorded traffic (the probes
// that actually arrived) plus the credential- and spend-reaching endpoints
// the finding is about.
func TestLLMEndpointAllowlist(t *testing.T) {
	allowed := []struct{ provider, method, path string }{
		{"claudesdk", "POST", "/v1/messages"},
		{"claudesdk", "POST", "/v1/messages/count_tokens"},
		{"claudesdk", "POST", "/v1/chat/completions"},
		{"claudesdk", "GET", "/v1/models"},
		{"claudesdk", "GET", "/v1/models/claude-sonnet-5"},
		{"claudesdk", "GET", "/api/hello"},  // Claude Code's connectivity check
		{"claudesdk", "HEAD", "/api/hello"}, // ...which it actually sends as HEAD
		{"claudesdk", "HEAD", "/v1/models"},
		{"venice", "POST", "/api/v1/chat/completions"},
		{"venice", "GET", "/api/v1/models"},
	}
	for _, c := range allowed {
		if !llmPathAllowed(c.provider, c.method, c.path) {
			t.Errorf("%s %s %s must be allowed — it is traffic koto itself generates", c.provider, c.method, c.path)
		}
	}
	refused := []struct{ provider, method, path string }{
		{"claudesdk", "POST", "/v1/files"},            // Files API: storage that outlives the VM
		{"claudesdk", "POST", "/v1/messages/batches"}, // spend that outlives the turn
		{"claudesdk", "GET", "/v1/organizations/me"},
		{"venice", "POST", "/api/v1/api_keys"}, // mint a key and read it back
		{"venice", "GET", "/api/v1/api_keys"},
		{"claudesdk", "GET", "/media/../secret.txt"}, // seen in the wild
		{"claudesdk", "GET", "/"},
		{"claudesdk", "GET", "/api/tags"},
		{"claudesdk", "DELETE", "/v1/messages"}, // right path, wrong method
		{"claudesdk", "GET", "/v1/messages"},
		{"venice", "POST", "/v1/messages"},    // the other provider's route
		{"claudesdk", "POST", "/api/hello"},   // right path, wrong method
		{"claudesdk", "HEAD", "/v1/messages"}, // HEAD is GET, and GET is not allowed here
		{"claudesdk", "GET", "/v1/models/"},   // the prefix rule needs an id
	}
	for _, c := range refused {
		if llmPathAllowed(c.provider, c.method, c.path) {
			t.Errorf("%s %s %s must be refused", c.provider, c.method, c.path)
		}
	}
	// Traversal is judged by where it RESOLVES, not by how it is spelled:
	// Go keeps `..` in an origin-form request target, and the upstream
	// resolves it, so the allowlist has to as well — in both directions.
	if !llmPathAllowed("claudesdk", "POST", "/v1/foo/../messages") {
		t.Error("a path that resolves onto an allowed route must be allowed")
	}
	if llmPathAllowed("claudesdk", "POST", "/v1/messages/../files") {
		t.Error("a path that resolves OFF an allowed route must be refused")
	}
}

// M2: concurrency is bounded per group and globally, and a slot is always
// returned. The per-group limit is the one a single runaway group hits first,
// so that is what this pins; the global limit is the same mechanism one level
// up.
func TestProxyInflightBounded(t *testing.T) {
	// A cancelled context makes proxyAcquire answer immediately instead of
	// waiting proxyInflightWait, so "would have blocked" is observable
	// without the test sleeping for a minute.
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	var releases []func()
	for i := 0; i < proxyMaxInflightPerGroup; i++ {
		rel, ok := proxyAcquire(context.Background(), "m2group")
		if !ok {
			t.Fatalf("slot %d of %d refused; the limit must be reachable", i+1, proxyMaxInflightPerGroup)
		}
		releases = append(releases, rel)
	}
	if _, ok := proxyAcquire(dead, "m2group"); ok {
		t.Fatalf("a %dst concurrent request must not get a slot", proxyMaxInflightPerGroup+1)
	}
	// A different group is unaffected: the per-group limit must not become a
	// fleet-wide one.
	rel, ok := proxyAcquire(dead, "m2other")
	if !ok {
		t.Fatal("another group must still get a slot — the limit is per group")
	}
	rel()

	releases[0]()
	rel2, ok := proxyAcquire(dead, "m2group")
	if !ok {
		t.Fatal("releasing a slot must make it available again")
	}
	rel2()
	for _, r := range releases[1:] {
		r()
	}
	// Every slot returned: the group can fill up from empty once more.
	for i := 0; i < proxyMaxInflightPerGroup; i++ {
		r, ok := proxyAcquire(dead, "m2group")
		if !ok {
			t.Fatalf("slot %d leaked — released slots must return to the pool", i+1)
		}
		defer r()
	}
}

// L10: `koto tui` must never resolve its binary from the state directory.
// That is the one path the daemon can write (ReadWritePaths=), so a binary
// planted there and then run by the operator is a tier-2 -> tier-1 step. The
// dev-clone fallback survives only for a real checkout.
func TestTUIBinaryNotResolvedFromStateDir(t *testing.T) {
	src, err := os.ReadFile("tui_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `filepath.Join(*state, "koto-tui")`) {
		t.Error("koto tui resolves its binary from the state dir again — the daemon can write there")
	}
	dir := t.TempDir()
	if isKotoCheckout(dir) {
		t.Error("an empty dir must not pass as a koto checkout")
	}
	if err := os.WriteFile(filepath.Join(dir, "go.work"), []byte("go 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isKotoCheckout(dir) {
		t.Error("go.work alone must not pass — both markers are required")
	}
	if err := os.Mkdir(filepath.Join(dir, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isKotoCheckout(dir) {
		t.Error("go.work + daemon/ is a koto checkout")
	}
}

// M10: host-side uploads are bounded, and the bound is checked BEFORE the
// write — the point of the finding was that every Send with an image cost
// host disk before the queue applied any backpressure, and host disk
// exhaustion is the fleet-wide failure (every guest remounts read-only).
// This was the one fix in the batch that no test pinned.
func TestUploadsPendingBounded(t *testing.T) {
	oldRoot := ROOT
	ROOT = t.TempDir()
	t.Cleanup(func() { ROOT = oldRoot })
	if err := os.MkdirAll(filepath.Join(ROOT, "g", ".cs"), 0o750); err != nil {
		t.Fatal(err)
	}

	// One image may not exceed maxImageBytes, whatever the pending total.
	if _, err := saveImage("g", make([]byte, maxImageBytes+1), "image/png"); err == nil {
		t.Fatal("an oversize single image must be refused")
	}

	// Fill the pending budget. Each accepted image is real bytes on disk, so
	// the loop terminates against the cap rather than a counter.
	chunk := make([]byte, 4<<20)
	accepted := 0
	var lastErr error
	for i := 0; i < 1000; i++ {
		if _, err := saveImage("g", chunk, "image/png"); err != nil {
			lastErr = err
			break
		}
		accepted++
	}
	if lastErr == nil {
		t.Fatal("pending uploads grew without limit — this is the finding")
	}
	if !strings.Contains(lastErr.Error(), "too many pending uploads") {
		t.Fatalf("refusal should name the cause, got %v", lastErr)
	}
	dir := filepath.Join(ROOT, "g", ".cs", "uploads")
	if used := dirBytes(dir); used > maxUploadsPending {
		t.Fatalf("pending bytes %d exceed the cap %d — the check must run BEFORE the write", used, maxUploadsPending)
	}
	if accepted == 0 {
		t.Fatal("the cap must still admit ordinary uploads")
	}

	// Delivery is what frees the budget: fcSendMsg removes each host copy
	// once the tar into the guest succeeds. Emulate that and confirm the
	// group can upload again — without it a group would wedge permanently
	// after 64 MiB of images, which would be a worse bug than the one fixed.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := saveImage("g", chunk, "image/png"); err != nil {
		t.Fatalf("after delivery removes the host copies, uploads must be accepted again: %v", err)
	}
}

// 2026-09-11 H1: a workspace.img that is not a regular file is refused before
// anything truncates, fscks, chowns or bind-mounts it into a VM as /dev/vdb.
func TestWorkspaceImgRefusesNonRegularFile(t *testing.T) {
	origRoot, origHere := ROOT, HERE
	defer func() { ROOT, HERE = origRoot, origHere }()
	HERE = t.TempDir()
	ROOT = filepath.Join(HERE, "groups")
	g := "victim"
	if err := os.MkdirAll(vol(g), 0o750); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(HERE, "secret")
	if err := os.WriteFile(secret, []byte("operator data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, fcWorkspaceImg(g)); err != nil {
		t.Fatal(err)
	}
	if err := fcEnsureWorkspaceImg(g); err == nil {
		t.Fatal("symlinked workspace.img accepted as a VM disk")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("unexpected error: %v", err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "operator data" {
		t.Fatalf("symlink target was modified: %q", b)
	}
}
