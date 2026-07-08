package main

// fcnet.go — L3 networking for internet=full microVM groups.
//
// The default group has no NIC; egress is an L7 proxy over vsock (proxy.go).
// internet=full additionally attaches a userspace gVisor network stack
// (containers/gvisor-tap-vsock) that gives the guest a real L3 interface:
// arbitrary outbound TCP and UDP (any port) with NAT + DNS — without a host
// tap/bridge or root, so it fits the rootless cs_host. (ICMP/ping is
// best-effort: the gateway must be able to open a raw/unprivileged ICMP socket
// on the host; TCP/UDP — all clawson tooling — need no special privilege.)
//
// Topology (per full group):
//
//   guest 192.168.127.2/24  ──TAP──┐
//                                   │  Qemu-framed ethernet
//                                   │  (4-byte BE length prefix per frame)
//                              vsock 9003 (fcPortNet)
//                                   │
//   host  gVisor gateway 192.168.127.1  ──NAT/DNS──▶ real network
//
// fc-agent (guest) creates the TAP and pumps frames to vsock 9003; the daemon
// accepts that connection and hands it to the group's VirtualNetwork via
// AcceptQemu. Attaching is gated on internet=full — a `none` group never gets
// the 9003 listener, so no route exists (the "no NIC = no egress" invariant is
// preserved for the default).
//
// Egress authority: gvisor-tap-vsock has no destination-filter hook (its
// forwarder net.Dial's the packet's destination directly), so we filter at the
// frame layer BEFORE the netstack sees a packet — fcEgressConn drops guest
// frames addressed to the control plane (loopback, link-local, the daemon's
// gRPC bind). This replaces, for the L3 path, what egressTargetAllowed does for
// the L7 proxy (proxy.go). Ec2MetadataAccess=false additionally blocks
// 169.254.169.254 inside the netstack as defense in depth.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

const (
	fcNetSubnet     = "192.168.127.0/24"
	fcNetGatewayIP  = "192.168.127.1"
	fcNetGuestIP    = "192.168.127.2"
	fcNetGuestMAC   = "5a:94:ef:e4:0c:02" // fc-agent sets the TAP to this
	fcNetGatewayMAC = "5a:94:ef:e4:0c:01"
	fcNetMTU        = 1500
)

// fcNetGateway builds the per-group virtual network (outbound NAT + DNS).
//
// Inbound published ports keep riding the existing vsock portBridge path
// (fc.go), which delivers to the guest's loopback services and is independent
// of L3 — so we deliberately set no gvisor Forwards here (that would also fight
// the vsock publish loop for the same cs_host port). L3-native inbound (the
// guest accepting on its 192.168.127.2 address via gateway Forwards) is a
// documented follow-up.
func fcNetGateway() (*virtualnetwork.VirtualNetwork, error) {
	cfg := &types.Configuration{
		MTU:               fcNetMTU,
		Subnet:            fcNetSubnet,
		GatewayIP:         fcNetGatewayIP,
		GatewayMacAddress: fcNetGatewayMAC,
		// Static lease so the gateway's NAT/ARP knows the guest by MAC even
		// though fc-agent self-assigns the address (no in-guest DHCP client).
		DHCPStaticLeases: map[string]string{fcNetGuestMAC: fcNetGuestIP},
		// No DNS zones → the gateway forwards queries to the host resolver, so
		// the guest resolves public names via 192.168.127.1.
		//
		// Block the cloud metadata endpoint inside the stack. fcEgressConn also
		// drops link-local at the frame layer; this is belt-and-suspenders.
		Ec2MetadataAccess: false,
	}
	return virtualnetwork.New(cfg)
}

// fcNetServe attaches one guest link connection to the gateway, filtering
// egress at the frame layer. Blocks until the connection ends.
func fcNetServe(ctx context.Context, vn *virtualnetwork.VirtualNetwork, conn net.Conn) error {
	return vn.AcceptQemu(ctx, fcNewEgressConn(conn))
}

// ---- frame-layer egress filter ---------------------------------------------

// fcBlockedDst reports whether a guest packet to ip must be dropped: the
// control plane the guest must never reach over L3. Mirrors the intent of
// proxy.go's egressTargetAllowed (loopback / link-local / the daemon port),
// but at L3 by destination IP. Everything else — including the host LAN — is
// allowed, matching the documented "internet=full can reach the LAN" posture.
func fcBlockedDst(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true // 127.0.0.0/8, ::1, 169.254.0.0/16, fe80::/10
	}
	// If the daemon's gRPC control plane binds a non-loopback address (e.g. a
	// WireGuard mesh IP via CLAWSON_BIND), block it too — the loopback rule
	// above already covers the default 127.0.0.1 bind.
	if b := os.Getenv("CLAWSON_BIND"); b != "" {
		if bip := net.ParseIP(b); bip != nil && bip.Equal(ip) {
			return true
		}
	}
	return false
}

// fcEgressConn wraps the guest link conn. Reads (guest→host) are reframed so
// that only allowed Qemu frames pass through to the netstack; writes
// (host→guest) pass straight through. Because both ends use identical framing
// (4-byte big-endian length + ethernet frame), dropping a frame is invisible
// to the netstack — a blocked SYN simply never arrives, so the guest's connect
// times out, same as a firewall DROP.
type fcEgressConn struct {
	net.Conn
	buf []byte // leftover allowed [len][frame] bytes not yet consumed by Read
}

func fcNewEgressConn(c net.Conn) *fcEgressConn { return &fcEgressConn{Conn: c} }

func (e *fcEgressConn) Read(p []byte) (int, error) {
	for len(e.buf) == 0 {
		frame, err := e.readFrame()
		if err != nil {
			return 0, err
		}
		if fcFrameAllowed(frame) {
			hdr := make([]byte, 4)
			binary.BigEndian.PutUint32(hdr, uint32(len(frame)))
			e.buf = append(hdr, frame...)
		}
		// else: dropped — loop and read the next frame.
	}
	n := copy(p, e.buf)
	e.buf = e.buf[n:]
	return n, nil
}

// readFrame reads one length-prefixed ethernet frame from the underlying conn.
func (e *fcEgressConn) readFrame() ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(e.Conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > fcNetMTU+64 { // ethernet header + slack; guards against junk
		return nil, fmt.Errorf("fcnet: bad frame length %d", n)
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(e.Conn, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// fcFrameAllowed parses an ethernet frame's L3 destination and applies
// fcBlockedDst. Non-IP frames (ARP, etc.) and unparseable frames are allowed —
// ARP is needed for the guest↔gateway link, and the netstack ignores garbage.
func fcFrameAllowed(frame []byte) bool {
	if len(frame) < 14 {
		return true
	}
	ethType := binary.BigEndian.Uint16(frame[12:14])
	switch ethType {
	case 0x0800: // IPv4
		if len(frame) < 34 {
			return true
		}
		return !fcBlockedDst(net.IP(frame[30:34]))
	case 0x86DD: // IPv6
		if len(frame) < 54 {
			return true
		}
		return !fcBlockedDst(net.IP(frame[38:54]))
	default:
		return true
	}
}
