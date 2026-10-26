// Package credproxy implements koto's universal credentials proxy: a host-side
// HTTPS MITM that swaps guest-held placeholder tokens (KOTO_CRED_<name>) for the
// real secret ONLY when (group, dest host, header) matches a deny-by-default
// policy, and redacts the real secret out of responses flowing back to the guest.
//
// Stdlib only. See notes/credproxy/design.md for the design + threat model.
package credproxy

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// PlaceholderPrefix is the literal prefix of every placeholder a guest holds.
// For a secret named "gh" the guest carries the string "KOTO_CRED_gh".
const PlaceholderPrefix = "KOTO_CRED_"

// Secret is one credential entry: the real value lives host-only; the guest
// only ever sees Placeholder().
type Secret struct {
	Name   string   `json:"name"`   // placeholder = PlaceholderPrefix + Name
	Value  string   `json:"value"`  // real secret, host-only, never sent to guest
	Groups []string `json:"groups"` // exact group names allowed (no wildcard)
	Hosts  []string `json:"hosts"`  // exact dest hostnames (no wildcard by default)
	Header string   `json:"header"` // header to rewrite, e.g. "Authorization"
	Scheme string   `json:"scheme"` // e.g. "Bearer", "Basic", "" for raw value
	// SubstituteIn: default ["header"]. "query"/"body" are opt-in, higher risk.
	SubstituteIn []string `json:"substitute_in,omitempty"`
}

// Placeholder returns the literal string the guest holds for this secret.
func (s Secret) Placeholder() string { return PlaceholderPrefix + s.Name }

// substitutesHeader reports whether header substitution is enabled (the default).
func (s Secret) substitutesHeader() bool {
	if len(s.SubstituteIn) == 0 {
		return true // default ["header"]
	}
	for _, loc := range s.SubstituteIn {
		if loc == "header" {
			return true
		}
	}
	return false
}

// Policy is the full host-side credential policy.
type Policy struct {
	Secrets []Secret `json:"secrets"`
}

// LoadPolicy reads and parses a policy JSON file. The caller is responsible for
// ensuring the file is 0600 host-only (see design §3/§4).
func LoadPolicy(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Values returns every known real secret value (for the redactor to scan for).
func (p *Policy) Values() []string {
	out := make([]string, 0, len(p.Secrets))
	for _, s := range p.Secrets {
		if s.Value != "" {
			out = append(out, s.Value)
		}
	}
	return out
}

// findByPlaceholder returns the secret whose placeholder equals the given string.
func (p *Policy) findByPlaceholder(ph string) *Secret {
	for i := range p.Secrets {
		if p.Secrets[i].Placeholder() == ph {
			return &p.Secrets[i]
		}
	}
	return nil
}

// Request captures the per-request facts the policy decision needs. ConnectHost
// is the authority from the CONNECT line (the policy anchor); SNI and HostHeader
// are cross-checked against it (design §7).
type Request struct {
	Group       string
	ConnectHost string
	SNI         string
	HostHeader  string
	Header      http.Header
}

// Decision is the outcome of evaluating a request against the policy.
type Decision struct {
	Allow  bool    // true => substitute the secret and forward
	Block  bool    // true => do NOT forward (a placeholder was misused)
	Reason string  // audit reason, never contains the secret value
	Secret *Secret // the matched secret when Allow is true
}

// hostMatches reports whether dest matches an allowlist entry. Exact match by
// default; an opt-in "*.suffix" entry matches subdomains (discouraged, §4).
func hostMatches(allow, dest string) bool {
	allow = strings.ToLower(strings.TrimSpace(allow))
	dest = strings.ToLower(strings.TrimSpace(dest))
	if allow == dest {
		return true
	}
	if strings.HasPrefix(allow, "*.") {
		suf := allow[1:] // ".suffix"
		return strings.HasSuffix(dest, suf) && len(dest) > len(suf)
	}
	return false
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// hostOf strips any :port from a host[:port] authority.
func hostOf(authority string) string {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return ""
	}
	// IPv6 literal in brackets: [::1]:443
	if strings.HasPrefix(authority, "[") {
		if i := strings.Index(authority, "]"); i >= 0 {
			return authority[1:i]
		}
	}
	if i := strings.LastIndex(authority, ":"); i >= 0 {
		// only treat as port if the remainder is a plausible port (no other colon)
		if !strings.Contains(authority[:i], ":") {
			return authority[:i]
		}
	}
	return authority
}

// Evaluate applies the deny-by-default matching rules (design §4 and §7). It
// never mutates the request; the caller performs substitution when Allow is set.
//
// Rules, ALL must hold for Allow:
//  1. A known placeholder appears in the configured header.
//  2. Group is in the secret's Groups.
//  3. ConnectHost matches one of the secret's Hosts exactly.
//  4. SNI == ConnectHost and HostHeader == ConnectHost (name consistency).
//
// If a placeholder is present but any check fails, the result is Block (the
// request is refused, not silently forwarded).
func (p *Policy) Evaluate(req Request) Decision {
	connHost := hostOf(req.ConnectHost)
	sni := hostOf(req.SNI)
	host := hostOf(req.HostHeader)

	// Scan every configured header for a known placeholder.
	for i := range p.Secrets {
		s := &p.Secrets[i]
		ph := s.Placeholder()
		hv := req.Header.Get(s.Header)
		if hv == "" || !strings.Contains(hv, ph) {
			continue
		}
		if !s.substitutesHeader() {
			return Decision{Block: true, Reason: "placeholder in header but header substitution disabled for " + s.Name}
		}
		// Placeholder found. Now it must clear every gate or we BLOCK.
		if !containsStr(s.Groups, req.Group) {
			return Decision{Block: true, Reason: "group not allowed for secret " + s.Name, Secret: s}
		}
		matched := false
		for _, h := range s.Hosts {
			if hostMatches(h, connHost) {
				matched = true
				break
			}
		}
		if !matched {
			return Decision{Block: true, Reason: "dest host not allowlisted for secret " + s.Name, Secret: s}
		}
		// Name-consistency: SNI and Host header must equal the CONNECT authority.
		if sni != "" && sni != connHost {
			return Decision{Block: true, Reason: "SNI/CONNECT host mismatch", Secret: s}
		}
		if host != "" && host != connHost {
			return Decision{Block: true, Reason: "Host/CONNECT host mismatch", Secret: s}
		}
		return Decision{Allow: true, Reason: "allow " + s.Name + " -> " + connHost, Secret: s}
	}
	return Decision{Reason: "no placeholder present"}
}

// headerValueFor returns the header value the proxy should set upstream after
// substituting the placeholder with the real secret value.
func (s *Secret) headerValueFor(current string) string {
	real := s.Value
	if s.Scheme != "" {
		real = s.Scheme + " " + s.Value
	}
	// Replace just the placeholder token (and any scheme the guest put in front
	// is overwritten by the full computed value).
	if s.Scheme != "" {
		return real
	}
	return strings.ReplaceAll(current, s.Placeholder(), real)
}

// Substitute rewrites the configured header of req in place with the real secret
// for the given decision. No-op unless d.Allow.
func (d Decision) Substitute(h http.Header) {
	if !d.Allow || d.Secret == nil {
		return
	}
	cur := h.Get(d.Secret.Header)
	h.Set(d.Secret.Header, d.Secret.headerValueFor(cur))
}
