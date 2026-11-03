package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"koto/netallow"
)

// allowProxyEnv: group "tg" config+booted at `network`, allowlist from
// config.json `allow` (nil = key absent), fake DNS map, recording dialer.
// Returns a function that sends one CONNECT and reports (status, dialed).
func allowProxyEnv(t *testing.T, network string, allow string, dns map[string]string) func(target string) (int, []string) {
	t.Helper()
	fcHarness(t)
	d := filepath.Join(vol("tg"), ".cs")
	os.MkdirAll(d, 0o755)
	cfg := `{"network":"` + network + `"}`
	if allow != "" {
		cfg = `{"network":"` + network + `","allow":` + allow + `}`
	}
	if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	proxySetBootNetwork("tg", network)
	liveAllow.Delete("tg")
	seedAllow("tg")
	origLookup, origDial := egressLookupIP, egressDial
	t.Cleanup(func() {
		proxySetBootNetwork("tg", fcNetNone)
		liveAllow.Delete("tg")
		egressLookupIP, egressDial = origLookup, origDial
	})
	egressLookupIP = func(h string) ([]net.IP, error) {
		if ip, ok := dns[strings.TrimSuffix(strings.ToLower(h), ".")]; ok {
			return []net.IP{net.ParseIP(ip)}, nil
		}
		return nil, fmt.Errorf("nxdomain %s", h)
	}
	var dialed []string
	egressDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, fmt.Errorf("no network in test")
	}
	return func(target string) (int, []string) {
		dialed = nil
		h := &handler{group: "tg"}
		req := &http.Request{Method: http.MethodConnect, Host: target, URL: &url.URL{Host: target}, Header: http.Header{}}
		rec := httptest.NewRecorder()
		h.serveEgress(rec, req.WithContext(context.Background()))
		return rec.Code, append([]string(nil), dialed...)
	}
}

var allowDNS = map[string]string{
	"api.example.com":   "93.184.216.34",
	"other.example.com": "93.184.216.35",
	"evil.test":         "203.0.113.9",
	"static.test":       "198.51.100.7", // name not listed, IP is
	"nas.lan":           "192.168.1.20",
}

// dialedOK: the request passed both gates — the (stubbed) dial happened.
func dialedOK(code int, dialed []string) bool {
	return len(dialed) == 1 && code == http.StatusBadGateway
}

func blocked(code int, dialed []string) bool { return len(dialed) == 0 && code == http.StatusForbidden }

func TestProxyAllowEmptyIsOldBehaviour(t *testing.T) {
	send := allowProxyEnv(t, fcNetWAN, "", allowDNS)
	for _, tg := range []string{"api.example.com:443", "evil.test:443", "203.0.113.9:443"} {
		if c, d := send(tg); !dialedOK(c, d) {
			t.Errorf("empty allowlist, wan, %s: want dial, got %d %v", tg, c, d)
		}
	}
	// the profile still applies: wan never reaches the LAN
	if c, d := send("nas.lan:443"); !blocked(c, d) {
		t.Errorf("empty allowlist, wan, LAN name: want 403, got %d %v", c, d)
	}
}

func TestProxyAllowHostAndIP(t *testing.T) {
	send := allowProxyEnv(t, fcNetWAN, `["*.example.com","198.51.100.0/24","192.168.1.*"]`, allowDNS)
	for _, c := range []struct {
		target string
		ok     bool
		why    string
	}{
		{"api.example.com:443", true, "one-level wildcard name"},
		{"API.Example.com.:443", true, "case + trailing dot"},
		{"a.b.example.com:443", false, "two levels deep (NXDOMAIN here too, but the list must not match)"},
		{"evil.test:443", false, "unlisted name, unlisted IP"},
		{"203.0.113.9:443", false, "unlisted IP literal"},
		{"198.51.100.9:443", true, "IP literal inside listed CIDR"},
		{"static.test:443", true, "unlisted name whose vetted IP is listed"},
		{"nas.lan:443", false, "listed 192.168.1.* but profile wan: profile ∩ allowlist"},
		{"192.168.1.20:443", false, "listed LAN literal under wan"},
	} {
		code, d := send(c.target)
		if c.ok && !dialedOK(code, d) {
			t.Errorf("%s (%s): want dial, got %d %v", c.target, c.why, code, d)
		}
		if !c.ok && !blocked(code, d) {
			t.Errorf("%s (%s): want 403 no dial, got %d %v", c.target, c.why, code, d)
		}
	}
}

// The pinned dial target is the vetted IP even for an allowed name.
func TestProxyAllowDialsVettedIP(t *testing.T) {
	send := allowProxyEnv(t, fcNetFull, `["api.example.com"]`, allowDNS)
	_, d := send("api.example.com:443")
	if len(d) != 1 || d[0] != "93.184.216.34:443" {
		t.Fatalf("dialed %v; want 93.184.216.34:443", d)
	}
	// exact entry: the sibling is not allowed even though full allows it
	if c, d := send("other.example.com:443"); !blocked(c, d) {
		t.Errorf("exact entry must not admit sibling: got %d %v", c, d)
	}
}

func TestProxyAllowLiveChange(t *testing.T) {
	send := allowProxyEnv(t, fcNetWAN, `["api.example.com"]`, allowDNS)
	if c, d := send("evil.test:443"); !blocked(c, d) {
		t.Fatalf("before change: want 403, got %d %v", c, d)
	}
	l, err := netallow.Parse("evil.test")
	if err != nil {
		t.Fatal(err)
	}
	publishAllowForTest("tg", l)
	if c, d := send("evil.test:443"); !dialedOK(c, d) {
		t.Errorf("after live add: want dial, got %d %v", c, d)
	}
	if c, d := send("api.example.com:443"); !blocked(c, d) {
		t.Errorf("after live removal: want 403, got %d %v", c, d)
	}
	// clearing the list restores the profile-only behaviour, live
	empty, _ := netallow.Parse("")
	publishAllowForTest("tg", empty)
	if c, d := send("api.example.com:443"); !dialedOK(c, d) {
		t.Errorf("after live clear: want dial, got %d %v", c, d)
	}
}

func TestProxyAllowCorruptDeniesAll(t *testing.T) {
	send := allowProxyEnv(t, fcNetFull, `["*.example.*"]`, allowDNS)
	if !currentAllow("tg").denyAll {
		t.Fatal("corrupt stored list should seed deny-all")
	}
	for _, tg := range []string{"api.example.com:443", "198.51.100.9:443"} {
		if c, d := send(tg); !blocked(c, d) {
			t.Errorf("corrupt list, %s: want 403, got %d %v", tg, c, d)
		}
	}
}

// Absolute-form plain HTTP goes through the same gate.
func TestProxyAllowAbsoluteForm(t *testing.T) {
	allowProxyEnv(t, fcNetWAN, `["api.example.com"]`, allowDNS)
	var dialed []string
	egressDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, fmt.Errorf("no network in test")
	}
	h := &handler{group: "tg"}
	req := httptest.NewRequest(http.MethodGet, "http://evil.test/x", nil)
	req.Host = "evil.test"
	rec := httptest.NewRecorder()
	h.serveEgress(rec, req)
	if rec.Code != http.StatusForbidden || len(dialed) != 0 {
		t.Fatalf("absolute-form unlisted: got %d %v", rec.Code, dialed)
	}
	req = httptest.NewRequest(http.MethodGet, "http://api.example.com/x", nil)
	req.Host = "api.example.com"
	rec = httptest.NewRecorder()
	h.serveEgress(rec, req)
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:80" {
		t.Fatalf("absolute-form listed: dialed %v (code %d)", dialed, rec.Code)
	}
}
