package mcpgw

import (
	"testing"

	"koto/mcpreg"
)

// Every decision carries a fixed Code, the only reason text a durable audit
// record keeps.
func TestGatewayAuditCodes(t *testing.T) {
	f := newFixture(t, false)
	last := func() AuditEntry {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.audit) == 0 {
			t.Fatal("no audit entry")
		}
		return f.audit[len(f.audit)-1]
	}
	check := func(what string, allowed bool, code string) {
		t.Helper()
		e := last()
		if e.Allowed != allowed || e.Code != code {
			t.Errorf("%s: allowed=%v code=%q, want %v %q (reason %q)", what, e.Allowed, e.Code, allowed, code, e.Reason)
		}
	}
	f.rpc("ga", "docs", "tools/list", nil)
	check("list", true, CodeOK)
	f.rpc("ga", "docs", "tools/call", map[string]any{"name": "x"})
	check("call x", true, CodeOK)
	f.rpc("gb", "docs", "tools/call", map[string]any{"name": "y", "arguments": map[string]any{"q": "much-too-long"}})
	check("call y bad arg", false, CodePolicy)
	f.rpc("gb", "docs", "tools/call", map[string]any{"name": "z"})
	check("call z ungranted", false, CodePolicy)
	f.rpc("gc", "docs", "tools/list", nil)
	check("not whitelisted", false, CodeNotWhitelisted)
	f.edit(func(r *mcpreg.Registry) error { return r.SetRate("docs", "ga", 1) })
	f.rpc("ga", "docs", "tools/call", map[string]any{"name": "x"})
	f.rpc("ga", "docs", "tools/call", map[string]any{"name": "x"})
	check("rate", false, CodeRateLimited)
	f.up.mu.Lock()
	f.up.descY = "drifted"
	f.up.mu.Unlock()
	f.rpc("gb", "docs", "tools/list", nil) // y no longer matches its pin
	f.rpc("gb", "docs", "tools/call", map[string]any{"name": "y", "arguments": map[string]any{"q": "a"}})
	check("schema", false, CodeSchemaChanged)
	f = newFixture(t, false)
	f.srv.Close()
	f.rpc("ga", "docs", "tools/list", nil)
	check("upstream down", false, CodeUpstream)
}
