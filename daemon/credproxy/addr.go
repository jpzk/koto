package credproxy

import "net"

// IsIPLiteral reports whether host is a raw IP literal (no hostname to match a
// policy host). IP-literal CONNECT is never substituted (design §9).
func IsIPLiteral(host string) bool {
	return net.ParseIP(hostOf(host)) != nil
}

// IsLocalDest reports whether host resolves to a loopback / link-local / unspecified
// address that must never be reached through the proxy (design §9). Only IP
// literals are classified here; hostnames are left to the dialer + policy.
func IsLocalDest(host string) bool {
	ip := net.ParseIP(hostOf(host))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
