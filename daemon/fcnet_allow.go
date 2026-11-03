package main

// fcnet_allow.go — the per-group egress allowlist at the L3 gateway.
//
// effective = profile ∩ allowlist. fcFrameAllowed applies the profile; this
// file narrows what survives it. The policy is read from currentAllow(group)
// on EVERY guest frame (one atomic load), so a `koto config -allow` change
// takes effect on the running VM immediately — new flows and established
// ones alike: a flow to a destination that is no longer allowed loses its
// next packet. An inactive policy (empty list) is a no-op: the profile alone
// decides, exactly the pre-allowlist behaviour.
//
// Hostname entries cannot match a frame directly (frames carry IPs). Instead
// koto's OWN host-side resolver (fcnet_dns.go), which answers the guest at
// 192.168.127.1:53 while an allowlist is active, pins the A/AAAA answers it
// got for an allowed name, for the minimum TTL along the CNAME chain
// (clamped, see netallow.Pins). Guest-path traffic is never parsed for DNS
// answers: the gateway → guest direction is reachable by the guest itself
// (gvisor switch reflection), so a pin sniffed there would be forgeable.
//
// On a pin-only destination:
//   - TCP :443 — a TLS ClientHello whose SNI is not an allowed name is
//     dropped (shared CDN IPs: pinning a.example.com's IP must not open
//     every other site on that edge). A ClientHello with no SNI is dropped.
//     A ClientHello whose SNI is not in the first segment is let through
//     (best effort — see docs/firecracker-vsock.md "Egress allowlist").
//   - UDP :443 (QUIC) is dropped: the SNI is not checkable without
//     decrypting the Initial; clients fall back to TCP.
//   - anything else is allowed by IP.
// DNS the guest sends ELSEWHERE (DoH, a public resolver it is allowed to
// reach) pins nothing, so a hostname entry only works through the gateway
// resolver. DNS over TCP to the gateway is answered by gvisor and pins
// nothing either: documented caveats.

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"time"

	"koto/netallow"
)

// fcAllow is one guest link's allowlist state: the policy source and the
// pins koto's resolver recorded for this link.
type fcAllow struct {
	policy func() *allowPolicy
	now    func() time.Time

	mu     sync.Mutex
	pinFor *allowPolicy // policy snapshot the pins were last reconciled to
	pins   *netallow.Pins
}

func newFcAllow(group string) *fcAllow {
	return &fcAllow{policy: func() *allowPolicy { return currentAllow(group) }, now: time.Now}
}

// pinSet returns the pin table reconciled to policy p. When the policy
// changed since the last call, pins whose hostname is no longer allowed are
// dropped; pins of still-allowed names survive (the guest's own DNS cache
// would otherwise strand them until it expires).
func (a *fcAllow) pinSet(p *allowPolicy) *netallow.Pins {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pins == nil {
		a.pins = netallow.NewPins(a.now)
		a.pinFor = p
	}
	if a.pinFor != p {
		a.pins.Retain(p.list.MatchHost)
		a.pinFor = p
	}
	return a.pins
}

// fcPkt is the slice of an IP packet the allowlist needs.
type fcPkt struct {
	src, dst         netip.Addr
	proto            byte   // transport after IPv6 ext walk; 0 if unknown
	srcPort, dstPort uint16 // TCP/UDP only
	payload          []byte // TCP/UDP payload (nil if unreadable / fragment)
}

