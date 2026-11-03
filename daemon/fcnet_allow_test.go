package main

// fcnet_allow_test.go — profile ∩ allowlist at the L3 gateway (fcnet_allow.go):
// static entries, hostname-only-after-pin, SNI on pin-only IPs, empty list ==
// old behaviour, and a live list change with no new link / restart.

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"koto/netallow"
)

// ipv4Frame builds ethernet+IPv4+{TCP,UDP} with a payload and valid lengths.
func ipv4Frame(src, dst string, proto byte, sport, dport uint16, payload []byte) []byte {
	l4len := 20
	if proto == 17 {
		l4len = 8
	}
	f := make([]byte, 14+20+l4len+len(payload))
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	f[14] = 0x45
	binary.BigEndian.PutUint16(f[16:18], uint16(20+l4len+len(payload)))
	f[23] = proto
	copy(f[26:30], net.ParseIP(src).To4())
	copy(f[30:34], net.ParseIP(dst).To4())
	l4 := f[34:]
	binary.BigEndian.PutUint16(l4[0:2], sport)
	binary.BigEndian.PutUint16(l4[2:4], dport)
	if proto == 6 {
		l4[12] = 0x50
		l4[13] = 0x18 // PSH|ACK
	} else {
		binary.BigEndian.PutUint16(l4[4:6], uint16(8+len(payload)))
	}
	copy(l4[l4len:], payload)
	return guestMACs(f)
}

func guestTCP(dst string, port uint16, payload []byte) []byte {
	return ipv4Frame(fcNetGuestIP, dst, 6, 40000, port, payload)
}

// dnsAnswer builds a gateway → guest DNS response frame, length-prefixed as
// the gvisor switch writes it.
func dnsAnswer(t *testing.T, src, q string, recs ...dnsmessage.Resource) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeSuccess})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(q), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		var err error
		switch body := r.Body.(type) {
		case *dnsmessage.AResource:
			err = b.AResource(r.Header, *body)
		case *dnsmessage.AAAAResource:
			err = b.AAAAResource(r.Header, *body)
		case *dnsmessage.CNAMEResource:
			err = b.CNAMEResource(r.Header, *body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	f := ipv4Frame(src, fcNetGuestIP, 17, 53, 33333, msg)
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(f))), f...)
}

func rrA(name, ip string, ttl uint32) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.AResource{A: [4]byte(net.ParseIP(ip).To4())},
	}
}

func rrCNAME(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 60},
		Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)},
	}
}

// clientHello captures a real crypto/tls ClientHello for serverName.
func clientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	c, s := net.Pipe()
	defer s.Close()
	go func() {
		// X25519 only: a post-quantum key share would push the hello past one
		// MTU-sized segment, which the gateway sees as separate frames.
		cl := tls.Client(c, &tls.Config{ServerName: serverName, CurvePreferences: []tls.CurveID{tls.X25519}})
		_ = cl.Handshake()
		c.Close()
	}()
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(s, hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:5]))
	if _, err := io.ReadFull(s, body); err != nil {
		t.Fatal(err)
	}
	return append(hdr, body...)
}

// allowGroup publishes list for a fresh test group through the real live
// slot (allowApplyLive) and returns the group name.
func allowGroup(t *testing.T, list string) string {
	t.Helper()
	g := "allowtest-" + t.Name()
	l, err := netallow.Parse(list)
	if err != nil {
		t.Fatal(err)
	}
	allowSlot(g).Store(&allowPolicy{list: l})
	t.Cleanup(func() { liveAllow.Delete(g) })
	return g
}

func pinSelf(t *testing.T) {
	prev := fcSelfIPs
	fcSelfIPs = func() []net.IP { return nil }
	t.Cleanup(func() { fcSelfIPs = prev })
}

// pump runs one batch of guest frames through a real fcEgressConn (profile +
// allowlist, over a fresh pipe each batch — the fcAllow state, i.e. the live
// link, is kept) and reports which survived.
func pump(t *testing.T, e *fcEgressConn, frames ...[]byte) []bool {
	t.Helper()
	var in bytes.Buffer
	for _, f := range frames {
		writeFrame(&in, f)
	}
	a, b := net.Pipe()
	e.Conn = a
	go func() { _, _ = b.Write(in.Bytes()); b.Close() }()
	out, _ := io.ReadAll(e)
	a.Close()
	got := make([]bool, len(frames))
	i := 0
	for len(out) >= 4 {
		n := int(binary.BigEndian.Uint32(out))
		f := out[4 : 4+n]
		out = out[4+n:]
		for i < len(frames) && !bytes.Equal(f, frames[i]) {
			i++
		}
		if i == len(frames) {
			t.Fatalf("filter emitted a frame it was never given: %x", f)
		}
		got[i] = true
		i++
	}
	return got
}

