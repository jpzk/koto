package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"koto-protocol/pb"
	"koto/mcpreg"
)

const mcpTestSecret = "s3cr3t-upstream-token-do-not-leak"

func mcpHarness(t *testing.T) {
	t.Helper()
	fcHarness(t) // registers main, tg, peer
	t.Setenv("KOTO_HOME", HERE)
}

func mcpOp(t *testing.T, r mcpAdminReq) map[string]any {
	t.Helper()
	v, err := mcpAdmin(r)
	if err != nil {
		t.Fatalf("%s: %v", r.Op, err)
	}
	return v
}

func mcpJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// ACL: the registry is admin-only, never grantable through acl.json — not by
// a "*" wildcard, not by an explicit "mcp" grant.
func TestMcpVerbAdminOnly(t *testing.T) {
	acl := aclTable{"ops": {"*": targetSet{any: true}}, "mc": {"mcp": targetSet{any: true}}}
	if !rolesAllowed(acl, []string{defaultRole}, "mcp", "", false) {
		t.Fatal("admin denied mcp")
	}
	for _, r := range []string{"ops", "mc", "agent"} {
		if rolesAllowed(acl, []string{r}, "mcp", "", false) {
			t.Errorf("role %s granted mcp via acl.json", r)
		}
	}
	if !adminOnlyVerbs["mcp"] {
		t.Fatal("mcp not in adminOnlyVerbs")
	}
	if v := verbFromMethod(pb.Koto_Mcp_FullMethodName); v != "mcp" {
		t.Fatalf("verb = %q", v)
	}
}

// Posture: "mcp" as a config key is refused on the in-guest ctl plane (main
// included — main cannot grant peers), classed config_posture on gRPC, and
// refused by configCmd on every path. A bare ctl "mcp" verb is refused too.
func TestMcpPostureRefused(t *testing.T) {
	mcpHarness(t)
	for _, line := range []string{
		`{"cmd":"config_set","group":"tg","mcp":"grant"}`,
		`{"cmd":"config_set","group":"peer","mcp":{"conn":"x","tools":["a"]}}`,
		`{"cmd":"config_set","group":"main","model":"m","mcp":"x"}`,
	} {
		r := ctlConfigSet(t, line)
		if r["ok"] == true || !strings.Contains(fmt.Sprint(r["error"]), "mcp is an operator-only setting") {
			t.Fatalf("%s: not refused: %v", line, r)
		}
	}
	for _, g := range []string{ctlMainGroup, "tg"} {
		for _, line := range []string{
			`{"cmd":"mcp","op":"allow","conn":"x","group":"tg"}`,
			`{"cmd":"mcp_grant","conn":"x","group":"tg","tool":"a"}`,
		} {
			b, _ := json.Marshal(ctlDispatch(g, []byte(line)))
			if strings.Contains(string(b), `"ok":true`) {
				t.Fatalf("%s from %s: accepted: %s", line, g, b)
			}
		}
	}
	s := "x"
	if v := postureVerb("config", &pb.ConfigReq{Group: "tg", Mcp: &s}); v != "config_posture" {
		t.Fatalf("postureVerb = %q", v)
	}
	if r := configCmd(configReq{Group: "tg", Mcp: json.RawMessage(`"x"`)}); r.OK || !strings.Contains(r.Error, "koto ctl mcp") {
		t.Fatalf("configCmd accepted mcp: %+v", r)
	}
	if r := configCmd(fromPBConfigReq(&pb.ConfigReq{Group: "tg", Mcp: &s})); r.OK {
		t.Fatal("gRPC ConfigReq.mcp accepted")
	}
	if _, err := os.Stat(mcpRegistryPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused request wrote the registry: %v", err)
	}
}

