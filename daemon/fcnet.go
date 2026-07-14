package main

// fcnet.go — L3 networking for networked microVM groups (network=wan|lan|full).
//
// The default group (network=none) has no NIC; egress is an L7 proxy over
// vsock (proxy.go). The networked profiles attach a userspace gVisor network
// stack (containers/gvisor-tap-vsock) that gives the guest a real L3
// interface: outbound TCP and UDP (any port) with NAT + DNS — without a host
// tap/bridge or root, so it fits the rootless cs_host. (ICMP/ping is
// best-effort: the gateway must be able to open a raw/unprivileged ICMP socket
// on the host; TCP/UDP — all clawson tooling — need no special privilege.)
//
// Topology (per networked group):
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
// AcceptQemu. Attaching is gated on network != none — a `none` group never
// gets the 9003 listener, so no route exists (the "no NIC = no egress"
// invariant is preserved for the default).
//
// Egress authority: gvisor-tap-vsock has no destination-filter hook (its
// forwarder net.Dial's the packet's destination directly), so we filter at the
// frame layer BEFORE the netstack sees a packet. fcEgressConn classifies each
// guest frame's destination (fcClassifyDst) and applies the group's network
// profile (fcDstAllowed):
//
//   ctl  loopback / link-local / cs_host's own interface IPs (daemon gRPC +
//        every proxy port) — ALWAYS dropped, every profile.
//   gw   192.168.127.0/24, the guest↔gateway subnet (DNS at .1) — always
//        allowed; carved out because it sits inside the 192.168/16 LAN range.
//   lan  RFC1918 + IPv6 ULA + multicast + limited broadcast — the host LAN.
//        Allowed for lan|full, dropped for wan.
//   wan  everything else, including CGNAT 100.64/10 (the tailnet — treated as
//        intentionally-shared infra, not LAN). Allowed for wan|full, dropped
//        for lan.
//
// This replaces, for the L3 path, what egressTargetAllowed does for the L7
// proxy (proxy.go) — both share fcClassifyDst. Ec2MetadataAccess=false
// additionally blocks 169.254.169.254 inside the netstack as defense in depth.
//
// Caveat (documented, accepted): guest DNS is resolved by the gateway on the
// host, outside the frame filter — a lan guest can still resolve public names
// (and exfiltrate bits via query names). Actual traffic cannot bypass the
// filter: data frames carry the resolved destination IP.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

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

// Network profile values (config.json "network"). fcNetNone means no NIC at
// all; the other three select which destination classes the frame filter
// passes. String-typed to match the config value end to end.
const (
	fcNetNone = "none"
	fcNetWAN  = "wan"
	fcNetLAN  = "lan"
	fcNetFull = "full"
)

// fcNetSubnetNet is fcNetSubnet parsed once for the classifier's gateway-
// subnet carve-out.
var fcNetSubnetNet = func() *net.IPNet {
	_, n, err := net.ParseCIDR(fcNetSubnet)
	if err != nil {
		panic(err) // literal const; cannot fail
	}
	return n
}()

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
// egress at the frame layer under the group's network profile. Blocks until
// the connection ends. The policy is captured per link conn — i.e. fixed for
// the VM's lifetime; a profile change applies on /restart, same as the
// gateway attach itself.
func fcNetServe(ctx context.Context, vn *virtualnetwork.VirtualNetwork, conn net.Conn, policy string) error {
	return vn.AcceptQemu(ctx, fcNewEgressConn(conn, policy))
}

// ---- frame-layer egress filter ---------------------------------------------

// fcDstClass buckets a destination IP for the egress policy. See the header
// comment for the class semantics.
type fcDstClass int

const (
	fcDstCtl fcDstClass = iota // control plane — blocked under every profile
	fcDstGW                    // guest↔gateway subnet — always allowed (frame layer)
	fcDstLAN                   // private ranges / multicast / broadcast — the host LAN
	fcDstWAN                   // everything else (incl. CGNAT 100.64/10 = tailnet)
)

