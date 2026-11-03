// Package netallow parses and evaluates a group's egress allowlist — the
// optional narrowing layered on top of the network profile (wan|lan|full).
//
// Semantics: effective = profile ∩ allowlist. An empty list means "no
// narrowing" (the profile alone decides, exactly the pre-allowlist
// behaviour). A non-empty list admits a destination only when the profile
// admits it AND some entry matches it.
//
// v1 entry forms (comma-separated on the CLI, stored as a JSON array):
//
//	203.0.113.7                 IPv4 single address
//	2001:db8::1                 IPv6 single address
//	10.0.0.0/8, 2001:db8::/32   CIDR (IPv4 or IPv6; host bits must be zero)
//	192.168.1.*                 last-octet wildcard (== 192.168.1.0/24)
//	10.0.0.5-10.0.0.20          inclusive range, same family, low <= high
//	example.com                 exact hostname
//	*.example.com               exactly ONE label under example.com
//
// Deliberately rejected at parse time (operator decision 2026-11-03):
//
//	*.example.*  and any other glob — would match example.evil.com
//	~regex       — unanchored-by-default footgun, no v1 use case
//
// Hostnames cannot be enforced at L3 directly (frames carry IPs). The
// gateway pins the IPs its own resolver returned for an allowed name (see
// Pins) and, on :443, checks the TLS SNI; see docs/network.md for the CDN
// shared-IP and DoH caveats.
package netallow

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaxEntries bounds a list. The check is linear per new flow; a few hundred
// entries is plenty for an allowlist and keeps a hand-pasted blob bounded.
const MaxEntries = 256

type addrRange struct{ lo, hi netip.Addr }

func (r addrRange) contains(a netip.Addr) bool {
	return a.BitLen() == r.lo.BitLen() && r.lo.Compare(a) <= 0 && a.Compare(r.hi) <= 0
}

// List is a parsed, immutable allowlist. The zero value / nil is the empty
// list (allows everything the profile allows).
type List struct {
	entries  []string // canonical forms, input order, de-duplicated
	prefixes []netip.Prefix
	ranges   []addrRange
	hosts    map[string]bool // exact names
	wild     map[string]bool // parent of a "*." entry: "example.com"
}

// Parse splits a comma-separated list (whitespace tolerated) and parses each
// entry. "" yields the empty list.
func Parse(s string) (*List, error) {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return ParseEntries(parts)
}

// ParseEntries parses individual entries. Any invalid entry fails the whole
// list — a typo must never silently widen or narrow egress.
func ParseEntries(in []string) (*List, error) {
	l := &List{hosts: map[string]bool{}, wild: map[string]bool{}}
	seen := map[string]bool{}
	for _, raw := range in {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		canon, err := l.add(e)
		if err != nil {
			return nil, fmt.Errorf("allow entry %q: %w", e, err)
		}
		if !seen[canon] {
			seen[canon] = true
			l.entries = append(l.entries, canon)
		}
		if len(l.entries) > MaxEntries {
			return nil, fmt.Errorf("allowlist too long (max %d entries)", MaxEntries)
		}
	}
	return l, nil
}

// Error sentinels so callers/tests can tell the rejection classes apart.
var (
	ErrGlob      = errors.New("glob patterns are not supported (v1 allows only a leading one-level '*.' wildcard, e.g. *.example.com; '*.example.*' would also match example.evil.com)")
	ErrRegex     = errors.New("regex entries ('~...') are not supported in v1; list exact hosts or *.example.com")
	ErrCIDR      = errors.New("invalid CIDR")
	ErrRange     = errors.New("invalid range")
	ErrReversed  = errors.New("reversed range: low address is greater than high address")
	ErrWildcard  = errors.New("invalid IPv4 wildcard: only the last octet may be '*' (e.g. 192.168.1.*)")
	ErrHostname  = errors.New("invalid hostname")
	ErrHostBits  = errors.New("CIDR has host bits set")
	ErrMixFamily = errors.New("range mixes IPv4 and IPv6")
)

