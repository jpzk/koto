package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto-protocol/pb"
	"koto/mcpreg"
)

func mcpPost(sock string) (int, string, error) {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	resp, err := c.Post("http://mcp/mcp/docs", "application/json", bytes.NewBufferString(body))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func mcpSockExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// Default ON: unset (or any other value) enables the gateway; 0/false/off/no
// is the kill switch.
func TestMcpGatewayDefaultOn(t *testing.T) {
	for _, v := range []string{"", "1", "true", " on "} {
		t.Setenv("KOTO_MCP_GATEWAY", v)
		if !mcpGatewayEnabled() {
			t.Fatalf("KOTO_MCP_GATEWAY=%q: gateway off, want default on", v)
		}
	}
	for _, v := range []string{"0", "false", "FALSE", "off", "no", " 0 "} {
		t.Setenv("KOTO_MCP_GATEWAY", v)
		if mcpGatewayEnabled() {
			t.Fatalf("KOTO_MCP_GATEWAY=%q did not switch the gateway off", v)
		}
	}
	os.Unsetenv("KOTO_MCP_GATEWAY")
	if !mcpGatewayEnabled() {
		t.Fatal("unset: gateway off, want default on")
	}
}

// KOTO_MCP_GATEWAY=0: no MCP servers in any turn and no 9005 socket, even
// for a group with a full grant.
func TestMcpGatewayKillSwitch(t *testing.T) {
	mcpHarness(t)
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search"}})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search"})
	t.Setenv("KOTO_MCP_GATEWAY", "0")
	if s := mcpServersFor("tg"); len(s) != 0 {
		t.Fatalf("servers with the kill switch: %v", s)
	}
	base := filepath.Join(t.TempDir(), "uds")
	m, err := fcStartMcpListener("tg", base, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Open() || mcpSockExists(base+"_9005") {
		t.Fatal("9005 socket with the kill switch")
	}
}

// Inert without grants (gateway on by default): a group whitelisted with
// nothing granted, or not whitelisted at all, has an empty effective set —
// no MCP servers in its turns and no 9005 socket.
func TestMcpGatewayInertWithoutGrants(t *testing.T) {
	mcpHarness(t)
	t.Setenv("KOTO_MCP_GATEWAY", "")
	dir := t.TempDir()
	check := func(stage string) {
		t.Helper()
		for _, g := range []string{"tg", "peer"} {
			if s := mcpServersFor(g); len(s) != 0 {
				t.Fatalf("%s: %s servers = %v", stage, g, s)
			}
			m, err := fcStartMcpListener(g, filepath.Join(dir, g), -1)
			if err != nil {
				t.Fatal(err)
			}
			if m.Open() || mcpSockExists(filepath.Join(dir, g)+"_9005") {
				t.Fatalf("%s: %s has a 9005 socket", stage, g)
			}
			m.Close()
		}
	}
	check("empty registry")
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search"}})
	check("conn, no whitelist")
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	check("whitelisted, no grant")
	// A write-class connection exposes nothing yet, so a grant there is inert too.
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "wr", URL: "https://w.example.com/mcp", Class: "write", Tools: []string{"put"}})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "wr", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "wr", Group: "tg", Tool: "put"})
	check("write-class grant")
}

