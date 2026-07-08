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

func TestFcBlockedDst(t *testing.T) {
	t.Setenv("CLAWSON_BIND", "192.0.2.1")
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},   // loopback (daemon gRPC/proxy live here)
		{"127.5.6.7", true},   // all of 127/8
		{"169.254.169.254", true}, // cloud metadata / link-local
		{"192.0.2.1", true},    // configured CLAWSON_BIND (WireGuard control plane)
		{"1.1.1.1", false},    // public
		{"10.89.0.4", false},  // clawson-net LAN peer — allowed (parity with L7)
		{"::1", true},         // v6 loopback
		{"8.8.8.8", false},
	}
	for _, c := range cases {
		if got := fcBlockedDst(net.ParseIP(c.ip)); got != c.blocked {
			t.Errorf("fcBlockedDst(%s) = %v, want %v", c.ip, got, c.blocked)
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

func TestFcFrameAllowed(t *testing.T) {
	if !fcFrameAllowed(ethFrame("1.1.1.1")) {
		t.Error("public dest should be allowed")
	}
	if fcFrameAllowed(ethFrame("127.0.0.1")) {
		t.Error("loopback dest should be dropped")
	}
	// ARP (non-IP) must pass — the guest↔gateway link needs it.
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:14], 0x0806)
	if !fcFrameAllowed(arp) {
		t.Error("ARP frame should be allowed")
	}
	// Runt frames are passed through (the netstack drops garbage).
	if !fcFrameAllowed([]byte{1, 2, 3}) {
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

	ec := fcNewEgressConn(a)
	out, _ := io.ReadAll(ec)

	var want bytes.Buffer
	writeFrame(&want, good1)
	writeFrame(&want, good2)
	if !bytes.Equal(out, want.Bytes()) {
		t.Errorf("filtered stream mismatch:\n got %x\nwant %x", out, want.Bytes())
	}
}