func (l *List) add(e string) (string, error) {
	low := strings.ToLower(e)
	switch {
	case strings.HasPrefix(low, "~"):
		return "", ErrRegex
	case strings.Contains(low, "/"):
		p, err := netip.ParsePrefix(low)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrCIDR, err)
		}
		if p.Addr().Zone() != "" {
			return "", fmt.Errorf("%w: zones are not allowed", ErrCIDR)
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-unmapShift(p))
		if !p.IsValid() {
			return "", ErrCIDR
		}
		if m := p.Masked(); m != p {
			return "", fmt.Errorf("%w (did you mean %s?)", ErrHostBits, m)
		}
		l.prefixes = append(l.prefixes, p)
		return p.String(), nil
	case strings.Contains(low, "-") && looksLikeRange(low):
		a, b, _ := strings.Cut(low, "-")
		lo, err1 := parseAddr(strings.TrimSpace(a))
		hi, err2 := parseAddr(strings.TrimSpace(b))
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("%w: both ends must be full IP addresses", ErrRange)
		}
		if lo.BitLen() != hi.BitLen() {
			return "", ErrMixFamily
		}
		if lo.Compare(hi) > 0 {
			return "", ErrReversed
		}
		l.ranges = append(l.ranges, addrRange{lo, hi})
		return lo.String() + "-" + hi.String(), nil
	case strings.Contains(low, "*") && isIPv4Wildcard(low):
		p, err := parseV4Wildcard(low)
		if err != nil {
			return "", err
		}
		l.prefixes = append(l.prefixes, p)
		return strings.TrimSuffix(p.Addr().String(), ".0") + ".*", nil
	}
	if a, err := parseAddr(low); err == nil {
		l.prefixes = append(l.prefixes, netip.PrefixFrom(a, a.BitLen()))
		return a.String(), nil
	}
	if strings.Contains(low, ":") {
		return "", fmt.Errorf("%w: not a valid IPv6 address", ErrHostname)
	}
	// Hostname forms.
	if strings.HasPrefix(low, "*.") {
		parent := strings.TrimSuffix(low[2:], ".")
		if strings.Contains(parent, "*") {
			return "", ErrGlob
		}
		if err := validHost(parent); err != nil {
			return "", err
		}
		l.wild[parent] = true
		return "*." + parent, nil
	}
	if strings.Contains(low, "*") {
		return "", ErrGlob
	}
	h := strings.TrimSuffix(low, ".")
	if err := validHost(h); err != nil {
		return "", err
	}
	l.hosts[h] = true
	return h, nil
}

// unmapShift converts a ::ffff:a.b.c.d/N prefix to its IPv4 bit count.
func unmapShift(p netip.Prefix) int {
	if p.Addr().Is4In6() {
		return 96
	}
	return 0
}

func parseAddr(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	if a.Zone() != "" {
		return netip.Addr{}, errors.New("zones are not allowed")
	}
	return a.Unmap(), nil
}

// looksLikeRange: a '-' entry is a range only if it is made of address
// characters (hostnames may contain '-' too).
func looksLikeRange(s string) bool {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return false
	}
	isAddrish := func(x string) bool {
		x = strings.TrimSpace(x)
		if x == "" {
			return false
		}
		for _, c := range x {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c == '.' || c == ':') {
				return false
			}
		}
		return strings.ContainsAny(x, ".:") && (strings.Contains(x, ":") || isDigitsDots(x))
	}
	return isAddrish(a) && isAddrish(b)
}

func isDigitsDots(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c == '.') {
			return false
		}
	}
	return true
}

// isIPv4Wildcard: four dot-separated parts, each digits or '*'.
func isIPv4Wildcard(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p != "*" && (p == "" || !isDigitsDots(p)) {
			return false
		}
	}
	return true
}

func parseV4Wildcard(s string) (netip.Prefix, error) {
	parts := strings.Split(s, ".")
	if parts[3] != "*" || strings.Contains(strings.Join(parts[:3], "."), "*") {
		return netip.Prefix{}, ErrWildcard
	}
	a, err := netip.ParseAddr(strings.Join(parts[:3], ".") + ".0")
	if err != nil || !a.Is4() {
		return netip.Prefix{}, ErrWildcard
	}
	return netip.PrefixFrom(a, 24), nil
}

