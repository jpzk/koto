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
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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
	// The server certificate is reissued when it is MISSING or no longer
	// usable, not only when the file is absent (audit 2026-09-11 L144). The
	// test was os.Stat alone, and the setup step's detector agrees with it —
	// so an expired certificate (they are minted for a year), one that no
	// longer chains to the current CA, or an unparseable pair, survived every
	// `koto setup` and every `koto pki init` untouched. What the operator then
	// gets is a daemon whose clients all refuse it, from a setup run that
	// reported success, with `koto pki server` as the fix nothing had told
	// them to run.
	if why := pkiServerCertNeedsReissue(credsDir, caCert); why != "" {
		if why != "absent" {
			fmt.Printf("reissuing server.crt: %s\n", why)
		}
		if err := pkiServerCert(credsDir, caKey, caCert, serverSANs); err != nil {
			return err
		}
	}
	seed := `{
  "agent": {
    "list": "*", "send": "*", "history": "*", "metrics": "*",
    "sched_list": "*",
    "subscribe_group": "*", "watch_state": "*"
  }
}
`
	if _, err := pkiCreateNew(filepath.Join(credsDir, "acl.json"), []byte(seed), 0o600); err != nil {
		return err
	}
	return nil
}

// pkiServerCertNeedsReissue reports why the server certificate must be minted
// again, or "" when the existing one is fine. Deliberately generous about what
// counts: this runs in a provisioning path, where reissuing a healthy-looking
// certificate costs a few milliseconds and keeping a broken one costs an
// outage. SANs are NOT checked here — changing them is the operator's explicit
// `koto pki server -san …`, and silently narrowing or widening what the daemon
// answers to would be a different decision from renewal.
func pkiServerCertNeedsReissue(credsDir string, caCert *x509.Certificate) string {
	crtPath := filepath.Join(credsDir, "server.crt")
	if _, err := os.Stat(crtPath); err != nil {
		return "absent"
	}
	if _, err := os.Stat(filepath.Join(credsDir, "server.key")); err != nil {
		return "server.key is missing"
	}
	cert, err := pkiReadCert(crtPath)
	if err != nil {
		return fmt.Sprintf("unreadable (%v)", err)
	}
	now := time.Now()
	switch {
	case now.After(cert.NotAfter):
		return fmt.Sprintf("expired on %s", cert.NotAfter.Format("2006-01-02"))
	case now.Add(pkiRenewWindow).After(cert.NotAfter):
		return fmt.Sprintf("expires on %s, inside the %s renewal window",
			cert.NotAfter.Format("2006-01-02"), pkiRenewWindow)
	case now.Before(cert.NotBefore):
		return "not valid yet (check the host clock)"
	}
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		return "does not chain to this CA"
	}
	return ""
}

// pkiRenewWindow is how long before expiry a provisioning run renews. Long
// enough that an operator who runs setup even twice a year never meets an
// expired certificate.
const pkiRenewWindow = 30 * 24 * time.Hour

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
	// A ca.crt with no ca.key is a HALF CA, and generating over it is the one
	// outcome that must not happen (audit 2026-09-11 L137). The reuse test was
	// `ca.key exists`, so an absent key made this mint a fresh CA and write
	// BOTH paths — replacing the trust anchor. Every client certificate ever
	// issued then fails verification against the new ca.crt on the daemon's
	// next start, and any client still holding the old CA rejects the new
	// server certificate: a total mTLS outage from a state that reads, to the
	// setup detector, as "PKI missing, initialise it".
	//
	// Refusing is the only safe answer, because this code cannot tell the two
	// causes apart: a key deleted by accident (the certificate is still the
	// fleet's anchor and the key must be restored) or a genuinely new install
	// on top of a stray file (the certificate is junk and should be moved
	// aside). The operator can, and the message says what each looks like.
	if _, err := os.Stat(crtPath); err == nil {
		return nil, nil, false, fmt.Errorf(
			"%s exists but %s does not — refusing to mint a new CA over it.\n"+
				"If the key was lost, restore it from a backup: without it no new client or server "+
				"certificate can be issued, and generating a replacement CA invalidates every "+
				"certificate already issued to every client.\n"+
				"If this certificate is a leftover and you really want a fresh PKI, move it aside "+
				"(mv %s %s.old) and re-run.", crtPath, keyPath, crtPath, crtPath)
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
	// EXCLUSIVE: this is the one file in the tree that must never be written
	// twice or written through a link. The os.Stat above is a fast path for
	// "already there", not the check — a dangling symlink at ca.key makes Stat
	// say absent, and O_CREATE alone would then have put the CA private key
	// wherever the link pointed (audit M146).
	created, err := pkiCreateNew(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "EC PRIVATE KEY", Bytes: pkiMustMarshalKey(key)}), 0o600)
	if err != nil {
		return nil, nil, false, err
	}
	if !created {
		return nil, nil, false, fmt.Errorf("%s appeared while this CA was being generated — "+
			"refusing to overwrite it; re-run to use the existing CA", keyPath)
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
// pkiClientNameRE bounds a client identity's name. It becomes three FILENAMES
// (token-<name>, client-<name>.crt, client-<name>.key), a tokens.json key and a
// certificate CN, and `koto pki client` took it straight off argv (audit
// 2026-09-11 L33). The fixed prefix eats the first `..` component, but
// "../../x" still resolves above credsDir, so the writes landed wherever the
// caller pointed them — and a name with a control character or a space would
// have produced material the allowlist and the CN check could never match
// again.
var pkiClientNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func pkiClientNameOK(name string) bool {
	return pkiClientNameRE.MatchString(name) && !strings.Contains(name, "..")
}

func pkiClient(credsDir, name string, roles []string) (token string, err error) {
	if !pkiClientNameOK(name) {
		return "", fmt.Errorf("invalid client name %q: letters, digits and ._- only, "+
			"no path separators (it becomes creds/token-%s and creds/client-%s.{crt,key})", name, "<name>", "<name>")
	}
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
		// O_NOFOLLOW for the same reason as everything else here: an append
		// through a planted link writes an allowlist entry into somebody
		// else's file (audit M146).
		f, err := os.OpenFile(allowPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return "", pkiSymlinkErr(allowPath, err)
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
	if err := pkiWriteFile(filepath.Join(credsDir, "token-"+name), []byte(token), 0o600); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(token))
	tokPath := filepath.Join(credsDir, "tokens.json")

	// The read-modify-write is under an INTERPROCESS lock (audit 2026-09-11
	// L133). Each `koto pki client` invocation read the registry, added its
	// own entry and renamed a fixed temporary name into place — so two runs
	// with overlapping snapshots left the loser's token hash out of the file
	// that authentication actually reads. The per-client `token-<name>` file
	// still existed, so the failure looked like a working credential the
	// daemon inexplicably rejects. `make pki-client` in a loop, or two
	// terminals, is all it takes.
	//
	// flock, not a lockfile-by-rename: it is released by the kernel when the
	// process exits, so an interrupted provisioning run cannot wedge every
	// later one. The temporary name is unique as well, so the two never fight
	// over it even if the lock is unavailable on some exotic filesystem.
	unlock, err := pkiLockCreds(credsDir)
	if err != nil {
		return "", err
	}
	defer unlock()

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
	tmpf, err := os.CreateTemp(credsDir, ".tokens.json.*")
	if err != nil {
		return "", err
	}
	tmp := tmpf.Name()
	tmpf.Close()
	if err := pkiWriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, tokPath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return token, nil
}

// pkiLockCreds takes an exclusive lock on the credentials directory for the
// duration of a registry update. The lock file is its own inode so it is never
// the thing being renamed, and flock is advisory between the processes that
// take it — which is every writer here, the only ones that exist.
func pkiLockCreds(credsDir string) (func(), error) {
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(credsDir, ".tokens.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", credsDir, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// ---- helpers ---------------------------------------------------------------

func pkiSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err) // rand.Reader failing is unrecoverable
	}
	return n
}