// Full operator flow: add (secret write-only, file 0600), allow requires a
// real group, grant can't exceed the ceiling, effective = ceiling ∩ grant,
// revoke and deny apply, rm drops everything.
func TestMcpAdminFlow(t *testing.T) {
	mcpHarness(t)
	v := mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp",
		Tools: []string{"search", "fetch", "delete"}, AuthName: "Authorization", AuthValue: mcpTestSecret,
		RulesJSON: `{"search":{"args":{"q":{"type":"string","maxLength":64}}}}`})
	if strings.Contains(mcpJSON(v), mcpTestSecret) {
		t.Fatal("add view leaks the auth value")
	}
	fi, err := os.Stat(mcpRegistryPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("registry file: %v mode %v", err, fi)
	}
	if di, _ := os.Stat(filepath.Dir(mcpRegistryPath())); di.Mode().Perm() != 0o700 {
		t.Fatalf("registry dir mode %v", di.Mode().Perm())
	}
	for _, op := range []string{"ls", "show"} {
		if s := mcpJSON(mcpOp(t, mcpAdminReq{Op: op, Conn: "docs"})); strings.Contains(s, mcpTestSecret) {
			t.Fatalf("%s leaks the auth value", op)
		}
	}
	// whitelist: only registered groups
	if _, err := mcpAdmin(mcpAdminReq{Op: "allow", Conn: "docs", Group: "ghost"}); err == nil {
		t.Fatal("allowed an unregistered group")
	}
	// grant before allow: refused (deny by default)
	if _, err := mcpAdmin(mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search"}); err == nil {
		t.Fatal("grant on a non-whitelisted group accepted")
	}
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "peer"})
	// a grant can never widen the ceiling
	if _, err := mcpAdmin(mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "shell"}); err == nil {
		t.Fatal("grant of a tool outside the ceiling accepted")
	}
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search", RuleJSON: `{"args":{"q":{"type":"string","maxLength":8}},"additional_args":true}`})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "fetch"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "peer", Tool: "fetch"})
	if _, err := mcpAdmin(mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search", RuleJSON: `{"args":{"q":{"format":"x"}}}`}); err == nil {
		t.Fatal("unknown arg-rule keyword accepted")
	}

	st, err := mcpStore()
	if err != nil {
		t.Fatal(err)
	}
	reg := st.Current()
	eA, err := reg.Effective("docs", "tg")
	if err != nil || fmt.Sprint(eA.Names()) != "[fetch search]" {
		t.Fatalf("tg effective = %v, %v", eA.Names(), err)
	}
	eB, _ := reg.Effective("docs", "peer")
	if fmt.Sprint(eB.Names()) != "[fetch]" || eB.Allowed("search") {
		t.Fatalf("peer effective = %v", eB.Names())
	}
	if _, err := reg.Effective("docs", "main"); !errors.Is(err, mcpreg.ErrNotWhitelisted) {
		t.Fatalf("main (not whitelisted) err = %v", err)
	}
	// additional_args in the grant cannot admit an arg the ceiling refuses
	if err := eA.Check("search", json.RawMessage(`{"q":"ok","extra":1}`)); err == nil {
		t.Fatal("grant additional_args widened the ceiling")
	}
	if err := eA.Check("search", json.RawMessage(`{"q":"toolongforgrant"}`)); err == nil {
		t.Fatal("grant narrowing (maxLength 8) not applied")
	}
	if err := eA.Check("search", json.RawMessage(`{"q":"short"}`)); err != nil {
		t.Fatalf("valid call refused: %v", err)
	}
	ev := mcpJSON(mcpOp(t, mcpAdminReq{Op: "effective", Group: "main"}))
	if strings.Contains(ev, "fetch") {
		t.Fatalf("un-whitelisted group sees tools: %s", ev)
	}

	// revoke applies to the next registry read
	mcpOp(t, mcpAdminReq{Op: "revoke", Conn: "docs", Group: "tg", Tool: "fetch"})
	if e, _ := st.Current().Effective("docs", "tg"); e.Allowed("fetch") {
		t.Fatal("revoke did not apply")
	}
	mcpOp(t, mcpAdminReq{Op: "rate", Conn: "docs", Group: "tg", PerMinute: 30})
	mcpOp(t, mcpAdminReq{Op: "deny", Conn: "docs", Group: "peer"})
	if _, err := st.Current().Effective("docs", "peer"); err == nil {
		t.Fatal("deny did not remove the whitelist entry")
	}
	// re-adding the connection keeps the whitelist and the stored secret
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search"}})
	c := st.Current().Conns["docs"]
	if c.Auth == nil || c.Auth.Value != mcpTestSecret || c.Groups["tg"] == nil {
		t.Fatal("re-add dropped auth or grants")
	}
	if e, _ := st.Current().Effective("docs", "tg"); fmt.Sprint(e.Names()) != "[search]" {
		t.Fatalf("after ceiling shrink: %v", e.Names())
	}
	// the secret is on disk only in the 0600 file, never in a log line
	mcpOp(t, mcpAdminReq{Op: "rm", Conn: "docs"})
	if len(st.Current().Conns) != 0 {
		t.Fatal("rm left the connection")
	}
}

