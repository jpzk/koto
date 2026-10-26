package credproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// testPolicy is a minimal deny-by-default policy used across the Cred tests.
func testPolicy() *Policy {
	return &Policy{Secrets: []Secret{{
		Name:   "gh",
		Value:  "ghp_REALSECRETvalue1234567890",
		Groups: []string{"g1"},
		Hosts:  []string{"api.github.com"},
		Header: "Authorization",
		Scheme: "Bearer",
	}}}
}

func placeholderHeader(p *Policy) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+p.Secrets[0].Placeholder())
	return h
}

// Criterion 3a: a placeholder destined for a non-allowlisted host is NEVER
// substituted — the request is blocked and the real secret never materialises.
func TestCredPlaceholderNotSubstitutedOnNonAllowlistedHost(t *testing.T) {
	p := testPolicy()
	real := p.Secrets[0].Value

	req := Request{
		Group:       "g1",
		ConnectHost: "evil.example.com",
		SNI:         "evil.example.com",
		HostHeader:  "evil.example.com",
		Header:      placeholderHeader(p),
	}
	d := p.Evaluate(req)
	if d.Allow {
		t.Fatalf("expected non-allowlisted host to be denied, got Allow=true (%s)", d.Reason)
	}
	if !d.Block {
		t.Fatalf("expected Block=true for placeholder misuse, got %+v", d)
	}

	// Substitute must be a no-op for a non-allow decision.
	h := placeholderHeader(p)
	d.Substitute(h)
	if got := h.Get("Authorization"); strings.Contains(got, real) {
		t.Fatalf("real secret leaked into header on non-allowlisted host: %q", got)
	}
	if got := h.Get("Authorization"); !strings.Contains(got, p.Secrets[0].Placeholder()) {
		t.Fatalf("header should still carry only the placeholder, got %q", got)
	}

	// Positive control: the allowlisted host DOES substitute.
	okReq := Request{
		Group:       "g1",
		ConnectHost: "api.github.com",
		SNI:         "api.github.com",
		HostHeader:  "api.github.com",
		Header:      placeholderHeader(p),
	}
	od := p.Evaluate(okReq)
	if !od.Allow {
		t.Fatalf("expected allowlisted host to be allowed, got %+v", od)
	}
	oh := placeholderHeader(p)
	od.Substitute(oh)
	if got := oh.Get("Authorization"); got != "Bearer "+real {
		t.Fatalf("expected substituted header %q, got %q", "Bearer "+real, got)
	}
}

// Criterion 3b: a cross-host 3xx redirect strips the auth header and redacts
// the secret from Location so it cannot ride along to another origin.
func TestCredRedirectToOtherHostStripsSecret(t *testing.T) {
	p := testPolicy()
	real := p.Secrets[0].Value
	red := NewRedactor(p.Values())

	resp := &http.Response{
		StatusCode: http.StatusFound, // 302
		Header:     http.Header{},
	}
	resp.Header.Set("Authorization", "Bearer "+real)
	resp.Header.Set("Location", "https://attacker.example.net/callback?t="+real)

	SanitizeResponse(resp, "api.github.com", "Authorization", red)

	if got := resp.Header.Get("Authorization"); got != "" {
		t.Fatalf("auth header should be stripped on cross-host redirect, got %q", got)
	}
	loc := resp.Header.Get("Location")
	if strings.Contains(loc, real) {
		t.Fatalf("secret leaked in cross-host Location: %q", loc)
	}
	if !strings.Contains(loc, Replacement) {
		t.Fatalf("expected redacted Location to contain %q, got %q", Replacement, loc)
	}

	// Same-host redirect must NOT strip the auth header (it stays on the origin).
	resp2 := &http.Response{StatusCode: http.StatusFound, Header: http.Header{}}
	resp2.Header.Set("Authorization", "Bearer "+real)
	resp2.Header.Set("Location", "https://api.github.com/next")
	SanitizeResponse(resp2, "api.github.com", "Authorization", red)
	if resp2.Header.Get("Authorization") == "" {
		t.Fatalf("same-host redirect should keep the auth header")
	}
	// But the header value, like all headers, is still redacted.
	if strings.Contains(resp2.Header.Get("Authorization"), real) {
		t.Fatalf("secret not redacted from same-host auth header: %q", resp2.Header.Get("Authorization"))
	}
}

// Criterion 3c: the real secret value is redacted from a response BODY stream
// headed to the guest, including when the secret is split across read
// boundaries (carry/overlap logic).
func TestCredSecretRedactedFromResponseBody(t *testing.T) {
	p := testPolicy()
	real := p.Secrets[0].Value
	red := NewRedactor(p.Values())

	body := "prefix-data " + real + " middle " + real + "-suffix"

	// iotest.OneByteReader forces the stream through single-byte reads, which
	// guarantees the secret straddles read boundaries and exercises the carry.
	src := iotest.OneByteReader(strings.NewReader(body))
	out, err := io.ReadAll(red.Reader(src))
	if err != nil {
		t.Fatalf("read redacted stream: %v", err)
	}
	got := string(out)
	if strings.Contains(got, real) {
		t.Fatalf("secret survived streaming redaction: %q", got)
	}
	if n := strings.Count(got, Replacement); n != 2 {
		t.Fatalf("expected 2 redactions, got %d in %q", n, got)
	}
	if !strings.HasPrefix(got, "prefix-data ") || !strings.HasSuffix(got, "-suffix") {
		t.Fatalf("non-secret bytes corrupted: %q", got)
	}

	// Whole-buffer path too.
	if b := red.Bytes([]byte(body)); strings.Contains(string(b), real) {
		t.Fatalf("secret survived whole-buffer redaction: %q", string(b))
	}
}
