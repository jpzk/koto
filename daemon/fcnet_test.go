package main

// fcnet_test.go — the L3 egress filter (fcnet.go). Pure wire logic: no gVisor
// stack, no VM. Verifies that guest frames to the control plane are dropped and
// everything else passes, and that fcEgressConn reframes the surviving stream
// byte-for-byte in the Qemu framing the gateway expects.

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

func TestFcDstAllowed(t *testing.T) {
	// Full destination × profile matrix. cs_host's own IPs are also ctl
	// (fcSelfIPs), but those depend on the host's interfaces, so this table
	// covers only the static classes.
	cases := []struct {
		ip             string
		wan, lan, full bool
	}{
		// ctl — blocked under every profile
		{"127.0.0.1", false, false, false},       // loopback (daemon gRPC/proxy)
		{"127.5.6.7", false, false, false},       // all of 127/8
		{"169.254.169.254", false, false, false}, // metadata / link-local
		{"::1", false, false, false},             // v6 loopback
		{"fe80::1", false, false, false},         // v6 link-local
		{"224.0.0.251", false, false, false},     // mDNS — link-local multicast = ctl
		// gw subnet — always allowed (guest↔gateway; DNS at .1)
		{"192.168.127.1", true, true, true},
		{"192.168.127.2", true, true, true},
		// LAN — allowed only under lan|full
		{"192.168.1.5", false, true, true},
		{"10.0.0.7", false, true, true},
		{"172.16.0.1", false, true, true},
		{"fd00::1", false, true, true},            // IPv6 ULA
		{"::ffff:192.168.1.5", false, true, true}, // v4-mapped LAN
		{"239.255.255.250", false, true, true},    // SSDP — admin-scoped multicast
		{"255.255.255.255", false, true, true},    // limited broadcast
		{"100.100.1.1", false, true, true},        // CGNAT / tailnet — LAN, not WAN
		// WAN — allowed only under wan|full
		{"1.1.1.1", true, false, true},
		{"8.8.8.8", true, false, true},
		{"2606:4700::1111", true, false, true},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		for _, p := range []struct {
			pol  string
			want bool
		}{{fcNetWAN, c.wan}, {fcNetLAN, c.lan}, {fcNetFull, c.full}} {
			if got := fcDstAllowed(ip, p.pol); got != p.want {
				t.Errorf("fcDstAllowed(%s, %s) = %v, want %v", c.ip, p.pol, got, p.want)
			}
		}
	}
}

// ethFrame builds a minimal ethernet+IPv4 frame with the given destination.
func ethFrame(dstIP string) []byte {
	f := make([]byte, 34)
	// 0..5 dst MAC, 6..11 src MAC (left zero)
	binary.BigEndian.PutUint16(f[12:14], 0x0800) // IPv4
	copy(f[30:34], net.ParseIP(dstIP).To4())     // IP header dst @ eth+16
	return f
}

// ethFrame6 builds a minimal ethernet+IPv6 frame with the given destination.
func ethFrame6(dstIP string) []byte {
	f := make([]byte, 54)
	binary.BigEndian.PutUint16(f[12:14], 0x86DD) // IPv6
	copy(f[38:54], net.ParseIP(dstIP).To16())    // IPv6 dst @ eth+24
	return f
}