// validHost: LDH labels, 1..63 each, <=253 total, at least two labels, ASCII
// only (IDNs must be given as punycode), and a non-numeric last label so
// "10.0.0.300" is not mistaken for a name.
func validHost(h string) error {
	if h == "" || len(h) > 253 {
		return fmt.Errorf("%w: empty or longer than 253 bytes", ErrHostname)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: need at least two labels (e.g. example.com)", ErrHostname)
	}
	for _, lb := range labels {
		if lb == "" || len(lb) > 63 {
			return fmt.Errorf("%w: empty or over-long label", ErrHostname)
		}
		if lb[0] == '-' || lb[len(lb)-1] == '-' {
			return fmt.Errorf("%w: label %q starts or ends with '-'", ErrHostname, lb)
		}
		for i := 0; i < len(lb); i++ {
			c := lb[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return fmt.Errorf("%w: character %q not allowed (use punycode for IDNs)", ErrHostname, c)
			}
		}
	}
	if isDigitsDots(labels[len(labels)-1]) {
		return fmt.Errorf("%w: not a valid IPv4 address or hostname", ErrHostname)
	}
	return nil
}

// Empty reports whether the list imposes no narrowing.
func (l *List) Empty() bool { return l == nil || len(l.entries) == 0 }

// Entries returns the canonical entries (copy).
func (l *List) Entries() []string {
	if l == nil {
		return nil
	}
	return append([]string(nil), l.entries...)
}

// String is the canonical comma-separated form.
func (l *List) String() string { return strings.Join(l.Entries(), ",") }

// HasHosts reports whether any hostname entry exists (pins/SNI only matter
// then).
func (l *List) HasHosts() bool { return l != nil && (len(l.hosts) > 0 || len(l.wild) > 0) }

// AllowIP reports whether a STATIC (IP/CIDR/wildcard/range) entry matches a.
// Hostname entries never match here — see Pins.
func (l *List) AllowIP(a netip.Addr) bool {
	if l == nil {
		return false
	}
	a = a.Unmap()
	for _, p := range l.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	for _, r := range l.ranges {
		if r.contains(a) {
			return true
		}
	}
	return false
}

// MatchHost reports whether name matches an exact entry or a one-level
// wildcard ("*.example.com" matches "a.example.com", not "example.com" nor
// "a.b.example.com"). Case-insensitive, trailing dot ignored.
func (l *List) MatchHost(name string) bool {
	if l == nil {
		return false
	}
	n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if n == "" {
		return false
	}
	if l.hosts[n] {
		return true
	}
	if _, parent, ok := strings.Cut(n, "."); ok && l.wild[parent] {
		return true
	}
	return false
}

// ---- resolver pins --------------------------------------------------------

// Pin TTL clamps. A pin lives max(TTL, PinMinTTL) capped at PinMaxTTL, so a
// TTL-0 answer is still usable for the connect that follows it and a
// day-long TTL does not keep a stale IP open for a day.
const (
	PinMinTTL = 30 * time.Second
	PinMaxTTL = time.Hour
	MaxPins   = 4096
)

// Pins is the set of IPs the gateway's OWN resolver returned for allowed
// hostnames, each with an expiry. Only answers observed on the gateway →
// guest path are pinned (the guest cannot forge those); safe for concurrent
// use.
type Pins struct {
	mu  sync.Mutex
	m   map[netip.Addr]time.Time
	now func() time.Time
}

// NewPins returns an empty pin set. now may be nil (time.Now).
func NewPins(now func() time.Time) *Pins {
	if now == nil {
		now = time.Now
	}
	return &Pins{m: map[netip.Addr]time.Time{}, now: now}
}

// Add pins a for ttl (clamped). Returns false when the table is full of live
// pins — fail closed: the IP stays unreachable.
func (p *Pins) Add(a netip.Addr, ttl time.Duration) bool {
	if ttl < PinMinTTL {
		ttl = PinMinTTL
	}
	if ttl > PinMaxTTL {
		ttl = PinMaxTTL
	}
	a = a.Unmap()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	exp := now.Add(ttl)
	if old, ok := p.m[a]; ok {
		if exp.After(old) {
			p.m[a] = exp
		}
		return true
	}
	if len(p.m) >= MaxPins {
		for k, e := range p.m {
			if !e.After(now) {
				delete(p.m, k)
			}
		}
		if len(p.m) >= MaxPins {
			return false
		}
	}
	p.m[a] = exp
	return true
}

// Has reports whether a is pinned and unexpired.
func (p *Pins) Has(a netip.Addr) bool {
	if p == nil {
		return false
	}
	a = a.Unmap()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[a]
	if !ok {
		return false
	}
	if !e.After(p.now()) {
		delete(p.m, a)
		return false
	}
	return true
}

// Snapshot returns the live pins, sorted (diagnostics/tests).
func (p *Pins) Snapshot() []netip.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []netip.Addr
	for a, e := range p.m {
		if e.After(now) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}
