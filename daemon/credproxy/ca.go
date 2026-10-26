package credproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// leafTTL bounds how long a minted leaf cert is valid (design §2: short validity).
const leafTTL = 24 * time.Hour

// CA is the per-install root used to mint leaf certs for MITM'd (allowlisted)
// hosts. The root PRIVATE key is host-only and never leaves this process/disk
// (design §2). Leaf certs are minted on demand and cached per host.
type CA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// NewCA generates a fresh per-install root CA (P-256 ECDSA, ~10y, IsCA, pathlen 0).
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "koto cred-proxy CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, certDER: der, key: key, leaves: map[string]*tls.Certificate{}}, nil
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// CertPEM returns the CA certificate in PEM — the ONLY material that may be
// injected into a guest trust store. The private key is never exported.
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.certDER})
}

// Save writes the CA cert (PEM) and private key (PEM, 0600) to dir. The key file
// is host-only; callers must keep dir out of any guest mount (design §2/§3).
func (c *CA) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), c.CertPEM(), 0o600); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0o600)
}

// LoadCA reads a CA previously written by Save.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, err
	}
	cblk, _ := pem.Decode(certPEM)
	if cblk == nil {
		return nil, fmt.Errorf("credproxy: bad CA cert PEM")
	}
	cert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, err
	}
	kblk, _ := pem.Decode(keyPEM)
	if kblk == nil {
		return nil, fmt.Errorf("credproxy: bad CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, certDER: cblk.Bytes, key: key, leaves: map[string]*tls.Certificate{}}, nil
}

// LoadOrCreateCA loads a CA from dir, or generates and persists one if absent.
func LoadOrCreateCA(dir string) (*CA, error) {
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err == nil {
		return LoadCA(dir)
	}
	ca, err := NewCA()
	if err != nil {
		return nil, err
	}
	if err := ca.Save(dir); err != nil {
		return nil, err
	}
	return ca, nil
}

// LeafFor mints (or returns a cached) short-lived leaf cert for host, signed by
// the root, with host in the SAN. A fresh leaf key is used per host; the CA key
// is only ever used to sign. Expired cache entries are re-minted.
func (c *CA) LeafFor(host string) (*tls.Certificate, error) {
	host = hostOf(host)
	c.mu.Lock()
	defer c.mu.Unlock()
	if tc, ok := c.leaves[host]; ok {
		if leaf := tc.Leaf; leaf != nil && time.Now().Before(leaf.NotAfter.Add(-time.Minute)) {
			return tc, nil
		}
		delete(c.leaves, host)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(leafTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &leafKey.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	tc := &tls.Certificate{
		Certificate: [][]byte{der, c.certDER},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
	c.leaves[host] = tc
	return tc, nil
}
