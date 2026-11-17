package main

// mcp_view_test.go — the MCP registry view (alt+m, /mcp): key is free and
// toggles, list renders name / auth set-unset / class / #groups / host-only /
// tools, the upstream URL never reaches the screen beyond its host, a
// non-admin cert gets a read-only refusal, and help + palette list it.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"koto-protocol/pb"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func altM() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}, Alt: true} }

// stubMcp swaps the Mcp rpc for the test; returns the captured requests.
func stubMcp(t *testing.T, fn func(*pb.McpReq) (*pb.McpResp, error)) *[]*pb.McpReq {
	t.Helper()
	var got []*pb.McpReq
	old := mcpRPC
	mcpRPC = func(_ context.Context, r *pb.McpReq) (*pb.McpResp, error) {
		got = append(got, r)
		return fn(r)
	}
	t.Cleanup(func() { mcpRPC = old })
	return &got
}

const mcpTestSecretURL = "https://user:pw@api.example.com:8443/v1/mcp?token=QUERYSECRET#frag"

func mcpLsView(t *testing.T) *structpb.Struct {
	t.Helper()
	v, err := structpb.NewStruct(map[string]any{"conns": []any{
		map[string]any{"name": "linear", "url": mcpTestSecretURL, "class": "read", "auth": true,
			"tools": []any{"get_issue", "list_issues"}, "groups": []any{"main", "researcher"}, "enabled": true},
		map[string]any{"name": "wiki", "url": "http://wiki.internal/mcp", "class": "read", "auth": false,
			"tools": []any{"search"}, "groups": []any{}, "enabled": false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// mcpModel: view opened with alt+m and the fixture ls consumed through the
// real command path (stubbed rpc).
func mcpModel(t *testing.T) Model {
	t.Helper()
	stubMcp(t, func(r *pb.McpReq) (*pb.McpResp, error) {
		return &pb.McpResp{Ok: true, View: mcpLsView(t)}, nil
	})
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	m = nm.(Model)
	if m.focus != focusMcp {
		t.Fatalf("focus = %v after alt+m, want focusMcp", m.focus)
	}
	if cmd == nil {
		t.Fatal("opening the MCP view must dispatch an ls")
	}
	nm, _ = m.Update(cmd())
	return nm.(Model)
}

func TestMcpKeyIsFree(t *testing.T) {
	bound := func(km any) map[string]bool {
		out := map[string]bool{}
		v := reflect.ValueOf(km)
		for i := 0; i < v.NumField(); i++ {
			if b, ok := v.Field(i).Interface().(key.Binding); ok {
				for _, k := range b.Keys() {
					out[k] = true
				}
			}
		}
		return out
	}
	ti, ta := bound(textinput.DefaultKeyMap), bound(textarea.DefaultKeyMap)
	if !ti["alt+b"] {
		t.Fatal("keymap walk broken: alt+b (word back) not found")
	}
	if ti["alt+m"] || ta["alt+m"] {
		t.Fatal("alt+m is bound by bubbles")
	}
}

func TestMcpViewToggles(t *testing.T) {
	m := mcpModel(t)
	nm, _ := m.Update(altM())
	m = nm.(Model)
	if m.focus != focusInput {
		t.Fatalf("focus = %v after second alt+m, want focusInput", m.focus)
	}
	m = pressTree(t, m)
	nm, _ = m.Update(altM())
	m = press(t, nm.(Model), tea.KeyEsc)
	if m.focus != focusTree {
		t.Fatalf("focus = %v after esc, want focusTree (pre-open focus)", m.focus)
	}
}

func TestMcpSlashOpens(t *testing.T) {
	stubMcp(t, func(*pb.McpReq) (*pb.McpResp, error) { return &pb.McpResp{Ok: true, View: mcpLsView(t)}, nil })
	m := focusModel(t)
	m.input.SetValue("/mcp")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := nm.(Model).focus; got != focusMcp {
		t.Fatalf("/mcp focus = %v, want focusMcp", got)
	}
}

func TestMcpViewRendersRows(t *testing.T) {
	m := mcpModel(t)
	if m.mcp.idx != 0 || len(m.mcp.items) != 2 {
		t.Fatalf("items = %d", len(m.mcp.items))
	}
	out := stripANSI(m.View())
	for _, want := range []string{"NAME", "AUTH", "CLASS", "GRP", "HOST", "TOOLS",
		"linear", "wiki", "api.example.com:8443", "wiki.internal", "get_issue,list_issues",
		"2 MCP connections · 1 enabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	// auth column: set for linear, unset for wiki
	var linRow, wikiRow string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "linear") {
			linRow = l
		}
		if strings.Contains(l, "wiki ") {
			wikiRow = l
		}
	}
	if !strings.Contains(linRow, " set ") || !strings.Contains(wikiRow, "unset") {
		t.Errorf("auth column wrong:\n%s\n%s", linRow, wikiRow)
	}
	if !strings.Contains(linRow, " 2 ") || !strings.Contains(wikiRow, " 0 ") {
		t.Errorf("group counts wrong:\n%s\n%s", linRow, wikiRow)
	}
	// host only: no userinfo, path, query or fragment on screen
	for _, bad := range []string{"user", "pw@", "QUERYSECRET", "/v1/mcp", "frag", "https://"} {
		if strings.Contains(out, bad) {
			t.Errorf("view leaks %q", bad)
		}
	}
	// the model keeps no URL at all
	if strings.Contains(strings.ToLower(reflect.TypeOf(mcpConn{}).String()+fieldNames(mcpConn{})), "url") {
		t.Error("mcpConn grew a URL field")
	}
	// narrow: HOST drops whole, NAME/AUTH/TOOLS stay
	m.width = 60
	m.resizeMcpViewport()
	m.refreshMcpViewport()
	out = stripANSI(m.View())
	if strings.Contains(out, "HOST") || !strings.Contains(out, "linear") || !strings.Contains(out, "AUTH") {
		t.Errorf("60-col degrade wrong:\n%s", out)
	}
	// selection moves
	m = press(t, m, tea.KeyDown)
	if s := m.mcpSelected(); s == nil || s.Name != "wiki" {
		t.Fatalf("down: selected %+v", s)
	}
}

func fieldNames(v any) string {
	t := reflect.TypeOf(v)
	var b strings.Builder
	for i := 0; i < t.NumField(); i++ {
		b.WriteString(t.Field(i).Name + " ")
	}
	return b.String()
}

func TestMcpHost(t *testing.T) {
	for in, want := range map[string]string{
		mcpTestSecretURL:           "api.example.com:8443",
		"http://wiki.internal/mcp": "wiki.internal",
		"https://[::1]:9000/x?y=z": "[::1]:9000",
		"not a url":                "?",
		"":                         "?",
	} {
		if got := mcpHost(in); got != want {
			t.Errorf("mcpHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMcpNonAdminReadOnly: a PermissionDenied ls renders the refusal (no
// table, no daemon role wording) and the view stays usable/closable.
func TestMcpNonAdminReadOnly(t *testing.T) {
	stubMcp(t, func(*pb.McpReq) (*pb.McpResp, error) {
		return nil, status.Error(codes.PermissionDenied, "roles [peer] may not call mcp")
	})
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	nm, _ = nm.(Model).Update(cmd())
	m = nm.(Model)
	if !m.mcp.denied {
		t.Fatal("PermissionDenied not recognised as non-admin")
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "admin-only") || !strings.Contains(out, "read-only") {
		t.Errorf("refusal missing:\n%s", out)
	}
	if strings.Contains(out, "roles [peer]") {
		t.Error("daemon role wording echoed")
	}
	m = press(t, m, tea.KeyEsc)
	if m.focus != focusInput {
		t.Fatalf("esc focus = %v", m.focus)
	}
}

func TestMcpListedInHelpAndPalette(t *testing.T) {
	h := stripANSI(helpContent(200))
	for _, want := range []string{"alt+m", "MCP VIEW (alt+m)", "/mcp"} {
		if !strings.Contains(h, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	m := pressPalette(t, focusModel(t), "open MCP connections view")
	_ = m
}
