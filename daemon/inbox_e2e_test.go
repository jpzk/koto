package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	"koto-protocol/pb"
)

// TestInboxE2E is the inbox end to end in an ISOLATED daemon (fresh HERE /
// ROOT / creds under t.TempDir, loopback listener on an ephemeral port — never
// :8443 or /var/lib/koto):
//
//   - the daemon side is the production gRPC stack: pkiInit + pkiClient creds,
//     serverTLSConfig (mTLS), authUnary/authStream, kotoServer;
//   - the PEER side is the shipped sidecar/cs-notify and sidecar/cs-ask shell
//     scripts, run unmodified except that /workspace/.cs points at a scratch
//     dir. A guest-agent shim does what fcguest does with their lines: strict
//     protojson -> CtlRequest -> ctlDispatchPB (the guest wire entry point)
//     under the peer's owner identity, ask on its own connection, and the
//     response appended to .cs/ctl.out (same flattening as fcguest
//     ctlResponseJSON). Only the microVM transport is skipped — this host has
//     no firecracker/guest image;
//   - the CLIENT is a real gRPC client over mTLS: WatchInbox sees both items,
//     Approve answers the ask with option + message, InboxDismiss removes the
//     notification (an agent identity is refused), and the cs-ask process
//     prints the answer and exits 0.
//
// Skipped unless KOTO_INBOX_E2E=1 (scripts/e2e-inbox.sh runs it).
func TestInboxE2E(t *testing.T) {
	if os.Getenv("KOTO_INBOX_E2E") != "1" {
		t.Skip("KOTO_INBOX_E2E=1 not set (scripts/e2e-inbox.sh)")
	}
	for _, b := range []string{"sh", "base64", "timeout", "sed", "grep", "tail"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("peer scripts need %s: %v", b, err)
		}
	}
	const peer, goalID, goalName = "peer", "ge2e", "e2e"
	logf := func(f string, a ...any) { t.Logf("E2E "+f, a...) }

	// ---- isolated daemon state ------------------------------------------
	inboxRig(t) // fresh HERE/ROOT (fcHarness), no approvals/goals/notifications
	steerMirrorRec(t)
	goalLock.Lock()
	goals = append(goals, goalItem{ID: goalID, Group: peer, Name: goalName, Text: "e2e goal", Criteria: "c",
		Status: goalStatusRunning, Iteration: 1, MaxIterations: 3, Mode: modeYolo, CreatedAt: goalNow()})
	goalDoneOpen[goalID] = peer // the work turn is in flight: the ask window is open
	goalLock.Unlock()
	t.Cleanup(func() { goalLock.Lock(); delete(goalDoneOpen, goalID); goalLock.Unlock() })
	creds := filepath.Join(HERE, "creds")
	if err := pkiInit(creds, []string{"DNS:koto-daemon"}); err != nil {
		t.Fatal(err)
	}
	opTok, err := pkiClient(creds, "e2e-op", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	agTok, err := pkiClient(creds, "e2e-agent", []string{"agent"})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := serverTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	pb.RegisterKotoServer(srv, &kotoServer{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(srv.Stop)
	logf("daemon: HERE=%s listen=%s (isolated, mTLS)", HERE, l.Addr())

	dial := func(name, tok string) pb.KotoClient {
		cert, err := tls.LoadX509KeyPair(filepath.Join(creds, "client-"+name+".crt"), filepath.Join(creds, "client-"+name+".key"))
		if err != nil {
			t.Fatal(err)
		}
		caPEM, _ := os.ReadFile(filepath.Join(creds, "ca.crt"))
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caPEM)
		cc, err := grpc.NewClient(l.Addr().String(),
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert},
				RootCAs: pool, ServerName: "koto-daemon", MinVersion: tls.VersionTLS13})),
			grpc.WithPerRPCCredentials(testToken{tok}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cc.Close() })
		return pb.NewKotoClient(cc)
	}
	op, agent := dial("e2e-op", opTok), dial("e2e-agent", agTok)

	// ---- client: WatchInbox ---------------------------------------------
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := op.WatchInbox(ctx, &pb.InboxReq{})
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan *pb.InboxResp, 64)
	go func() {
		for {
			r, err := st.Recv()
			if err != nil {
				close(frames)
				return
			}
			frames <- r
		}
	}()
	next := func(what string, ok func([]*pb.InboxItem) bool) []*pb.InboxItem {
		t.Helper()
		dl := time.After(15 * time.Second)
		for {
			select {
			case r, open := <-frames:
				if !open {
					t.Fatalf("WatchInbox stream ended waiting for: %s", what)
				}
				if ok(r.GetItems()) {
					logf("client WatchInbox frame (%s): %v", what, inboxKinds(r.GetItems()))
					return r.GetItems()
				}
			case <-dl:
				t.Fatalf("no WatchInbox frame: %s", what)
			}
		}
	}
	next("initial empty snapshot", func(it []*pb.InboxItem) bool { return len(it) == 0 })

	// ---- peer: shipped sidecar scripts against a shim guest agent ----------
	ws := t.TempDir()
	csDir := filepath.Join(ws, ".cs")
	if err := os.MkdirAll(csDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(csDir, "ctl"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopShim := e2eGuestShim(t, peer, csDir, logf)
	t.Cleanup(stopShim)
	script := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "sidecar", name))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(ws, name)
		if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(b), "/workspace/.cs", csDir)), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := append(os.Environ(), "KOTO_SESSION=goal-"+goalName)

	n := exec.Command(script("cs-notify"), "e2e: deploy done", "v1.4 live, smoke green")
	n.Env = env
	if out, err := n.CombinedOutput(); err != nil {
		t.Fatalf("cs-notify: %v %s", err, out)
	}
	logf("peer ran: cs-notify \"e2e: deploy done\" \"v1.4 live, smoke green\" (rc=0)")

	ask := exec.Command(script("cs-ask"), "-g", goalID, "which database?", "postgres", "sqlite")
	ask.Env = env
	var askOut, askErr strings.Builder
	ask.Stdout, ask.Stderr = &askOut, &askErr
	if err := ask.Start(); err != nil {
		t.Fatal(err)
	}
	askDone := make(chan error, 1)
	go func() { askDone <- ask.Wait() }()
	t.Cleanup(func() { _ = ask.Process.Kill() })
	logf("peer started: cs-ask -g %s \"which database?\" postgres sqlite (blocking)", goalID)

	var askID, noteID string
	items := next("ask + notification visible", func(it []*pb.InboxItem) bool {
		askID, noteID = "", ""
		for _, x := range it {
			if a := x.GetApproval(); a != nil && a.GetKind() == kindAsk && a.GetGroup() == peer {
				askID = a.GetId()
			}
			if nt := x.GetNotification(); nt != nil && nt.GetGroup() == peer {
				noteID = nt.GetId()
			}
		}
		return askID != "" && noteID != ""
	})
	for _, x := range items {
		if a := x.GetApproval(); a != nil {
			logf("  ask: id=%s group=%s session=%s q=%q options=%v", a.GetId(), a.GetGroup(), a.GetSession(), a.GetSummary(), a.GetOptions())
			if a.GetSession() != "goal-"+goalName || len(a.GetOptions()) != 2 {
				t.Fatalf("ask item = %v", a)
			}
		}
		if nt := x.GetNotification(); nt != nil {
			logf("  notification: id=%s group=%s session=%s title=%q body=%q", nt.GetId(), nt.GetGroup(), nt.GetSession(), nt.GetTitle(), nt.GetBody())
			if nt.GetTitle() != "e2e: deploy done" || nt.GetBody() != "v1.4 live, smoke green" {
				t.Fatalf("notification item = %v", nt)
			}
		}
	}

	// ---- client answers + dismisses ------------------------------------
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rcancel()
	if _, err := agent.InboxDismiss(rctx, &pb.InboxDismissReq{Ids: []string{noteID}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("agent InboxDismiss: want PermissionDenied, got %v", err)
	}
	logf("agent identity InboxDismiss -> PermissionDenied (as specified)")
	ar, err := op.Approve(rctx, &pb.ApproveReq{Group: peer, Session: "goal-" + goalName, Id: askID, Allow: true, Option: 2, Msg: "smaller ops"})
	if err != nil || ar.GetError() != "" {
		t.Fatalf("Approve: %v %v", ar, err)
	}
	logf("client Approve{id=%s allow option=2 msg=%q} -> decision=%s", askID, "smaller ops", ar.GetDecision())
	dr, err := op.InboxDismiss(rctx, &pb.InboxDismissReq{Ids: []string{noteID}})
	if err != nil || !dr.GetOk() || dr.GetDismissed() != 1 {
		t.Fatalf("InboxDismiss: %v %v", dr, err)
	}
	logf("client InboxDismiss{%s} -> ok dismissed=%d", noteID, dr.GetDismissed())
	next("inbox empty again", func(it []*pb.InboxItem) bool { return len(it) == 0 })

	// ---- peer receives the answer ----------------------------------------
	select {
	case err := <-askDone:
		if err != nil {
			t.Fatalf("cs-ask exit: %v stderr=%s", err, askErr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("cs-ask never returned; stderr=%s", askErr.String())
	}
	got := strings.TrimSpace(askOut.String())
	logf("peer cs-ask exited rc=0, printed: %q", got)
	if got != "sqlite — smaller ops" {
		t.Fatalf("peer got %q", got)
	}
	logf("PASS")
}

// e2eGuestShim stands in for fcguest's ctl loop: read lines from the .cs/ctl
// FIFO, strict protojson into a CtlRequest, ctlDispatchPB as owner, append
// the flattened response to .cs/ctl.out. ask goes on its own goroutine (its
// own connection in fcguest) so it never holds the line.
func e2eGuestShim(t *testing.T, owner, csDir string, logf func(string, ...any)) func() {
	fifo, out := filepath.Join(csDir, "ctl"), filepath.Join(csDir, "ctl.out")
	var mu sync.Mutex
	var wg sync.WaitGroup
	stop := make(chan struct{})
	appendOut := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			logf("shim: ctl.out: %v", err)
			return
		}
		f.Write(b)
		f.Close()
	}
	handle := func(line []byte) {
		defer wg.Done()
		req, err := ctlRequestFromJSON(line)
		if err != nil {
			b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
			appendOut(append(b, '\n'))
			return
		}
		resp := ctlDispatchPB(owner, req)
		logf("shim: guest[%s] ctl %s -> ok=%v error=%q", owner, ctlVerbOf(line), resp.GetOk(), resp.GetError())
		appendOut(e2eCtlResponseJSON(resp))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := os.Open(fifo) // blocks until a writer opens
			if err != nil {
				return
			}
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := append([]byte(nil), sc.Bytes()...)
				wg.Add(1)
				go handle(line)
			}
			f.Close()
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	return func() {
		close(stop)
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close() // unblock the reader's open
		} else if !errors.Is(err, syscall.ENXIO) {
			logf("shim: stop: %v", err)
		}
		<-done
		wg.Wait()
	}
}

func ctlVerbOf(line []byte) string {
	var v struct {
		Cmd string `json:"cmd"`
	}
	_ = json.Unmarshal(line, &v)
	return v.Cmd
}

// e2eCtlResponseJSON mirrors fcguest/ctl.go ctlResponseJSON (separate module).
func e2eCtlResponseJSON(resp *pb.CtlResponse) []byte {
	out := map[string]json.RawMessage{}
	out["ok"], _ = json.Marshal(resp.Ok)
	if resp.Error != "" {
		out["error"], _ = json.Marshal(resp.Error)
	}
	ref := resp.ProtoReflect()
	if fd := ref.WhichOneof(ref.Descriptor().Oneofs().ByName("result")); fd != nil {
		v := ref.Get(fd)
		if fd.Kind() == protoreflect.MessageKind {
			b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(v.Message().Interface())
			var inner map[string]json.RawMessage
			_ = json.Unmarshal(b, &inner)
			for k, val := range inner {
				if k != "ok" && k != "error" {
					out[k] = val
				}
			}
		} else {
			out[string(fd.Name())], _ = json.Marshal(v.Interface())
		}
	}
	b, _ := json.Marshal(out)
	return append(b, '\n')
}