// The group is the listener, not the request: tg (granted) is served on its
// own socket; peer has no socket at all; deny bites on the next request.
func TestMcpGatewayListenerIdentity(t *testing.T) {
	mcpHarness(t)
	t.Setenv("KOTO_MCP_GATEWAY", "")
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search"},
		AuthName: "Authorization", AuthValue: mcpTestSecret})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search"})
	if s := mcpServersFor("tg"); len(s) != 1 || s[0] != "docs" {
		t.Fatalf("tg servers = %v", s)
	}
	if s := mcpServersFor("peer"); len(s) != 0 {
		t.Fatalf("peer servers = %v", s)
	}
	dir := t.TempDir()
	lnA, err := fcStartMcpListener("tg", filepath.Join(dir, "a"), -1)
	if err != nil || !lnA.Open() {
		t.Fatalf("tg listener: %v", err)
	}
	lnB, err := fcStartMcpListener("peer", filepath.Join(dir, "b"), -1)
	if err != nil || lnB.Open() {
		t.Fatalf("peer listener: open=%v %v", lnB != nil && lnB.Open(), err)
	}
	defer lnB.Close()
	code, body, err := mcpPost(filepath.Join(dir, "a_9005"))
	if err != nil || code != 200 {
		t.Fatalf("tg initialize: %d %v %s", code, err, body)
	}
	var r struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &r) != nil || r.Result.ServerInfo.Name != "koto-mcp/docs" {
		t.Fatalf("tg initialize body: %s", body)
	}
	if strings.Contains(body, mcpTestSecret) || strings.Contains(body, "mcp.example.com") {
		t.Fatal("initialize leaks upstream url/secret")
	}
	if _, _, err := mcpPost(filepath.Join(dir, "b_9005")); err == nil {
		t.Fatal("peer (no grant) reached a gateway")
	}
	// Not registered as a running VM, so the slot is not re-synced by the
	// write: the gateway's own per-request policy check still refuses.
	mcpOp(t, mcpAdminReq{Op: "deny", Conn: "docs", Group: "tg"})
	if code, _, err := mcpPost(filepath.Join(dir, "a_9005")); err != nil || code != http.StatusNotFound {
		t.Fatalf("tg after deny = %d %v, want 404", code, err)
	}
	lnA.Close()
	if _, _, err := mcpPost(filepath.Join(dir, "a_9005")); err == nil {
		t.Fatal("request served after listener close")
	}
}

// The listener follows the grants live on a running VM: grant opens it on
// the next turn (fcSendMsg syncs), revoke/deny/rm close it as soon as the
// registry write lands, and teardown is final.
func TestMcpGatewayListenerFollowsGrants(t *testing.T) {
	mcpHarness(t)
	t.Setenv("KOTO_MCP_GATEWAY", "")
	base := filepath.Join(t.TempDir(), "uds")
	sock := base + "_9005"
	m, err := fcStartMcpListener("tg", base, -1)
	if err != nil {
		t.Fatal(err)
	}
	fcMu.Lock()
	prev := fcVMs["tg"]
	fcVMs["tg"] = &fcVM{mcp: m, listeners: []net.Listener{m}}
	fcMu.Unlock()
	t.Cleanup(func() {
		fcMu.Lock()
		if prev == nil {
			delete(fcVMs, "tg")
		} else {
			fcVMs["tg"] = prev
		}
		fcMu.Unlock()
		m.Close()
	})
	open := func(want bool, stage string) {
		t.Helper()
		if m.Open() != want || mcpSockExists(sock) != want {
			t.Fatalf("%s: listener open=%v socket=%v, want %v", stage, m.Open(), mcpSockExists(sock), want)
		}
	}
	open(false, "boot, empty registry")
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search", "read"}})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	open(false, "allow without grant")
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "search"})
	open(true, "grant")
	if code, _, err := mcpPost(sock); err != nil || code != 200 {
		t.Fatalf("granted initialize = %d %v", code, err)
	}
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "read"})
	mcpOp(t, mcpAdminReq{Op: "revoke", Conn: "docs", Group: "tg", Tool: "search"})
	open(true, "one tool still granted")
	mcpOp(t, mcpAdminReq{Op: "revoke", Conn: "docs", Group: "tg", Tool: "read"})
	open(false, "last revoke")
	if _, _, err := mcpPost(sock); err == nil {
		t.Fatal("served after the last revoke")
	}
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "read"})
	open(true, "re-grant")
	mcpOp(t, mcpAdminReq{Op: "deny", Conn: "docs", Group: "tg"})
	open(false, "deny")
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	mcpOp(t, mcpAdminReq{Op: "grant", Conn: "docs", Group: "tg", Tool: "read"})
	open(true, "allow+grant")
	mcpOp(t, mcpAdminReq{Op: "rm", Conn: "docs"})
	open(false, "rm")

	// A registry change that bypassed mcpAdmin (hand edit, OAuth status) is
	// picked up by the per-turn sync in fcSendMsg.
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search"}})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	m.mu.Lock()
	m.closeLocked() // as if never synced since the grant below
	m.mu.Unlock()
	st, err := mcpStore()
	if err != nil {
		t.Fatal(err)
	}
	g := mcpAdminReq{Conn: "docs", Group: "tg", Tool: "search"}
	if err := st.Update(func(r *mcpreg.Registry) error { return r.Grant(g.Conn, g.Group, g.Tool, nil) }); err != nil {
		t.Fatal(err)
	}
	open(false, "store write without sync")
	mcpSyncGroup("tg")
	open(true, "per-turn sync")

	// Teardown: closed for good, a later grant change does not reopen it.
	m.Close()
	open(false, "teardown")
	mcpSyncAll()
	open(false, "sync after teardown")
}

