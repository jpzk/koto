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

// fcNetGatewayIPParsed is the one gateway-subnet address that is a carve-out
// (fcClassifyDst); the rest of the /24 is LAN.
var fcNetGatewayIPParsed = net.ParseIP(fcNetGatewayIP)

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
func fcNetServe(ctx context.Context, vn *virtualnetwork.VirtualNetwork, conn net.Conn, group, policy string) (err error) {
	// The netstack parses every frame a guest emits, inside the daemon
	// process (audit M14). A panic on THIS goroutine — the accept/reframe
	// path — must not take the fleet down with it; it becomes this group's
	// gateway error and the VM loses its link. Goroutines gvisor spawns
	// internally are outside this frame's reach; that residue is why the
	// long-term shape is a separately jailed gateway process.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("l3 gateway: netstack panic: %v", r)
		}
	}()
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

// fcIMDSv6 is AWS's IPv6 instance-metadata address (fd00:ec2::254). See
// fcClassifyDst for why it needs naming separately from the v4 endpoint.
var fcIMDSv6 = net.ParseIP("fd00:ec2::254")

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
	// The unspecified address is the control plane too: Linux delivers a
	// connect() to 0.0.0.0 / :: LOCALLY, to whatever listens on that port on
	// any interface — so at L7 `CONNECT 0.0.0.0:8788` reached a peer group's
	// proxy port and every other loopback-only host service (audit H2,
	// 2026-09-04; reproduced). The netstack itself refuses it at the frame
	// layer, but the proxy dials from the daemon process and did not.
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fcDstCtl // 0.0.0.0, ::, 127.0.0.0/8, ::1, 169.254.0.0/16, fe80::/10
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
	// Only the gateway's own address is the carve-out. The rest of the /24 is
	// ordinary RFC1918 space that the forwarder would dial verbatim on the
	// host — a real 192.168.127.0/24 LAN or VPN was reachable under `wan`
	// (audit L2). DNS goes to .1, so nothing legitimate loses out.
	if ip.Equal(fcNetGatewayIPParsed) {
		return fcDstGW
	}
	// Cloud instance-metadata endpoints are control plane wherever koto runs,
	// because they hand out the host's own role credentials to anything that
	// asks. The IPv4 one (169.254.169.254 — AWS, GCP, Azure, and most of the
	// rest) is already caught above as link-local; AWS's IPv6 IMDS is a ULA,
	// which IsPrivate() classes as LAN, so a root-enabled lan|full guest
	// could reach it on a dual-stack host and read temporary credentials.
	// Deny by address rather than trusting Ec2MetadataAccess:false, which
	// only governs the netstack's own answer, not a host-routed dial.
	if ip.Equal(fcIMDSv6) {
		return fcDstCtl
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

// fcSelfIPsTTL bounds how stale the self-address list may be. Interface
// addresses change while the daemon runs — tailscale coming up after boot
// (CGNAT, classed LAN), a VPN, a second NIC, a DHCP renewal — and the list
// used to be a sync.Once snapshot taken at the FIRST guest frame, so anything
// acquired later was never blocked (audit M8). net.InterfaceAddrs is a
// netlink dump, cheap enough to refresh every few seconds; the flow-log
// dedupe means the extra syscalls are not per packet.
const fcSelfIPsTTL = 5 * time.Second

// fcSelfIPs is the daemon/cs_host's own interface addresses, refreshed on a
// short TTL. Guest L3 packets to any of these are the control plane and are
// dropped. A var so tests can pin the list.
var fcSelfIPs = func() func() []net.IP {
	var mu sync.Mutex
	var ips []net.IP
	var at time.Time
	return func() []net.IP {
		mu.Lock()
		defer mu.Unlock()
		if ips != nil && time.Since(at) < fcSelfIPsTTL {
			return ips
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			// Keep the last known-good list and retry on the next call. The
			// error used to be discarded and the (empty) result stored as
			// fresh, which UNBLOCKS every host address for the next TTL —
			// exactly backwards for a denylist, and on both the L3 and L7
			// paths since they share this classifier (audit M34). A netlink
			// dump failing is rare enough that retrying beats caching a
			// fail-open answer; `at` is left alone so the next call retries
			// immediately rather than after another TTL.
			emitLogf("egress", "warn", "self-address refresh failed (%v) — keeping the previous %d address(es)", err, len(ips))
			return ips
		}
		fresh := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				fresh = append(fresh, n.IP)
			}
		}
		ips, at = fresh, time.Now()
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
	bytes  *fcTokenBucket
	frames *fcTokenBucket
}

