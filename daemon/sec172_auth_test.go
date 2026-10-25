package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func sec172Cert(t *testing.T, cn string) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		DNSNames:              []string{"koto-daemon"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return c, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// 1.7.2 fix(8b), audit low: clients.allow was consulted only in the TLS
// handshake (VerifyPeerCertificate). gRPC keeps one connection open for the
// life of a client, so deleting a device's line did not revoke it until it
// reconnected — and a resumed TLS session would skip the callback entirely.
// authFromCtx (both interceptors) must re-check the fingerprint per RPC.
func TestSec172ClientsAllowRecheckedPerRPC(t *testing.T) {
	origHERE := HERE
	t.Cleanup(func() { HERE = origHERE })
	HERE = t.TempDir()
	creds := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(creds, 0o700); err != nil {
		t.Fatal(err)
	}
	cert, _, _ := sec172Cert(t, "alice")
	const tok = "sec172-alice-token"
	th := sha256.Sum256([]byte(tok))
	if err := os.WriteFile(filepath.Join(creds, "tokens.json"),
		[]byte(`{"alice":{"hash":"`+hex.EncodeToString(th[:])+`","roles":["admin"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(cert.Raw)
	writeAllow := func(body string) {
		if err := os.WriteFile(filepath.Join(creds, "clients.allow"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
	})
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+tok))

	writeAllow(hex.EncodeToString(fp[:]) + " alice\n")
	if _, err := authFromCtx(ctx); err != nil {
		t.Fatalf("allowlisted cert + matching token must authenticate: %v", err)
	}
	// Revoke: delete alice's line, keep the connection (same ctx).
	writeAllow(strings.Repeat("0", 64) + " someone-else\n")
	if _, err := authFromCtx(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked cert on a live connection must be Unauthenticated, got %v", err)
	}
	// Unreadable allowlist fails closed.
	if err := os.Remove(filepath.Join(creds, "clients.allow")); err != nil {
		t.Fatal(err)
	}
	if _, err := authFromCtx(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing clients.allow must fail closed, got %v", err)
	}
}

// 1.7.2 fix(8c): no TLS session tickets on the daemon listener, so a client
// can never resume past VerifyPeerCertificate.
func TestSec172ServerTLSNoSessionTickets(t *testing.T) {
	origHERE := HERE
	t.Cleanup(func() { HERE = origHERE })
	HERE = t.TempDir()
	creds := filepath.Join(HERE, "creds")
	if err := os.MkdirAll(creds, 0o700); err != nil {
		t.Fatal(err)
	}
	_, certPEM, keyPEM := sec172Cert(t, "koto-daemon")
	for name, b := range map[string][]byte{"server.crt": certPEM, "server.key": keyPEM, "ca.crt": certPEM} {
		if err := os.WriteFile(filepath.Join(creds, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := serverTLSConfig()
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}
	if !cfg.SessionTicketsDisabled {
		t.Fatal("server TLS config must set SessionTicketsDisabled")
	}
}

// 1.7.2 fix(8a), audit low: ctl send / sched_add / goal_set accepted any
// name matching groupNameRE, creating host-side state (session dirs,
// queues, schedules, goal records) for groups that were never spawned.
func TestSec172CtlVerbsRequireRegisteredGroup(t *testing.T) {
	prevRoot, prevGF, prevHERE := ROOT, GROUPS_FILE, HERE
	t.Cleanup(func() { ROOT, GROUPS_FILE, HERE = prevRoot, prevGF, prevHERE })
	HERE = t.TempDir()
	ROOT = filepath.Join(HERE, "groups")
	t.Chdir(HERE) // base wrote stray per-group state relative to cwd
	GROUPS_FILE = filepath.Join(HERE, "groups.json")
	if err := os.MkdirAll(ROOT, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GROUPS_FILE, []byte(`{"main":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const ghost = "ghostpeer"
	for _, line := range []string{
		`{"cmd":"send","group":"` + ghost + `","msg":"hi"}`,
		`{"cmd":"sched_add","group":"` + ghost + `","cron":"@daily","msg":"hi"}`,
		`{"cmd":"goal_set","group":"` + ghost + `","name":"g","text":"t","criteria":"c","max_iterations":1}`,
	} {
		resp := ctlDispatch(ctlMainGroup, []byte(line))
		if !strings.Contains(sec172RespErr(resp), "no such group") {
			t.Errorf("%s: unregistered target must be refused with \"no such group\", got %#v", line, resp)
		}
	}
	if _, err := os.Stat(filepath.Join(ROOT, ghost)); !os.IsNotExist(err) {
		t.Errorf("a refused verb must not create %s (stat err %v)", filepath.Join(ROOT, ghost), err)
	}
}

func sec172RespErr(r any) string {
	b, _ := json.Marshal(r)
	var v struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Error
}

// sec172RegisterGroups points GROUPS_FILE at a temp groups.json listing
// names, so tests that drive ctl send / sched_add / goal_set at a peer target
// a registered group (fix(8a) refuses unregistered ones).
func sec172RegisterGroups(t *testing.T, names ...string) {
	t.Helper()
	prev := GROUPS_FILE
	t.Cleanup(func() { GROUPS_FILE = prev })
	GROUPS_FILE = filepath.Join(t.TempDir(), "groups.json")
	m := map[string]int{}
	for i, n := range names {
		m[n] = 3100 + i
	}
	writeGroups(m)
}

// sec172AllowedPeerStream returns a server stream whose context carries a
// fresh client certificate listed in $HERE/creds/clients.allow, plus a func
// that drops it from the allowlist. aclStream.revalidate re-checks the cert
// (fix(8b)), so stream tests need a peer the way a real connection has one.
func sec172AllowedPeerStream(t *testing.T) (grpc.ServerStream, func()) {
	t.Helper()
	cert, _, _ := sec172Cert(t, "watcher")
	fp := sha256.Sum256(cert.Raw)
	allow := filepath.Join(HERE, "creds", "clients.allow")
	if err := os.WriteFile(allow, []byte(hex.EncodeToString(fp[:])+" watcher\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
	})
	revoke := func() {
		if err := os.WriteFile(allow, []byte(strings.Repeat("0", 64)+" someone-else\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &logStreamRec{ctx: ctx}, revoke
}
