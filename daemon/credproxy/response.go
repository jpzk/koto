package credproxy

import (
	"net/http"
	"strings"
)

// SanitizeResponse scrubs a response before it is relayed to the guest:
//   - every secret value is redacted from all response headers (incl. Location),
//   - on a cross-host redirect, request-auth echo headers are stripped so the
//     secret can never be re-attached to a hop outside the allowlisted host
//     (design §8). The body must be wrapped with r.Reader by the caller.
//
// currentHost is the allowlisted host this response came from (the CONNECT
// authority). authHeader is the header name the policy rewrites (e.g.
// "Authorization"); it is removed from the response on a cross-host 3xx.
func SanitizeResponse(resp *http.Response, currentHost, authHeader string, r *Redactor) {
	if resp == nil {
		return
	}
	if r != nil {
		r.Header(resp.Header)
	}
	if isRedirect(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		if loc != "" && redirectHost(loc) != "" && !sameHost(redirectHost(loc), currentHost) {
			// Cross-host redirect: never let an auth-bearing header ride along,
			// and make sure the (already redacted) Location carries no secret.
			if authHeader != "" {
				resp.Header.Del(authHeader)
			}
			if r != nil {
				resp.Header.Set("Location", r.String(loc))
			}
		}
	}
}

func isRedirect(code int) bool { return code >= 300 && code < 400 }

func sameHost(a, b string) bool {
	return strings.EqualFold(hostOf(a), hostOf(b))
}

// redirectHost extracts the host from a Location value. Relative Locations (no
// scheme/host) return "" => treated as same-host.
func redirectHost(loc string) string {
	loc = strings.TrimSpace(loc)
	i := strings.Index(loc, "://")
	if i < 0 {
		return "" // relative redirect, same host
	}
	rest := loc[i+3:]
	// authority ends at first '/', '?', or '#'
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '/', '?', '#':
			return hostOf(rest[:j])
		}
	}
	return hostOf(rest)
}
