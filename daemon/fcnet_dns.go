package main

// fcnet_dns.go — koto's own resolver for the guest, host-side.
//
// Hostname allowlist entries are enforced by pinning the IPs a name resolves
// to (fcnet_allow.go). Those pins come from exactly ONE place: this file.
// While a group has an active allowlist, every UDP DNS query its guest sends
// to the gateway resolver (192.168.127.1:53) is taken off the link in
// fcEgressConn.Read — BEFORE the gvisor netstack ever sees it — and answered
// here:
//
//   - koto builds a FRESH query of its own (random transaction id, the one
//     question, EDNS0) and sends it to the host's resolvers
//     (/etc/resolv.conf), never relaying guest bytes upstream verbatim;
//   - the upstream answer must match that id and question, else SERVFAIL;
//   - A/AAAA answers reachable from the question via the CNAME chain are
//     pinned when the question name or a CNAME on the path to them is
//     allowed, for the MINIMUM TTL along that path (a short-lived CNAME must
//     not keep its old target's IP open for the A record's longer TTL);
//   - the reply is synthesised and written to the guest.
//
// Nothing on the gateway → guest direction (fcEgressConn.Write) is parsed.
// That direction used to be "observed" for DNS answers on the premise that
// the guest cannot forge it — false: the gvisor switch reflects any guest
// frame whose destination MAC is not the gateway's back down the guest's own
// link, so a guest could mint pins for any IP with a forged answer (Strix,
// PR #104). The reflection itself is also closed at ingress (fcGuestMACOK).
//
// Out of scope, documented: DNS over TCP to the gateway is still served by
// gvisor's resolver and pins nothing (a TC=1 retry over TCP gets addresses
// the pins already cover only if the answer is stable); DNS the guest sends
// elsewhere (DoH, a public resolver it may reach) pins nothing either.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"koto/netallow"
)

const (
	fcDNSTimeout   = 3 * time.Second
	fcDNSInflight  = 32   // concurrent upstream lookups per guest link
	fcDNSUDPMax    = 1232 // EDNS0 payload size we offer / honour (DNS flag day 2020)
	fcDNSChainMax  = 8    // CNAME hops followed from the question
	fcDNSUDPLegacy = 512  // RFC 1035 limit for a query without EDNS0
)

// fcDNSExchange sends one wire-format query to the host's resolvers and
// returns the wire-format response. A var so tests substitute an upstream.
var fcDNSExchange = fcDNSHostExchange

// fcDNS answers one guest link's gateway DNS queries.
type fcDNS struct {
	allow  *fcAllow
	inject func(frame []byte) error // writes one frame towards the guest
	sem    chan struct{}
	wg     sync.WaitGroup // in-flight lookups (tests wait on it)
}

func newFcDNS(a *fcAllow, inject func([]byte) error) *fcDNS {
	return &fcDNS{allow: a, inject: inject, sem: make(chan struct{}, fcDNSInflight)}
}

// fcDNSQuery is where a guest query came from — what the reply goes back to.
type fcDNSQuery struct {
	mac     [6]byte
	ip      [4]byte
	port    uint16
	payload []byte
}

// intercept consumes a guest → gateway UDP :53 frame while the group has an
// active allowlist and answers it asynchronously. false = not ours, the frame
// continues to the netstack unchanged (no allowlist: gvisor's resolver, the
// pre-allowlist behaviour).
func (d *fcDNS) intercept(frame []byte) bool {
	if d == nil || d.allow == nil || !d.allow.policy().active() {
		return false
	}
	if len(frame) < 34 || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 ||
		!fcNetGatewayIPParsed.Equal(net.IP(frame[30:34])) {
		return false
	}
	pk, ok := fcParsePkt(frame)
	if !ok || pk.proto != 17 {
		return false
	}
	if pk.dstPort != 53 {
		// A non-first fragment has no ports: it cannot be a whole query
		// and gvisor would only reassemble half of one we already dropped.
		return frame[23] == 17 && binary.BigEndian.Uint16(frame[20:22])&0x1fff != 0
	}
	if pk.payload == nil {
		return true // unreadable query to the resolver: drop, never forward
	}
	q := fcDNSQuery{ip: [4]byte(frame[26:30]), port: pk.srcPort, payload: append([]byte(nil), pk.payload...)}
	copy(q.mac[:], frame[6:12])
	select {
	case d.sem <- struct{}{}:
	default:
		return true // over budget: drop, the guest's stub resolver retries
	}
	d.wg.Add(1)
	go func() {
		defer func() { <-d.sem; d.wg.Done() }()
		if resp := d.answer(q.payload); resp != nil && d.inject != nil {
			_ = d.inject(fcUDPReply(q, resp))
		}
	}()
	return true
}

