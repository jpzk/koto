package main

// mcp_view projection for what this branch adds to a connection: the
// enable/disable flag and OAuth auth. Only the grant TYPE and the gateway's
// token health are exposed — never the client id or secret, the refresh or
// access token, the issuer or the token endpoint (mcp_view.go).

import "koto/mcpreg"

func init() {
	mcpViewEnabled = func(c *mcpreg.Conn) bool { return !c.Disabled }
	mcpViewAuthExt = func(name string, c *mcpreg.Conn) (mcpVAuth, bool) {
		if c.OAuth == nil {
			return mcpVAuth{}, false
		}
		g := c.OAuth.Grant
		switch g {
		case mcpreg.GrantClientCredentials, mcpreg.GrantRefreshToken:
		default:
			g = "other"
		}
		return mcpVAuth{Kind: "oauth", Grant: g, Status: mcpGateway().AuthStatus(name)}, true
	}
}
