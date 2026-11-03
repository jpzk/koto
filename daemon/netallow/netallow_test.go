package netallow

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseV1Forms(t *testing.T) {
	cases := []struct {
		name, in, canon     string
		allowIP, denyIP     []string
		allowHost, denyHost []string
	}{
		{name: "ipv4 single", in: "203.0.113.7", canon: "203.0.113.7",
			allowIP: []string{"203.0.113.7", "::ffff:203.0.113.7"}, denyIP: []string{"203.0.113.8"}},
		{name: "ipv6 single", in: "2001:DB8::1", canon: "2001:db8::1",
			allowIP: []string{"2001:db8::1"}, denyIP: []string{"2001:db8::2"}},
		{name: "ipv4 cidr", in: "10.0.0.0/8", canon: "10.0.0.0/8",
			allowIP: []string{"10.255.1.2"}, denyIP: []string{"11.0.0.1"}},
		{name: "ipv6 cidr", in: "2001:db8::/32", canon: "2001:db8::/32",
			allowIP: []string{"2001:db8:ffff::1"}, denyIP: []string{"2001:db9::1", "10.0.0.1"}},
		{name: "v4-mapped cidr", in: "::ffff:192.0.2.0/120", canon: "192.0.2.0/24",
			allowIP: []string{"192.0.2.9"}, denyIP: []string{"192.0.3.9"}},
		{name: "last-octet wildcard", in: "192.168.1.*", canon: "192.168.1.*",
			allowIP: []string{"192.168.1.0", "192.168.1.255"}, denyIP: []string{"192.168.2.1"}},
		{name: "ipv4 range", in: "10.0.0.5-10.0.0.20", canon: "10.0.0.5-10.0.0.20",
			allowIP: []string{"10.0.0.5", "10.0.0.12", "10.0.0.20"}, denyIP: []string{"10.0.0.4", "10.0.0.21"}},
		{name: "ipv6 range", in: "2001:db8::10 - 2001:db8::20", canon: "2001:db8::10-2001:db8::20",
			allowIP: []string{"2001:db8::15"}, denyIP: []string{"2001:db8::21", "10.0.0.15"}},
		{name: "single-address range", in: "10.1.1.1-10.1.1.1", canon: "10.1.1.1-10.1.1.1",
			allowIP: []string{"10.1.1.1"}, denyIP: []string{"10.1.1.2"}},
		{name: "exact host", in: "Example.COM.", canon: "example.com",
			allowHost: []string{"example.com", "EXAMPLE.com."}, denyHost: []string{"a.example.com", "example.com.evil.io", "evilexample.com"},
			denyIP: []string{"93.184.216.34"}},
		{name: "hyphen host is not a range", in: "my-api.example-1.com", canon: "my-api.example-1.com",
			allowHost: []string{"my-api.example-1.com"}},
		{name: "one-level wildcard", in: "*.example.com", canon: "*.example.com",
			allowHost: []string{"a.example.com", "API.example.com"},
			denyHost:  []string{"example.com", "a.b.example.com", "a.example.com.evil.io", "a.example.org"}},
		{name: "mixed list", in: " 1.2.3.4 , *.ex.io,10.0.0.0/8,,1.2.3.4", canon: "1.2.3.4,*.ex.io,10.0.0.0/8",
			allowIP: []string{"1.2.3.4", "10.9.9.9"}, allowHost: []string{"x.ex.io"}, denyHost: []string{"ex.io"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.in, err)
			}
			if got := l.String(); got != c.canon {
				t.Errorf("canon = %q, want %q", got, c.canon)
			}
			// canonical form must re-parse to itself
			l2, err := Parse(l.String())
			if err != nil || l2.String() != c.canon {
				t.Errorf("re-parse canon: %v %q", err, l2.String())
			}
			for _, s := range c.allowIP {
				if !l.AllowIP(netip.MustParseAddr(s)) {
					t.Errorf("AllowIP(%s) = false, want true", s)
				}
			}
			for _, s := range c.denyIP {
				if l.AllowIP(netip.MustParseAddr(s)) {
					t.Errorf("AllowIP(%s) = true, want false", s)
				}
			}
			for _, s := range c.allowHost {
				if !l.MatchHost(s) {
					t.Errorf("MatchHost(%s) = false, want true", s)
				}
			}
			for _, s := range c.denyHost {
				if l.MatchHost(s) {
					t.Errorf("MatchHost(%s) = true, want false", s)
				}
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		in   string
		want error
	}{
		{"*.example.*", ErrGlob},
		{"example.*", ErrGlob},
		{"ex*mple.com", ErrGlob},
		{"*.*.example.com", ErrGlob},
		{"*", ErrGlob},
		{"~^api\\.example\\.com$", ErrRegex},
		{"~.*", ErrRegex},
		{"10.0.0.0/33", ErrCIDR},
		{"10.0.0/8", ErrCIDR},
		{"2001:db8::/129", ErrCIDR},
		{"10.0.0.1/24", ErrHostBits},
		{"::ffff:1.2.3.0/90", ErrCIDR},
		{"10.0.0.20-10.0.0.5", ErrReversed},
		{"2001:db8::20-2001:db8::10", ErrReversed},
		{"10.0.0.1-2001:db8::1", ErrMixFamily},
		{"10.0.0.1-10.0.0", ErrRange},
		{"192.168.*.*", ErrWildcard},
		{"192.*.1.1", ErrWildcard},
		{"300.1.1.*", ErrWildcard},
		{"10.0.0.300", ErrHostname},
		{"localhost", ErrHostname},
		{"-bad.example.com", ErrHostname},
		{"bad_ünicode.com", ErrHostname},
		{"2001:db8::zz", ErrHostname},
		{"fe80::1%eth0", ErrHostname},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			l, err := Parse(c.in)
			if err == nil {
				t.Fatalf("Parse(%q) = %q, want error %v", c.in, l.String(), c.want)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("Parse(%q) error = %v, want %v", c.in, err, c.want)
			}
			if !strings.Contains(err.Error(), strconv.Quote(c.in)) {
				t.Errorf("error should name the bad entry: %v", err)
			}
		})
	}
	// one bad entry poisons the whole list
	if _, err := Parse("10.0.0.0/8,*.example.*"); !errors.Is(err, ErrGlob) {
		t.Fatalf("mixed list with glob: %v", err)
	}
}

