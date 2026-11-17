package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"koto-protocol/pb"
	"koto/mcpgw"
	"koto/mcpreg"
)

const oauthTestRT = "RT-oauth-refresh-do-not-leak"
const oauthTestCS = "CS-oauth-client-secret-do-not-leak"

func TestMcpAdminOAuthAddViewsRedacted(t *testing.T) {
	mcpHarness(t)
	var got mcpgw.OnboardReq
	old := mcpOAuthOnboard
	t.Cleanup(func() { mcpOAuthOnboard = old })
	mcpOAuthOnboard = func(_ context.Context, _ *http.Client, u string, r mcpgw.OnboardReq) (*mcpreg.OAuth, error) {
		got = r
		return &mcpreg.OAuth{Grant: mcpreg.GrantRefreshToken, Resource: u, Issuer: "http://127.0.0.1:7",
			TokenEndpoint: "http://127.0.0.1:7/token", ClientID: "cid", ClientSecret: oauthTestCS,
			AuthMethod: "client_secret_basic", Scope: "mcp:add", RefreshToken: oauthTestRT}, nil
	}
	v := mcpOp(t, mcpAdminReq{Op: "add", Conn: "adder", URL: "http://127.0.0.1:7/mcp", Tools: []string{"add"},
		OAuthJSON: `{"grant":"authorization_code","scope":"mcp:add"}`})
	if got.Grant != "authorization_code" || got.Scope != "mcp:add" {
		t.Fatalf("onboard req %+v", got)
	}
	all := mcpJSON(v) + mcpJSON(mcpOp(t, mcpAdminReq{Op: "ls"})) + mcpJSON(mcpOp(t, mcpAdminReq{Op: "show", Conn: "adder"}))
	for _, s := range []string{oauthTestRT, oauthTestCS, "redacted"} {
		if strings.Contains(all, s) {
			t.Fatalf("view carries %q: %s", s, all)
		}
	}
	if !strings.Contains(all, `"client_id":"cid"`) || !strings.Contains(all, `"grant":"refresh_token"`) {
		t.Fatalf("view lacks the non-secret oauth fields: %s", all)
	}
	st, _ := mcpStore()
	if st.Current().Conns["adder"].OAuth.RefreshToken != oauthTestRT {
		t.Fatal("refresh token not stored")
	}
	// rotation persists through the store, CAS-guarded
	if err := mcpRotateRefresh("adder", oauthTestRT, "RT-2"); err != nil {
		t.Fatal(err)
	}
	if err := mcpRotateRefresh("adder", oauthTestRT, "RT-3"); err == nil {
		t.Fatal("stale rotation accepted")
	}
	if st.Current().Conns["adder"].OAuth.RefreshToken != "RT-2" {
		t.Fatal("rotation not persisted")
	}
	// refusals
	for _, r := range []mcpAdminReq{
		{Op: "add", Conn: "x", URL: "http://127.0.0.1:7/mcp", OAuthJSON: `{"grant":"x","bogus":1}`},
		{Op: "add", Conn: "x", URL: "http://127.0.0.1:7/mcp", OAuthJSON: `{"grant":"client_credentials"}`, AuthName: "A", AuthValue: "b"},
	} {
		if _, err := mcpAdmin(r); err == nil {
			t.Fatalf("accepted %+v", r.OAuthJSON)
		}
	}
}

func TestMcpCliParseOAuth(t *testing.T) {
	rf := func(p string) ([]byte, error) {
		return []byte(`{"client_id":"pre","client_secret":"` + oauthTestCS + `"}`), nil
	}
	r, err := mcpCliParse([]string{"add", "adder", "--url", "http://127.0.0.1:7/mcp", "--tools", "add",
		"--oauth", "client_credentials", "--oauth-scope", "mcp:add", "--oauth-client-file", "-"}, rf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.OauthJson, `"grant":"client_credentials"`) || !strings.Contains(r.OauthJson, `"client_id":"pre"`) ||
		!strings.Contains(r.OauthJson, `"scope":"mcp:add"`) {
		t.Fatalf("oauth json %s", r.OauthJson)
	}
	for _, args := range [][]string{
		{"add", "a", "--url", "http://127.0.0.1/x", "--oauth", "password"},
		{"add", "a", "--url", "http://127.0.0.1/x", "--oauth", "client_credentials", "--auth-file", "f"},
		{"add", "a", "--url", "http://127.0.0.1/x", "--oauth-scope", "x"},
		{"ls", "--oauth", "client_credentials"},
	} {
		if _, err := mcpCliParse(args, rf); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// The hand-maintained McpReq.oauth_json descriptor must round-trip.
func TestMcpReqOAuthJSONWire(t *testing.T) {
	in := &pb.McpReq{Op: "add", OauthJson: `{"grant":"client_credentials"}`, PerMinute: 3}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &pb.McpReq{}
	if err := proto.Unmarshal(b, out); err != nil || out.GetOauthJson() != in.OauthJson || out.PerMinute != 3 {
		t.Fatalf("round trip: %v %+v", err, out)
	}
	fd := out.ProtoReflect().Descriptor().Fields().ByName("oauth_json")
	if fd == nil || fd.Number() != 13 || fd.JSONName() != "oauthJson" {
		t.Fatalf("descriptor: %v", fd)
	}
}