// answer resolves one guest query and returns the reply message (nil = no
// reply at all, for input that is not a query).
func (d *fcDNS) answer(msg []byte) []byte {
	var in dnsmessage.Message
	if err := in.Unpack(msg); err != nil || in.Header.Response {
		return nil
	}
	reply := dnsmessage.Message{Header: dnsmessage.Header{
		ID: in.Header.ID, Response: true, OpCode: in.Header.OpCode,
		RecursionDesired: in.Header.RecursionDesired, RecursionAvailable: true,
	}, Questions: in.Questions}
	limit := fcDNSUDPLegacy
	for _, r := range in.Additionals {
		if r.Header.Type != dnsmessage.TypeOPT {
			continue
		}
		// The guest speaks EDNS0: answer in kind (an OPT of our own) and
		// honour its payload size up to ours.
		if n := int(r.Header.Class); n > limit {
			limit = min(n, fcDNSUDPMax)
		}
		var opt dnsmessage.ResourceHeader
		if opt.SetEDNS0(fcDNSUDPMax, dnsmessage.RCodeSuccess, false) == nil {
			reply.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
		}
		break
	}
	if in.Header.OpCode != 0 || len(in.Questions) != 1 || in.Questions[0].Class != dnsmessage.ClassINET {
		reply.Header.RCode = dnsmessage.RCodeNotImplemented
		return fcDNSPack(reply, limit)
	}
	q := in.Questions[0]
	up, err := fcDNSResolve(q)
	if err != nil {
		reply.Header.RCode = dnsmessage.RCodeServerFailure
		return fcDNSPack(reply, limit)
	}
	if p := d.allow.policy(); p.active() && !p.denyAll && p.list.HasHosts() &&
		up.Header.RCode == dnsmessage.RCodeSuccess {
		fcPinAnswers(p.list, d.allow.pinSet(p), q.Name.String(), up.Answers)
	}
	reply.Header.RCode = up.Header.RCode
	reply.Header.Authoritative = false
	reply.Answers, reply.Authorities = up.Answers, up.Authorities
	return fcDNSPack(reply, limit)
}

// fcDNSResolve asks the host's resolvers q with a fresh id and validates that
// the response answers exactly that.
func fcDNSResolve(q dnsmessage.Question) (*dnsmessage.Message, error) {
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint16(idb[:])
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.EnableCompression()
	var opt dnsmessage.ResourceHeader
	if err := opt.SetEDNS0(fcDNSUDPMax, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, err
	}
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAdditionals() != nil ||
		b.OPTResource(opt, dnsmessage.OPTResource{}) != nil {
		return nil, errors.New("dns: cannot build query")
	}
	wire, err := b.Finish()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fcDNSTimeout)
	defer cancel()
	raw, err := fcDNSExchange(ctx, wire)
	if err != nil {
		return nil, err
	}
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		return nil, err
	}
	if !m.Header.Response || m.Header.ID != id || len(m.Questions) != 1 ||
		m.Questions[0].Type != q.Type || m.Questions[0].Class != q.Class ||
		!strings.EqualFold(m.Questions[0].Name.String(), q.Name.String()) {
		return nil, errors.New("dns: upstream answer does not match the query")
	}
	return &m, nil
}

// fcDNSPack packs reply within limit bytes, dropping records from the end and
// setting TC when it does not fit (pins were already taken from the full
// upstream answer).
func fcDNSPack(m dnsmessage.Message, limit int) []byte {
	for {
		out, err := m.Pack()
		if err != nil {
			return nil
		}
		if len(out) <= limit {
			return out
		}
		m.Header.Truncated = true
		switch {
		case len(m.Authorities) > 0:
			m.Authorities = m.Authorities[:len(m.Authorities)-1]
		case len(m.Answers) > 0:
			m.Answers = m.Answers[:len(m.Answers)-1]
		default:
			return nil
		}
	}
}

// fcPinAnswers pins the A/AAAA records reachable from qname through the CNAME
// chain, when qname or a CNAME on the path to the record is an allowed name.
// The pin is tagged with the first allowed name on that path (what a later
// list change is judged against) and lives for the minimum TTL along the path
// including the address record itself.
func fcPinAnswers(l *netallow.List, pins *netallow.Pins, qname string, ans []dnsmessage.Resource) int {
	norm := func(s string) string { return strings.TrimSuffix(strings.ToLower(s), ".") }
	type reach struct {
		ttl uint32
		tag string // first allowed name on the path, "" = none yet
	}
	start := norm(qname)
	seen := map[string]reach{start: {ttl: ^uint32(0)}}
	if l.MatchHost(start) {
		seen[start] = reach{ttl: ^uint32(0), tag: start}
	}
	frontier := []string{start}
	for hop := 0; hop < fcDNSChainMax && len(frontier) > 0; hop++ {
		var next []string
		for _, owner := range frontier {
			from := seen[owner]
			for _, r := range ans {
				c, ok := r.Body.(*dnsmessage.CNAMEResource)
				if !ok || norm(r.Header.Name.String()) != owner {
					continue
				}
				tgt := norm(c.CNAME.String())
				if _, dup := seen[tgt]; dup {
					continue
				}
				to := reach{ttl: min(from.ttl, r.Header.TTL), tag: from.tag}
				if to.tag == "" && l.MatchHost(tgt) {
					to.tag = tgt
				}
				seen[tgt] = to
				next = append(next, tgt)
			}
		}
		frontier = next
	}
	n := 0
	for _, r := range ans {
		at, ok := seen[norm(r.Header.Name.String())]
		if !ok || at.tag == "" {
			continue
		}
		var a netip.Addr
		switch body := r.Body.(type) {
		case *dnsmessage.AResource:
			a = netip.AddrFrom4(body.A)
		case *dnsmessage.AAAAResource:
			a = netip.AddrFrom16(body.AAAA).Unmap()
		default:
			continue
		}
		ttl := min(at.ttl, r.Header.TTL)
		if pins.AddHost(a, at.tag, time.Duration(ttl)*time.Second) {
			n++
		}
	}
	return n
}

