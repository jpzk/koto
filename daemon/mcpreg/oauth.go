package mcpreg

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// OAuth grant types the gateway can run unattended against an upstream AS.
const (
	// GrantClientCredentials: confidential client, the daemon mints access
	// tokens on demand with client_id/secret (RFC 6749 §4.4).
	GrantClientCredentials = "client_credentials"
	// GrantRefreshToken: onboarding ran authorization_code + PKCE once and
	// stored the refresh token; the gateway refreshes (rotating) from then on.
	GrantRefreshToken = "refresh_token"
)

// OAuth is an upstream OAuth 2.1 client registration (MCP authorization
// spec). ClientSecret and RefreshToken are secrets: like AuthHeader.Value
// they only ever travel daemon -> authorization server, never into a guest,
// a log or a CLI view. Access tokens are NEVER persisted (gateway memory).
type OAuth struct {
	Grant         string `json:"grant"`
	Resource      string `json:"resource"`       // RFC 8707 resource = canonical server URL
	Issuer        string `json:"issuer"`         // AS issuer (from PRM authorization_servers)
	TokenEndpoint string `json:"token_endpoint"` // from RFC 8414 metadata
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret,omitempty"`
	AuthMethod    string `json:"auth_method,omitempty"` // client_secret_basic|client_secret_post|none
	Scope         string `json:"scope,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
}

// String redacts the secrets so a stray %v never leaks them.
func (o OAuth) String() string {
	return fmt.Sprintf("oauth{grant=%s client_id=%s scope=%q resource=%s secret=[redacted] refresh=[redacted]}",
		o.Grant, o.ClientID, o.Scope, o.Resource)
}

// GoString redacts too (%#v).
func (o OAuth) GoString() string { return o.String() }

// Redact blanks the secret values (for views), keeping presence visible.
func (o *OAuth) Redact() {
	if o.ClientSecret != "" {
		o.ClientSecret = "[redacted]"
	}
	if o.RefreshToken != "" {
		o.RefreshToken = "[redacted]"
	}
}

func badSecret(s string) bool { return strings.ContainsAny(s, "\r\n\x00") }

func validateOAuth(o *OAuth) error {
	switch o.Grant {
	case GrantClientCredentials:
		if o.ClientSecret == "" {
			return errors.New("client_credentials needs a client_secret")
		}
	case GrantRefreshToken:
		if o.RefreshToken == "" {
			return errors.New("refresh_token grant needs a refresh_token")
		}
	default:
		return fmt.Errorf("unsupported oauth grant %q", o.Grant)
	}
	switch o.AuthMethod {
	case "", "client_secret_basic", "client_secret_post", "none":
	default:
		return fmt.Errorf("unsupported oauth auth_method %q", o.AuthMethod)
	}
	if o.AuthMethod == "none" && o.Grant == GrantClientCredentials {
		return errors.New("client_credentials needs a confidential client")
	}
	if o.ClientID == "" || badSecret(o.ClientID) {
		return errors.New("oauth client_id empty or contains control characters")
	}
	if badSecret(o.ClientSecret) || badSecret(o.RefreshToken) || badSecret(o.Scope) {
		return errors.New("oauth secret/scope contains control characters") // never echo
	}
	for _, s := range []string{o.Resource, o.Issuer, o.TokenEndpoint} {
		if err := ValidateURL(s); err != nil {
			return fmt.Errorf("oauth url: %w", err)
		}
	}
	tu, _ := url.Parse(o.TokenEndpoint)
	if tu.Scheme != "https" && !IsLoopback(tu.Hostname()) {
		return errors.New("oauth token_endpoint must be https (http only on loopback)")
	}
	return nil
}

// IsLoopback reports whether host is a loopback name/address.
func IsLoopback(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasPrefix(h, "127.")
}
