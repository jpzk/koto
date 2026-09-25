package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"koto-protocol/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestCtlCLI exercises the built `koto ctl` binary end to end against an
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
	pb.RegisterKotoServer(srv, &kotoServer{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Stop()

	// Build the binary from the package under test (the daemon dir is the
	// test's working directory), so `koto ctl` is the code we just wrote.
	bin := filepath.Join(t.TempDir(), "koto")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	run := func(client string, args ...string) (string, string, error) {
		cmd := exec.Command(bin, append([]string{"ctl"}, args...)...)
		cmd.Env = append(os.Environ(),
			"KOTO_ADDR="+l.Addr().String(),
			"KOTO_CREDS_DIR="+filepath.Join(HERE, "creds"),
			"KOTO_CLIENT="+client,
			"KOTO_SERVER_NAME=localhost",
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

	// config read is a group-scoped verb; agent's ACL grants it on main in
	// the seeded acl.json, so this also exercises the group-first parsing.
	t.Run("config-read-ok", func(t *testing.T) {
		stdout, stderr, err := run("tui", "config", "main")
		if err != nil {
			t.Fatalf("config read failed: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, `"ok":true`) {
			t.Fatalf("expected ok:true, got: %s", stdout)
		}
	})

	// config's group-first grammar: flags after the group must parse (the
	// bug the ordering fix addressed). -effort "" is a no-op clear, safe to
	// run against a real daemon.
	t.Run("config-set-flags-after-group", func(t *testing.T) {
		stdout, stderr, err := run("tui", "config", "main", "-effort", "")
		if err != nil {
			t.Fatalf("config set failed: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, `"ok":true`) {
			t.Fatalf("expected ok:true, got: %s", stdout)
		}
	})

	t.Run("bad-token-fails", func(t *testing.T) {
		cmd := exec.Command(bin, "ctl", "list")
		cmd.Env = append(os.Environ(),
			"KOTO_ADDR="+l.Addr().String(),
			"KOTO_CREDS_DIR="+filepath.Join(HERE, "creds"),
			"KOTO_CLIENT=agent",
			"KOTO_SERVER_NAME=localhost",
			"KOTO_TOKEN=wrong",
		)
		if err := cmd.Run(); err == nil {
			t.Fatal("bad token should fail")
		}
	})
}

// TestCtlDrainVerb takes the Drain verb end to end through the real stack —
// the built `koto ctl` binary, mTLS, the auth + ACL interceptors, and the
// handler — because most of what makes a new verb work is mechanical and
// therefore unasserted anywhere else: the ACL name comes from the method name
// (Drain → drain), the target comes from the request TYPE (GroupReq), and
// neither would fail to compile if either were wrong.
//
// Needs only the admin identity, unlike TestCtlCLI, so it runs in a dev clone
// that never minted the `agent` client.
func TestCtlDrainVerb(t *testing.T) {
	initPaths()
	for _, f := range []string{"server.crt", "client-tui.crt", "token-tui"} {
		if _, err := os.Stat(credFile(f)); err != nil {
			t.Skipf("missing %s — mint test PKI first (make pki-init && make pki-client NAME=tui)", f)
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
	pb.RegisterKotoServer(srv, &kotoServer{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Stop()

	bin := filepath.Join(t.TempDir(), "koto")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, append([]string{"ctl"}, args...)...)
		cmd.Env = append(os.Environ(),
			"KOTO_ADDR="+l.Addr().String(),
			"KOTO_CREDS_DIR="+filepath.Join(HERE, "creds"),
			"KOTO_CLIENT=tui",
			"KOTO_SERVER_NAME=localhost",
		)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	// An in-band {ok:false} is exit 1 with the message on stderr (ctlPrint's
	// contract for every verb). Reaching that answer at all is the point:
	// it proves the verb authorized, targeted and dispatched.
	//
	// A typo must not read as "dropped 0", which is what an empty backlog
	// reports — hence a refusal rather than a cheerful zero.
	t.Run("unknown-group", func(t *testing.T) {
		_, stderr, err := run("drain", "definitely-no-such-group")
		if err == nil {
			t.Fatal("drain of an unknown group should exit non-zero")
		}
		if !strings.Contains(stderr, "no such group") {
			t.Fatalf("expected a no-such-group refusal, got: %s", stderr)
		}
	})

	// Goal sessions are the driver's; /goals interrupt is their verb. Asked
	// of a group that does not exist either, so this also pins the ordering:
	// a request that may not be made at all is refused on its SHAPE, not on
	// whichever check the fleet's current state happens to trip first.
	t.Run("goal-session-refused", func(t *testing.T) {
		_, stderr, err := run("drain", "-session", goalSessionPrefix+"x", "definitely-no-such-group")
		if err == nil {
			t.Fatal("draining a goal session should exit non-zero")
		}
		if !strings.Contains(stderr, "reserved for the goal loop") {
			t.Fatalf("expected the reserved-session refusal, got: %s", stderr)
		}
	})

	// Usage errors exit 2, like every other ctl verb.
	t.Run("usage", func(t *testing.T) {
		_, stderr, err := run("drain")
		if err == nil {
			t.Fatal("bare `drain` should be a usage error")
		}
		if !strings.Contains(stderr, "usage: koto ctl drain") {
			t.Fatalf("stderr = %s", stderr)
		}
	})
}