// fcParsePkt reads src/dst at the same fixed offsets fcFrameAllowed judges
// (so the two filters can never disagree on WHICH destination a frame has),
// then best-effort parses the transport. ok=false for non-IP / too short.
func fcParsePkt(frame []byte) (fcPkt, bool) {
	var pk fcPkt
	if len(frame) < 14 {
		return pk, false
	}
	var l4 []byte
	switch binary.BigEndian.Uint16(frame[12:14]) {
	case 0x0800:
		if len(frame) < 34 {
			return pk, false
		}
		pk.src = netip.AddrFrom4([4]byte(frame[26:30]))
		pk.dst = netip.AddrFrom4([4]byte(frame[30:34]))
		ihl := int(frame[14]&0x0f) * 4
		tot := int(binary.BigEndian.Uint16(frame[16:18]))
		if frame[14]>>4 != 4 || ihl < 20 || tot < ihl || 14+tot > len(frame) ||
			binary.BigEndian.Uint16(frame[20:22])&0x1fff != 0 {
			return pk, true // dst known, transport unreadable / not first fragment
		}
		pk.proto = frame[23]
		l4 = frame[14+ihl : 14+tot]
	case 0x86DD:
		if len(frame) < 54 {
			return pk, false
		}
		pk.src = netip.AddrFrom16([16]byte(frame[22:38])).Unmap()
		pk.dst = netip.AddrFrom16([16]byte(frame[38:54])).Unmap()
		pay := int(binary.BigEndian.Uint16(frame[18:20]))
		if frame[14]>>4 != 6 || 54+pay > len(frame) {
			return pk, true
		}
		proto, rest, opaque := fcWalkIPv6Ext(frame[20], frame[54:54+pay])
		if opaque != "" {
			return pk, true
		}
		pk.proto, l4 = proto, rest
	default:
		return pk, false
	}
	switch pk.proto {
	case 6:
		if len(l4) >= 20 {
			if off := int(l4[12]>>4) * 4; off >= 20 && off <= len(l4) {
				pk.srcPort = binary.BigEndian.Uint16(l4[0:2])
				pk.dstPort = binary.BigEndian.Uint16(l4[2:4])
				pk.payload = l4[off:]
			}
		}
	case 17:
		if len(l4) >= 8 {
			if dl := int(binary.BigEndian.Uint16(l4[4:6])); dl >= 8 && dl <= len(l4) {
				pk.srcPort = binary.BigEndian.Uint16(l4[0:2])
				pk.dstPort = binary.BigEndian.Uint16(l4[2:4])
				pk.payload = l4[8:dl]
			}
		}
	}
	return pk, true
}

// allowFrame narrows a guest → host frame the profile already admitted.
func (a *fcAllow) allowFrame(frame []byte) bool {
	p := a.policy()
	if !p.active() {
		return true // empty list: the profile alone decides
	}
	pk, ok := fcParsePkt(frame)
	if !ok {
		return true // non-IP (ARP) / runt: same as the profile filter
	}
	if fcClassifyDst(net.IP(pk.dst.AsSlice())) == fcDstGW {
		return true // gateway resolver: DNS must work for pinning to
	}
	if p.denyAll {
		return false
	}
	if p.list.AllowIP(pk.dst) {
		return true
	}
	if !p.list.HasHosts() || !a.pinSet(p).Has(pk.dst) {
		return false
	}
	// Pin-only destination: the IP was reached through an allowed NAME.
	if pk.dstPort == 443 {
		switch pk.proto {
		case 17:
			return false // QUIC: SNI not checkable; force TCP fallback
		case 6:
			if sni, isHello, complete := fcTLSSNI(pk.payload); isHello && complete {
				return sni != "" && p.list.MatchHost(sni)
			}
		}
	}
	return true
}

// fcTLSSNI extracts the server_name from a TLS ClientHello at the start of a
// TCP payload. isHello reports the payload starts a handshake record carrying
// a ClientHello; complete reports the extensions block was fully present (so
// an empty sni means "no SNI sent", not "cut off by the segment boundary").
func fcTLSSNI(p []byte) (sni string, isHello, complete bool) {
	// record: type(1)=22 version(2) len(2); handshake: type(1)=1 len(3)
	if len(p) < 9 || p[0] != 0x16 || p[1] != 0x03 || p[5] != 0x01 {
		return "", false, false
	}
	b := p[9:]
	// legacy_version(2) random(32)
	if len(b) < 34 {
		return "", true, false
	}
	b = b[34:]
	skip := func(lenBytes int) bool { // skip a length-prefixed vector
		if len(b) < lenBytes {
			return false
		}
		n := 0
		for i := 0; i < lenBytes; i++ {
			n = n<<8 | int(b[i])
		}
		if len(b) < lenBytes+n {
			return false
		}
		b = b[lenBytes+n:]
		return true
	}
	if !skip(1) || !skip(2) || !skip(1) { // session_id, cipher_suites, compression
		return "", true, false
	}
	if len(b) < 2 {
		return "", true, false
	}
	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < extLen {
		// Truncated: scan what is present for the SNI anyway.
		complete = false
	} else {
		b, complete = b[:extLen], true
	}
	for len(b) >= 4 {
		typ := binary.BigEndian.Uint16(b)
		n := int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+n {
			return "", true, false
		}
		body := b[4 : 4+n]
		b = b[4+n:]
		if typ != 0 { // server_name
			continue
		}
		// server_name_list(2) { name_type(1)=0 host_name<2> }
		if len(body) < 5 || body[2] != 0 {
			return "", true, true // malformed SNI: judged as "no allowed name"
		}
		hl := int(binary.BigEndian.Uint16(body[3:5]))
		if len(body) < 5+hl {
			return "", true, true
		}
		return string(body[5 : 5+hl]), true, true
	}
	return "", true, complete
}