// pkiMustMarshalKey is MarshalECPrivateKey for a key this process just
// generated: the only error it can return is an unsupported curve, and the
// curve is P-256 three lines up.
func pkiMustMarshalKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic("pki: P-256 key failed to marshal: " + err.Error())
	}
	return der
}

func pkiWriteKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return pkiWritePEM(path, "EC PRIVATE KEY", der, 0o600)
}

func pkiWritePEM(path, typ string, der []byte, mode os.FileMode) error {
	return pkiWriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

// pkiWriteFile writes one credential file, REFUSING to write through a symlink
// (audit M146). os.WriteFile follows the final component, so a link planted at
// creds/<name> before the operator runs `koto pki` sends a private key, a
// certificate or an allowlist entry to a path of somebody else's choosing.
// O_NOFOLLOW makes that an ELOOP instead. Overwrite is still allowed — a server
// cert is reissued in place — it is only the LINK that is refused.
func pkiWriteFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return pkiSymlinkErr(path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// pkiCreateNew writes a file that must not already exist, and reports whether
// it did the writing. O_EXCL is the absence check — which is the point: the
// stat-then-write it replaces had both halves of the problem, since os.Stat
// FOLLOWS a symlink and reported a dangling one as "absent", after which
// O_CREATE followed the same link and created the file at its target (audit
// M146). With O_EXCL|O_NOFOLLOW an existing path is an existing path, symlink
// included, and there is no window between the question and the answer.
func pkiCreateNew(path string, data []byte, mode os.FileMode) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		if os.IsExist(err) {
			// O_EXCL outranks O_NOFOLLOW, so a symlink at the final component
			// comes back EEXIST rather than ELOOP — indistinguishable from the
			// ordinary "already seeded" case that must pass. Nothing was
			// written either way; the question is whether to keep going, and
			// for acl.json the answer is no: this is the file loadACL READS,
			// so accepting a link means taking the authorization policy from
			// wherever it points.
			return false, pkiNoSymlink(path)
		}
		return false, pkiSymlinkErr(path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// pkiNoSymlink refuses a path whose final component is a symlink. Used where
// the file is READ as well as written — the CA key and certificate, the ACL
// seed — since following a link there takes the material from somewhere the
// operator did not choose, which is the same defect as writing through one.
func pkiNoSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil // absent, or unreadable for reasons the caller will hit anyway
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink — refusing to use credential material through it", path)
	}
	return nil
}

// pkiSymlinkErr names the cause when the refusal was a symlink, because ELOOP
// on a path the operator just typed reads as nonsense otherwise.
func pkiSymlinkErr(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("%s is a symlink — refusing to write credential material through it", path)
	}
	return err
}

func pkiReadKey(path string) (*ecdsa.PrivateKey, error) {
	if err := pkiNoSymlink(path); err != nil {
		return nil, err
	}
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
	if err := pkiNoSymlink(path); err != nil {
		return nil, err
	}
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
