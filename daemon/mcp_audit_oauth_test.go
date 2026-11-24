package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"koto/mcpgw"
	"koto/mcpreg"
)

// OAuth onboarding: the --oauth request (client secret) and the onboarding
// result (tokens) never reach the audit spool, a failed onboarding's error
// only as error.len/.sha256; and a granted call refused by the upstream's
// scope is recorded with reason insufficient_scope.
func TestMcpAuditOAuth(t *testing.T) {
	mcpHarness(t)
	home := t.TempDir()
	if err := auditStart(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(auditStop)
	old := mcpOAuthOnboard
	t.Cleanup(func() { mcpOAuthOnboard = old })
	fail := true
	mcpOAuthOnboard = func(_ context.Context, _ *http.Client, u string, r mcpgw.OnboardReq) (*mcpreg.OAuth, error) {
		if fail {
			return nil, errors.New("token endpoint said: bad client " + oauthTestCS)
		}
		return &mcpreg.OAuth{Grant: mcpreg.GrantRefreshToken, Resource: u, Issuer: "http://127.0.0.1:7",
			TokenEndpoint: "http://127.0.0.1:7/token", ClientID: "cid-audit-sentinel", ClientSecret: oauthTestCS,
			AuthMethod: "client_secret_basic", Scope: "mcp:add", RefreshToken: oauthTestRT}, nil
	}
	req := mcpAdminReq{Op: "add", Conn: "adder", URL: "http://127.0.0.1:7/mcp", Tools: []string{"add"},
		OAuthJSON: `{"grant":"client_credentials","client_id":"cid-audit-sentinel","client_secret":"` + oauthTestCS + `"}`}
	if _, err := mcpAdmin(req); err == nil {
		t.Fatal("failed onboarding accepted")
	}
	fail = false
	mcpOp(t, req)
	mcpAudit(mcpgw.AuditEntry{Group: "tg", Conn: "adder", Method: "tools/call", Tool: "add",
		Reason: "upstream refused: insufficient_scope (needs mcp:admin)", Code: mcpgw.CodeInsufficientScope})
	auditStop()
	dir := auditDir(home)
	raw, err := os.ReadFile(filepath.Join(dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{oauthTestCS, oauthTestRT, "cid-audit-sentinel", "127.0.0.1:7", "mcp:admin"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("spool carries %q", s)
		}
	}
	for _, s := range []string{`"verb":"mcp_admin"`, `"error.sha256"`, `"reason":"insufficient_scope"`} {
		if !bytes.Contains(raw, []byte(s)) {
			t.Errorf("spool lacks %s:\n%s", s, raw)
		}
	}
	var out, errb bytes.Buffer
	if rc := auditCli([]string{"verify", "--dir", dir}, &out, &errb); rc != 0 {
		t.Fatalf("koto audit verify rc=%d: %s%s", rc, out.String(), errb.String())
	}
}
