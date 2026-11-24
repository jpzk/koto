# How remote MCP servers authenticate (survey, 2026-11-17)

Method: live unauthenticated probes of each server's MCP endpoint (401 +
WWW-Authenticate), fetch of its RFC 9728 protected-resource metadata (PRM)
and RFC 8414 authorization-server (AS) metadata, plus the vendor's own docs.
Raw captures: notes/mcp-oauth/src/ in the workspace (probes.txt,
wellknown.txt, vendor docs). "Observed" = seen on the wire today;
"Documented" = vendor docs say so.

## Spec baseline

- MCP spec, Authorization, 2025-06-18:
  https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization
- MCP spec, Authorization, 2025-11-25:
  https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization
- Authorization is OPTIONAL; HTTP transports that do it SHOULD follow the
  spec; stdio servers take creds from env instead.
- Server = OAuth 2.1 resource server. On a missing/invalid token: 401 with
  `WWW-Authenticate: Bearer resource_metadata="<PRM URL>"` (RFC 9728),
  optionally `scope="..."`. Clients fall back to the well-known PRM URL.
- PRM lists `authorization_servers`; client fetches RFC 8414 (or OIDC)
  metadata from the AS.
- Client registration, 2025-11-25 priority: pre-registered > Client ID
  Metadata Documents (CIMD, `client_id_metadata_document_supported`) >
  Dynamic Client Registration RFC 7591 (now "MAY", kept for backwards
  compatibility) > ask the user. 2025-06-18 had DCR as SHOULD.
- Flow: authorization code + PKCE S256 (MUST). `resource` parameter
  (RFC 8707) MUST be sent on authorize and token requests; the server MUST
  validate the token was issued for it (audience) and MUST NOT pass the
  token through to upstream APIs.
- Scope errors: 403 + `WWW-Authenticate: Bearer error="insufficient_scope",
  scope="..."` (RFC 6750 3.1) -> client may step up. `client_credentials`
  clients (acting on their own behalf) are explicitly mentioned (step-up
  section) and static client credentials SHOULD be supported.

## Servers

1. GitHub remote MCP, https://api.githubcopilot.com/mcp/
   - Docs: https://github.com/github/github-mcp-server
   - Observed: 401 + resource_metadata; PRM -> AS
     https://github.com/login/oauth; AS metadata has authorization_code,
     refresh_token, device_code, PKCE S256. No registration_endpoint (no DCR):
     clients use a pre-registered GitHub/OAuth app.
   - Scopes: classic GitHub scopes (repo, read:org, gist, ...); tools are
     filtered by the scopes the token actually holds.
   - Documented: static PAT as `Authorization: Bearer <PAT>` is a
     first-class alternative.
2. Linear, https://mcp.linear.app/mcp
   - Docs: https://linear.app/docs/mcp
   - Observed: 401 + resource_metadata + scope="read write"; AS on same
     host; DCR (/register), CIMD supported, PKCE S256 only, refresh_token,
     jwt-bearer (ID-JAG).
   - Documented: "OAuth 2.1 with dynamic client registration. You can also
     authenticate directly with a bearer token or Linear API key"
     (restricted API keys for read-only).
3. Notion, https://mcp.notion.com/mcp
   - Docs: https://developers.notion.com/guides/mcp/overview
   - Observed: 401 + resource_metadata; DCR, CIMD, PKCE S256,
     refresh_token, introspection; single scope "default".
   - Documented: OAuth flow is the path for the hosted server; personal
     access tokens exist for the REST API / self-hosted server.
4. Stripe, https://mcp.stripe.com
   - Docs: https://docs.stripe.com/mcp
   - Observed: 401 + resource_metadata; AS https://access.stripe.com/mcp;
     DCR, public clients only (token_endpoint_auth_methods: none), PKCE
     S256, refresh_token, single scope "mcp".
   - Documented: OAuth for interactive clients; "agent API key" (restricted
     key) passed as Bearer for autonomous clients.