// deliver pushes one gateway → guest write through e.Write.
func deliver(t *testing.T, e *fcEgressConn, b []byte) {
	t.Helper()
	a, g := net.Pipe()
	e.Conn = a
	go func() { _, _ = io.Copy(io.Discard, g) }()
	if _, err := e.Write(b); err != nil {
		t.Fatal(err)
	}
	a.Close()
}

func newConn(group, profile string) *fcEgressConn { return fcNewEgressConn(nil, group, profile) }

func wantPass(t *testing.T, got []bool, want ...bool) {
	t.Helper()
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("pass pattern = %v, want %v", got, want)
		}
	}
}

func TestAllowlistEmptyIsOldBehaviour(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "")
	frames := [][]byte{guestTCP("1.1.1.1", 443, nil), guestTCP("192.168.1.5", 22, nil), guestTCP("8.8.8.8", 80, nil)}
	for _, prof := range []string{fcNetWAN, fcNetLAN, fcNetFull} {
		e := newConn(g, prof)
		got := pump(t, e, frames...)
		for i, f := range frames {
			if got[i] != fcFrameAllowed(f, prof) {
				t.Errorf("%s frame %d: allowlist-empty=%v, profile-only=%v", prof, i, got[i], !got[i])
			}
		}
	}
}

func TestAllowlistStaticEntries(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "1.1.1.1,10.0.0.0/8,192.168.1.*,9.9.9.1-9.9.9.9")
	frames := [][]byte{
		guestTCP("1.1.1.1", 443, nil),                                       // listed IP
		guestTCP("8.8.8.8", 443, nil),                                       // not listed
		guestTCP("10.1.2.3", 22, nil),                                       // CIDR (LAN)
		guestTCP("192.168.1.77", 80, nil),                                   // wildcard (LAN)
		guestTCP("9.9.9.5", 53, nil),                                        // range
		guestTCP("9.9.9.10", 53, nil),                                       // past range
		ipv4Frame(fcNetGuestIP, fcNetGatewayIP, 17, 33333, 53, []byte("q")), // gateway DNS: koto resolver
	}
	e := newConn(g, fcNetFull)
	wantPass(t, pump(t, e, frames...), true, false, true, true, true, false, false)
	// profile ∩ allowlist: under wan the listed LAN ranges stay blocked.
	e = newConn(g, fcNetWAN)
	wantPass(t, pump(t, e, frames...), true, false, false, false, true, false, false)
}

func TestAllowlistHostnameOnlyAfterPin(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "example.com,*.cdn.test")
	e := newConn(g, fcNetWAN)
	syn := guestTCP("93.184.216.34", 80, nil)
	wantPass(t, pump(t, e, syn), false) // before any resolution: dropped

	// An answer for a NON-allowed name pins nothing.
	resolve(t, e, "evil.test.", rrA("evil.test.", "93.184.216.34", 300))
	wantPass(t, pump(t, e, syn), false)
	// A gateway → guest "answer" on the Write path is never trusted.
	deliver(t, e, dnsAnswer(t, fcNetGatewayIP, "example.com.", rrA("example.com.", "93.184.216.34", 300)))
	wantPass(t, pump(t, e, syn), false)
	// koto's resolver answering the guest's query for the allowed name pins.
	resolve(t, e, "example.com.", rrA("example.com.", "93.184.216.34", 300))
	wantPass(t, pump(t, e, syn, guestTCP("93.184.216.35", 80, nil)), true, false)

	// CNAME chain from an allowed wildcard name pins the final A only.
	resolve(t, e, "img.cdn.test.",
		rrCNAME("img.cdn.test.", "edge.provider.net."), rrA("edge.provider.net.", "203.0.113.9", 60),
		rrA("unrelated.net.", "203.0.113.66", 60))
	if !e.allow.pinSet(currentAllow(g)).Has(netip.MustParseAddr("203.0.113.9")) {
		t.Error("CNAME target address of an allowed name was not pinned")
	}
	if e.allow.pinSet(currentAllow(g)).Has(netip.MustParseAddr("203.0.113.66")) {
		t.Error("an answer record not on the question's CNAME chain was pinned")
	}
	// one-level wildcard only: a.b.cdn.test is not allowed.
	resolve(t, e, "a.b.cdn.test.", rrA("a.b.cdn.test.", "203.0.113.77", 60))
	if e.allow.pinSet(currentAllow(g)).Has(netip.MustParseAddr("203.0.113.77")) {
		t.Error("two-level name pinned under *.cdn.test")
	}
}