func TestFcFrameAllowed(t *testing.T) {
	if !fcFrameAllowed(ethFrame("1.1.1.1"), fcNetWAN) {
		t.Error("public dest should be allowed under wan")
	}
	if fcFrameAllowed(ethFrame("192.168.1.5"), fcNetWAN) {
		t.Error("LAN dest should be dropped under wan")
	}
	if !fcFrameAllowed(ethFrame("192.168.1.5"), fcNetLAN) {
		t.Error("LAN dest should be allowed under lan")
	}
	if fcFrameAllowed(ethFrame("127.0.0.1"), fcNetFull) {
		t.Error("loopback dest should be dropped under every profile")
	}
	// IPv6 frame parsing.
	if !fcFrameAllowed(ethFrame6("2606:4700::1111"), fcNetWAN) {
		t.Error("public IPv6 dest should be allowed under wan")
	}
	if fcFrameAllowed(ethFrame6("fd00::1"), fcNetWAN) {
		t.Error("IPv6 ULA should be dropped under wan")
	}
	// ARP (non-IP) must pass — the guest↔gateway link needs it.
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:14], 0x0806)
	if !fcFrameAllowed(arp, fcNetWAN) {
		t.Error("ARP frame should be allowed")
	}
	// 802.1Q VLAN-tagged frames are dropped (they'd evade the offset parser).
	vlan := make([]byte, 42)
	binary.BigEndian.PutUint16(vlan[12:14], 0x8100)
	if fcFrameAllowed(vlan, fcNetFull) {
		t.Error("VLAN-tagged frame should be dropped")
	}
	// Runt frames are passed through (the netstack drops garbage).
	if !fcFrameAllowed([]byte{1, 2, 3}, fcNetWAN) {
		t.Error("short frame should be allowed (passthrough)")
	}
}

// writeFrame appends one Qemu-framed frame (4-byte BE length + payload).
func writeFrame(buf *bytes.Buffer, frame []byte) {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(frame)))
	buf.Write(h[:])
	buf.Write(frame)
}

func TestFcEgressConnFilters(t *testing.T) {
	// A guest→host stream: allowed, blocked, allowed. Only the two allowed
	// frames should survive, still correctly length-prefixed.
	good1 := ethFrame("1.1.1.1")
	bad := ethFrame("127.0.0.1")
	good2 := ethFrame("8.8.8.8")

	var in bytes.Buffer
	writeFrame(&in, good1)
	writeFrame(&in, bad)
	writeFrame(&in, good2)

	// pipe the crafted stream in as the "guest" side of the conn.
	a, b := net.Pipe()
	go func() {
		_, _ = b.Write(in.Bytes())
		b.Close()
	}()

	ec := fcNewEgressConn(a, "testgroup", fcNetFull)
	out, _ := io.ReadAll(ec)

	var want bytes.Buffer
	writeFrame(&want, good1)
	writeFrame(&want, good2)
	if !bytes.Equal(out, want.Bytes()) {
		t.Errorf("filtered stream mismatch:\n got %x\nwant %x", out, want.Bytes())
	}
}

func TestFcEgressConnWANDropsLAN(t *testing.T) {
	// Under wan: a LAN frame is dropped, the gateway-subnet frame survives
	// (DNS must work), and a public frame survives.
	gw := ethFrame("192.168.127.1")
	lan := ethFrame("192.168.1.5")
	pub := ethFrame("1.1.1.1")

	var in bytes.Buffer
	writeFrame(&in, gw)
	writeFrame(&in, lan)
	writeFrame(&in, pub)

	a, b := net.Pipe()
	go func() {
		_, _ = b.Write(in.Bytes())
		b.Close()
	}()

	ec := fcNewEgressConn(a, "testgroup", fcNetWAN)
	out, _ := io.ReadAll(ec)

	var want bytes.Buffer
	writeFrame(&want, gw)
	writeFrame(&want, pub)
	if !bytes.Equal(out, want.Bytes()) {
		t.Errorf("wan-filtered stream mismatch:\n got %x\nwant %x", out, want.Bytes())
	}
}

// tcpFrame builds an ethernet+IPv4+TCP frame (20-byte IP header, IHL=5).
func tcpFrame(dst string, port uint16, flags byte) []byte {
	f := make([]byte, 14+20+20)
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	f[14] = 0x45 // v4, IHL 5
	f[23] = 6    // TCP
	copy(f[26:30], net.ParseIP("192.168.127.2").To4())
	copy(f[30:34], net.ParseIP(dst).To4())
	binary.BigEndian.PutUint16(f[36:38], port) // TCP dst port
	f[47] = flags                              // TCP flags
	return f
}