5. Sentry, https://mcp.sentry.dev/mcp
   - Docs: https://github.com/getsentry/sentry-mcp (sentry toolkit README)
   - Observed: 401 + resource_metadata; DCR, CIMD, PKCE S256,
     refresh_token. Fine-grained scopes (org:read, project:write,
     event:write, ...) that map onto tool groups.
   - Documented: stdio/self-hosted mode takes a user access token
     (SENTRY_ACCESS_TOKEN).
6. Atlassian, https://mcp.atlassian.com/v1/mcp
   - Docs: https://support.atlassian.com/atlassian-ai-gateway/docs/get-started-with-the-atlassian-remote-mcp-server/
   - Observed: 401 WITHOUT resource_metadata and no PRM (404); AS metadata
     at the root well-known; DCR, PKCE plain+S256, refresh_token. I.e. the
     older 2025-03-26 shape (clients guess AS from origin).
   - Documented: "Authentication via API token is also available as
     optional."
7. Asana, https://mcp.asana.com/v2/mcp
   - Docs: https://developers.asana.com/docs/using-asanas-mcp-server
   - Observed: 401 + resource_metadata; PRM -> AS https://app.asana.com
     (main product AS, not a dedicated MCP AS); scope "default".
   - Documented: PATs exist as bearer credentials for the platform.
8. Context7, https://mcp.context7.com/mcp
   - Docs: https://github.com/upstash/context7
   - Observed: anonymous requests get 200 (auth optional); PRM exists ->
     AS Clerk (clerk.context7.com) for the OAuth path.
   - Documented: "pass your API key via the Authorization: Bearer
     YOUR_API_KEY header" for higher rate limits.
9. Cloudflare workers-oauth-provider (framework behind many hosted MCP
   servers on Workers)
   - Docs: https://github.com/cloudflare/workers-oauth-provider
   - Implements OAuth 2.1 AS + RS for the Worker: PKCE, DCR, RFC 8414
     metadata, RFC 9728 PRM, token storage as hashes, refresh rotation.

## Findings

- Expected finding CONFIRMED with nuances: 7 of 8 hosted servers return
  401 + `resource_metadata` (RFC 9728); all 8 with OAuth expose RFC 8414 AS
  metadata; PKCE S256 is universal.
- DCR is the dominant registration path (Linear, Notion, Stripe, Sentry,
  Atlassian, Cloudflare). GitHub is the exception: pre-registered client
  only. CIMD (new in 2025-11-25) is already advertised by Linear, Notion,
  Sentry.
- Refresh tokens: every AS offers refresh_token. Rotation is
  implementation-specific (Cloudflare documents rotation).
- Static bearer alternative is common: GitHub (PAT), Linear (API key),
  Stripe (agent API key), Atlassian (API token), Context7 (API key). So a
  gateway needs BOTH modes.
- Scopes: mostly coarse (Notion/Asana "default", Stripe "mcp", Linear
  read/write); GitHub and Sentry are fine-grained and use scopes to filter
  tools. tools/list filtering by scope is the norm where scopes are
  fine-grained; the spec's runtime answer is 403 insufficient_scope.
- `client_credentials`: NOT offered by any of the surveyed hosted AS
  (all are user-delegated). It is only relevant for machine clients and
  pre-registered relationships; the spec mentions it.

## Design consequences for koto (gateway = host-side OAuth client)

- Implement the spec discovery chain (401 -> PRM -> AS metadata, with
  well-known fallback for Atlassian-style servers).
- Register via DCR when `registration_endpoint` exists; allow a
  pre-registered client_id/secret otherwise (GitHub). CIMD: later.
- Onboarding grant: auth code + PKCE S256 (what real servers offer), with
  `resource=<server URL>`; client_credentials supported for machine ASes
  and the test server.
- Keep refresh token + client secret host-side (0600 registry store),
  refresh on expiry/401, handle rotation.
- Keep the existing static-header mode as the "PAT / API key" mode.
- Test server (tools/mcp-adder): 401 + resource_metadata, PRM, AS
  metadata, DCR, code+PKCE, client_credentials, refresh rotation,
  aud-bound tokens, 403 insufficient_scope for the privileged tool
  (spec behavior), plus static-PAT mode.
