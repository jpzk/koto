package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"koto/mcpreg"
	"koto/ociimage"
)

// redirUp is a minimal MCP upstream; when redirect is set, every tools/call
// answers 307 to it.
type redirUp struct{ redirect string }

func (p *redirUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	if len(req.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	var res any = map[string]any{}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "s")
	case "tools/list":
		res = map[string]any{"tools": []any{
			map[string]any{"name": "fast", "inputSchema": map[string]any{"type": "object"}}}}
	case "tools/call":
		if p.redirect != "" {
			http.Redirect(w, r, p.redirect, http.StatusTemporaryRedirect)
			return
		}
		res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
}

func redirCall(gw *Gateway, args string) string {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fast","arguments":%s}}`, args)
	rec := httptest.NewRecorder()
	gw.Handler("g").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/c", strings.NewReader(body)))
	return rec.Body.String()
}

// The gateway must not follow an upstream redirect: neither the static auth
// header nor the guest-chosen body may reach the redirect target.
func TestGatewayDoesNotFollowRedirects(t *testing.T) {
	for _, injected := range []bool{false, true} {
		hits := make(chan string, 4)
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits <- r.Header.Get("X-Api-Key")
			w.WriteHeader(500)
		}))
		up := &redirUp{}
		srv := httptest.NewServer(up)
		reg := mcpreg.New()
		must(t, reg.PutConn("c", &mcpreg.Conn{URL: srv.URL, Class: mcpreg.ClassRead,
			Auth:  &mcpreg.AuthHeader{Name: "X-Api-Key", Value: "STATIC-UPSTREAM-SECRET"},
			Tools: map[string]*mcpreg.ToolRule{"fast": {AdditionalArgs: true}}}, false))
		must(t, reg.Allow("c", "g"))
		must(t, reg.Grant("c", "g", "fast", nil))
		must(t, reg.Validate())
		gw := &Gateway{Policy: func() *mcpreg.Registry { return reg }}
		if injected {
			gw.Client = &http.Client{Timeout: 5 * time.Second} // follows redirects by default
		}
		if out := redirCall(gw, "{}"); !strings.Contains(out, "ok") {
			t.Fatalf("injected=%v warm call: %s", injected, out)
		}
		up.redirect = strings.Replace(sink.URL, "127.0.0.1", "localhost", 1) + "/internal/admin"
		out := redirCall(gw, `{"cmd":"guest-chosen-payload"}`)
		select {
		case k := <-hits:
			t.Fatalf("injected=%v: redirect followed (X-Api-Key=%q)", injected, k)
		case <-time.After(300 * time.Millisecond):
		}
		if strings.Contains(out, "STATIC-UPSTREAM-SECRET") || strings.Contains(out, "internal/admin") {
			t.Fatalf("injected=%v: guest response leaks redirect detail: %s", injected, out)
		}
		srv.Close()
		sink.Close()
	}
}

func TestHTTPClientNoRedirect(t *testing.T) {
	sinkHit := false
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sinkHit = true }))
	defer sink.Close()
	src := httptest.NewServer(http.RedirectHandler(sink.URL, http.StatusFound))
	defer src.Close()
	resp, err := HTTPClient(5 * time.Second).Get(src.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || sinkHit {
		t.Fatalf("status=%d sinkHit=%v, want 302 returned and sink untouched", resp.StatusCode, sinkHit)
	}
}

// https endpoints may not dial non-public addresses (checked on the resolved
// address, so a name that resolves inward is refused as well); plain http
// endpoints are dialled as configured.
func TestHTTPClientHTTPSRefusesNonPublic(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	port := fmt.Sprint(srv.Listener.Addr().(*net.TCPAddr).Port)
	hc := HTTPClient(5 * time.Second)
	for _, u := range []string{
		srv.URL,                                 // 127.0.0.1
		"https://localhost:" + port + "/",       // name resolving to loopback
		"https://[::1]:" + port + "/",           // v6 loopback
		"https://169.254.169.254:" + port + "/", // link-local (metadata)
		"https://10.0.0.1:" + port + "/",        // RFC1918
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := hc.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: dialled, want refused", u)
		}
		if !errors.Is(err, ociimage.ErrBlockedAddr) {
			t.Fatalf("%s: err=%v, want ErrBlockedAddr", u, err)
		}
	}
	// plain http to loopback (an operator's local MCP server) still works
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	resp, err := hc.Get(plain.URL)
	if err != nil {
		t.Fatalf("plain http loopback: %v", err)
	}
	resp.Body.Close()
	// the guard itself admits a public address
	if err := publicOnly("93.184.215.14:443", nil); err != nil {
		t.Fatalf("public address refused: %v", err)
	}
	if err := publicOnly("127.0.0.1:443", func(netip.AddrPort) bool { return true }); err != nil {
		t.Fatalf("allow hook ignored: %v", err)
	}
}
