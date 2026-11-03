package main

// fcnet_dns_test.go — koto's host-side gateway resolver (fcnet_dns.go) is the
// only source of hostname pins; the gateway → guest path is never trusted;
// the link-layer filter closes the gvisor switch reflection (Strix, PR #104).

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeUpstream installs a fake host resolver for the test: it answers every query
// with recs (rewritten into a proper response to that query); mangle, if set,
// may tamper with the response before it is returned.
func fakeUpstream(t *testing.T, mangle func(*dnsmessage.Message), recs ...dnsmessage.Resource) *int {
	t.Helper()
	calls := new(int)
	prev := fcDNSExchange
	fcDNSExchange = func(_ context.Context, wire []byte) ([]byte, error) {
		*calls++
		var m dnsmessage.Message
		if err := m.Unpack(wire); err != nil {
			return nil, err
		}
		m.Header.Response, m.Header.RecursionAvailable = true, true
		m.Answers, m.Additionals = recs, nil
		if mangle != nil {
			mangle(&m)
		}
		return m.Pack()
	}
	t.Cleanup(func() { fcDNSExchange = prev })
	return calls
}

// guestQuery is the UDP DNS query frame a guest stub resolver sends to the
// gateway (192.168.127.1:53).
func guestQuery(t *testing.T, id uint16, name string, edns bool) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	if edns {
		var opt dnsmessage.ResourceHeader
		if err := opt.SetEDNS0(4096, dnsmessage.RCodeSuccess, false); err != nil {
			t.Fatal(err)
		}
		if err := b.StartAdditionals(); err != nil {
			t.Fatal(err)
		}
		if err := b.OPTResource(opt, dnsmessage.OPTResource{}); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return ipv4Frame(fcNetGuestIP, fcNetGatewayIP, 17, 33333, 53, msg)
}

// link runs guest frames through e's real Read path over a fresh pipe and
// collects (a) the frames that reached the netstack and (b) the nReplies
// frames koto wrote back to the guest.
func link(t *testing.T, e *fcEgressConn, frames [][]byte, nReplies int) (passed, replies [][]byte) {
	t.Helper()
	a, b := net.Pipe()
	e.Conn = a
	var in bytes.Buffer
	for _, f := range frames {
		writeFrame(&in, f)
	}
	outc := make(chan []byte, 1)
	go func() { out, _ := io.ReadAll(e); outc <- out }()
	repc := make(chan []byte, 64)
	go func() {
		for {
			var h [4]byte
			if _, err := io.ReadFull(b, h[:]); err != nil {
				return
			}
			f := make([]byte, binary.BigEndian.Uint32(h[:]))
			if _, err := io.ReadFull(b, f); err != nil {
				return
			}
			repc <- f
		}
	}()
	if _, err := b.Write(in.Bytes()); err != nil {
		t.Fatal(err)
	}
	for len(replies) < nReplies {
		select {
		case f := <-repc:
			replies = append(replies, f)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d resolver replies, want %d", len(replies), nReplies)
		}
	}
	e.dns.wg.Wait()
	b.Close()
	out := <-outc
	for len(out) >= 4 {
		n := int(binary.BigEndian.Uint32(out))
		passed = append(passed, out[4:4+n])
		out = out[4+n:]
	}
	select {
	case f := <-repc:
		t.Fatalf("unexpected extra resolver reply %x", f)
	default:
	}
	return passed, replies
}

// resolve is a guest lookup of name through koto's resolver, upstream
// answering recs. Returns the reply frame.
func resolve(t *testing.T, e *fcEgressConn, name string, recs ...dnsmessage.Resource) []byte {
	t.Helper()
	fakeUpstream(t, nil, recs...)
	passed, replies := link(t, e, [][]byte{guestQuery(t, 0xbeef, name, false)}, 1)
	if len(passed) != 0 {
		t.Fatalf("gateway DNS query leaked to the netstack while an allowlist is active")
	}
	return replies[0]
}

