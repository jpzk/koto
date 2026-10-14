package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
)

// The Models RPC maps the cached catalog onto the wire shape.
func TestModelsRPCLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.Error(w, "bad", 404)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"kimi-k2.5","type":"text","model_spec":{"name":"Kimi \u001b[31mK2.5"}}]}`)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	resp, err := (&kotoServer{}).Models(context.Background(), &pb.ModelsReq{Provider: "venice"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ok || resp.Source != modelsSourceLive || resp.Provider != "venice" || resp.Reason != "" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Models) != 1 || resp.Models[0].Id != "kimi-k2.5" || resp.Models[0].DisplayName != "Kimi K2.5" {
		t.Fatalf("models = %+v", resp.Models)
	}
}

// Upstream failure is not an RPC error: the fallback list plus the reason.
func TestModelsRPCFallbackCarriesReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 503)
	}))
	defer srv.Close()
	modelsFake(t, srv)
	resp, err := (&kotoServer{}).Models(context.Background(), &pb.ModelsReq{Provider: "claudesdk"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ok || resp.Source != modelsSourceFallback || !strings.Contains(resp.Reason, "HTTP 503") || len(resp.Models) == 0 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestModelsRPCUnknownProvider(t *testing.T) {
	resp, err := (&kotoServer{}).Models(context.Background(), &pb.ModelsReq{Provider: "evil\x1b[2J"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Ok || resp.Error == "" || len(resp.Models) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}

// A hung upstream must not hold the RPC past modelsRPCWait.
func TestModelsRPCBoundedWait(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	modelsFake(t, srv)
	old := modelsRPCWait
	modelsRPCWait = 50 * time.Millisecond
	defer func() { modelsRPCWait = old }()
	start := time.Now()
	resp, err := (&kotoServer{}).Models(context.Background(), &pb.ModelsReq{Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("RPC blocked %v", d)
	}
	if !resp.Ok || resp.Source != modelsSourceFallback || resp.Reason != "still loading" {
		t.Fatalf("resp = %+v", resp)
	}
}

// models is an untargeted verb, denied by default to every non-admin role
// (an unlisted verb is denied), grantable through acl.json, and always
// available to the hardcoded admin.
func TestModelsVerbAuthorization(t *testing.T) {
	if v := verbFromMethod(pb.Koto_Models_FullMethodName); v != "models" {
		t.Fatalf("verb = %q", v)
	}
	if _, targeted := targetOf(&pb.ModelsReq{Provider: "venice"}); targeted {
		t.Fatal("models must be untargeted (no group)")
	}
	acl := parseACL([]byte(`{
		"agent":  {"list": "*", "spawn": ["main"], "config": "*"},
		"picker": {"models": "*"}
	}`))
	if roleAllowed(acl, "agent", "models", "", false) {
		t.Error("agent must NOT have models without a grant")
	}
	if !roleAllowed(acl, "picker", "models", "", false) {
		t.Error("granted role must have models")
	}
	if !roleAllowed(acl, defaultRole, "models", "", false) {
		t.Error("admin must have models")
	}
	if adminOnlyVerbs["models"] {
		t.Error("models must stay grantable")
	}
	err := aclCheck(context.Background(), clientIdentity{Name: "t", Roles: []string{"nobody-has-this-role"}}, "models", &pb.ModelsReq{})
	if err == nil {
		t.Error("unknown role must be denied models")
	}
}

// Guests never reach the host-credential fetch: the ctl plane has no models
// verb, for main or for peers.
func TestModelsNotOnCtlPlane(t *testing.T) {
	for _, owner := range []string{ctlMainGroup, "peer"} {
		b, _ := json.Marshal(ctlDispatch(owner, []byte(`{"cmd":"models","provider":"venice"}`)))
		if !strings.Contains(string(b), "verb not allowed") {
			t.Fatalf("%s: ctl answered models: %s", owner, b)
		}
	}
}
