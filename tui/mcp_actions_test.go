package main

// mcp_actions_test.go — MCP view actions: masked add (typed + 0600 file),
// the auth value never rendered / formatted / kept after submit, typed-name
// delete, enable/disable toggle, detail with the daemon's effective set,
// non-admin refusal of actions.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const mcpTestSecret = "TOPSECRET-auth-9f3c"

// mcpOpsModel opens the view with a stub that answers per op.
func mcpOpsModel(t *testing.T, extra func(*pb.McpReq) *structpb.Struct) (Model, *[]*pb.McpReq) {
	t.Helper()
	got := stubMcp(t, func(r *pb.McpReq) (*pb.McpResp, error) {
		if extra != nil {
			if v := extra(r); v != nil {
				return &pb.McpResp{Ok: true, View: v}, nil
			}
		}
		return &pb.McpResp{Ok: true, View: mcpLsView(t)}, nil
	})
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	nm, _ = nm.(Model).Update(cmd())
	*got = nil
	return nm.(Model), got
}

func typeStr(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	return m
}

func mcpKey(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(k)
	return nm.(Model), cmd
}

// mcpSettle executes a command chain (cmd -> msg -> Update) until it settles.
func mcpSettle(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil && i < 8; i++ {
		msg := cmd()
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				if c != nil {
					m = mcpSettle(t, m, c)
				}
			}
			return m
		}
		var nm tea.Model
		nm, cmd = m.Update(msg)
		m = nm.(Model)
	}
	return m
}

func fillAddBase(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if m.mcp.mode != mcpModeAdd || m.mcp.add == nil {
		t.Fatalf("a: mode = %v", m.mcp.mode)
	}
	m = typeStr(t, m, "gh")
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = typeStr(t, m, "https://mcp.github.example/mcp")
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = typeStr(t, m, "search,get_file")
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyTab}) // class (prefilled read)
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyTab}) // rules
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyTab}) // auth header
	m = typeStr(t, m, "Authorization")
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyTab}) // auth file / value
	return m
}

func assertNoSecret(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, mcpTestSecret) || strings.Contains(s, "TOPSECRET") {
		t.Fatalf("%s leaks the auth value:\n%s", where, s)
	}
}

func TestMcpAddTypedMasked(t *testing.T) {
	m, got := mcpOpsModel(t, nil)
	m = fillAddBase(t, m)
	if !m.mcp.add.fromFile || m.mcp.add.focus != mcpFAuthFile {
		t.Fatalf("file must be the default auth source (focus %d)", m.mcp.add.focus)
	}
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.mcp.add.fromFile || m.mcp.add.focus != mcpFAuthValue {
		t.Fatalf("ctrl+t: fromFile=%v focus=%d", m.mcp.add.fromFile, m.mcp.add.focus)
	}
	m = typeStr(t, m, mcpTestSecret)
	out := stripANSI(m.View())
	assertNoSecret(t, "add form view", out)
	if strings.Contains(out, strings.Repeat("*", 4)) || strings.Contains(out, "••••") {
		t.Errorf("masked field draws per-rune echo (length leak):\n%s", out)
	}
	if !strings.Contains(out, "[entered, hidden]") || !strings.Contains(out, "add MCP connection") {
		t.Errorf("form render:\n%s", out)
	}
	f := m.mcp.add
	assertNoSecret(t, "%v of form", fmt.Sprintf("%v %+v %#v %s", f, f, f, *f))

	m, cmd := mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("enter: no command, err=%q", m.mcp.add.err)
	}
	// cleared from the form at submit, before the rpc ran
	if v := m.mcp.add.in[mcpFAuthValue].Value(); v != "" {
		t.Fatalf("auth input not reset at submit: %d runes", len(v))
	}
	assertNoSecret(t, "view after submit", stripANSI(m.View()))
	m = mcpSettle(t, m, cmd)
	if len(*got) < 1 || (*got)[0].GetOp() != "add" {
		t.Fatalf("rpcs = %v", *got)
	}
	r := (*got)[0]
	if r.GetAuthValue() != "" {
		t.Error("request auth value not blanked after send")
	}
	if r.GetConn() != "gh" || r.GetAuthName() != "Authorization" || r.GetCls() != "read" ||
		strings.Join(r.GetTools(), ",") != "search,get_file" {
		t.Errorf("req = %v", r)
	}
	if m.mcp.mode != mcpModeList || m.mcp.add != nil {
		t.Errorf("after ok add: mode %v", m.mcp.mode)
	}
	if !strings.Contains(stripANSI(m.View()), "add gh: ok") {
		t.Errorf("no ok note:\n%s", stripANSI(m.View()))
	}
}

