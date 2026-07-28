package main

// fc_test.go — smoke tests for the daemon side of the Firecracker vsock
// multiplexer, driven against fakes (net.Pipe / a fake hybrid-vsock UDS
// endpoint standing in for the firecracker process). No VM, no KVM, no
// assets needed — this is the wire logic only.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fcHarness(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	HERE = dir
	ROOT = filepath.Join(dir, "groups")
	SOCK_DIR = filepath.Join(dir, "run")
	GROUPS_FILE = filepath.Join(dir, "groups.json")
	if err := os.MkdirAll(fcRunDir(), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestFcLogSink: bytes arriving on the vsock log stream land appended on the
// host-side group log — the invariant every downstream consumer (tailLog,
// History, /clear) relies on.
func TestFcLogSink(t *testing.T) {
	fcHarness(t)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { fcLogSink("tg", b); close(done) }()
	msg := "[ts:1]\nhello from guest\n[[turn_end]]\n"
	if _, err := a.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	a.Close()
	<-done
	got, err := os.ReadFile(filepath.Join(vol("tg"), ".cs", "log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != msg {
		t.Fatalf("log mirror mismatch: %q != %q", got, msg)
	}
	// Second connection appends, never truncates.
	a2, b2 := net.Pipe()
	done2 := make(chan struct{})
	go func() { fcLogSink("tg", b2); close(done2) }()
	if _, err := a2.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}
	a2.Close()
	<-done2
	got, _ = os.ReadFile(filepath.Join(vol("tg"), ".cs", "log"))
	if string(got) != msg+"more\n" {
		t.Fatalf("append semantics broken: %q", got)
	}
}

// TestFcCtlConn: a JSON line on the ctl stream is dispatched with the group's
// identity and the response comes back on the same connection.
func TestFcCtlConn(t *testing.T) {
	fcHarness(t)
	a, b := net.Pipe()
	go fcCtlConn("tg", b)
	if _, err := a.Write([]byte(`{"cmd":"sched_list"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(a).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("bad response %q: %v", line, err)
	}
	if !resp.OK {
		t.Fatalf("sched_list should be allowed for any group: %s", line)
	}
	// Authorization: non-main groups must not spawn.
	if _, err := a.Write([]byte(`{"cmd":"spawn","group":"x"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err = bufio.NewReader(a).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "not allowed") {
		t.Fatalf("non-main spawn should be rejected: %s", line)
	}
	a.Close()
}

// fakeFC emulates firecracker's hybrid-vsock host UDS: accept, expect
// "CONNECT <port>", reply "OK <port>", then hand the connection to the
// per-port guest handler.
func fakeFC(t *testing.T, g string, guest func(port int, c net.Conn)) net.Listener {
	t.Helper()
	// fcUDS now lives in a per-group socket dir (see fcSockDir); production
	// fcSpawn creates it before listening, so mirror that here.
	if err := os.MkdirAll(fcSockDir(g), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", fcUDS(g))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					c.Close()
					return
				}
				var port int
				if _, err := fmt.Sscanf(line, "CONNECT %d", &port); err != nil {
					c.Close()
					return
				}
				fmt.Fprintf(c, "OK %d\n", port)
				guest(port, c)
			}(c)
		}
	}()
	return ln
}

// TestFcAgentCall: full host-initiated round trip through the hybrid-vsock
// handshake and the agent's JSON-line framing.
func TestFcAgentCall(t *testing.T) {
	fcHarness(t)
	ln := fakeFC(t, "tg", func(port int, c net.Conn) {
		defer c.Close()
		if port != fcPortAgent {
			t.Errorf("expected CONNECT %d, got %d", fcPortAgent, port)
			return
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			t.Errorf("read req: %v", err)
			return
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Errorf("req not json: %v", err)
			return
		}
		if req["op"] != "msg" {
			t.Errorf("op = %v", req["op"])
		}
		fmt.Fprintf(c, `{"ok":true}`+"\n")
	})
	defer ln.Close()
	if err := fcSendMsg("tg", "", "aGk=", "system prompt", []byte(`{"provider":"venice"}`)); err != nil {
		t.Fatalf("fcSendMsg: %v", err)
	}
}

// TestFcAgentCallError: in-band {ok:false} surfaces as a Go error.
func TestFcAgentCallError(t *testing.T) {
	fcHarness(t)
	ln := fakeFC(t, "tg", func(port int, c net.Conn) {
		defer c.Close()
		bufio.NewReader(c).ReadString('\n')
		fmt.Fprintf(c, `{"ok":false,"error":"boom"}`+"\n")
	})
	defer ln.Close()
	_, err := fcAgentCall("tg", map[string]any{"op": "exec", "script": "true"}, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected in-band error, got %v", err)
	}
}