// fcClassifyDst classifies a guest packet's destination. Check order matters:
// control plane first (so a pathological cs_host address inside the gateway
// subnet still blocks), then the gateway carve-out (192.168.127.0/24 sits
// inside the 192.168/16 LAN range but carries guest↔gateway traffic — DNS to
// .1 — that every networked profile needs), then LAN, else WAN.
func fcClassifyDst(ip net.IP) fcDstClass {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fcDstCtl // 127.0.0.0/8, ::1, 169.254.0.0/16, fe80::/10
	}
	// cs_host's own addresses — the daemon and every group's proxy port live
	// here (gRPC on CLAWSON_BIND:CLAWSON_PORT, proxies on 0.0.0.0:<port>,
	// reachable on clawson-net as cs_host_go:<port>). (Bind 0.0.0.0 resolves
	// to every local IP, so enumerating our own addrs is the only reliable
	// block — a literal 0.0.0.0 check never matches a packet.)
	for _, self := range fcSelfIPs() {
		if self.Equal(ip) {
			return fcDstCtl
		}
	}
	if fcNetSubnetNet.Contains(ip) {
		return fcDstGW
	}
	// IsPrivate covers exactly RFC1918 (10/8, 172.16/12, 192.168/16) and IPv6
	// ULA fc00::/7, and is IPv4-mapped-IPv6 aware. Multicast is LAN by
	// definition (its scope is the local segment — mDNS/SSDP discovery is
	// precisely what wan exists to deny); link-local multicast was already
	// caught as ctl above. 255.255.255.255 is the limited broadcast.
	if ip.IsPrivate() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.Equal(net.IPv4bcast) {
		return fcDstLAN
	}
	return fcDstWAN
}

// fcDstAllowed is the frame-layer policy table: may a guest under the given
// network profile send to ip?
func fcDstAllowed(ip net.IP, policy string) bool {
	switch fcClassifyDst(ip) {
	case fcDstCtl:
		return false
	case fcDstGW:
		return true
	case fcDstLAN:
		return policy == fcNetLAN || policy == fcNetFull
	default: // fcDstWAN
		return policy == fcNetWAN || policy == fcNetFull
	}
}

// fcSelfIPs is the daemon/cs_host's own interface addresses, computed once.
// Guest L3 packets to any of these are the control plane and are dropped.
var fcSelfIPs = func() func() []net.IP {
	var once sync.Once
	var ips []net.IP
	return func() []net.IP {
		once.Do(func() {
			addrs, _ := net.InterfaceAddrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					ips = append(ips, n.IP)
				}
			}
		})
		return ips
	}
}()

// fcEgressConn wraps the guest link conn. Reads (guest→host) are reframed so
// that only allowed Qemu frames pass through to the netstack; writes
// (host→guest) pass straight through. Because both ends use identical framing
// (4-byte big-endian length + ethernet frame), dropping a frame is invisible
// to the netstack — a blocked SYN simply never arrives, so the guest's connect
// times out, same as a firewall DROP.
type fcEgressConn struct {
	net.Conn
	policy string // network profile: fcNetWAN | fcNetLAN | fcNetFull
	buf    []byte // leftover allowed [len][frame] bytes not yet consumed by Read
}

func fcNewEgressConn(c net.Conn, policy string) *fcEgressConn {
	return &fcEgressConn{Conn: c, policy: policy}
}

func (e *fcEgressConn) Read(p []byte) (int, error) {
	for len(e.buf) == 0 {
		frame, err := e.readFrame()
		if err != nil {
			return 0, err
		}
		if fcFrameAllowed(frame, e.policy) {
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
// fcDstAllowed under the group's network profile. Non-IP frames (ARP, etc.)
// and unparseable frames are allowed — ARP is needed for the guest↔gateway
// link, and the netstack ignores garbage. VLAN-tagged frames are the one
// exception: an 802.1Q/802.1ad tag would shift the IP header past our fixed
// offsets, hiding the destination from the filter, so they are dropped
// outright (the gateway link is untagged; nothing legitimate sends these).
func fcFrameAllowed(frame []byte, policy string) bool {
	if len(frame) < 14 {
		return true
	}
	ethType := binary.BigEndian.Uint16(frame[12:14])
	switch ethType {
	case 0x0800: // IPv4
		if len(frame) < 34 {
			return true
		}
		return fcDstAllowed(net.IP(frame[30:34]), policy)
	case 0x86DD: // IPv6
		if len(frame) < 54 {
			return true
		}
		return fcDstAllowed(net.IP(frame[38:54]), policy)
	case 0x8100, 0x88A8: // 802.1Q / 802.1ad VLAN tag — would evade the parser
		return false
	default:
		return true
	}
}
