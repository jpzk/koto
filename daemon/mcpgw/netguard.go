package mcpgw

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"koto/ociimage"
)

// HTTPClient is the client used for every upstream MCP / OAuth request:
//   - it never follows redirects (a 3xx comes back to the caller as-is, so
//     a request body or auth header is only ever sent to the configured URL);
//   - an https request that is not going through a proxy refuses to dial a
//     loopback, private, link-local or otherwise non-public address. The
//     check runs on the resolved address of each connection attempt, so a
//     public name that resolves inward is refused too. Plain http endpoints
//     (an operator's local MCP server) are dialled as configured.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     newGuardedRT(nil),
		CheckRedirect: noRedirect,
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

var defaultClient = HTTPClient(30 * time.Second)

type guardedRT struct {
	plain, public *http.Transport
}

func newGuardedRT(allow func(netip.AddrPort) bool) *guardedRT {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error { return publicOnly(address, allow) }}
	pub := http.DefaultTransport.(*http.Transport).Clone()
	pub.Proxy = nil
	pub.DialContext = d.DialContext
	return &guardedRT{plain: http.DefaultTransport.(*http.Transport).Clone(), public: pub}
}

func (t *guardedRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" {
		if p, err := http.ProxyFromEnvironment(r); err == nil && p == nil {
			return t.public.RoundTrip(r)
		}
	}
	return t.plain.RoundTrip(r)
}

func publicOnly(address string, allow func(netip.AddrPort) bool) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable %q", ociimage.ErrBlockedAddr, address)
	}
	if allow != nil && allow(ap) {
		return nil
	}
	if ociimage.BlockedAddr(ap.Addr()) {
		return fmt.Errorf("%w: %s", ociimage.ErrBlockedAddr, ap)
	}
	return nil
}