// fcUDPReply builds the gateway → guest ethernet/IPv4/UDP frame carrying a
// DNS reply from 192.168.127.1:53 back to where the query came from.
func fcUDPReply(q fcDNSQuery, payload []byte) []byte {
	f := make([]byte, 14+20+8+len(payload))
	copy(f[0:6], q.mac[:])
	gwMAC, _ := net.ParseMAC(fcNetGatewayMAC)
	copy(f[6:12], gwMAC)
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	ip := f[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+8+len(payload)))
	binary.BigEndian.PutUint16(ip[6:8], 0x4000) // DF
	ip[8], ip[9] = 64, 17
	copy(ip[12:16], fcNetGatewayIPParsed.To4())
	copy(ip[16:20], q.ip[:])
	binary.BigEndian.PutUint16(ip[10:12], ^fcCsum(0, ip))
	udp := f[34:]
	binary.BigEndian.PutUint16(udp[0:2], 53)
	binary.BigEndian.PutUint16(udp[2:4], q.port)
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(payload)))
	copy(udp[8:], payload)
	var ph [12]byte
	copy(ph[0:8], ip[12:20])
	ph[9] = 17
	binary.BigEndian.PutUint16(ph[10:12], uint16(len(udp)))
	cs := ^fcCsum(fcCsum(0, ph[:]), udp)
	if cs == 0 {
		cs = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], cs)
	return f
}

// fcCsum folds b into the running one's-complement sum s (RFC 1071).
func fcCsum(s uint16, b []byte) uint16 {
	sum := uint32(s)
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum > 0xffff {
		sum = sum>>16 + sum&0xffff
	}
	return uint16(sum)
}

// ---- the host's resolvers ---------------------------------------------------

// fcDNSHostExchange tries each nameserver of the host's /etc/resolv.conf over
// UDP (TCP on TC=1) — the same upstreams gvisor's own resolver forwards to.
func fcDNSHostExchange(ctx context.Context, q []byte) ([]byte, error) {
	servers := fcHostNameservers()
	if len(servers) == 0 {
		return nil, errors.New("dns: no host nameservers")
	}
	var last error
	for _, ns := range servers {
		resp, err := fcDNSExchangeOne(ctx, "udp", ns, q)
		if err == nil && len(resp) > 2 && resp[2]&0x02 != 0 { // TC
			resp, err = fcDNSExchangeOne(ctx, "tcp", ns, q)
		}
		if err == nil {
			return resp, nil
		}
		last = err
	}
	return nil, last
}

func fcDNSExchangeOne(ctx context.Context, network, addr string, q []byte) ([]byte, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if network == "udp" {
		if _, err := c.Write(q); err != nil {
			return nil, err
		}
		buf := make([]byte, 65535)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return nil, err
			}
			if n >= 2 && n <= len(buf) && binary.BigEndian.Uint16(buf) == binary.BigEndian.Uint16(q) {
				return append([]byte(nil), buf[:n]...), nil
			}
			// stray datagram with another id: keep waiting until the deadline
		}
	}
	msg := binary.BigEndian.AppendUint16(nil, uint16(len(q)))
	if _, err := c.Write(append(msg, q...)); err != nil {
		return nil, err
	}
	var lb [2]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return nil, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(c, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

var fcHostNameservers = func() func() []string {
	var (
		mu  sync.Mutex
		at  time.Time
		got []string
	)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < 30*time.Second && got != nil {
			return got
		}
		got, at = fcParseResolvConf("/etc/resolv.conf"), time.Now()
		return got
	}
}()

// fcParseResolvConf returns "ip:53" for each nameserver line.
func fcParseResolvConf(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	out := []string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 2 || fs[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(strings.SplitN(fs[1], "%", 2)[0])
		if err != nil {
			continue
		}
		out = append(out, netip.AddrPortFrom(a, 53).String())
	}
	return out
}
