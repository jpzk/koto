package credproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Proxy is the host-side explicit HTTPS proxy (design §1). Guests send
// CONNECT host:port. Hosts that appear in the policy are MITM'd with a leaf
// minted by CA; every other host is a raw TCP tunnel (passthrough, §10) so
// pinned clients keep working and no placeholder is ever substituted for them.
//
// One Proxy serves exactly one guest group: Group is fixed at construction
// time by the daemon (never taken from anything the guest sends).
type Proxy struct {
	Policy *Policy
	CA     *CA
	Group  string

	// UpstreamRoots verifies upstream servers. nil => system roots.
	UpstreamRoots *x509.CertPool
	// Dial opens the upstream TCP connection. nil => net.Dialer. Tests and the
	// e2e harness override it to map a hostname onto a local fake upstream.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Logf receives audit lines. Lines never contain secret values.
	Logf func(format string, args ...any)

	once      sync.Once
	redactor  *Redactor
	transport *http.Transport
}

func (p *Proxy) init() {
	p.once.Do(func() {
		p.redactor = NewRedactor(p.Policy.Values())
		dial := p.Dial
		if dial == nil {
			d := &net.Dialer{Timeout: 15 * time.Second}
			dial = d.DialContext
		}
		p.transport = &http.Transport{
			DialContext:           dial,
			TLSClientConfig:       &tls.Config{RootCAs: p.UpstreamRoots, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   4,
			// Never let a guest-supplied Proxy-* env leak in: upstream is direct.
			Proxy: nil,
		}
		if p.Logf == nil {
			p.Logf = log.Printf
		}
	})
}

func (p *Proxy) dial(ctx context.Context, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, "tcp", addr)
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

// hasPolicy reports whether any secret lists host as a destination. Only those
// hosts are ever MITM'd (design §2: the CA only mints for policied hosts).
func (p *Proxy) hasPolicy(host string) bool {
	for _, s := range p.Policy.Secrets {
		for _, h := range s.Hosts {
			if hostMatches(h, host) {
				return true
			}
		}
	}
	return false
}

// ServeHTTP accepts only CONNECT. Plain-HTTP proxying is refused: a
// placeholder must never travel in cleartext and there is no TLS name to bind
// the policy to.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	if r.Method != http.MethodConnect {
		http.Error(w, "cred-proxy: only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}
	authority := r.Host
	host := hostOf(authority)
	if host == "" {
		http.Error(w, "cred-proxy: bad CONNECT authority", http.StatusBadRequest)
		return
	}
	if !strings.Contains(authority, ":") || strings.HasSuffix(authority, "]") {
		authority = net.JoinHostPort(host, "443")
	}
	// §9: loopback / link-local / unspecified literals are never reachable.
	if IsLocalDest(host) {
		p.Logf("credproxy: group=%s CONNECT %s refused: local destination", p.Group, authority)
		http.Error(w, "cred-proxy: destination not allowed", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cred-proxy: hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	// §9: IP literals have no name to match => passthrough, never MITM.
	mitm := !IsIPLiteral(host) && p.hasPolicy(host)
	if !mitm {
		p.tunnel(conn, brw, authority)
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	p.mitm(conn, brw.Reader, host, authority)
}

// tunnel is the passthrough path: bytes are copied verbatim, nothing parsed.
func (p *Proxy) tunnel(conn net.Conn, brw *bufio.ReadWriter, authority string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	up, err := p.dial(ctx, authority)
	cancel()
	if err != nil {
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer up.Close()
	p.Logf("credproxy: group=%s CONNECT %s passthrough", p.Group, authority)
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, brw.Reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
}

// prefixConn replays bytes already buffered by the HTTP server's reader.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (p *Proxy) mitm(conn net.Conn, buffered *bufio.Reader, host, authority string) {
	var sni string
	leaf, err := p.CA.LeafFor(host) // always minted for the CONNECT host, never SNI
	if err != nil {
		p.Logf("credproxy: leaf for %s: %v", host, err)
		return
	}
	tconn := tls.Server(&prefixConn{Conn: conn, r: io.MultiReader(buffered, conn)}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			sni = hi.ServerName
			return leaf, nil
		},
	})
	_ = tconn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tconn.Handshake(); err != nil {
		return
	}
	_ = tconn.SetDeadline(time.Time{})
	defer tconn.Close()

	br := bufio.NewReader(tconn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		keep := p.handle(tconn, req, host, authority, sni)
		if req.Body != nil {
			_, _ = io.Copy(io.Discard, req.Body)
			req.Body.Close()
		}
		if !keep || req.Close {
			return
		}
	}
}

func writeStatus(w io.Writer, code int, msg string) {
	resp := &http.Response{
		StatusCode:    code,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(msg + "\n")),
		ContentLength: int64(len(msg) + 1),
	}
	_ = resp.Write(w)
}

// handle evaluates, substitutes, forwards and sanitizes one request. It returns
// whether the guest connection may be reused.
func (p *Proxy) handle(w io.Writer, req *http.Request, host, authority, sni string) bool {
	d := p.Policy.Evaluate(Request{
		Group:       p.Group,
		ConnectHost: host,
		SNI:         sni,
		HostHeader:  req.Host,
		Header:      req.Header,
	})
	p.Logf("credproxy: group=%s %s https://%s%s decision allow=%v block=%v reason=%q",
		p.Group, req.Method, host, req.URL.RequestURI(), d.Allow, d.Block, d.Reason)
	if d.Block {
		writeStatus(w, http.StatusForbidden, "cred-proxy: blocked: "+d.Reason)
		return false
	}

	out := req.Clone(context.Background())
	out.RequestURI = ""
	out.URL.Scheme = "https"
	out.URL.Host = authority // dial target is always the CONNECT authority (§7)
	out.Host = host
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	d.Substitute(out.Header)

	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		writeStatus(w, http.StatusBadGateway, "cred-proxy: upstream error")
		return false
	}
	defer resp.Body.Close()

	authHeader := ""
	if d.Secret != nil {
		authHeader = d.Secret.Header
	}
	SanitizeResponse(resp, host, authHeader, p.redactor)
	// Redaction can change the body length: re-frame any body as chunked.
	if bodyAllowed(req.Method, resp.StatusCode) {
		resp.Body = io.NopCloser(p.redactor.Reader(resp.Body))
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.TransferEncoding = []string{"chunked"}
	}
	resp.Close = false
	resp.ProtoMajor, resp.ProtoMinor = 1, 1
	if err := resp.Write(w); err != nil {
		return false
	}
	return true
}

// ErrNoPolicy is returned by NewProxy when constructed without a policy.
var ErrNoPolicy = errors.New("credproxy: nil policy or CA")

// NewProxy validates and returns a Proxy for one group.
func NewProxy(pol *Policy, ca *CA, group string) (*Proxy, error) {
	if pol == nil || ca == nil {
		return nil, ErrNoPolicy
	}
	return &Proxy{Policy: pol, CA: ca, Group: group}, nil
}

func bodyAllowed(method string, code int) bool {
	if method == http.MethodHead {
		return false
	}
	return code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified
}