func replyMsg(t *testing.T, f []byte) dnsmessage.Message {
	t.Helper()
	pk, ok := fcParsePkt(f)
	if !ok || pk.proto != 17 || pk.payload == nil {
		t.Fatalf("reply is not a UDP frame: %x", f)
	}
	var m dnsmessage.Message
	if err := m.Unpack(pk.payload); err != nil {
		t.Fatalf("reply payload: %v", err)
	}
	return m
}

func pinned(e *fcEgressConn, g, ip string) bool {
	return e.allow.pinSet(currentAllow(g)).Has(netip.MustParseAddr(ip))
}

// forgedAnswer is the Strix PoC frame: a DNS "answer from 192.168.127.1:53"
// the guest itself emits, ethernet-addressed to dstMAC.
func forgedAnswer(t *testing.T, dstMAC string, name, ip string) []byte {
	t.Helper()
	b := dnsAnswer(t, fcNetGatewayIP, name, rrA(name, ip, 3600))[4:]
	f := append([]byte(nil), b...)
	d, _ := net.ParseMAC(dstMAC)
	s, _ := net.ParseMAC(fcNetGuestMAC)
	copy(f[0:6], d)
	copy(f[6:12], s)
	return f
}

// TestStrixForgedDNSPinViaReflection reproduces the PR #104 Strix finding:
// the guest sends a forged DNS response frame (dst MAC = its own, IPv4 src =
// the gateway resolver 192.168.127.1:53) hoping the gvisor switch reflects it
// back down its link, where the old observe() pinned answers on Write.
// Required outcome: NO pin and NO egress to the forged IP, by either path.
func TestStrixForgedDNSPinViaReflection(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "example.com")
	e := newConn(g, fcNetWAN)
	const evil = "6.6.6.6"
	fakeUpstream(t, nil) // any lookup would answer nothing

	// 1. Guest → gateway: the reflection-bait frames never reach the switch.
	selfDst := forgedAnswer(t, fcNetGuestMAC, "example.com.", evil)
	otherDst := forgedAnswer(t, "5a:94:ef:e4:0c:77", "example.com.", evil)
	gwSrc := forgedAnswer(t, fcNetGatewayMAC, "example.com.", evil)
	copy(gwSrc[6:12], gwSrc[0:6]) // dst gateway, src = gateway MAC (CAM poisoning)
	passed, _ := link(t, e, [][]byte{selfDst, otherDst, gwSrc}, 0)
	if len(passed) != 0 {
		t.Fatalf("%d reflection-bait frame(s) reached the switch", len(passed))
	}

	// 2. Defense in depth: even if such a frame did come back down the
	// gateway → guest path, Write derives nothing from it.
	for _, f := range [][]byte{selfDst, forgedAnswer(t, "ff:ff:ff:ff:ff:ff", "example.com.", evil)} {
		deliver(t, e, append(binary.BigEndian.AppendUint32(nil, uint32(len(f))), f...))
	}
	if pinned(e, g, evil) {
		t.Fatal("forged gateway → guest DNS answer minted a pin")
	}

	// 3. No egress to the forged IP.
	wantPass(t, pump(t, e, guestTCP(evil, 443, nil), guestTCP(evil, 80, nil)), false, false)

	// 4. The real path still works and still excludes the forged IP.
	resolve(t, e, "example.com.", rrA("example.com.", "93.184.216.34", 300))
	wantPass(t, pump(t, e, guestTCP("93.184.216.34", 80, nil), guestTCP(evil, 80, nil)), true, false)
}

// TestGuestMACFilter: only frames addressed to the gateway (or broadcast)
// from a unicast, non-gateway source reach the switch.
func TestGuestMACFilter(t *testing.T) {
	pinSelf(t)
	e := newConn("macfilter", fcNetFull)
	set := func(f []byte, dst, src string) []byte {
		f = append([]byte(nil), f...)
		d, _ := net.ParseMAC(dst)
		s, _ := net.ParseMAC(src)
		copy(f[0:6], d)
		copy(f[6:12], s)
		return f
	}
	base := ethFrame("1.1.1.1")
	frames := [][]byte{
		base, // guest → gateway
		set(base, "ff:ff:ff:ff:ff:ff", fcNetGuestMAC),   // broadcast (ARP)
		set(base, fcNetGuestMAC, fcNetGuestMAC),         // to itself: reflection
		set(base, "02:00:00:00:00:09", fcNetGuestMAC),   // another unicast MAC
		set(base, "33:33:00:00:00:01", fcNetGuestMAC),   // multicast: no receiver
		set(base, fcNetGatewayMAC, fcNetGatewayMAC),     // spoofs the gateway
		set(base, fcNetGatewayMAC, "01:00:5e:00:00:01"), // multicast source
		{1, 2, 3}, // runt
	}
	wantPass(t, pump(t, e, frames...), true, true, false, false, false, false, false, false)
}