// TestFcSpliceToProxy: a guest connection on the proxy port is spliced
// bidirectionally into the group's TCP proxy listener.
func TestFcSpliceToProxy(t *testing.T) {
	fcHarness(t)
	// Fake proxy: echo server on an ephemeral port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1024)
				n, _ := c.Read(buf)
				c.Write(buf[:n])
				c.Close()
			}(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	a, b := net.Pipe()
	go fcSpliceToProxy(b, port)
	a.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := a.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := a.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("echo mismatch: %q", buf[:n])
	}
	a.Close()
}

// TestApplyConfigNetwork: the new "network" key validates none|wan|lan|full;
// the legacy "internet" key maps + migrates (full→network=wan, no "internet"
// key left on disk).
func TestApplyConfigNetwork(t *testing.T) {
	// Legacy internet=full migrates to network=wan, dropping the old key.
	cfg := map[string]any{"internet": "old"}
	applyConfig(cfg, "internet", json.RawMessage(`"full"`))
	if cfg["network"] != "wan" {
		t.Fatalf("internet=full should map to network=wan, got %v", cfg["network"])
	}
	if _, ok := cfg["internet"]; ok {
		t.Fatal("legacy internet key should be removed")
	}
	// Explicit network values.
	cfg = map[string]any{}
	applyConfig(cfg, "network", json.RawMessage(`"lan"`))
	if cfg["network"] != "lan" {
		t.Fatalf("network=lan not stored, got %v", cfg["network"])
	}
	// Bogus value rejected — prior value preserved.
	cfg = map[string]any{"network": "wan"}
	applyConfig(cfg, "network", json.RawMessage(`"bogus"`))
	if cfg["network"] != "wan" {
		t.Fatalf("bogus network should preserve prior value, got %v", cfg["network"])
	}
}

// TestGroupNetwork: default none; wan|lan|full honored; unknown → none; the
// legacy "internet" key maps (full→wan, none→none); explicit "network" wins
// when both keys are present.
func TestGroupNetwork(t *testing.T) {
	fcHarness(t)
	if groupNetwork("nope") != fcNetNone {
		t.Fatal("missing config should be none")
	}
	d := filepath.Join(vol("tg"), ".cs")
	os.MkdirAll(d, 0o755)
	write := func(s string) { os.WriteFile(filepath.Join(d, "config.json"), []byte(s), 0o644) }

	for _, v := range []string{fcNetWAN, fcNetLAN, fcNetFull} {
		write(`{"network":"` + v + `"}`)
		if got := groupNetwork("tg"); got != v {
			t.Fatalf("network=%s not honored, got %s", v, got)
		}
	}
	write(`{"network":"open"}`)
	if groupNetwork("tg") != fcNetNone {
		t.Fatal("unknown network value should fall back to none")
	}
	// Legacy internet key.
	write(`{"internet":"full"}`)
	if groupNetwork("tg") != fcNetWAN {
		t.Fatal("legacy internet=full should map to wan")
	}
	write(`{"internet":"none"}`)
	if groupNetwork("tg") != fcNetNone {
		t.Fatal("legacy internet=none should map to none")
	}
	// Both present → network wins.
	write(`{"internet":"full","network":"lan"}`)
	if groupNetwork("tg") != fcNetLAN {
		t.Fatal("explicit network should win over legacy internet")
	}
}

// TestGroupRootDefault: default no; "yes" (string) and true (bool) opt in.
func TestGroupRootDefault(t *testing.T) {
	fcHarness(t)
	if groupRoot("nope") {
		t.Fatal("missing config should be false")
	}
	d := filepath.Join(vol("tg"), ".cs")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"root":"yes"}`), 0o644)
	if !groupRoot("tg") {
		t.Fatal("explicit root=yes not honored")
	}
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"root":true}`), 0o644)
	if !groupRoot("tg") {
		t.Fatal("bool root=true not honored")
	}
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"root":"no"}`), 0o644)
	if groupRoot("tg") {
		t.Fatal("root=no should be false")
	}
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"root":"maybe"}`), 0o644)
	if groupRoot("tg") {
		t.Fatal("unknown value should fall back to false")
	}
}

