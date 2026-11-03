package main

import (
	"net"
	"net/netip"
	"strings"
)

// egressAllowlistOK applies the group's egress allowlist (config key
// "allow") to a forward-proxy target that already passed the profile gate in
// egressTargetAllowed. Without it, HTTP_PROXY would be a way around the L3
// filter: the daemon dials on the guest's behalf from its own process.
//
// The policy is read per request (currentAllow), so an edit applies to the
// next CONNECT / absolute-form request with no restart; tunnels already
// open stay up (they are one TCP connection the daemon spliced — the same
// as a flow at L3 whose next packet never comes).
//
//   - empty list           → true (profile only, today's behaviour)
//   - corrupt stored list  → false (deny-all, same as the gateway)
//   - target is a NAME     → allowed when the list names it (exact or
//     one-level *.x), or when the vetted IP the dial is pinned to matches a
//     static IP entry. The daemon resolved the name itself, so no pin is
//     needed here — the name IS the authority.
//   - target is an IP      → only static IP entries. Resolver pins live on
//     the guest's L3 link and are not consulted: a guest that wants an
//     allowed host through the proxy has to ask for it by name.
//
// vetted is the ip:port egressTargetAllowed returned; it is what gets
// dialed, so it is the address the static check must cover.
func egressAllowlistOK(g, target, vetted string) (bool, string) {
	p := currentAllow(g)
	if !p.active() {
		return true, ""
	}
	if p.denyAll {
		return false, "stored allowlist is invalid (deny-all)"
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	host = strings.Trim(host, "[]")
	_, litErr := netip.ParseAddr(host)
	if litErr != nil && p.list.MatchHost(host) {
		return true, ""
	}
	if ip := egressDialIP(vetted); ip != nil {
		if a, ok := netip.AddrFromSlice(ip); ok && p.list.AllowIP(a.Unmap()) {
			return true, ""
		}
	}
	return false, "not in this group's allowlist (" + p.list.String() + ")"
}