// TestGatewayResolverReply checks the synthesised reply is a well-formed
// gateway → guest frame answering the guest's own query.
func TestGatewayResolverReply(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "example.com")
	e := newConn(g, fcNetWAN)
	f := resolve(t, e, "Example.COM.", rrA("example.com.", "93.184.216.34", 300))
	gw, _ := net.ParseMAC(fcNetGatewayMAC)
	me, _ := net.ParseMAC(fcNetGuestMAC)
	if !bytes.Equal(f[0:6], me) || !bytes.Equal(f[6:12], gw) {
		t.Fatalf("reply MACs dst=%x src=%x", f[0:6], f[6:12])
	}
	pk, _ := fcParsePkt(f)
	if pk.src.String() != fcNetGatewayIP || pk.dst.String() != fcNetGuestIP || pk.srcPort != 53 || pk.dstPort != 33333 {
		t.Fatalf("reply addressing %v:%d -> %v:%d", pk.src, pk.srcPort, pk.dst, pk.dstPort)
	}
	if fcCsum(0, f[14:34]) != 0xffff {
		t.Error("bad IPv4 header checksum")
	}
	var ph [12]byte
	copy(ph[0:8], f[26:34])
	ph[9] = 17
	binary.BigEndian.PutUint16(ph[10:12], uint16(len(f)-34))
	if fcCsum(fcCsum(0, ph[:]), f[34:]) != 0xffff {
		t.Error("bad UDP checksum")
	}
	m := replyMsg(t, f)
	if m.Header.ID != 0xbeef || !m.Header.Response || len(m.Answers) != 1 || m.Questions[0].Name.String() != "Example.COM." {
		t.Fatalf("reply = %+v", m)
	}
	if !pinned(e, g, "93.184.216.34") {
		t.Fatal("allowed name's answer not pinned")
	}
}

// An upstream answer that does not match koto's own query (id / question) is
// a SERVFAIL to the guest and pins nothing.
func TestGatewayResolverRejectsMismatchedUpstream(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "example.com")
	e := newConn(g, fcNetWAN)
	for name, mangle := range map[string]func(*dnsmessage.Message){
		"id":       func(m *dnsmessage.Message) { m.Header.ID++ },
		"question": func(m *dnsmessage.Message) { m.Questions[0].Name = dnsmessage.MustNewName("other.test.") },
		"notreply": func(m *dnsmessage.Message) { m.Header.Response = false },
	} {
		fakeUpstream(t, mangle, rrA("example.com.", "203.0.113.50", 300))
		_, r := link(t, e, [][]byte{guestQuery(t, 7, "example.com.", false)}, 1)
		if m := replyMsg(t, r[0]); m.Header.RCode != dnsmessage.RCodeServerFailure {
			t.Errorf("%s: rcode %v, want SERVFAIL", name, m.Header.RCode)
		}
		if pinned(e, g, "203.0.113.50") {
			t.Fatalf("%s: mismatched upstream answer pinned", name)
		}
	}
	prev := fcDNSExchange
	fcDNSExchange = func(context.Context, []byte) ([]byte, error) { return nil, errors.New("down") }
	defer func() { fcDNSExchange = prev }()
	_, r := link(t, e, [][]byte{guestQuery(t, 8, "example.com.", false)}, 1)
	if m := replyMsg(t, r[0]); m.Header.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("upstream down: rcode %v", m.Header.RCode)
	}
}

