package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Off by default: no 9005 listener, no MCP servers in the turn.
func TestMcpGatewayOffByDefault(t *testing.T) {
	mcpHarness(t)
	t.Setenv("KOTO_MCP_GATEWAY", "")
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search"}})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	if mcpGatewayEnabled() {
		t.Fatal("gateway enabled without the flag")
	}
	if s := mcpServersFor("tg"); len(s) != 0 {
		t.Fatalf("servers with gateway off: %v", s)
	}
	ln, err := fcStartMcpListener("tg", filepath.Join(t.TempDir(), "uds"))
	if err != nil || ln != nil {
		t.Fatalf("listener with gateway off: %v %v", ln, err)
	}
}

// With the flag: the group is the listener, not the request. tg (whitelisted)
// is served; peer (not whitelisted) on its own listener gets 404 for the
// same path; closing the listener stops the server.
func TestMcpGatewayListenerIdentity(t *testing.T) {
	mcpHarness(t)
	t.Setenv("KOTO_MCP_GATEWAY", "1")
	mcpOp(t, mcpAdminReq{Op: "add", Conn: "docs", URL: "https://mcp.example.com/mcp", Tools: []string{"search"},
		AuthName: "Authorization", AuthValue: mcpTestSecret})
	mcpOp(t, mcpAdminReq{Op: "allow", Conn: "docs", Group: "tg"})
	if s := mcpServersFor("tg"); len(s) != 1 || s[0] != "docs" {
		t.Fatalf("tg servers = %v", s)
	}
	if s := mcpServersFor("peer"); len(s) != 0 {
		t.Fatalf("peer servers = %v", s)
	}
	dir := t.TempDir()
	lnA, err := fcStartMcpListener("tg", filepath.Join(dir, "a"))
	if err != nil || lnA == nil {
		t.Fatalf("tg listener: %v", err)
	}
	lnB, err := fcStartMcpListener("peer", filepath.Join(dir, "b"))
	if err != nil || lnB == nil {
		t.Fatalf("peer listener: %v", err)
	}
	defer lnB.Close()

	post := func(sock string) (int, string, error) {
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
	code, body, err := post(filepath.Join(dir, "a_9005"))
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
	if code, _, err := post(filepath.Join(dir, "b_9005")); err != nil || code != http.StatusNotFound {
		t.Fatalf("peer (not whitelisted) = %d %v, want 404", code, err)
	}
	// Deny bites on the next request, same listener, no restart.
	mcpOp(t, mcpAdminReq{Op: "deny", Conn: "docs", Group: "tg"})
	if code, _, err := post(filepath.Join(dir, "a_9005")); err != nil || code != http.StatusNotFound {
		t.Fatalf("tg after deny = %d %v, want 404", code, err)
	}
	lnA.Close()
	if _, _, err := post(filepath.Join(dir, "a_9005")); err == nil {
		t.Fatal("request served after listener close")
	}
}