// TestMcpAddSendsValue: the stub sees the value on the wire exactly once.
func TestMcpAddSendsValue(t *testing.T) {
	var wire string
	stubMcp(t, func(r *pb.McpReq) (*pb.McpResp, error) {
		if r.GetOp() == "add" {
			wire = r.GetAuthValue()
		}
		return &pb.McpResp{Ok: true, View: mcpLsView(t)}, nil
	})
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	nm, _ = nm.(Model).Update(cmd())
	m = fillAddBase(t, nm.(Model))
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
	m = typeStr(t, m, mcpTestSecret)
	m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	mcpSettle(t, m, cmd)
	if wire != mcpTestSecret {
		t.Fatalf("wire auth value = %d bytes, want the typed value", len(wire))
	}
}

func TestMcpAddFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth")
	if err := os.WriteFile(p, []byte(mcpTestSecret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var wire string
	got := stubMcp(t, func(r *pb.McpReq) (*pb.McpResp, error) {
		if r.GetOp() == "add" {
			wire = r.GetAuthValue()
		}
		return &pb.McpResp{Ok: true, View: mcpLsView(t)}, nil
	})
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	nm, _ = nm.(Model).Update(cmd())
	*got = nil
	m = fillAddBase(t, nm.(Model))
	m = typeStr(t, m, p)
	m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(*got) != 0 {
		t.Fatal("0644 auth file accepted")
	}
	e := m.mcp.add.err
	if !strings.Contains(e, "0644") || !strings.Contains(e, "0600") {
		t.Errorf("mode error = %q", e)
	}
	assertNoSecret(t, "file error", e+stripANSI(m.View()))

	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	// the path stays in the field after a local refusal: chmod, then enter again
	m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("0600 file refused: %q", m.mcp.add.err)
	}
	mcpSettle(t, m, cmd)
	if wire != mcpTestSecret {
		t.Fatalf("file value not sent trimmed (%d bytes)", len(wire))
	}
}

func TestMcpReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	w := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	if b, err := mcpReadSecretFile(w("ok", "Bearer x\r\n", 0o400)); err != nil || string(b) != "Bearer x" {
		t.Errorf("0400 crlf: %q %v", b, err)
	}
	for name, p := range map[string]string{
		"group-readable": w("g", "v", 0o640),
		"empty":          w("e", "\n", 0o600),
		"two lines":      w("two", mcpTestSecret+"\nmore", 0o600),
		"dir":            dir,
		"missing":        filepath.Join(dir, "nope"),
		"too big":        w("big", strings.Repeat("a", mcpSecretMax+1), 0o600),
	} {
		_, err := mcpReadSecretFile(p)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		assertNoSecret(t, name, err.Error())
	}
}

func TestMcpAddRejectedByDaemonClearsSecret(t *testing.T) {
	stubMcp(t, func(r *pb.McpReq) (*pb.McpResp, error) {
		if r.GetOp() == "add" {
			return &pb.McpResp{Ok: false, Error: "connection gh exists"}, nil
		}
		return &pb.McpResp{Ok: true, View: mcpLsView(t)}, nil
	})
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	nm, _ = nm.(Model).Update(cmd())
	m = fillAddBase(t, nm.(Model))
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
	m = typeStr(t, m, mcpTestSecret)
	m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = mcpSettle(t, m, cmd)
	if m.mcp.mode != mcpModeAdd || !strings.Contains(m.mcp.add.err, "exists") {
		t.Fatalf("daemon refusal not shown in form: mode %v err %q", m.mcp.mode, m.mcp.add.err)
	}
	if m.mcp.add.in[mcpFAuthValue].Value() != "" {
		t.Error("secret kept after a refused add")
	}
	assertNoSecret(t, "view after refusal", stripANSI(m.View()))
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mcp.mode != mcpModeList || m.focus != focusMcp {
		t.Errorf("esc from form: mode %v focus %v", m.mcp.mode, m.focus)
	}
}

func TestMcpAddValidation(t *testing.T) {
	m, got := mcpOpsModel(t, nil)
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m, cmd := mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(*got) != 0 || !strings.Contains(m.mcp.add.err, "name") {
		t.Fatalf("empty form sent: err %q", m.mcp.add.err)
	}
}

