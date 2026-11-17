package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"koto-protocol/pb"
)

// The wire is what the TUI and koto ctl actually receive, so check the
// marshalled McpResp bytes, not the Go map: no op may carry the auth value,
// and show carries the header NAME only (no value key, not even a
// "[redacted]" placeholder).
func TestMcpRespBytesCarryNoSecret(t *testing.T) {
	mcpHarness(t)
	s := &kotoServer{}
	ctx := context.Background()
	wire := func(r *pb.McpReq) (*pb.McpResp, []byte) {
		t.Helper()
		resp, err := s.Mcp(ctx, r)
		if err != nil {
			t.Fatalf("%s: rpc err %v", r.Op, err)
		}
		b, err := proto.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(mcpTestSecret)) {
			t.Fatalf("%s: McpResp bytes contain the auth value", r.Op)
		}
		return resp, b
	}
	add := &pb.McpReq{Op: "add", Conn: "docs", Url: "https://mcp.example.com/mcp",
		Tools: []string{"search", "fetch"}, AuthName: "X-Api-Key", AuthValue: mcpTestSecret}
	if resp, _ := wire(add); !resp.Ok {
		t.Fatalf("add: %s", resp.Error)
	}
	// a refused add (bad rules) must not echo the secret in its error either
	bad := proto.Clone(add).(*pb.McpReq)
	bad.RulesJson = `{"nope":{}}`
	if resp, _ := wire(bad); resp.Ok {
		t.Fatal("bad rules accepted")
	}
	for _, r := range []*pb.McpReq{
		{Op: "allow", Conn: "docs", Group: "tg"},
		{Op: "grant", Conn: "docs", Group: "tg", Tool: "search"},
		{Op: "rate", Conn: "docs", Group: "tg", PerMinute: 5},
		{Op: "disable", Conn: "docs"},
		{Op: "enable", Conn: "docs"},
		{Op: "revoke", Conn: "docs", Group: "tg", Tool: "search"},
		{Op: "ls"},
		{Op: "effective", Group: "tg"},
		{Op: "effective", Group: "tg", Conn: "docs"},
	} {
		if resp, _ := wire(r); !resp.Ok {
			t.Fatalf("%s: %s", r.Op, resp.Error)
		}
	}
	resp, b := wire(&pb.McpReq{Op: "show", Conn: "docs"})
	if !resp.Ok {
		t.Fatalf("show: %s", resp.Error)
	}
	if bytes.Contains(b, []byte("[redacted]")) {
		t.Fatal("show still carries a redacted placeholder value")
	}
	auth, ok := resp.View.AsMap()["auth"].(map[string]any)
	if !ok || auth["name"] != "X-Api-Key" || len(auth) != 1 {
		t.Fatalf("show auth = %#v, want only the header name", resp.View.AsMap()["auth"])
	}
	if resp, _ := wire(&pb.McpReq{Op: "rm", Conn: "docs"}); !resp.Ok {
		t.Fatalf("rm: %s", resp.Error)
	}
}

// Every MCP op, through the real unary interceptor with a cert+token
// identity: non-admin roles are refused before the handler runs, whatever
// acl.json says (wildcard, explicit mcp grant, group-scoped mcp grant, none).
// Admin passes, as the control.
func TestMcpNonAdminRefusedEveryOp(t *testing.T) {
	origHERE := HERE
	t.Cleanup(func() { HERE = origHERE })
	HERE = t.TempDir()
	creds := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(creds, 0o700); err != nil {
		t.Fatal(err)
	}
	who := map[string]string{"root": "admin", "ops": "ops", "mc": "mc", "scoped": "scoped", "agent": "agent"}
	toks, allow := []string{}, ""
	ctxs := map[string]context.Context{}
	for name, role := range who {
		cert, _, _ := sec172Cert(t, name)
		tok := "tok-" + name
		h := sha256.Sum256([]byte(tok))
		toks = append(toks, `"`+name+`":{"hash":"`+hex.EncodeToString(h[:])+`","roles":["`+role+`"]}`)
		fp := sha256.Sum256(cert.Raw)
		allow += hex.EncodeToString(fp[:]) + " " + name + "\n"
		c := peer.NewContext(context.Background(), &peer.Peer{
			Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
			AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
		})
		ctxs[name] = metadata.NewIncomingContext(c, metadata.Pairs("authorization", "Bearer "+tok))
	}
	for f, body := range map[string]string{
		"tokens.json":   "{" + strings.Join(toks, ",") + "}",
		"clients.allow": allow,
		"acl.json":      `{"ops":{"*":"*"},"mc":{"mcp":"*"},"scoped":{"mcp":["tg"]},"agent":{"list":"*"}}`,
	} {
		if err := os.WriteFile(filepath.Join(creds, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info := &grpc.UnaryServerInfo{FullMethod: pb.Koto_Mcp_FullMethodName}
	ops := []string{"", "ls", "show", "effective", "add", "rm", "enable", "disable",
		"allow", "deny", "grant", "revoke", "rate", "frob"}
	for _, op := range ops {
		req := &pb.McpReq{Op: op, Conn: "docs", Group: "tg", Tool: "search",
			Url: "https://mcp.example.com/mcp", Tools: []string{"search"},
			AuthName: "X-Api-Key", AuthValue: mcpTestSecret}
		for name := range who {
			ran := false
			h := func(context.Context, any) (any, error) { ran = true; return &pb.McpResp{Ok: true}, nil }
			_, err := authUnary(ctxs[name], req, info, h)
			if name == "root" {
				if err != nil || !ran {
					t.Fatalf("admin refused %q: %v", op, err)
				}
				continue
			}
			if status.Code(err) != codes.PermissionDenied || ran {
				t.Fatalf("%s (non-admin) op %q: err=%v handler ran=%v", name, op, err, ran)
			}
			if strings.Contains(err.Error(), mcpTestSecret) {
				t.Fatalf("denial echoes the auth value")
			}
		}
	}
}