// udpFrame builds an ethernet+IPv4+UDP frame.
func udpFrame(dst string, port uint16) []byte {
	f := make([]byte, 14+20+8)
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	f[14] = 0x45
	f[23] = 17 // UDP
	copy(f[26:30], net.ParseIP("192.168.127.2").To4())
	copy(f[30:34], net.ParseIP(dst).To4())
	binary.BigEndian.PutUint16(f[36:38], port) // UDP dst port
	return f
}

func TestFcParseFlow(t *testing.T) {
	// TCP SYN → a flow start.
	fl, ok := fcParseFlow(tcpFrame("1.1.1.1", 443, 0x02))
	if !ok || fl.proto != "TCP" || fl.dstPort != 443 || !fl.hasPort ||
		fl.dst.String() != "1.1.1.1" || fl.src.String() != "192.168.127.2" {
		t.Errorf("SYN parse = %+v ok=%v, want TCP 192.168.127.2 -> 1.1.1.1:443", fl, ok)
	}
	// SYN|ACK (handshake reply echoed back out) and mid-stream ACK: not starts.
	if _, ok := fcParseFlow(tcpFrame("1.1.1.1", 443, 0x12)); ok {
		t.Error("SYN|ACK should not parse as a flow start")
	}
	if _, ok := fcParseFlow(tcpFrame("1.1.1.1", 443, 0x10)); ok {
		t.Error("bare ACK should not parse as a flow start")
	}
	// UDP: every packet is a candidate (dedup handles repeats).
	fl, ok = fcParseFlow(udpFrame("9.9.9.9", 123))
	if !ok || fl.proto != "UDP" || fl.dstPort != 123 {
		t.Errorf("UDP parse = %+v ok=%v, want UDP :123", fl, ok)
	}
	// Headerless minimal frame (the ethFrame helper: IHL=0) and non-IP: no flow.
	if _, ok := fcParseFlow(ethFrame("1.1.1.1")); ok {
		t.Error("truncated IPv4 frame should not parse")
	}
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:14], 0x0806)
	if _, ok := fcParseFlow(arp); ok {
		t.Error("ARP should not parse as a flow")
	}
}

func TestFcFlowLoggerDedup(t *testing.T) {
	l := newFcFlowLogger("testgroup")
	n0 := len(logRing)

	l.record(tcpFrame("1.1.1.1", 443, 0x02), fcNetWAN, true)
	l.record(tcpFrame("1.1.1.1", 443, 0x02), fcNetWAN, true) // SYN retransmit — deduped
	if got := len(logRing) - n0; got != 1 {
		t.Errorf("same tuple logged %d times, want 1", got)
	}
	l.record(tcpFrame("1.1.1.1", 80, 0x02), fcNetWAN, true) // new port — new tuple
	if got := len(logRing) - n0; got != 2 {
		t.Errorf("second tuple: %d lines, want 2", got)
	}
	// Gateway-subnet traffic (DNS to .1) is not egress — never logged.
	l.record(udpFrame("192.168.127.1", 53), fcNetWAN, true)
	if got := len(logRing) - n0; got != 2 {
		t.Errorf("gw-subnet flow logged; %d lines, want 2", got)
	}
	// Blocked flow logs at warn with the profile in the message. Search the
	// tail rather than asserting on the last entry: a warn line is forwarded
	// as an operator notification (logalert.go), whose info-level mirror
	// lands in the ring right after it.
	l.record(tcpFrame("192.168.1.5", 445, 0x02), fcNetWAN, false)
	var blocked bool
	for _, le := range logRing[n0:] {
		if le.Level == "warn" && le.Subsystem == "egress" &&
			bytes.Contains([]byte(le.Msg), []byte("BLOCKED flow TCP 192.168.127.2 -> 192.168.1.5:445")) {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("no warn egress line for the blocked flow in ring tail")
	}
}