func TestEmpty(t *testing.T) {
	for _, s := range []string{"", " ", ",, ,"} {
		l, err := Parse(s)
		if err != nil || !l.Empty() || l.HasHosts() {
			t.Fatalf("Parse(%q) = %v,%v; want empty", s, l, err)
		}
	}
	var nl *List
	if !nl.Empty() || nl.AllowIP(netip.MustParseAddr("1.1.1.1")) || nl.MatchHost("a.b") {
		t.Fatal("nil list must be empty and match nothing")
	}
}

func TestTooLong(t *testing.T) {
	var b []string
	for i := 0; i <= MaxEntries; i++ {
		b = append(b, netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}).String())
	}
	if _, err := ParseEntries(b); err == nil {
		t.Fatal("want too-long error")
	}
}

func TestPinsTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	p := NewPins(func() time.Time { return now })
	a := netip.MustParseAddr("93.184.216.34")
	if p.Has(a) {
		t.Fatal("unpinned")
	}
	p.Add(a, 0) // clamped to PinMinTTL
	if !p.Has(a) {
		t.Fatal("pinned")
	}
	now = now.Add(PinMinTTL - time.Second)
	if !p.Has(a) {
		t.Fatal("still within min TTL")
	}
	now = now.Add(2 * time.Second)
	if p.Has(a) {
		t.Fatal("expired")
	}
	p.Add(a, 48*time.Hour) // clamped to PinMaxTTL
	now = now.Add(PinMaxTTL + time.Second)
	if p.Has(a) {
		t.Fatal("max TTL clamp")
	}
	// v4-mapped is the same address
	p.Add(netip.MustParseAddr("::ffff:1.2.3.4"), time.Minute)
	if !p.Has(netip.MustParseAddr("1.2.3.4")) {
		t.Fatal("unmap")
	}
}

