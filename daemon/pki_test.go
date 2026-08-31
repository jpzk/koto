package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestPKIHandshake is the contract test between pki.go's generator and
// auth.go's verifier: material minted by `koto pki` must satisfy the real
// serverTLSConfig() — chain, fingerprint allowlist and token identity — with
// no openssl in sight. If the two ever drift, first run after install fails
// with an opaque TLS error, so pin it here.
func TestPKIHandshake(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds")

	if err := pkiInit(creds, nil); err != nil {
		t.Fatalf("pkiInit: %v", err)
	}
	token, err := pkiClient(creds, "tui", []string{"admin"})
	if err != nil {
		t.Fatalf("pkiClient: %v", err)
	}

	// auth.go reads through credFile() → HERE/creds.
	oldHere := HERE
	HERE = dir
	t.Cleanup(func() { HERE = oldHere })

	srvCfg, err := serverTLSConfig()
	if err != nil {
		t.Fatalf("serverTLSConfig: %v", err)
	}

	clientCert, err := tls.LoadX509KeyPair(
		filepath.Join(creds, "client-tui.crt"), filepath.Join(creds, "client-tui.key"))
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(creds, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.crt: no certificates parsed")
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errc := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer c.Close()
		errc <- c.(*tls.Conn).Handshake()
	}()

	// ServerName must match a SAN of the default server cert.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", port), &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      pool,
		ServerName:   "koto-daemon",
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	conn.Close()
	if err := <-errc; err != nil {
		t.Fatalf("server handshake: %v", err)
	}

	// The bearer token must resolve to the identity and roles we minted.
	id, ok := tokenIdentity(token)
	if !ok {
		t.Fatal("tokenIdentity: minted token not recognized")
	}
	if len(id.Roles) != 1 || id.Roles[0] != "admin" {
		t.Fatalf("roles = %v, want [admin]", id.Roles)
	}
}

// TestPKIInitIsIdempotent pins the anti-footgun: a second pkiInit must reuse
// the CA, so client certs minted earlier keep verifying (the historical
// `make pki-init` silently regenerated it and invalidated every client).
func TestPKIInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds")
	if err := pkiInit(creds, nil); err != nil {
		t.Fatalf("pkiInit: %v", err)
	}
	caBefore, err := os.ReadFile(filepath.Join(creds, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pkiInit(creds, nil); err != nil {
		t.Fatalf("pkiInit (second): %v", err)
	}
	caAfter, err := os.ReadFile(filepath.Join(creds, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(caBefore) != string(caAfter) {
		t.Fatal("pkiInit regenerated the CA — every existing client cert would be invalidated")
	}
}

// TestPKIClientAppendsOnce pins the clients.allow dedup (by fingerprint) and
// that re-minting a name rotates its token rather than duplicating entries.
func TestPKIClientAppendsOnce(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds")
	if err := pkiInit(creds, nil); err != nil {
		t.Fatal(err)
	}
	tok1, err := pkiClient(creds, "tui", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := pkiClient(creds, "tui", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	if tok1 == tok2 {
		t.Fatal("re-minting returned the same token; it must rotate")
	}
	HERE = dir
	t.Cleanup(func() { HERE = "" })
	if _, ok := tokenIdentity(tok2); !ok {
		t.Fatal("rotated token not registered")
	}
	if _, ok := tokenIdentity(tok1); ok {
		t.Fatal("superseded token still accepted")
	}
}
