package mcpreg

import (
	"strings"
	"testing"
)

func oauthConn() *Conn {
	return &Conn{URL: "http://127.0.0.1:9/mcp", Class: ClassRead, OAuth: &OAuth{
		Grant: GrantRefreshToken, Resource: "http://127.0.0.1:9/mcp", Issuer: "http://127.0.0.1:9",
		TokenEndpoint: "http://127.0.0.1:9/token", ClientID: "c1", AuthMethod: "none",
		RefreshToken: "RT-very-secret"}}
}

func TestOAuthValidateAndRedact(t *testing.T) {
	r := New()
	if err := r.PutConn("a", oauthConn(), false); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := r.Redacted().Marshal()
	if strings.Contains(string(b), "RT-very-secret") {
		t.Fatal("refresh token in redacted view")
	}
	b, _ = r.Marshal()
	r2, err := Parse(b)
	if err != nil || r2.Conns["a"].OAuth.RefreshToken != "RT-very-secret" {
		t.Fatalf("round trip: %v", err)
	}
	bad := []func(*Conn){
		func(c *Conn) { c.Auth = &AuthHeader{Name: "Authorization", Value: "x"} },
		func(c *Conn) { c.OAuth.Grant = "password" },
		func(c *Conn) { c.OAuth.RefreshToken = "" },
		func(c *Conn) { c.OAuth.TokenEndpoint = "http://example.com/token" },
		func(c *Conn) { c.OAuth.ClientID = "" },
		func(c *Conn) { c.OAuth.RefreshToken = "a\nb" },
		func(c *Conn) { c.OAuth.Grant, c.OAuth.ClientSecret = GrantClientCredentials, "" },
	}
	for i, mut := range bad {
		c := oauthConn()
		mut(c)
		r := New()
		r.Conns["a"] = c
		if err := r.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		} else if strings.Contains(err.Error(), "RT-very-secret") {
			t.Fatalf("case %d: error echoes the secret", i)
		}
	}
}

func TestPutConnKeepsOAuthAndRotate(t *testing.T) {
	r := New()
	if err := r.PutConn("a", oauthConn(), false); err != nil {
		t.Fatal(err)
	}
	if err := r.PutConn("a", &Conn{URL: "http://127.0.0.1:9/mcp"}, true); err != nil {
		t.Fatal(err)
	}
	if r.Conns["a"].OAuth == nil {
		t.Fatal("re-add without auth dropped the oauth registration")
	}
	if err := r.RotateRefresh("a", "stale", "new"); err == nil {
		t.Fatal("CAS accepted a stale old token")
	}
	if err := r.RotateRefresh("a", "RT-very-secret", "RT-2"); err != nil || r.Conns["a"].OAuth.RefreshToken != "RT-2" {
		t.Fatalf("rotate: %v", err)
	}
}
