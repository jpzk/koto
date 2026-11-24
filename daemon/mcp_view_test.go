package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"koto/mcpreg"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// mcpViewSentinels maps every secret-bearing registry field to a unique
// marker. Branches that add secret fields extend it (mcpViewSeedExt).
var mcpViewSentinels = map[string]string{
	"header_value": "SENTINEL-hdr-value-7f3a",
	"pat":          "SENTINEL-pat-token-91cc",
	"url_query":    "SENTINEL-url-query-55d0",
}

// mcpViewSeedExt lets a branch seed extra connections / secrets.
var mcpViewSeedExt func(t *testing.T)

func mcpViewSeed(t *testing.T) {
	t.Helper()
	s := mcpViewSentinels
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "hdr", URL: "https://up.example.com:8443/mcp/v1?key=" + s["url_query"],
		Class: "read", Tools: []string{"search", "fetch"},
		RulesJSON: `{"search":{"args":{"q":{"type":"string","maxLength":64}}}}`,
		AuthName:  "X-Api-Key", AuthValue: s["header_value"]})
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "pat", URL: "https://api.example.org/mcp", Class: "read",
		Tools: []string{"list_issues"}, AuthName: "Authorization", AuthValue: "Bearer " + s["pat"]})
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "open", URL: "http://10.0.2.2:9100/mcp", Class: "read", Tools: []string{"add"}})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "hdr", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "hdr", Group: "tg", Tool: "search",
		RuleJSON: `{"args":{"q":{"type":"string","maxLength":16}}}`})
	mcpOp(t, mcpAdminReq{Op: "rate", Conn: "hdr", Group: "tg", PerMinute: 30})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "pat", Group: "peer"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "pat", Group: "peer", Tool: "list_issues"})
	if mcpViewSeedExt != nil {
		mcpViewSeedExt(t)
	}
}

// mcpViewCall runs one line through BOTH ctl faces (typed JSON core and the
// protobuf adapter the guest actually talks to) and returns every encoding a
// caller could see, plus the decoded JSON core answer.
func mcpViewCall(t *testing.T, owner, line string) (map[string]any, [][]byte) {
	t.Helper()
	resp := ctlDispatch(owner, []byte(line))
	core, _ := json.Marshal(resp)
	var m map[string]any
	_ = json.Unmarshal(core, &m)
	outs := [][]byte{core}
	req, err := ctlRequestFromJSON([]byte(line))
	if err != nil {
		t.Fatalf("%s: pb decode: %v", line, err)
	}
	pr := ctlDispatchPB(owner, req)
	wire, _ := proto.Marshal(pr)
	pj, _ := protojson.Marshal(pr)
	outs = append(outs, wire, pj)
	if pr.Ok != (m["ok"] == true) {
		t.Fatalf("%s: pb ok=%v core=%s", line, pr.Ok, core)
	}
	return m, outs
}

func mcpViewNoSentinel(t *testing.T, what string, outs [][]byte) {
	t.Helper()
	for _, b := range outs {
		for field, s := range mcpViewSentinels {
			if bytes.Contains(b, []byte(s)) {
				t.Fatalf("%s: %s leaked: %s", what, field, b)
			}
		}
	}
}

func mcpViewOps() []string {
	ops := []string{
		`{"cmd":"mcp_view"}`,
		`{"cmd":"mcp_view","op":"ls"}`,
		`{"cmd":"mcp_view","op":"effective","group":"tg"}`,
		`{"cmd":"mcp_view","op":"effective","group":"peer"}`,
		`{"cmd":"mcp_view","op":"effective","group":"main"}`,
	}
	st, _ := mcpStore()
	for _, n := range sortedKeysOf(st.Current().Conns) {
		ops = append(ops, fmt.Sprintf(`{"cmd":"mcp_view","op":"show","conn":%q}`, n))
	}
	return ops
}

