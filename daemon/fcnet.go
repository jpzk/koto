package main

// fcnet.go — L3 networking for networked microVM groups (network=wan|lan|full).
//
// The default group (network=none) has no NIC; egress is an L7 proxy over
// vsock (proxy.go). The networked profiles attach a userspace gVisor network
// stack (containers/gvisor-tap-vsock) that gives the guest a real L3
// interface: outbound TCP and UDP (any port) with NAT + DNS — without a host
// tap/bridge or root, so it fits the rootless cs_host. (ICMP/ping is
// best-effort: the gateway must be able to open a raw/unprivileged ICMP socket
// on the host; TCP/UDP — all koto tooling — need no special privilege.)
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
//   lan  RFC1918 + IPv6 ULA + multicast + limited broadcast + CGNAT 100.64/10
//        (the tailnet — treated as host-reachable infra, same trust tier as
//        the LAN) — the host LAN. Allowed for lan|full, dropped for wan.
//   wan  everything else. Allowed for wan|full, dropped for lan.
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
	"strconv"
	"sync"
	"time"

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
// gateway attach itself. group is only for the flow log lines.
func fcNetServe(ctx context.Context, vn *virtualnetwork.VirtualNetwork, conn net.Conn, group, policy string) error {
	return vn.AcceptQemu(ctx, fcNewEgressConn(conn, group, policy))
}

// ---- frame-layer egress filter ---------------------------------------------

// fcDstClass buckets a destination IP for the egress policy. See the header
// comment for the class semantics.
type fcDstClass int

const (
	fcDstCtl fcDstClass = iota // control plane — blocked under every profile
	fcDstGW                    // guest↔gateway subnet — always allowed (frame layer)
	fcDstLAN                   // private ranges / multicast / broadcast / tailnet — the host LAN
	fcDstWAN                   // everything else
)

// fcTailnetNet is the CGNAT range Tailscale allocates from (100.64.0.0/10),
// classified as LAN so a wan-only group cannot reach tailnet peers — tailnet
// access now requires lan|full, same as the rest of the host's network.
var fcTailnetNet = func() *net.IPNet {
	_, n, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		panic(err) // literal const; cannot fail
	}
	return n
}()

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
	// here (gRPC on KOTO_BIND:KOTO_PORT, proxies on 0.0.0.0:<port>,
	// reachable on koto-net as cs_host_go:<port>). (Bind 0.0.0.0 resolves
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
		ip.Equal(net.IPv4bcast) || fcTailnetNet.Contains(ip) {
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
	policy string        // network profile: fcNetWAN | fcNetLAN | fcNetFull
	buf    []byte        // leftover allowed [len][frame] bytes not yet consumed by Read
	flows  *fcFlowLogger // summarized per-flow egress log
}

func fcNewEgressConn(c net.Conn, group, policy string) *fcEgressConn {
	return &fcEgressConn{Conn: c, policy: policy, flows: newFcFlowLogger(group)}
}

