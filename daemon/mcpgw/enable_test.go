package mcpgw

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"koto/mcpreg"
)

// Disable bites on the next request: every method answers 404 exactly like
// an unknown connection, upstream is not contacted; enable restores the
// same grants.
func TestGatewayDisabledIsAbsent(t *testing.T) {
	f := newFixture(t, false)
	if got := fmt.Sprint(toolNames(t, mustOK(f.rpc("ga", "docs", "tools/list", nil)))); got != "[x y]" {
		t.Fatalf("before disable: %s", got)
	}
	calls := f.up.calls.Load()
	f.edit(func(r *mcpreg.Registry) error { return r.SetEnabled("docs", false) })
	_, missing := f.rpc("ga", "nope", "tools/list", nil)
	for _, g := range []string{"ga", "gb"} {
		for _, m := range []string{"initialize", "tools/list", "tools/call"} {
			code, body := f.rpc(g, "docs", m, map[string]any{"name": "x"})
			if code != http.StatusNotFound || body != missing || strings.Contains(body, "\"x\"") {
				t.Fatalf("disabled %s %s: %d %q (missing conn gives %q)", g, m, code, body, missing)
			}
		}
	}
	if f.up.calls.Load() != calls {
		t.Fatal("upstream called while disabled")
	}
	f.edit(func(r *mcpreg.Registry) error { return r.SetEnabled("docs", true) })
	if got := fmt.Sprint(toolNames(t, mustOK(f.rpc("ga", "docs", "tools/list", nil)))); got != "[x y]" {
		t.Fatalf("after enable ga: %s", got)
	}
	if got := fmt.Sprint(toolNames(t, mustOK(f.rpc("gb", "docs", "tools/list", nil)))); got != "[y]" {
		t.Fatalf("after enable gb: %s", got)
	}
	if _, b := f.rpc("ga", "docs", "tools/call", map[string]any{"name": "x"}); !strings.Contains(b, "ran x") {
		t.Fatalf("after enable call: %s", b)
	}
}