func TestMcpAdminRejects(t *testing.T) {
	mcpHarness(t)
	for _, r := range []mcpAdminReq{
		{Op: "frob"},
		{Op: "add", Conn: "Bad Name", URL: "https://x.example/mcp"},
		{Op: "add", Conn: "x", URL: "file:///etc/passwd"},
		{Op: "add", Conn: "x", URL: "https://x.example/mcp", AuthName: "Authorization"},
		{Op: "add", Conn: "x", URL: "https://x.example/mcp", Tools: []string{"a"}, RulesJSON: `{"b":{}}`},
		{Op: "add", Conn: "x", URL: "https://x.example/mcp", Class: "admin"},
		{Op: "show", Conn: "nope"},
		{Op: "rm", Conn: "nope"},
		{Op: "effective", Group: "../x"},
	} {
		if _, err := mcpAdmin(r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestMcpCliParse(t *testing.T) {
	files := map[string]string{
		"auth":  "Authorization: Bearer " + mcpTestSecret + "\n",
		"rules": `{"search":{}}`,
		"bad":   "no colon here",
	}
	rf := func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	r, err := mcpCliParse([]string{"add", "docs", "--url", "https://x.example/mcp", "--tools=search,fetch", "--rules", "rules", "--auth-file", "auth"}, rf)
	if err != nil || r.Op != "add" || r.Conn != "docs" || fmt.Sprint(r.Tools) != "[search fetch]" ||
		r.AuthName != "Authorization" || r.AuthValue != "Bearer "+mcpTestSecret || r.RulesJson != files["rules"] {
		t.Fatalf("add: %+v %v", r, err)
	}
	r, err = mcpCliParse([]string{"grant", "docs", "tg", "search", "--args", `{"args":{"q":{"maxLength":8}}}`}, rf)
	if err != nil || r.Group != "tg" || r.Tool != "search" || !strings.Contains(r.RuleJson, "maxLength") {
		t.Fatalf("grant: %+v %v", r, err)
	}
	if r, err = mcpCliParse([]string{"rate", "docs", "tg", "30"}, rf); err != nil || r.PerMinute != 30 {
		t.Fatalf("rate: %+v %v", r, err)
	}
	if r, err = mcpCliParse([]string{"effective", "tg"}, rf); err != nil || r.Group != "tg" || r.Conn != "" {
		t.Fatalf("effective: %+v %v", r, err)
	}
	if r, err = mcpCliParse(nil, rf); err != nil || r.Op != "ls" {
		t.Fatalf("default: %+v %v", r, err)
	}
	for _, args := range [][]string{
		{"frob"},
		{"add", "docs"}, // no --url
		{"add", "docs", "--url", "u", "--auth-file", "bad"},
		{"add", "docs", "--url", "u", "--auth-file", "missing"},
		{"grant", "docs", "tg"}, // missing tool
		{"allow", "docs", "tg", "extra"},
		{"rate", "docs", "tg", "-1"},
		{"rate", "docs", "tg", "x"},
		{"rate", "docs", "tg", "4294967297"},
		{"allow", "docs", "tg", "--url", "u"},         // add-only flag
		{"revoke", "docs", "tg", "a", "--args", "{}"}, // grant-only flag
		{"show", "docs", "--bogus", "1"},
		{"show", "docs", "--url"}, // flag without value
	} {
		if _, err := mcpCliParse(args, rf); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

// enable/disable through the admin op: disabled == absent in the harness
// config and the effective view, grants kept, re-enable restores; the view
// reports enabled, never the secret.
func TestMcpAdminEnableDisable(t *testing.T) {
	mcpHarness(t)
	t.Setenv("KOTO_MCP_GATEWAY", "1")
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp",
		Tools: []string{"search", "fetch"}, AuthName: "Authorization", AuthValue: mcpTestSecret})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search"})
	if s := mcpServersFor("tg"); len(s) != 1 {
		t.Fatalf("enabled servers = %v", s)
	}
	v := mcpOp(t, mcpAdminReq{Op: "disable", Conn: "docs"})
	if v["enabled"] != false || strings.Contains(mcpJSON(v), mcpTestSecret) {
		t.Fatalf("disable view: %s", mcpJSON(v))
	}
	if s := mcpServersFor("tg"); len(s) != 0 {
		t.Fatalf("disabled conn rendered into harness config: %v", s)
	}
	ls := mcpJSON(mcpOp(t, mcpAdminReq{Op: "ls"}))
	if !strings.Contains(ls, `"enabled":false`) || strings.Contains(ls, mcpTestSecret) {
		t.Fatalf("ls: %s", ls)
	}
	eff := mcpJSON(mcpOp(t, mcpAdminReq{Op: "effective", Group: "tg"}))
	if !strings.Contains(eff, `"tools":[]`) || !strings.Contains(eff, `"tools_when_enabled":["search"]`) {
		t.Fatalf("effective while disabled: %s", eff)
	}
	reg := mustStore(t).Current()
	if e, err := reg.Effective("docs", "tg"); !errors.Is(err, mcpreg.ErrNotWhitelisted) || len(e.Tools) != 0 {
		t.Fatalf("gateway policy while disabled: %v %v", err, e.Names())
	}
	if g := reg.Conns["docs"].Groups["tg"]; g == nil || len(g.Tools) != 1 {
		t.Fatalf("grant lost on disable: %+v", g)
	}
	if _, ok := reg.Conns["docs"].Groups["tg"].Tools["search"]; !ok {
		t.Fatal("search grant lost on disable")
	}
	v = mcpOp(t, mcpAdminReq{Op: "enable", Conn: "docs"})
	if v["enabled"] != true {
		t.Fatalf("enable view: %s", mcpJSON(v))
	}
	if s := mcpServersFor("tg"); len(s) != 1 || s[0] != "docs" {
		t.Fatalf("re-enabled servers = %v", s)
	}
	eff = mcpJSON(mcpOp(t, mcpAdminReq{Op: "effective", Group: "tg"}))
	if !strings.Contains(eff, `"tools":["search"]`) || !strings.Contains(eff, `"enabled":true`) {
		t.Fatalf("effective after enable: %s", eff)
	}
	if _, err := mcpAdmin(mcpAdminReq{Op: "disable", Conn: "ghost"}); err == nil {
		t.Fatal("disable on unknown connection accepted")
	}
	// CLI parse
	for _, op := range []string{"enable", "disable"} {
		req, err := mcpCliParse([]string{op, "docs"}, nil)
		if err != nil || req.Op != op || req.Conn != "docs" {
			t.Fatalf("parse %s: %v %+v", op, err, req)
		}
		if _, err := mcpCliParse([]string{op}, nil); err == nil {
			t.Fatalf("%s without conn accepted", op)
		}
	}
}

func mustStore(t *testing.T) *mcpreg.Store {
	t.Helper()
	st, err := mcpStore()
	if err != nil {
		t.Fatal(err)
	}
	return st
}