// Every op, every encoding: no secret ever appears, and the projection
// carries what the coordinator needs.
func TestMcpViewSentinel(t *testing.T) {
	mcpHarness(t)
	mcpViewSeed(t)
	ops := mcpViewOps()
	if len(ops) < 8 {
		t.Fatalf("only %d ops", len(ops))
	}
	for _, l := range ops {
		m, outs := mcpViewCall(t, ctlMainGroup, l)
		if m["ok"] != true {
			t.Fatalf("%s: %v", l, m)
		}
		mcpViewNoSentinel(t, l, outs)
	}
	// Shape: auth kind only, header names only, rules and grants present.
	m, _ := mcpViewCall(t, ctlMainGroup, `{"cmd":"mcp_view","op":"show","conn":"hdr"}`)
	c := m["mcp"].(map[string]any)["conn"].(map[string]any)
	auth := c["auth"].(map[string]any)
	if auth["kind"] != "header" || auth["header"] != "X-Api-Key" || auth["status"] != "ok" || len(auth) != 3 {
		t.Fatalf("hdr auth = %v", auth)
	}
	if c["url"] != "https://up.example.com:8443/mcp/v1" || c["enabled"] != true || c["class"] != "read" {
		t.Fatalf("hdr conn = %v", c)
	}
	js := mcpJSON(c)
	for _, want := range []string{`"name":"search"`, `"max_length":64`, `"group":"tg"`, `"per_minute":30`, `"max_length":16`} {
		if !strings.Contains(js, want) {
			t.Fatalf("show hdr missing %s: %s", want, js)
		}
	}
	m, _ = mcpViewCall(t, ctlMainGroup, `{"cmd":"mcp_view","op":"show","conn":"pat"}`)
	if a := m["mcp"].(map[string]any)["conn"].(map[string]any)["auth"].(map[string]any); a["kind"] != "pat" {
		t.Fatalf("pat auth = %v", a)
	}
	m, _ = mcpViewCall(t, ctlMainGroup, `{"cmd":"mcp_view","op":"show","conn":"open"}`)
	if a := m["mcp"].(map[string]any)["conn"].(map[string]any)["auth"].(map[string]any); a["kind"] != "none" {
		t.Fatalf("open auth = %v", a)
	}
	m, _ = mcpViewCall(t, ctlMainGroup, `{"cmd":"mcp_view","op":"effective","group":"tg"}`)
	js = mcpJSON(m["mcp"])
	if !strings.Contains(js, `"conn":"hdr"`) || !strings.Contains(js, `"name":"search"`) ||
		strings.Contains(js, `"name":"fetch"`) || strings.Contains(js, `"conn":"pat"`) {
		t.Fatalf("effective tg = %s", js)
	}
}

// Only main may read: every peer is refused, and the refusal carries no data.
func TestMcpViewPeerRefused(t *testing.T) {
	mcpHarness(t)
	mcpViewSeed(t)
	for _, g := range []string{"tg", "peer"} {
		for _, l := range mcpViewOps() {
			m, outs := mcpViewCall(t, g, l)
			if m["ok"] == true || !strings.Contains(fmt.Sprint(m["error"]), "not allowed for non-main") || m["mcp"] != nil {
				t.Fatalf("%s from %s: %v", l, g, m)
			}
			mcpViewNoSentinel(t, l, outs)
			for _, b := range outs {
				if bytes.Contains(b, []byte("example")) {
					t.Fatalf("%s from %s: registry data in refusal: %s", l, g, b)
				}
			}
		}
	}
}