func fcNewEgressConn(c net.Conn, group, policy string) *fcEgressConn {
	return &fcEgressConn{
		Conn:   c,
		policy: policy,
		flows:  newFcFlowLogger(group),
		bytes:  newFcTokenBucket(fcNetBytesPerSec, fcNetBytesBurst),
		frames: newFcTokenBucket(fcNetFramesPerSec, fcNetFramesBurst),
	}
}

func (e *fcEgressConn) Read(p []byte) (int, error) {
	for len(e.buf) == 0 {
		frame, err := e.readFrame()
		if err != nil {
			return 0, err
		}
		// Charge the guest for the work its frame is about to cost —
		// classification, flow accounting, and the netstack's own parse —
		// BEFORE doing any of it (audit 2026-09-11 L60). A dropped frame is
		// charged too: rejecting costs host CPU as surely as forwarding, so
		// exempting the denied ones would make the flood profile the cheap
		// one. See fcTokenBucket for why this throttles rather than drops.
		e.frames.take(1)
		e.bytes.take(len(frame) + 4)
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

// ---- per-guest work budget --------------------------------------------------
//
// A networked guest pumps frames into the daemon process as fast as vsock will
// carry them, and every one of them costs an allocation, a classification, a
// flow-table lookup and a netstack parse — all on the daemon's CPU, beside the
// gRPC server and the proxy every other group depends on (audit 2026-09-11
// L60). vsock backpressure bounds how much is QUEUED, not how much work a
// guest may demand per second, so a guest with code execution could take a
// share of the host's scheduling capacity simply by sending.
//
// The budget is deliberately loose — an OOM/CPU backstop, not a shaper, the
// same call the proxy's concurrency caps made (M2). A networked group exists
// to do real work (a git clone, an npm install, a container pull), and a limit
// tight enough to shape that traffic would make ordinary turns mysteriously
// slow. The bytes cap sits well above any single host NIC; the frame cap is
// what actually binds, because the cheapest flood is the smallest frame and
// frames — not bytes — are what the per-frame cost scales with.
//
// It THROTTLES rather than drops: the reader simply stops reading until its
// tokens refill, so the pressure propagates back down the vsock connection to
// the guest's own pump. Dropping would be indistinguishable from the egress
// filter's DROP and would silently corrupt a permitted TCP stream into a
// retransmit storm — more host work, not less.
const (
	fcNetBytesPerSec  = 64 << 20 // 64 MiB/s sustained, per guest
	fcNetBytesBurst   = 64 << 20 // one second of slack
	fcNetFramesPerSec = 100_000  // ~1.5x MTU-sized line rate at the byte cap
	fcNetFramesBurst  = 100_000
)

// fcTokenBucket is a plain refill-on-read token bucket. It is not safe for
// concurrent use and does not have to be: one lives per fcEgressConn, charged
// only from that conn's single Read goroutine.
type fcTokenBucket struct {
	rate    float64 // tokens per second
	burst   float64 // ceiling
	tokens  float64
	last    time.Time
	nowFn   func() time.Time
	sleepFn func(time.Duration)
}

func newFcTokenBucket(rate, burst float64) *fcTokenBucket {
	return &fcTokenBucket{
		rate: rate, burst: burst, tokens: burst,
		nowFn: time.Now, sleepFn: time.Sleep,
	}
}

// take charges n tokens, sleeping until they are available. A single charge
// larger than the burst would never be satisfiable, so it is clamped — the
// caller's unit (one frame, or one frame's bytes) is always far below both
// ceilings, and wedging the guest's link forever is not an acceptable answer
// to arithmetic.
func (b *fcTokenBucket) take(n int) {
	want := float64(n)
	if want > b.burst {
		want = b.burst
	}
	for {
		now := b.nowFn()
		if !b.last.IsZero() {
			b.tokens += now.Sub(b.last).Seconds() * b.rate
			if b.tokens > b.burst {
				b.tokens = b.burst
			}
		}
		b.last = now
		if b.tokens >= want {
			b.tokens -= want
			return
		}
		// Wait for the shortfall, but wake up regularly: a closed conn is
		// noticed by the next readFrame, and a long uninterruptible sleep
		// here would hold a teardown open for no reason.
		wait := time.Duration((want - b.tokens) / b.rate * float64(time.Second))
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		if wait <= 0 {
			wait = time.Millisecond
		}
		b.sleepFn(wait)
	}
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
			// Pathological churn — start over. Note what this COSTS: every
			// suppression currently in force is forgotten, so the keys that
			// were being deduped are all admitted again. That is why the
			// caller must not rely on this map alone as its rate limit (audit
			// 2026-09-11 L86); see fcFlowLogger's budget.
			d.seen = map[string]time.Time{}
		}
	}
	d.seen[key] = now
	return true
}

