package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"koto/mcpreg"
)

// OAuth secrets join the mcp_view sentinel set: every one is planted in the
// registry (or, for tokens, minted live through the daemon's own gateway)
// and must never appear in any mcp_view answer.
func init() {
	for k, v := range map[string]string{
		"client_id":      "SENTINEL-client-id-a1b2",
		"client_secret":  "SENTINEL-client-secret-77e1",
		"refresh_token":  "SENTINEL-refresh-0d9f",
		"rotated_rt":     "SENTINEL-rotated-rt-5c3e",
		"access_token":   "SENTINEL-access-token-e8a0",
		"token_endpoint": "SENTINEL-tokep-4e2a",
		"issuer":         "SENTINEL-issuer-c9d1",
	} {
		mcpViewSentinels[k] = v
	}
}

// mcpViewOAuthAS is a loopback AS (token endpoint only) + a dead upstream.
type mcpViewOAuthAS struct {
	srv    *httptest.Server
	refuse atomic.Bool // answer invalid_grant
	minted atomic.Int32
}

func newMcpViewOAuthAS(t *testing.T) *mcpViewOAuthAS {
	a := &mcpViewOAuthAS{}
	s := mcpViewSentinels
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/token") {
			http.Error(w, "no upstream here", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if a.refuse.Load() {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		a.minted.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": s["access_token"], "token_type": "Bearer",
			"expires_in": 1, "refresh_token": s["rotated_rt"]})
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *mcpViewOAuthAS) call(t *testing.T) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{}}}`
	rec := httptest.NewRecorder()
	mcpGateway().Handler("tg").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/oa", strings.NewReader(body)))
	if strings.Contains(rec.Body.String(), mcpViewSentinels["access_token"]) {
		t.Fatal("gateway echoed the access token")
	}
}

func mcpViewAuthOf(t *testing.T, conn string) map[string]any {
	t.Helper()
	m, outs := mcpViewCall(t, ctlMainGroup, fmt.Sprintf(`{"cmd":"mcp_view","op":"show","conn":%q}`, conn))
	mcpViewNoSentinel(t, "show "+conn, outs)
	if m["ok"] != true {
		t.Fatalf("show %s: %v", conn, m)
	}
	return m["mcp"].(map[string]any)["conn"].(map[string]any)
}

func init() {
	mcpViewSeedExt = func(t *testing.T) {
		s := mcpViewSentinels
		a := newMcpViewOAuthAS(t)
		st, err := mcpStore()
		if err != nil {
			t.Fatal(err)
		}
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(st.Update(func(r *mcpreg.Registry) error {
			if err := r.PutConn("oa", &mcpreg.Conn{URL: a.srv.URL + "/mcp", Class: mcpreg.ClassRead,
				Tools: map[string]*mcpreg.ToolRule{"add": {AdditionalArgs: true}},
				OAuth: &mcpreg.OAuth{Grant: mcpreg.GrantRefreshToken, Resource: a.srv.URL + "/mcp",
					Issuer: a.srv.URL + "/" + s["issuer"], TokenEndpoint: a.srv.URL + "/" + s["token_endpoint"] + "/token",
					ClientID: s["client_id"], ClientSecret: s["client_secret"], AuthMethod: "client_secret_basic",
					Scope: "mcp:add", RefreshToken: s["refresh_token"]}}, false); err != nil {
				return err
			}
			if err := r.Allow("oa", "tg"); err != nil {
				return err
			}
			if err := r.Grant("oa", "tg", "add", nil); err != nil {
				return err
			}
			return r.PutConn("off", &mcpreg.Conn{URL: "https://off.example.net/mcp", Class: mcpreg.ClassRead,
				Tools: map[string]*mcpreg.ToolRule{"x": nil}, Disabled: true}, false)
		}))
		// mint live: the access token and the rotated refresh token now
		// exist (gateway memory / registry file)
		a.call(t)
		if a.minted.Load() == 0 {
			t.Fatal("gateway never minted")
		}
		if st.Current().Conns["oa"].OAuth.RefreshToken != s["rotated_rt"] {
			t.Fatal("rotated refresh token not stored")
		}
		au := mcpViewAuthOf(t, "oa")["auth"].(map[string]any)
		if au["kind"] != "oauth" || au["grant"] != "refresh_token" || au["status"] != "ok" || len(au) != 3 {
			t.Fatalf("oauth auth after good mint = %v", au)
		}
		// the AS stops honouring the grant: the view says relogin_needed
		a.refuse.Store(true)
		a.call(t)
		if au := mcpViewAuthOf(t, "oa")["auth"].(map[string]any); au["status"] != "relogin_needed" {
			t.Fatalf("oauth auth after invalid_grant = %v", au)
		}
		if c := mcpViewAuthOf(t, "off"); c["enabled"] != false {
			t.Fatalf("disabled conn shows enabled: %v", c)
		}
	}
}