func TestAllowlistSNIOnPinnedIP(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "example.com,1.1.1.1")
	e := newConn(g, fcNetWAN)
	resolve(t, e, "example.com.", rrA("example.com.", "93.184.216.34", 300))
	good := clientHello(t, "example.com")
	bad := clientHello(t, "other-site-on-same-cdn.test")
	if sni, isHello, complete := fcTLSSNI(good); sni != "example.com" || !isHello || !complete {
		t.Fatalf("fcTLSSNI(real ClientHello) = %q %v %v", sni, isHello, complete)
	}
	frames := [][]byte{
		guestTCP("93.184.216.34", 443, nil),                                   // SYN: no payload, pinned → pass
		guestTCP("93.184.216.34", 443, good),                                  // SNI allowed
		guestTCP("93.184.216.34", 443, bad),                                   // SNI not allowed on a pin-only IP
		ipv4Frame(fcNetGuestIP, "93.184.216.34", 17, 5555, 443, []byte{0xc0}), // QUIC
		guestTCP("1.1.1.1", 443, bad),                                         // statically listed IP: no SNI gate
		guestTCP("93.184.216.34", 22, nil),                                    // other port on pinned IP: by IP
	}
	wantPass(t, pump(t, e, frames...), true, true, false, false, true, true)
	// A ClientHello cut before its extensions is let through (best effort,
	// documented): not judged complete.
	if _, isHello, complete := fcTLSSNI(bad[:60]); !isHello || complete {
		t.Errorf("truncated hello: isHello=%v complete=%v", isHello, complete)
	}
}

func TestAllowlistLiveChangeNoRestart(t *testing.T) {
	pinSelf(t)
	g := allowGroup(t, "1.1.1.1,a.example.com,b.example.com")
	e := newConn(g, fcNetWAN)
	resolve(t, e, "a.example.com.", rrA("a.example.com.", "203.0.113.1", 300))
	resolve(t, e, "b.example.com.", rrA("b.example.com.", "203.0.113.2", 300))
	est := []([]byte){guestTCP("1.1.1.1", 443, []byte("x")), guestTCP("8.8.8.8", 443, nil),
		guestTCP("203.0.113.1", 80, nil), guestTCP("203.0.113.2", 80, nil)}
	wantPass(t, pump(t, e, est...), true, false, true, true)

	// Operator changes the list on the SAME running link (no new conn).
	l, _ := netallow.Parse("8.8.8.8,a.example.com")
	if got := allowApplyLive(g, l); got == "" {
		t.Fatal("allowApplyLive returned no status")
	}
	// established 1.1.1.1 flow loses its next packet; 8.8.8.8 opens;
	// a.example.com's pin survives, b.example.com's is dropped.
	wantPass(t, pump(t, e, est...), false, true, true, false)

	// Clearing the list restores the plain profile on the live link.
	allowApplyLive(g, nil)
	wantPass(t, pump(t, e, est...), true, true, true, true)
}

func TestAllowlistCorruptDeniesAll(t *testing.T) {
	pinSelf(t)
	g := "allowtest-corrupt"
	allowSlot(g).Store(&allowPolicy{denyAll: true})
	t.Cleanup(func() { liveAllow.Delete(g) })
	e := newConn(g, fcNetFull)
	dns := ipv4Frame(fcNetGuestIP, fcNetGatewayIP, 17, 33333, 53, []byte("q"))
	wantPass(t, pump(t, e, guestTCP("1.1.1.1", 443, nil), dns), false, false) // dns: consumed by koto's resolver
}