// fcFlowRate is the ceiling on flow lines ONE guest can put into the daemon
// log per minute, with a burst for the honest case (audit 2026-09-11 L86).
//
// The dedup map is not a rate limit and cannot be made into one: its key is
// (protocol, destination, port), every field of which the guest chooses, so
// walking a port range mints a fresh key per packet — and when the map hits
// fcFlowSeenMax it drops every live suppression and admits the lot again.
// Each admitted line is then formatted, written to stderr, inserted into the
// global log ring (evicting real entries) and pushed to every SubscribeLogs
// subscriber, which is an audit-availability problem as much as a CPU one: the
// cheapest way to hide one flow is to bury it under ten thousand.
//
// So the line count is bounded independently of the key space. The suppressed
// count is reported when the budget refills, because "the log is incomplete" is
// itself the finding an operator needs — silence there would be the same
// erasure by a slower route. Sized so an ordinary networked turn (a clone, an
// npm install, a container pull — tens of distinct flows) never reaches it.
const (
	fcFlowRatePerMin = 120
	fcFlowRateBurst  = 240
)

type fcFlowLogger struct {
	group string
	seen  *logDedup

	mu         sync.Mutex
	tokens     float64
	last       time.Time
	suppressed int
}

func newFcFlowLogger(group string) *fcFlowLogger {
	return &fcFlowLogger{
		group:  group,
		seen:   newLogDedup(fcFlowTTL, fcFlowSeenMax),
		tokens: fcFlowRateBurst,
		last:   time.Now(),
	}
}

// budget spends one line's worth of the guest's allowance. When it returns
// false the line is dropped; when it returns true it may also report how many
// were dropped since the last one got through.
func (l *fcFlowLogger) budget() (ok bool, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens += now.Sub(l.last).Minutes() * fcFlowRatePerMin
	if l.tokens > fcFlowRateBurst {
		l.tokens = fcFlowRateBurst
	}
	l.last = now
	if l.tokens < 1 {
		l.suppressed++
		return false, 0
	}
	l.tokens--
	dropped, l.suppressed = l.suppressed, 0
	return true, dropped
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
	ok2, dropped := l.budget()
	if !ok2 {
		return
	}
	if dropped > 0 {
		emitLogfG("egress", l.group, "warn", "[%s] %d flow lines were dropped: this guest is over its logging budget "+
			"(%d/min) — the flow log for it is INCOMPLETE", l.group, dropped, fcFlowRatePerMin)
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