func TestMcpDeleteTypedName(t *testing.T) {
	m, got := mcpOpsModel(t, nil)
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if m.mcp.mode != mcpModeDelete || m.mcp.del.conn != "linear" {
		t.Fatalf("d: mode %v", m.mcp.mode)
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "type linear to confirm") {
		t.Errorf("confirm render:\n%s", out)
	}
	// y / enter alone never delete
	m = typeStr(t, m, "y")
	m, cmd := mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(*got) != 0 || m.mcp.mode != mcpModeDelete {
		t.Fatal("mismatched name deleted")
	}
	if !strings.Contains(stripANSI(m.View()), "exactly") {
		t.Error("no mismatch error")
	}
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = typeStr(t, m, "Linear") // case-sensitive
	if m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("case-mismatched name deleted")
	}
	for range "Linear" {
		m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = typeStr(t, m, "linear")
	m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = mcpSettle(t, m, cmd)
	if len(*got) < 1 || (*got)[0].GetOp() != "rm" || (*got)[0].GetConn() != "linear" {
		t.Fatalf("rpcs = %v", *got)
	}
	if m.mcp.mode != mcpModeList {
		t.Errorf("mode after delete %v", m.mcp.mode)
	}
	// esc cancels
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mcp.mode != mcpModeList || m.focus != focusMcp {
		t.Errorf("esc: mode %v focus %v", m.mcp.mode, m.focus)
	}
}

func TestMcpToggle(t *testing.T) {
	m, got := mcpOpsModel(t, nil)
	m, cmd := mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = mcpSettle(t, m, cmd)
	m = press(t, m, tea.KeyDown)
	m, cmd = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	mcpSettle(t, m, cmd)
	var ops []string
	for _, r := range *got {
		if r.GetOp() != "ls" {
			ops = append(ops, r.GetOp()+":"+r.GetConn())
		}
	}
	if strings.Join(ops, " ") != "disable:linear enable:wiki" {
		t.Fatalf("toggle ops = %v", ops)
	}
}

func TestMcpDetail(t *testing.T) {
	show := func(r *pb.McpReq) *structpb.Struct {
		var v map[string]any
		switch r.GetOp() {
		case "show":
			v = map[string]any{"name": "linear", "url": mcpTestSecretURL, "class": "read", "enabled": true,
				"auth":  map[string]any{"name": "Authorization", "value": mcpTestSecret},
				"tools": map[string]any{"get_issue": nil, "list_issues": map[string]any{"args": map[string]any{}}},
				"groups": map[string]any{
					"main":       map[string]any{"tools": map[string]any{"get_issue": nil}, "rate": map[string]any{"per_minute": 30}},
					"researcher": map[string]any{},
				}}
		case "effective":
			tools := []any{}
			if r.GetGroup() == "main" {
				tools = []any{"get_issue"}
			}
			v = map[string]any{"group": r.GetGroup(), "effective": []any{
				map[string]any{"conn": "linear", "whitelisted": true, "enabled": true, "tools": tools}}}
		default:
			return nil
		}
		s, err := structpb.NewStruct(v)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	m, got := mcpOpsModel(t, show)
	m, cmd := mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mcp.mode != mcpModeDetail {
		t.Fatal("enter did not open detail")
	}
	m = mcpSettle(t, m, cmd)
	out := stripANSI(m.View())
	for _, want := range []string{"MCP connection linear", "api.example.com:8443", "Authorization header",
		"get_issue,list_issues*", "whitelist (2 groups)", "main · rate 30/min", "researcher · rate no limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.Contains(l, "main · rate") && (i+2 >= len(lines) || !strings.Contains(lines[i+2], "effective  get_issue")) {
			t.Errorf("main effective line wrong:\n%s", out)
		}
	}
	assertNoSecret(t, "detail", out)
	if strings.Contains(out, "QUERYSECRET") {
		t.Error("detail leaks URL query")
	}
	var ops []string
	for _, r := range *got {
		ops = append(ops, r.GetOp()+":"+r.GetGroup())
	}
	if strings.Join(ops, " ") != "show: effective:main effective:researcher" {
		t.Errorf("detail rpcs = %v", ops)
	}
	m, _ = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mcp.mode != mcpModeList || m.focus != focusMcp {
		t.Errorf("esc from detail: mode %v focus %v", m.mcp.mode, m.focus)
	}
}

func TestMcpActionsRefusedForNonAdmin(t *testing.T) {
	got := stubMcp(t, func(*pb.McpReq) (*pb.McpResp, error) {
		return nil, status.Error(codes.PermissionDenied, "roles [peer] may not call mcp")
	})
	m := focusModel(t)
	nm, cmd := m.Update(altM())
	nm, _ = nm.(Model).Update(cmd())
	m = nm.(Model)
	*got = nil
	for _, k := range "adt" {
		var c tea.Cmd
		m, c = mcpKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{k}})
		if c != nil || m.mcp.mode != mcpModeList {
			t.Fatalf("%c: action allowed for non-admin", k)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("rpcs sent: %v", *got)
	}
	if !strings.Contains(stripANSI(m.View()), "read-only") {
		t.Error("no read-only message")
	}
}
