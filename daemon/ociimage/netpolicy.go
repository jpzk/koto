package ociimage

// netpolicy.go is the registry client's outbound policy. A registry decides
// where koto connects next (token realm in Www-Authenticate, 30x redirects to
// blob CDNs), so every connection, not only the first, is checked:
//
//   - https only, on the first request and on every redirect hop;
//   - the dial hook refuses loopback, private (RFC 1918 / ULA), link-local
//     (incl. 169.254.169.254 metadata), CGNAT, multicast, unspecified and
//     reserved addresses AFTER DNS resolution, so a public name that resolves
//     to an internal address is refused too (no DNS-rebinding gap: the check
//     runs on the address actually being connected to);
//   - no environment proxy (a proxy would move the dial off the checked
//     address);
//   - at most maxRedirects hops; the registry bearer token is never sent to
//     a host other than the registry it was issued for;
//   - transport-level timeouts for dial / TLS / response headers, and a stall
//     timer on response bodies instead of a whole-request deadline, so a
//     2 GiB layer on a slow link is not cut off mid-transfer.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

const (
	maxRedirects  = 5
	dialTimeout   = 30 * time.Second
	headerTimeout = 60 * time.Second
	// bodyStall: a response body that delivers no byte for this long is
	// aborted (reported as a timeout, so the bounded retry applies).
	bodyStall = 2 * time.Minute
)

// ErrBlockedAddr is returned when a registry, token realm or redirect
// target resolves to an address the outbound policy refuses.
var ErrBlockedAddr = errors.New("outbound policy: refusing non-public address")

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // CGNAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved + broadcast
		"100::/64",        // discard
		"2001:db8::/32",   // documentation
		"fec0::/10",       // deprecated site-local
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// BlockedAddr reports whether the outbound policy refuses address a.
func BlockedAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if nat64.Contains(a) { // well-known NAT64: judge the embedded IPv4
		b := a.As16()
		return BlockedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	return false
}

// dialControl runs on the resolved address of every connection attempt.
func (c *Client) dialControl(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable %q", ErrBlockedAddr, address)
	}
	if c.allowAddr != nil && c.allowAddr(ap) {
		return nil
	}
	if BlockedAddr(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrBlockedAddr, ap)
	}
	return nil
}

// checkRedirect caps hops, keeps https, and drops the registry token when a
// hop leaves the host the original request went to.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("outbound policy: more than %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("outbound policy: refusing non-https redirect to %s", req.URL.Redacted())
	}
	if req.URL.User != nil {
		return errors.New("outbound policy: refusing redirect with userinfo")
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// hc builds the policy-carrying client once. There is deliberately no
// http.Client.Timeout (it would bound body reads of large blobs); the slow
// phases are bounded at the transport and by the body stall timer.
func (c *Client) hc() *http.Client {
	c.hcOnce.Do(func() {
		d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: c.dialControl}
		tr := &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.rootCAs},
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: headerTimeout,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          8,
		}
		c.http = &http.Client{Transport: tr, CheckRedirect: checkRedirect}
	})
	return c.http
}

func (c *Client) ctx() context.Context {
	if c.Ctx != nil {
		return c.Ctx
	}
	return context.Background()
}

// do sends req under a per-request context: cancelled with the client's Ctx,
// or when the body stalls for bodyStall. The returned body cancels on Close.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(c.ctx())
	stall := c.stall
	if stall <= 0 {
		stall = bodyStall
	}
	t := time.AfterFunc(stall, func() { cancel(errStall) })
	resp, err := c.hc().Do(req.WithContext(ctx))
	if err != nil {
		t.Stop()
		if context.Cause(ctx) == errStall {
			err = fmt.Errorf("%w: %v", errStall, err)
		}
		cancel(nil)
		return nil, err
	}
	resp.Body = &stallBody{rc: resp.Body, t: t, d: stall, ctx: ctx, cancel: cancel}
	return resp, nil
}

var errStall = errors.New("registry response stalled (timeout)")

type stallBody struct {
	rc     io.ReadCloser
	t      *time.Timer
	d      time.Duration
	ctx    context.Context
	cancel context.CancelCauseFunc
	once   sync.Once
}

func (b *stallBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.t.Reset(b.d)
	}
	if err != nil && err != io.EOF && context.Cause(b.ctx) == errStall {
		err = fmt.Errorf("%w: %v", errStall, err)
	}
	return n, err
}

func (b *stallBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() { b.t.Stop(); b.cancel(nil) })
	return err
}

// testTrust is used by tests only: trust pool's roots and let allow pass the
// dial policy (httptest servers listen on loopback).
func (c *Client) testTrust(pool *x509.CertPool, allow func(netip.AddrPort) bool) {
	c.rootCAs, c.allowAddr = pool, allow
}