// TestEgressGate: the profile gate (403 for none) + the policy-aware
// self-target/LAN guard. The 403 path returns before hijacking, so a plain
// recorder suffices; the tunnel path is covered live in the smoke run (needs a
// real socket).
func TestEgressGate(t *testing.T) {
	fcHarness(t)
	d := filepath.Join(vol("tg"), ".cs")
	os.MkdirAll(d, 0o755)
	h := &handler{group: "tg"}

	// Stub DNS so hostname classification is deterministic and offline.
	orig := egressLookupIP
	egressLookupIP = func(host string) ([]net.IP, error) {
		switch host {
		case "github.com", "registry.npmjs.org":
			return []net.IP{net.ParseIP("140.82.112.3")}, nil // public
		case "intranet.local":
			return []net.IP{net.ParseIP("192.168.1.10")}, nil // LAN
		case "sneaky.example":
			// One public + one loopback IP — one bad IP must deny.
			return []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("127.0.0.1")}, nil
		}
		return nil, fmt.Errorf("nxdomain")
	}
	defer func() { egressLookupIP = orig }()

	allowed := func(hp, pol string) bool { ok, _ := egressTargetAllowed(hp, pol); return ok }

	// none → 403 for CONNECT.
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"network":"none"}`), 0o644)
	req := &http.Request{Method: http.MethodConnect, Host: "example.com:443", URL: &url.URL{Host: "example.com:443"}}
	rec := httptest.NewRecorder()
	h.serveEgress(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("none group: expected 403, got %d", rec.Code)
	}

	// Control plane blocked under EVERY profile, including full.
	for _, pol := range []string{fcNetWAN, fcNetLAN, fcNetFull} {
		if allowed("127.0.0.1:9", pol) || allowed("cs_host_go:8080", pol) ||
			allowed("example.com:8443", pol) || allowed("[::1]:80", pol) {
			t.Fatalf("guard should block loopback/cs_host/daemon-port under %s", pol)
		}
	}
	// DNS name resolving to a loopback IP is blocked even under full.
	if allowed("sneaky.example:443", fcNetFull) {
		t.Fatal("name resolving to loopback should be denied (one-bad-IP)")
	}
	// wan: public allowed, LAN denied.
	if !allowed("github.com:443", fcNetWAN) || !allowed("registry.npmjs.org:443", fcNetWAN) {
		t.Fatal("wan should allow public hosts")
	}
	if allowed("192.168.1.5:80", fcNetWAN) || allowed("intranet.local:80", fcNetWAN) ||
		allowed("10.0.0.1:22", fcNetWAN) {
		t.Fatal("wan should block LAN targets (literal and by-name)")
	}
	// lan: LAN allowed, public denied.
	if !allowed("192.168.1.5:80", fcNetLAN) || !allowed("intranet.local:80", fcNetLAN) {
		t.Fatal("lan should allow LAN targets")
	}
	if allowed("1.1.1.1:443", fcNetLAN) || allowed("github.com:443", fcNetLAN) {
		t.Fatal("lan should block public targets")
	}
	// CGNAT / tailnet is LAN, not WAN — a wan-only group cannot reach it.
	if allowed("100.100.1.1:443", fcNetWAN) || !allowed("100.100.1.1:443", fcNetLAN) {
		t.Fatal("tailnet (100.64/10) should be LAN-class")
	}
}

// TestEnsureProviderConfigSeedsProvider: an empty config gets the default
// provider seeded; a valid explicit provider is never overwritten.
func TestEnsureProviderConfigSeedsProvider(t *testing.T) {
	fcHarness(t)
	d := filepath.Join(vol("tg"), ".cs")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{}`), 0o644)
	if err := ensureProviderConfig("tg"); err != nil {
		t.Fatal(err)
	}
	if p := groupProviderName("tg"); p != defaultProvider {
		t.Fatalf("provider not seeded: %s", p)
	}
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"provider":"venice"}`), 0o644)
	if err := ensureProviderConfig("tg"); err != nil {
		t.Fatal(err)
	}
	if p := groupProviderName("tg"); p != "venice" {
		t.Fatalf("explicit provider was clobbered: %s", p)
	}
}

// TestFcClearStalePids: pidfiles left by a previous daemon run are swept at
// start — a recycled pid in one can belong to another group's fresh VMM
// (defeating pidIsFirecracker), so fcRunning must never see them. The sweep
// removes only *.pid; sibling run/fc files (console logs, sock dirs) stay.
func TestFcClearStalePids(t *testing.T) {
	fcHarness(t)
	for _, g := range []string{"a", "b"} {
		if err := os.WriteFile(fcPidPath(g), []byte("123\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(fcRunDir(), "a.console.log")
	os.WriteFile(keep, []byte("x"), 0o644)

	fcClearStalePids()

	for _, g := range []string{"a", "b"} {
		if _, err := os.Stat(fcPidPath(g)); !os.IsNotExist(err) {
			t.Fatalf("stale pidfile %s survived the sweep", fcPidPath(g))
		}
		if fcRunning(g) {
			t.Fatalf("fcRunning(%s) true after sweep", g)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-pid file was swept: %v", err)
	}
}
