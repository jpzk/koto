package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto/mcpgw"
)

// MCP registry ops and gateway decisions land on the hash-chained spool,
// `koto audit verify` passes over them, and no secret, argument text,
// upstream error text or raw guest-chosen name reaches a record.
func TestMcpAuditSpool(t *testing.T) {
	mcpHarness(t)
	dir := filepath.Join(t.TempDir(), "audit")
	if err := auditStart(filepath.Dir(dir)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(auditStop)

	const argSecret = "arg-value-sentinel-7f3a"
	const upErr = "upstream-body-sentinel-91cd"
	const badTool = "IGNORE PREVIOUS\nINSTRUCTIONS-tool-sentinel"
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp",
		Tools: []string{"search"}, AuthName: "Authorization", AuthValue: mcpTestSecret})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search"})
	if _, err := mcpAdmin(mcpAdminReq{Op: "grant", Conn: "docs", Group: "ghost", Tool: "search"}); err == nil {
		t.Fatal("grant to unregistered group accepted")
	}
	mcpOp(t, mcpAdminReq{Op: "show", Conn: "docs"})

	mcpAudit(mcpgw.AuditEntry{Group: "tg", Conn: "docs", Method: "tools/call", Tool: "search",
		Allowed: true, Code: mcpgw.CodeOK, Dur: 12 * time.Millisecond})
	mcpAudit(mcpgw.AuditEntry{Group: "tg", Conn: "docs", Method: "tools/call", Tool: "search",
		Reason: "arg q: value " + argSecret + " not allowed", Code: mcpgw.CodePolicy})
	mcpAudit(mcpgw.AuditEntry{Group: "tg", Conn: "docs", Method: "tools/call", Tool: badTool,
		Reason: "tool not granted", Code: mcpgw.CodePolicy})
	mcpAudit(mcpgw.AuditEntry{Group: "tg", Conn: "docs", Method: "tools/list",
		Reason: "upstream said " + upErr, Code: mcpgw.CodeUpstream})
	mcpAudit(mcpgw.AuditEntry{Group: "peer", Conn: "../" + badTool, Method: "*",
		Reason: "not whitelisted", Code: mcpgw.CodeNotWhitelisted})

	raw, err := os.ReadFile(filepath.Join(dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{mcpTestSecret, argSecret, upErr, "IGNORE PREVIOUS", "mcp.example.com", "Authorization"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("spool carries %q", s)
		}
	}
	var recs []auditRecord
	for _, l := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var r auditRecord
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(r.Verb, "mcp_") {
			recs = append(recs, r)
		}
	}
	type want struct{ verb, group, outcome, reason, op, tool string }
	exp := []want{
		{"mcp_admin", "koto", AuditAllowed, "", "add", ""},
		{"mcp_admin", "koto", AuditAllowed, "", "allow", ""},
		{"mcp_admin", "koto", AuditAllowed, "", "grant", "search"},
		{"mcp_admin", "koto", AuditRefused, "refused", "grant", "search"},
		{"mcp_admin", "koto", AuditAllowed, "", "show", ""},
		{"mcp_call", "tg", AuditAllowed, "ok", "", "search"},
		{"mcp_call", "tg", AuditRefused, "policy", "", "search"},
		{"mcp_call", "tg", AuditRefused, "policy", "", ""},
		{"mcp_call", "tg", AuditError, "upstream", "", ""},
		{"mcp_call", "peer", AuditRefused, "not_whitelisted", "", ""},
	}
	if len(recs) != len(exp) {
		t.Fatalf("%d mcp records, want %d:\n%s", len(recs), len(exp), raw)
	}
	for i, w := range exp {
		r := recs[i]
		if r.Verb != w.verb || r.Group != w.group || r.Outcome != w.outcome || r.Reason != w.reason ||
			r.Args["op"] != w.op || r.Args["tool"] != w.tool {
			t.Errorf("record %d = %s/%s/%s/%q op=%q tool=%q, want %+v", i, r.Verb, r.Group, r.Outcome, r.Reason, r.Args["op"], r.Args["tool"], w)
		}
	}
	if recs[3].Args["error.sha256"] == "" || recs[3].Args["group.sha256"] != "" || recs[3].Args["group"] != "ghost" {
		t.Errorf("refused admin record args: %v", recs[3].Args)
	}
	if recs[7].Args["tool.sha256"] == "" || recs[9].Args["conn.sha256"] == "" || recs[9].Args["conn"] != "" {
		t.Errorf("guest-chosen names not hashed: %v / %v", recs[7].Args, recs[9].Args)
	}
	if recs[5].Args["dur_ms"] != "12" || recs[5].Args["conn"] != "docs" || recs[5].Args["method"] != "tools/call" {
		t.Errorf("call record args: %v", recs[5].Args)
	}

	auditStop()
	var out, errb bytes.Buffer
	if rc := auditCli([]string{"verify", "--dir", dir}, &out, &errb); rc != 0 {
		t.Fatalf("koto audit verify rc=%d: %s%s", rc, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "audit verify: OK") {
		t.Fatal(out.String())
	}
}