// fcSendMsg syncs the slot before the MsgReq goes out, so the servers a turn
// is told about and the 9005 socket always agree — including for a registry
// change that did not go through mcpAdmin.
func TestFcSendMsgSyncsMcpListener(t *testing.T) {
	mcpHarness(t)
	t.Setenv("KOTO_MCP_GATEWAY", "")
	got := make(chan []string, 4)
	ln := fakeFC(t, "tg", func(port int, c net.Conn) {
		defer c.Close()
		req := &pb.AgentRequest{}
		if err := fcReadFrame(c, fcFrameMaxHost, req); err != nil {
			return
		}
		got <- req.GetMsg().GetMcpServers()
		fcWriteFrame(c, &pb.AgentResponse{Ok: true})
	})
	defer ln.Close()
	m, err := fcStartMcpListener("tg", fcUDS("tg"), -1)
	if err != nil {
		t.Fatal(err)
	}
	fcMu.Lock()
	prev := fcVMs["tg"]
	fcVMs["tg"] = &fcVM{mcp: m, listeners: []net.Listener{m}}
	fcMu.Unlock()
	t.Cleanup(func() {
		fcMu.Lock()
		if prev == nil {
			delete(fcVMs, "tg")
		} else {
			fcVMs["tg"] = prev
		}
		fcMu.Unlock()
		m.Close()
	})
	st, err := mcpStore()
	if err != nil {
		t.Fatal(err)
	}
	send := func(stage string, want []string) {
		t.Helper()
		if err := fcSendMsg("tg", "", 1, "hi", "sp", nil, ""); err != nil {
			t.Fatalf("%s: fcSendMsg: %v", stage, err)
		}
		s := <-got
		if strings.Join(s, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: McpServers = %q, want %q", stage, s, want)
		}
		if m.Open() != (len(want) > 0) {
			t.Fatalf("%s: listener open=%v with servers %q", stage, m.Open(), want)
		}
	}
	send("no registry", nil)
	if err := st.Update(func(r *mcpreg.Registry) error {
		if err := r.PutConn("docs", &mcpreg.Conn{URL: "https://mcp.example.com/mcp", Class: mcpreg.ClassRead,
			Tools: map[string]*mcpreg.ToolRule{"search": nil}}, true); err != nil {
			return err
		}
		if err := r.Allow("docs", "tg"); err != nil {
			return err
		}
		return r.Grant("docs", "tg", "search", nil)
	}); err != nil {
		t.Fatal(err)
	}
	if m.Open() {
		t.Fatal("opened without a sync")
	}
	send("granted behind mcpAdmin's back", []string{"docs"})
	if err := st.Update(func(r *mcpreg.Registry) error { return r.Revoke("docs", "tg", "search") }); err != nil {
		t.Fatal(err)
	}
	send("revoked behind mcpAdmin's back", nil)
}