func (e *fcEgressConn) Read(p []byte) (int, error) {
	for len(e.buf) == 0 {
		frame, err := e.readFrame()
		if err != nil {
			return 0, err
		}
		allowed := fcFrameAllowed(frame, e.policy)
		e.flows.record(frame, e.policy, allowed)
		if allowed {
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

// ---- summarized flow logging ------------------------------------------------
//
// Networked profiles trade the L7 proxy audit for real L3 — general egress is
// otherwise invisible. The flow logger restores a summarized audit trail on
// the `egress` subsystem: one line per new guest-initiated flow, at the same
// choke point as the filter (every guest→host frame passes through Read).
// "New flow" means a TCP SYN (without ACK), or the first UDP/ICMP packet of a
// (proto, dst, port) tuple in fcFlowTTL — so an HTTPS request logs once, not
// per packet, and SYN retransmits / DNS-heavy tools don't spam. Blocked flows
// log at warn (mirroring the L7 proxy's BLOCKED lines), allowed at info.

const (
	fcFlowTTL     = time.Minute // one line per (proto,dst,port) tuple per TTL
	fcFlowSeenMax = 4096        // dedup-map bound; prune expired, then reset
)

// logDedup rate-limits log lines to one per key per TTL. Shared by the L3
// flow logger (below) and the L7 LLM-flow log (proxy.go).
type logDedup struct {
	ttl  time.Duration
	max  int
	mu   sync.Mutex
	seen map[string]time.Time
}

func newLogDedup(ttl time.Duration, max int) *logDedup {
	return &logDedup{ttl: ttl, max: max, seen: map[string]time.Time{}}
}

// allow reports whether key hasn't fired within ttl, and records it if so.
func (d *logDedup) allow(key string) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, seen := d.seen[key]; seen && now.Sub(t) < d.ttl {
		return false
	}
	if len(d.seen) >= d.max {
		for k, t := range d.seen {
			if now.Sub(t) >= d.ttl {
				delete(d.seen, k)
			}
		}
		if len(d.seen) >= d.max {
			d.seen = map[string]time.Time{} // pathological churn — start over
		}
	}
	d.seen[key] = now
	return true
}

type fcFlowLogger struct {
	group string
	seen  *logDedup
}

func newFcFlowLogger(group string) *fcFlowLogger {
	return &fcFlowLogger{group: group, seen: newLogDedup(fcFlowTTL, fcFlowSeenMax)}
}

// fcFlow is one parsed guest-egress flow start.
type fcFlow struct {
	proto   string // TCP | UDP | ICMP | ICMPv6
	src     net.IP
	dst     net.IP
	dstPort int  // meaningful only when hasPort
	hasPort bool // false for ICMP
}

// record logs one summarized line for a frame the filter just judged, if the
// frame starts a flow (see fcParseFlow) and the tuple hasn't logged within
// fcFlowTTL. Guest↔gateway traffic (fcDstGW: DNS to .1) is not real egress
// and is skipped.
func (l *fcFlowLogger) record(frame []byte, policy string, allowed bool) {
	fl, ok := fcParseFlow(frame)
	if !ok || fcClassifyDst(fl.dst) == fcDstGW {
		return
	}
	dst := fl.dst.String()
	if fl.hasPort {
		dst = net.JoinHostPort(dst, strconv.Itoa(fl.dstPort))
	}
	if !l.seen.allow(fl.proto + "|" + dst) {
		return
	}
	if allowed {
		emitLogfG("egress", l.group, "info", "[%s] flow %s %s -> %s", l.group, fl.proto, fl.src, dst)
	} else {
		emitLogfG("egress", l.group, "warn", "[%s] BLOCKED flow %s %s -> %s (network profile '%s')", l.group, fl.proto, fl.src, dst, policy)
	}
}

// fcParseFlow extracts the L4 flow start from an ethernet frame. Returns
// ok=false for anything that is not the start of a flow worth logging:
// non-IP frames, IP fragments past the first, TCP packets that aren't an
// initial SYN, IPv6 extension headers (rare; the filter still applies, only
// the log line is skipped), and truncated headers.
func fcParseFlow(frame []byte) (fcFlow, bool) {
	if len(frame) < 14 {
		return fcFlow{}, false
	}
	var proto byte
	var src, dst net.IP
	var l4 []byte
	switch binary.BigEndian.Uint16(frame[12:14]) {
	case 0x0800: // IPv4
		if len(frame) < 34 {
			return fcFlow{}, false
		}
		ihl := int(frame[14]&0x0f) * 4
		if ihl < 20 || len(frame) < 14+ihl {
			return fcFlow{}, false
		}
		if binary.BigEndian.Uint16(frame[20:22])&0x1fff != 0 {
			return fcFlow{}, false // fragment continuation — no L4 header
		}
		proto = frame[23]
		src, dst = net.IP(frame[26:30]), net.IP(frame[30:34])
		l4 = frame[14+ihl:]
	case 0x86DD: // IPv6
		if len(frame) < 54 {
			return fcFlow{}, false
		}
		proto = frame[20] // next header; extension headers → unknown proto below
		src, dst = net.IP(frame[22:38]), net.IP(frame[38:54])
		l4 = frame[54:]
	default:
		return fcFlow{}, false
	}
	switch proto {
	case 6: // TCP — log connection starts only: SYN set, ACK clear
		if len(l4) < 14 || l4[13]&0x12 != 0x02 {
			return fcFlow{}, false
		}
		return fcFlow{proto: "TCP", src: src, dst: dst,
			dstPort: int(binary.BigEndian.Uint16(l4[2:4])), hasPort: true}, true
	case 17: // UDP
		if len(l4) < 4 {
			return fcFlow{}, false
		}
		return fcFlow{proto: "UDP", src: src, dst: dst,
			dstPort: int(binary.BigEndian.Uint16(l4[2:4])), hasPort: true}, true
	case 1:
		return fcFlow{proto: "ICMP", src: src, dst: dst}, true
	case 58:
		return fcFlow{proto: "ICMPv6", src: src, dst: dst}, true
	}
	return fcFlow{}, false
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