// Without an allowlist the gateway's own (gvisor) resolver keeps answering:
// the query reaches the netstack and koto's resolver is never asked.
func TestGatewayResolverOffWithoutAllowlist(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "")
	e := newConn(g, fcNetWAN)
	calls := fakeUpstream(t, nil)
	q := guestQuery(t, 1, "example.com.", false)
	passed, _ := link(t, e, [][]byte{q}, 0)
	if len(passed) != 1 || !bytes.Equal(passed[0], q) || *calls != 0 {
		t.Fatalf("no allowlist: passed=%d calls=%d", len(passed), *calls)
	}
}

// (e) A pin lives for the MINIMUM TTL along the CNAME chain, not the A
// record's own; and a CNAME target that is itself allowed pins (docs).
func TestGatewayResolverCNAMEMinTTL(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "www.example.com,edge.allowed.net")
	e := newConn(g, fcNetWAN)
	now := time.Unix(1_800_000_000, 0)
	e.allow.now = func() time.Time { return now }
	short := rrCNAME("www.example.com.", "edge.cdn.net.")
	short.Header.TTL = 45
	resolve(t, e, "www.example.com.", short, rrA("edge.cdn.net.", "203.0.113.10", 3000))
	// Question not allowed, but a CNAME on the chain is.
	resolve(t, e, "alias.other.test.", rrCNAME("alias.other.test.", "edge.allowed.net."),
		rrA("edge.allowed.net.", "203.0.113.11", 3000))
	// Neither allowed: nothing.
	resolve(t, e, "x.other.test.", rrCNAME("x.other.test.", "y.other.test."), rrA("y.other.test.", "203.0.113.12", 3000))
	if !pinned(e, g, "203.0.113.10") || !pinned(e, g, "203.0.113.11") || pinned(e, g, "203.0.113.12") {
		t.Fatal("chain pinning wrong")
	}
	now = now.Add(40 * time.Second)
	if !pinned(e, g, "203.0.113.10") {
		t.Fatal("pin gone before the CNAME TTL")
	}
	now = now.Add(10 * time.Second) // 50s > 45s CNAME TTL, << 3000s A TTL
	if pinned(e, g, "203.0.113.10") {
		t.Fatal("pin outlived the CNAME's TTL (A record TTL used)")
	}
	if !pinned(e, g, "203.0.113.11") { // 60s CNAME TTL (rrCNAME) not yet over
		t.Fatal("pin under an allowed CNAME target expired early")
	}
}

// A reply too big for a non-EDNS guest is truncated (TC) — but the pins were
// taken from the full upstream answer.
func TestGatewayResolverTruncates(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "big.example.com")
	e := newConn(g, fcNetWAN)
	var recs []dnsmessage.Resource
	for i := 0; i < 60; i++ {
		recs = append(recs, rrA("big.example.com.", netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)}).String(), 300))
	}
	m := replyMsg(t, resolve(t, e, "big.example.com.", recs...))
	if !m.Header.Truncated || len(m.Answers) >= 60 {
		t.Fatalf("TC=%v answers=%d", m.Header.Truncated, len(m.Answers))
	}
	if !pinned(e, g, "198.51.100.60") {
		t.Fatal("record dropped by truncation was not pinned")
	}
	fakeUpstream(t, nil, recs...)
	_, r := link(t, e, [][]byte{guestQuery(t, 2, "big.example.com.", true)}, 1)
	if m := replyMsg(t, r[0]); m.Header.Truncated || len(m.Answers) != 60 {
		t.Fatalf("EDNS guest: TC=%v answers=%d", m.Header.Truncated, len(m.Answers))
	}
}

func TestParseResolvConf(t *testing.T) {
	p := t.TempDir() + "/resolv.conf"
	if err := os.WriteFile(p, []byte("# c\nnameserver 10.0.0.2\nnameserver fe80::1%eth0\nsearch x\nnameserver bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := fcParseResolvConf(p)
	if len(got) != 2 || got[0] != "10.0.0.2:53" || got[1] != "[fe80::1]:53" {
		t.Fatalf("got %v", got)
	}
}
