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
		ip                  string
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
		{"fd00::1", false, true, true}, // IPv6 ULA
		{"::ffff:192.168.1.5", false, true, true}, // v4-mapped LAN
		{"239.255.255.250", false, true, true},    // SSDP — admin-scoped multicast
		{"255.255.255.255", false, true, true},    // limited broadcast
		// WAN — allowed only under wan|full; 100.64/10 (tailnet) is WAN
		{"1.1.1.1", true, false, true},
		{"8.8.8.8", true, false, true},
		{"2606:4700::1111", true, false, true},
		{"100.100.1.1", true, false, true}, // CGNAT / tailnet
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

	ec := fcNewEgressConn(a, fcNetFull)
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

	ec := fcNewEgressConn(a, fcNetWAN)
	out, _ := io.ReadAll(ec)

	var want bytes.Buffer
	writeFrame(&want, gw)
	writeFrame(&want, pub)
	if !bytes.Equal(out, want.Bytes()) {
		t.Errorf("wan-filtered stream mismatch:\n got %x\nwant %x", out, want.Bytes())
	}
}
