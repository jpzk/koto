package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"clawson-protocol/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestCtlCLI exercises the built `clawson ctl` binary end to end against an
// in-process gRPC server running the real TLS config, auth + ACL
// interceptors, and handlers. It asserts the happy path (agent list), the
// role/ACL gate (agent may not stop, admin may), the admin-only ACL verbs
// (agent may not acl-get, admin may), and that a bad token exits non-zero.
//
// Skips unless creds/ holds the PKI + tokens this needs:
//
//	make pki-init
//	make pki-client NAME=tui             # admin (or any admin identity)
//	make pki-client NAME=agent ROLE=agent
//
// and creds/acl.json grants the agent role `list` but not `stop`.
func TestCtlCLI(t *testing.T) {
	initPaths()
	for _, f := range []string{"server.crt", "client-agent.crt", "token-agent", "client-tui.crt", "token-tui"} {
		if _, err := os.Stat(credFile(f)); err != nil {
			t.Skipf("missing %s — mint test PKI first", f)
		}
	}

	tlsCfg, err := serverTLSConfig()
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(authUnary),
		grpc.ChainStreamInterceptor(authStream),
	)
	pb.RegisterClawsonServer(srv, &clawsonServer{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Stop()

	// Build the binary from the package under test (the daemon dir is the
	// test's working directory), so `clawson ctl` is the code we just wrote.
	bin := filepath.Join(t.TempDir(), "clawson")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	run := func(client string, args ...string) (string, string, error) {
		cmd := exec.Command(bin, append([]string{"ctl"}, args...)...)
		cmd.Env = append(os.Environ(),
			"CLAWSON_ADDR="+l.Addr().String(),
			"CLAWSON_CREDS_DIR="+filepath.Join(HERE, "creds"),
			"CLAWSON_CLIENT="+client,
			"CLAWSON_SERVER_NAME=localhost",
		)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	t.Run("agent-list-ok", func(t *testing.T) {
		stdout, stderr, err := run("agent", "list")
		if err != nil {
			t.Fatalf("agent list failed: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, `"ok":true`) {
			t.Fatalf("expected ok:true, got: %s", stdout)
		}
	})

	t.Run("agent-stop-denied", func(t *testing.T) {
		_, stderr, err := run("agent", "stop", "no-such-group")
		if err == nil {
			t.Fatal("agent stop should be denied")
		}
		if !strings.Contains(stderr, "PermissionDenied") {
			t.Fatalf("expected PermissionDenied, stderr: %s", stderr)
		}
	})

	t.Run("agent-aclget-denied", func(t *testing.T) {
		_, stderr, err := run("agent", "acl", "get")
		if err == nil {
			t.Fatal("agent acl get should be denied")
		}
		if !strings.Contains(stderr, "PermissionDenied") {
			t.Fatalf("expected PermissionDenied, stderr: %s", stderr)
		}
	})

	t.Run("admin-stop-ok", func(t *testing.T) {
		stdout, stderr, err := run("tui", "stop", "no-such-group")
		if err != nil {
			t.Fatalf("admin stop failed: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, `"ok":true`) {
			t.Fatalf("expected ok:true, got: %s", stdout)
		}
	})

	t.Run("admin-aclget-ok", func(t *testing.T) {
		stdout, stderr, err := run("tui", "acl", "get")
		if err != nil {
			t.Fatalf("admin acl get failed: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, `"ok":true`) {
			t.Fatalf("expected ok:true, got: %s", stdout)
		}
	})

	t.Run("bad-token-fails", func(t *testing.T) {
		cmd := exec.Command(bin, "ctl", "list")
		cmd.Env = append(os.Environ(),
			"CLAWSON_ADDR="+l.Addr().String(),
			"CLAWSON_CREDS_DIR="+filepath.Join(HERE, "creds"),
			"CLAWSON_CLIENT=agent",
			"CLAWSON_SERVER_NAME=localhost",
			"CLAWSON_TOKEN=wrong",
		)
		if err := cmd.Run(); err == nil {
			t.Fatal("bad token should fail")
		}
	})
}
