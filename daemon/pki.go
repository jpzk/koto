package main

// The Makefile's openssl pki-init / pki-client recipes, in Go, so `koto setup`
// and `koto install` need neither openssl nor jq on the host. Same artifacts,
// byte-compatible with what auth.go verifies: EC P-256 keys, a self-signed CA
// (CN=koto-ca, 10y), a server cert (CN=koto-daemon, 825d, SANs), client certs
// whose DER-leaf SHA-256 fingerprint goes into clients.allow, and bearer
// tokens whose SHA-256 goes into tokens.json as {hash, roles}. The daemon
// re-reads clients.allow per handshake and tokens.json/acl.json per call, so
// minting a client against a running daemon needs no restart.
//
// pkiInit never regenerates an existing CA — that would silently invalidate
// every client cert (the historical `make pki-init` footgun). The server cert
// alone may be reissued (SAN changes) via pkiServerCert.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func newPkiFlags(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ExitOnError) }

var defaultServerSANs = []string{"DNS:koto-daemon", "DNS:localhost", "IP:127.0.0.1"}

// pkiInit creates creds/ca.{key,crt} (only if absent) and the daemon server
// cert, and seeds acl.json with the same agent role the Makefile seeds.
// Idempotent: an existing CA is reused, an existing valid server cert kept.
func pkiInit(credsDir string, serverSANs []string) error {
	if err := os.MkdirAll(credsDir, 0o750); err != nil { // the installer's mode (audit I4)
		return err
	}
	if len(serverSANs) == 0 {
		serverSANs = defaultServerSANs
	}
	caKey, caCert, created, err := pkiEnsureCA(credsDir)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("new CA written to %s\n", filepath.Join(credsDir, "ca.crt"))
	}
	if _, err := os.Stat(filepath.Join(credsDir, "server.crt")); err != nil {
		if err := pkiServerCert(credsDir, caKey, caCert, serverSANs); err != nil {
			return err
		}
	}
	aclPath := filepath.Join(credsDir, "acl.json")
	if _, err := os.Stat(aclPath); err != nil {
		seed := `{
  "agent": {
    "list": "*", "send": "*", "history": "*", "metrics": "*",
    "sched_list": "*",
    "subscribe_group": "*", "watch_state": "*"
  }
}
`
		if err := os.WriteFile(aclPath, []byte(seed), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// pkiEnsureCA loads the CA, creating it only when absent.
func pkiEnsureCA(credsDir string) (*ecdsa.PrivateKey, *x509.Certificate, bool, error) {
	keyPath := filepath.Join(credsDir, "ca.key")
	crtPath := filepath.Join(credsDir, "ca.crt")
	if _, err := os.Stat(keyPath); err == nil {
		key, err := pkiReadKey(keyPath)
		if err != nil {
			return nil, nil, false, fmt.Errorf("ca.key: %w", err)
		}
		cert, err := pkiReadCert(crtPath)
		if err != nil {
			return nil, nil, false, fmt.Errorf("ca.crt: %w", err)
		}
		return key, cert, false, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, false, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          pkiSerial(),
		Subject:               pkix.Name{CommonName: "koto-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(0, 0, 3650),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, false, err
	}
	if err := pkiWriteKey(keyPath, key); err != nil {
		return nil, nil, false, err
	}
	if err := pkiWritePEM(crtPath, "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, false, err
	}
	cert, err := x509.ParseCertificate(der)
	return key, cert, true, err
}

// pkiServerCert (re)issues the daemon server cert against the existing CA.
// SANs use the openssl subjectAltName syntax ("DNS:x", "IP:1.2.3.4") to stay
// interchangeable with the Makefile's SERVER_SAN variable.
func pkiServerCert(credsDir string, caKey *ecdsa.PrivateKey, caCert *x509.Certificate, sans []string) error {
	var dns []string
	var ips []net.IP
	for _, s := range sans {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "DNS:"):
			dns = append(dns, strings.TrimPrefix(s, "DNS:"))
		case strings.HasPrefix(s, "IP:"):
			ip := net.ParseIP(strings.TrimPrefix(s, "IP:"))
			if ip == nil {
				return fmt.Errorf("bad SAN %q", s)
			}
			ips = append(ips, ip)
		case s == "":
		default:
			return fmt.Errorf("bad SAN %q (want DNS:… or IP:…)", s)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: pkiSerial(),
		Subject:      pkix.Name{CommonName: "koto-daemon"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 825),
		DNSNames:     dns,
		IPAddresses:  ips,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := pkiWriteKey(filepath.Join(credsDir, "server.key"), key); err != nil {
		return err
	}
	return pkiWritePEM(filepath.Join(credsDir, "server.crt"), "CERTIFICATE", der, 0o644)
}

// pkiClient mints a client identity: cert+key, fingerprint line in
// clients.allow (dedup by fingerprint, like the Makefile), a fresh bearer
// token at token-<name> (0600) and its hash+roles in tokens.json. Returns the
// token so callers (wizard) can use it immediately.
func pkiClient(credsDir, name string, roles []string) (token string, err error) {
	if name == "" {
		return "", fmt.Errorf("client name required")
	}
	caKey, err := pkiReadKey(filepath.Join(credsDir, "ca.key"))
	if err != nil {
		return "", fmt.Errorf("ca.key (run `koto pki init` first): %w", err)
	}
	caCert, err := pkiReadCert(filepath.Join(credsDir, "ca.crt"))
	if err != nil {
		return "", fmt.Errorf("ca.crt: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: pkiSerial(),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 825),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return "", err
	}
	if err := pkiWriteKey(filepath.Join(credsDir, "client-"+name+".key"), key); err != nil {
		return "", err
	}
	if err := pkiWritePEM(filepath.Join(credsDir, "client-"+name+".crt"), "CERTIFICATE", der, 0o644); err != nil {
		return "", err
	}

	// allowlist: "<sha256hex of DER leaf> <name>" — exactly what the
	// VerifyPeerCertificate hook in auth.go computes over rawCerts[0].
	fp := sha256.Sum256(der)
	fpHex := hex.EncodeToString(fp[:])
	allowPath := filepath.Join(credsDir, "clients.allow")
	existing, _ := os.ReadFile(allowPath)
	if !strings.Contains(string(existing), fpHex) {
		f, err := os.OpenFile(allowPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(f, "%s %s\n", fpHex, name); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
	}

	// bearer token + tokens.json entry {hash, roles}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token = hex.EncodeToString(raw)
	if err := os.WriteFile(filepath.Join(credsDir, "token-"+name), []byte(token), 0o600); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(token))
	tokPath := filepath.Join(credsDir, "tokens.json")
	toks := map[string]json.RawMessage{}
	if b, err := os.ReadFile(tokPath); err == nil {
		if err := json.Unmarshal(b, &toks); err != nil {
			return "", fmt.Errorf("tokens.json is corrupt — fix it on disk first: %w", err)
		}
	}
	entry, _ := json.Marshal(map[string]any{"hash": hex.EncodeToString(sum[:]), "roles": roles})
	toks[name] = entry
	out, err := json.MarshalIndent(toks, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := tokPath + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	return token, os.Rename(tmp, tokPath)
}

// ---- helpers ---------------------------------------------------------------

func pkiSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err) // rand.Reader failing is unrecoverable
	}
	return n
}

func pkiWriteKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return pkiWritePEM(path, "EC PRIVATE KEY", der, 0o600)
}

func pkiWritePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func pkiReadKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

func pkiReadCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

// ---- CLI -------------------------------------------------------------------

// pkiCliMain implements `koto pki init|client` — the openssl-free counterpart
// of the Makefile targets, sharing their file formats and idempotency rules.
func pkiCliMain(args []string) {
	usage := func() {
		fmt.Fprintln(os.Stderr, `usage: koto pki init   [-creds DIR] [-san DNS:a,IP:1.2.3.4]
       koto pki client [-creds DIR] [-role r1,r2] <name>
       koto pki server [-creds DIR] -san DNS:a,IP:1.2.3.4

init   creates the CA (if absent), the daemon server cert and acl.json
client mints a client identity: cert, key and bearer token
server reissues ONLY the server cert — use it to add a name or address
       after the fact (a phone, a LAN IP). Existing clients keep working:
       the CA is untouched, so nothing they hold is invalidated. Restart
       the daemon to pick it up.`)
		os.Exit(2)
	}
	if len(args) < 1 {
		usage()
	}
	switch args[0] {
	case "init":
		fs := newPkiFlags("pki init")
		creds := fs.String("creds", "creds", "creds directory")
		san := fs.String("san", strings.Join(defaultServerSANs, ","), "server cert SANs")
		_ = fs.Parse(args[1:])
		if err := pkiInit(*creds, strings.Split(*san, ",")); err != nil {
			ctlFatal(1, "pki init: %v", err)
		}
		fmt.Printf("CA + server cert ready in %s. Distribute ca.crt to clients.\n", *creds)
	case "server":
		// Reissuing the server cert is the one regeneration that is always
		// safe: it is signed by the same CA, so every client identity keeps
		// verifying. Adding a SAN after the fact is a normal operation
		// (someone wants the TUI on their phone), not a reinstall.
		fs := newPkiFlags("pki server")
		creds := fs.String("creds", "creds", "creds directory")
		san := fs.String("san", "", "server cert SANs (comma-separated)")
		_ = fs.Parse(args[1:])
		if *san == "" {
			ctlFatal(2, "pki server: -san is required (e.g. -san %s,IP:192.168.1.20)",
				strings.Join(defaultServerSANs, ","))
		}
		caKey, err := pkiReadKey(filepath.Join(*creds, "ca.key"))
		if err != nil {
			ctlFatal(1, "pki server: ca.key: %v", err)
		}
		caCert, err := pkiReadCert(filepath.Join(*creds, "ca.crt"))
		if err != nil {
			ctlFatal(1, "pki server: ca.crt: %v", err)
		}
		if err := pkiServerCert(*creds, caKey, caCert, strings.Split(*san, ",")); err != nil {
			ctlFatal(1, "pki server: %v", err)
		}
		fmt.Printf("server cert reissued for %s — restart the daemon to apply\n", *san)
	case "client":
		fs := newPkiFlags("pki client")
		creds := fs.String("creds", "creds", "creds directory")
		role := fs.String("role", "admin", "comma-separated roles")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			usage()
		}
		name := fs.Arg(0)
		var roles []string
		for _, r := range strings.Split(*role, ",") {
			if r = strings.TrimSpace(r); r != "" {
				roles = append(roles, r)
			}
		}
		if _, err := pkiClient(*creds, name, roles); err != nil {
			ctlFatal(1, "pki client: %v", err)
		}
		fmt.Printf("client cert %s/client-%s.{crt,key} minted; token in %s/token-%s; roles %v registered\n",
			*creds, name, *creds, name, roles)
	default:
		usage()
	}
}