func TestPinsFullFailsClosed(t *testing.T) {
	now := time.Unix(0, 0)
	p := NewPins(func() time.Time { return now })
	for i := 0; i < MaxPins; i++ {
		if !p.Add(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), time.Minute) {
			t.Fatal("early full")
		}
	}
	x := netip.MustParseAddr("203.0.113.1")
	if p.Add(x, time.Minute) || p.Has(x) {
		t.Fatal("full table must refuse new pins")
	}
	now = now.Add(2 * time.Minute) // all expired -> reclaimed
	if !p.Add(x, time.Minute) || !p.Has(x) {
		t.Fatal("expired pins should be reclaimed")
	}
}

func TestPinsRetainByHost(t *testing.T) {
	p := NewPins(nil)
	a := netip.MustParseAddr("203.0.113.1") // shared by two names
	b := netip.MustParseAddr("203.0.113.2")
	p.AddHost(a, "a.example.com", time.Minute)
	p.AddHost(a, "B.example.com.", time.Minute)
	p.AddHost(b, "b.example.com", time.Minute)
	p.Retain(func(h string) bool { return h == "a.example.com" })
	if !p.Has(a) {
		t.Error("a still has an allowed name; must survive Retain")
	}
	if p.Has(b) {
		t.Error("b's only name was removed; must be dropped")
	}
	p.Retain(func(string) bool { return false })
	if p.Has(a) {
		t.Error("Retain(none) must empty the set")
	}
}

// TestDuplicatesCountedAndNotIndexed: duplicates (literal or spelled
// differently but canonically equal) count against MaxEntries and are
// merged into the index only once.
func TestDuplicatesCountedAndNotIndexed(t *testing.T) {
	dup := make([]string, MaxEntries+1)
	for i := range dup {
		dup[i] = "1.1.1.1"
	}
	if _, err := ParseEntries(dup); err == nil {
		t.Fatal("MaxEntries+1 copies of one entry must be refused")
	}
	if _, err := Parse(strings.Repeat("example.com,", MaxEntries+1)); err == nil {
		t.Fatal("MaxEntries+1 copies via Parse must be refused")
	}
	in := []string{"1.1.1.1", "1.1.1.1", "::ffff:1.1.1.1", "10.0.0.0/8", "10.0.0.0/8",
		"10.0.0.5-10.0.0.20", "10.0.0.5 - 10.0.0.20", "192.168.1.*", "192.168.1.*",
		"Example.COM", "example.com.", "*.Foo.com", "*.foo.com"}
	l, err := ParseEntries(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.1.1.1", "10.0.0.0/8", "10.0.0.5-10.0.0.20", "192.168.1.*", "example.com", "*.foo.com"}
	if got := l.Entries(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	if len(l.prefixes) != 3 || len(l.ranges) != 1 || len(l.hosts) != 1 || len(l.wild) != 1 {
		t.Fatalf("index not deduped: %d prefixes, %d ranges, %d hosts, %d wild",
			len(l.prefixes), len(l.ranges), len(l.hosts), len(l.wild))
	}
	if !l.AllowIP(netip.MustParseAddr("1.1.1.1")) || !l.AllowIP(netip.MustParseAddr("10.0.0.7")) ||
		!l.MatchHost("example.com") || !l.MatchHost("a.foo.com") || l.MatchHost("evil.com") {
		t.Fatal("deduped list lost semantics")
	}
	exact := make([]string, MaxEntries)
	for i := range exact {
		exact[i] = "1.1.1.1"
	}
	if _, err := ParseEntries(exact); err != nil {
		t.Fatalf("exactly MaxEntries entries must parse: %v", err)
	}
}