// mcp_view is read-only for main too: write ops are refused and the registry
// file is byte-identical afterwards; the strict pb face refuses write-shaped
// fields outright.
func TestMcpViewWriteRefused(t *testing.T) {
	mcpHarness(t)
	mcpViewSeed(t)
	before, err := os.ReadFile(mcpRegistryPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"add", "rm", "allow", "deny", "grant", "revoke", "rate", "enable", "disable", "set", "LS2"} {
		l := fmt.Sprintf(`{"cmd":"mcp_view","op":%q,"conn":"hdr","group":"peer"}`, op)
		m, outs := mcpViewCall(t, ctlMainGroup, l)
		if m["ok"] == true || !strings.Contains(fmt.Sprint(m["error"]), "read-only") {
			t.Fatalf("%s: %v", l, m)
		}
		mcpViewNoSentinel(t, l, outs)
	}
	for _, l := range []string{
		`{"cmd":"mcp_view","op":"grant","conn":"hdr","group":"peer","tool":"fetch"}`,
		`{"cmd":"mcp_view","op":"add","conn":"x","url":"https://evil.example/"}`,
		`{"cmd":"mcp_view","op":"rate","conn":"hdr","group":"tg","per_minute":0}`,
	} {
		if _, err := ctlRequestFromJSON([]byte(l)); err == nil {
			t.Fatalf("%s: strict pb accepted a write-shaped field", l)
		}
		// The JSON core ignores the extra fields and still refuses the op.
		if b, _ := json.Marshal(ctlDispatch(ctlMainGroup, []byte(l))); bytes.Contains(b, []byte(`"ok":true`)) {
			t.Fatalf("%s: accepted: %s", l, b)
		}
	}
	after, _ := os.ReadFile(mcpRegistryPath())
	if !bytes.Equal(before, after) {
		t.Fatal("registry changed through mcp_view")
	}
}

func TestMcpViewURLRedaction(t *testing.T) {
	for in, want := range map[string]string{
		"https://up.example.com/mcp":                       "https://up.example.com/mcp",
		"https://up.example.com:8443/a/b?token=x&y=z":      "https://up.example.com:8443/a/b",
		"https://user:pw@up.example.com/mcp?k=v#frag":      "https://up.example.com/mcp",
		"http://10.0.2.2:9100/mcp?":                        "http://10.0.2.2:9100/mcp",
		"https://[::1]:9/x%2Fy?q=1":                        "https://[::1]:9/x%2Fy",
		"https://up.example.com":                           "https://up.example.com",
		"ftp://up.example.com/x":                           "",
		"not a url":                                        "",
		"https://user@up.example.com/p;params?q=SENTINEL-": "https://up.example.com/p;params",
	} {
		if got := mcpViewURL(in); got != want {
			t.Errorf("mcpViewURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The answer is bounded: ls drops trailing connections and says so; a single
// show that cannot fit is refused rather than truncated mid-object.
func TestMcpViewSizeCap(t *testing.T) {
	big := func(n int) *mcpreg.Conn {
		c := &mcpreg.Conn{URL: "https://up.example.com/mcp", Class: mcpreg.ClassRead, Tools: map[string]*mcpreg.ToolRule{}}
		for i := 0; i < n; i++ {
			c.Tools[fmt.Sprintf("%0120d", i)] = nil
		}
		return c
	}
	reg := mcpreg.New()
	for i := 0; i < 40; i++ {
		reg.Conns[fmt.Sprintf("c%02d", i)] = big(30)
	}
	v, err := mcpViewBuild(reg, ctlMcpViewReq{Op: "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Truncated || len(v.Conns) == 0 || len(v.Conns) == 40 || mcpViewSize(v) > mcpViewMax {
		t.Fatalf("ls: truncated=%v conns=%d size=%d", v.Truncated, len(v.Conns), mcpViewSize(v))
	}

	mcpHarness(t)
	st, err := mcpStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(r *mcpreg.Registry) error { return r.PutConn("huge", big(700), false) }); err != nil {
		t.Fatal(err)
	}
	m, _ := mcpViewCall(t, ctlMainGroup, `{"cmd":"mcp_view","op":"show","conn":"huge"}`)
	if m["ok"] == true || !strings.Contains(fmt.Sprint(m["error"]), "exceeds") {
		t.Fatalf("huge show: %.200s", mcpJSON(m))
	}
}
