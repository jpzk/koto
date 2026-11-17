# MCP connection registry

The daemon keeps a registry of named MCP connections. One connection (one
upstream URL and one credential) can be shared by many groups. The operator
decides which groups may use it and what each of them may do with it. Guests
never see the upstream URL's credential: they talk to a gateway on the host,
and the gateway forwards only what the group is allowed to do.

Source: `daemon/mcpreg` (registry, validation, effective permissions),
`daemon/mcp_admin.go` (admin RPC), `daemon/mcp_cli.go` (`koto ctl mcp`),
`daemon/mcpgw` (gateway).

Scope of this release: remote streamable-HTTP connections, a static auth
header or an OAuth 2.1 client (see [OAuth upstreams](#oauth-upstreams)),
read-class connections, calls decided automatically by policy.
Parked approvals, interactive (browser) OAuth consent and stdio servers
are not supported yet.

## Model

Three layers. Each layer can only narrow the layer above it.

1. **Connection ceiling** — set when the connection is added. It lists
   the tools anyone may ever use through this connection, with optional
   argument rules per tool, and a class (`read` or `write`).
2. **Group whitelist** — the groups allowed to use the connection. A group
   that is not on the list gets nothing: no tools in `tools/list`, and every
   request answers 404, the same answer as for a connection that does not
   exist (`mcpreg.ErrNotWhitelisted`). Deny is the default.
3. **Group grant** — for each whitelisted group, the tools from the ceiling
   that this group may call, with optional extra argument rules and an
   optional rate limit (calls per minute).

The permission a group actually gets is the intersection:

    effective(conn, group) = ceiling(conn) ∩ grant(conn, group)

Rules (`mcpreg.Registry.Effective`, `mcpreg.Effective.Check`):

- A tool is effective only if it is in the ceiling AND in the grant.
- Argument rules are combined with AND: a call must pass the ceiling's rules
  and the grant's rules. A grant cannot loosen a ceiling rule, and setting
  `additional_args` in a grant does not allow arguments the ceiling refuses.
- `koto ctl mcp grant` refuses a tool that is not in the ceiling. If the
  ceiling later shrinks (the connection is re-added with fewer tools), old
  grants for removed tools stay in the file but never become effective.
- A connection of class `write` gives an empty effective set in this
  release, because write calls need operator approval, which is a
  follow-up.
- Whitelisting a group (`allow`) gives it no tools. Tools come only from
  `grant`.

Example: the ceiling of `docs` is {search, fetch, delete}. Group `alpha` is
granted {search, fetch}; group `beta` is granted {search}; group `gamma` is
not whitelisted.

    effective(docs, alpha) = {search, fetch}
    effective(docs, beta)  = {search}
    effective(docs, gamma) = {}   (404, no tools)

No group can reach `delete`, whatever its grant says.

## Operator commands

All `koto ctl mcp` verbs go through one admin-only RPC (`Mcp`, listed in
`adminOnlyVerbs` in `daemon/acl.go`). Any other role is refused.

    koto ctl mcp ls
    koto ctl mcp show <conn>
    koto ctl mcp add <conn> --url <https://...> --tools a,b \
        [--class read|write] [--rules <file.json>] \
        [--auth-file <file>|- | --oauth client_credentials|authorization_code \
         [--oauth-scope "a b"] [--oauth-client-file <file>|-]]
    koto ctl mcp rm <conn>
    koto ctl mcp disable <conn>
    koto ctl mcp enable  <conn>
    koto ctl mcp allow <conn> <group>
    koto ctl mcp deny  <conn> <group>
    koto ctl mcp grant  <conn> <group> <tool> [--args '<json>']
    koto ctl mcp revoke <conn> <group> <tool>
    koto ctl mcp rate <conn> <group> <per_minute>    (0 clears)
    koto ctl mcp effective <group> [<conn>]

Notes:

- `--auth-file` reads one line `Header-Name: value` from a file, or from
  stdin with `-`. The value is never accepted on the command line, so it
  does not end up in shell history or process listings.
- Re-running `add` on an existing connection replaces URL, ceiling and
  class but keeps the whitelist, grants and the stored auth header (unless
  a new `--auth-file` is given).
- `deny` removes the group and all its grants. `rm` removes the
  connection with all of its groups.
- `show`, `ls` and `effective` never print the auth header value; it is
  shown as `[redacted]`.


## OAuth upstreams

Most hosted MCP servers protect their endpoint with OAuth 2.1 as described
in the MCP authorization spec
(<https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization>).
A connection can use an OAuth client instead of a static header:
`--oauth` and `--auth-file` cannot be combined. The surveyed servers and
how they authenticate are listed in `notes/mcp-oauth/auth-survey.md`.

    koto ctl mcp add adder --url http://127.0.0.1:8931/mcp --tools add \
        --oauth client_credentials --oauth-scope mcp:add
    koto ctl mcp add adder --url https://mcp.example.com/mcp --tools add \
        --oauth authorization_code --oauth-scope mcp:add

When you run `add`, the daemon (host side, `mcpgw.Onboard`) does the
following:

1. Discovery. It sends an unauthenticated `initialize` and expects a 401.
   It reads `resource_metadata` from `WWW-Authenticate`, falling back to
   the RFC 9728 well-known paths. It then fetches the protected-resource
   metadata, takes the first `authorization_servers` entry and fetches its
   RFC 8414 metadata (falling back to OIDC). The PRM `resource` must equal
   `--url`, and the metadata `issuer` must equal the issuer that was asked
   for. Every discovered endpoint must be `https`; plain `http` is allowed
   on loopback only.
2. Client. It registers a client with RFC 7591 dynamic client
   registration. For `client_credentials` this is a confidential client
   using `client_secret_basic`. For `authorization_code` it is a public
   client with PKCE and redirect `http://127.0.0.1:8976/koto/oauth/callback`.
   If the AS has no `registration_endpoint` (GitHub, for example), pass a
   pre-registered client with `--oauth-client-file`, a file (or `-` for
   stdin) holding `{"client_id":"..","client_secret":".."}`.
3. First grant. Every token request carries `resource=<url>` (RFC 8707),
   so the AS binds the token's audience to this server.
   - `client_credentials`: one token is minted to prove the client works.
     The client secret is stored.
   - `authorization_code`: PKCE S256 with `state` and an RFC 9207 `iss`
     check. This is **headless**: the AS must approve without a browser
     and answer with a redirect, as the bundled test AS and pre-consented
     clients do. The refresh token is stored, and from then on the
     connection's grant is `refresh_token`. Interactive consent is a
     follow-up.

In the registry (`conns.<name>.oauth`) the stored fields are `grant`,
`resource`, `issuer`, `token_endpoint`, `client_id`, `auth_method`,
`scope`, and the secrets `client_secret` / `refresh_token`. Access tokens
are never written to disk.

At call time, the gateway (`mcpgw.Gateway.bearer`) does this:

- It mints an access token on first use: `client_credentials`, or a
  refresh. The token is cached in memory per upstream session and renewed
  10s before `expires_in`.
- When the upstream returns 401 (expired or revoked token), it discards
  the cached token, gets a new one, and retries once.
- Refresh rotation: if the AS returns a new refresh token, it is written
  to the registry with compare-and-swap (`Registry.RotateRefresh`) before
  the new access token is used. A stale writer cannot overwrite a newer
  token, and the consumed token is never replayed. If the new refresh
  token cannot be persisted, the call fails closed.
- When the upstream returns 403 with `error="insufficient_scope"`, koto
  has allowed the call but the server's scopes have not. The guest gets a
  **tool error** (`isError: true`, text
  `upstream refused: insufficient_scope (needs mcp:admin)`), not a
  transport failure, and the audit line records the reason. The two
  layers stay independent: koto's grant hides and refuses tools the group
  was not granted, and the AS scope limits what the token can do even for
  granted tools.

The guest-facing side is unchanged: the guest talks to the gateway exactly
as before and never sees a token. No token, secret or refresh token
appears in any gateway response, error or audit entry
(`TestOAuthNoTokenInGuestBytes`). `show` lists the non-secret oauth fields
by whitelist and has no secret key at all (`TestMcpAdminOAuthAddViewsRedacted`).
Errors from the AS give the HTTP status and the RFC 6749 `error` code only,
never the response body.

`tools/mcp-adder` is a real OAuth-protected test server with a bundled TEST
authorization server (DCR, auth code + PKCE, client_credentials, refresh
rotation, aud-bound ES256 JWTs) and a static-PAT mode.
`scripts/e2e-mcp-oauth.sh` runs it against a private daemon with a real
model.

## Deactivating a connection

    koto ctl mcp disable <conn>
    koto ctl mcp enable  <conn>

`disable` switches a connection off without forgetting it. The ceiling, the
stored auth header, the whitelist and every grant stay in the registry, but
from the next request on the gateway treats the connection exactly as if it
did not exist: every request answers 404, `tools/list` is empty, and the
connection is left out of the harness configuration of every group
(`mcpreg.Registry.Effective`, `mcpreg.Registry.ConnsFor`). `enable` restores
the access each group had before.

- The flag is stored as `"disabled": true` on the connection. Registry
  files written before the flag existed have no such field and load as
  enabled.
- Re-running `add` on a disabled connection keeps it disabled.
- `ls` and `show` report `enabled`. `effective` still lists a disabled
  connection for the operator, with empty `tools` (what the gateway
  enforces) and `tools_when_enabled` (the grants it keeps,
  `mcpreg.Registry.Preview`).

## Argument rules

`--rules` (ceiling, per tool) and `--args` (grant, one tool) use the same
format, a small subset of JSON Schema:

    {"search": {"args": {
        "query": {"type": "string", "maxLength": 200},
        "limit": {"type": "integer", "minimum": 1, "maximum": 50}
    }}}

Supported keywords per argument: `type` (string, number, integer, boolean),
`const`, `enum`, `pattern` (RE2, always anchored as the whole value),
`maxLength`, `minimum`, `maximum`. An argument that has no rule is refused
unless `additional_args` is true for that tool (`mcpreg.ToolRule`).

The two layers have different defaults on purpose (`mcpreg.EffTool`):

- A ceiling tool listed in `--tools` without a rule may only be called
  with no arguments. To let a tool take any arguments, say so explicitly:
  `{"fetch": {"additional_args": true}}`.
- A grant without `--args` adds no further narrowing: the group gets the
  ceiling rule for that tool as it is.

## Who can change what

- Only the operator (admin role) can add connections, whitelist groups and
  grant tools.
- `mcp` is not a group config key on any path. `koto ctl config ... mcp=`
  is refused (`daemon/config.go`), and the in-guest ctl plane refuses an
  `mcp` posture key (`configPostureKey` in `daemon/ctl.go`). An agent cannot
  grant itself access, and `main` cannot grant access to its peers.

## TUI

The TUI drives the same admin RPC (`Mcp`) as `koto ctl mcp`; it has no RPC
of its own.

### MCP view

Open it with `alt+m` or `/mcp`; `esc` or `alt+m` closes it. It is also in
the command palette (`ctrl+p`) and in the help page (`ctrl+h`). Source:
`tui/mcp_view.go`, `tui/mcp_actions.go`.

One row per connection, with these columns:

- `ON`: enabled or disabled.
- `NAME`: the connection name.
- `AUTH`: `set` or `unset`. The view never gets the value.
- `CLASS`: `read` or `write`.
- `GRP`: how many groups are whitelisted.
- `HOST`: the host of the upstream URL. Path and query are not shown.
- `TOOLS`: the ceiling tools.

Keys in the list:

- `up`/`down` moves the selection and `g` refreshes.
- `enter` opens the detail: the whitelist, the grants of each group, and
  the effective set as the daemon reports it (`effective` for each
  whitelisted group; the TUI does not work it out itself). In the detail,
  `tab` selects a whitelisted group and `e` opens its MCP access dialog
  (see below). There, `ctrl+s` applies the changes at once and re-reads
  the detail.
- `a` opens the add form: name, URL, tools, class, optional rules JSON,
  the auth header name, and the auth value. By default the value is read
  from a file path. The file is read on the TUI host and must be a
  regular file, no larger than 8 KiB, with no group or other permission
  bits (0600 or stricter); a single trailing newline is dropped. `ctrl+t`
  switches to typing the value instead, in a field that draws nothing and
  shows only `[entered, hidden]` or `[empty]`.
- `t` disables or enables the selected connection (see "Deactivating a
  connection").
- `d` deletes it, but only after you type the connection name, the same
  way group destroy asks for confirmation.

### Group MCP access (new / edit group)

In the new group dialog (`ctrl+n`) and the edit group dialog (`ctrl+e`),
`alt+m` opens the group MCP access dialog (`tui/group_mcp.go`):

- `space` on a connection allows it for the group or takes it away.
  Disabled connections are greyed out, and their preview reads "(when
  enabled)".
- Under an allowed connection, only the tools in that connection's
  ceiling are offered. `space` grants or revokes a tool; `enter` sets an
  optional argument rule as JSON (`args` / `additional_args`, see
  "Argument rules").
- The rate row sets calls per minute (empty or 0 means no rate limit).
- A live preview shows the effective set (ceiling ∩ grant) and how many
  changes are pending. A write-class connection shows no effective tools,
  the same as `mcpreg.Registry.Effective`.
- `ctrl+s` keeps the plan in the parent dialog. `esc` throws it away.

Nothing is sent until the parent dialog is submitted. The plan is sent as
the usual `allow`, `revoke`, `grant`, `rate` and `deny` ops:

- New group: the ops are sent after the group has been created. If the
  create fails, no MCP op is sent. If an MCP op fails, the TUI denies
  every connection it allowed, shows the error, and the group is left
  without MCP grants.
- Edit group: the ops are sent after the config changes. Save is
  available even when the only change is MCP access.

### Non-admin sessions

The registry is admin-only in the daemon (`adminOnlyVerbs` in
`daemon/acl.go`), whatever the client does. A TUI connected with a
non-admin client certificate gets PermissionDenied on `ls`. The view then
shows that the registry is admin-only and is read-only: `a`, `t`, `d` and
`e` are refused in the TUI and send nothing. That check only spares a
round trip; the daemon refuses the request anyway.

## Threat notes

- The auth value only ever goes one way, into the daemon. `ls` sends a
  set/unset bool and `show` sends the header name only. Neither response
  has a value field, not even a `[redacted]` placeholder
  (`mcpViewConn`, tested on the serialized response bytes in
  `TestMcpRespBytesCarryNoSecret`).
- In the TUI the value is held as a byte slice only until it is sent. The
  input fields are reset at submit, and the bytes are zeroed after the
  RPC returns, whether it succeeded or not. If the daemon refuses the add,
  the form stays open with the secret gone, and you have to enter it
  again. The value is never drawn, put in the history, or included in an
  error message (file errors give the path and mode only), and the form's
  `String`/`GoString` are redacted. A Go string copy inside the request
  can stay in memory until garbage collection; that is the limit of what
  the TUI can promise.
- The TUI's admin check is cosmetic. Every MCP op is refused for a
  non-admin identity in `authUnary`, before the handler runs
  (`TestMcpNonAdminRefusedEveryOp`).

## Storage

The registry lives at `<KOTO_HOME>/creds/mcp/registry.json`
(`mcpRegistryPath`), file mode 0600 in a 0700 directory.

- Writes are atomic: temporary file, fsync, rename (`mcpreg.writeAtomic`).
- The file is opened with `O_NOFOLLOW`; a symlinked file or directory, or a
  file readable by group or others, is refused.
- Unknown fields and duplicate keys are rejected on load.
- If the file stops parsing (for example after a bad manual edit), the
  daemon keeps using the last good version and reports the error; a
  missing file means an empty registry (nothing allowed).
- The auth header value, OAuth client secret and refresh token are never
  written to logs or audit lines. OAuth access tokens are never written
  to disk at all.

## Enforcement (gateway)

Each group reaches MCP only through a host-side gateway (`daemon/mcpgw`).
Inside the guest the endpoint is `http://127.0.0.1:18889/mcp/<conn>`; the
guest agent relays it over vsock to the host, which knows which group the
VM belongs to. The guest never chooses its own group name.

For every request the gateway:

1. reads the current registry (no cache, so a `revoke` or `deny` applies
   to the next request);
2. answers 404 if the group is not whitelisted for the connection or the
   connection is disabled;
3. on `tools/list`, returns only the effective tools;
4. on `tools/call`, refuses tools outside the effective set, arguments
   that fail the ceiling or grant rules, and calls over the rate limit;
5. adds the auth header only on the host-to-upstream request;
6. writes one audit line (group, connection, method, tool, allowed or
   refused, reason, duration). Arguments, results and credentials are not
   logged.

All groups on one connection share one upstream MCP session.

Tool schema pinning: the first time the gateway sees a tool's definition
during a daemon run, it records a hash. If the upstream later changes that
definition, the tool is hidden and refused until the daemon restarts or the
connection is re-added. This protects against a server that swaps a tool's
description or schema after it was approved.

### Turning it on

The gateway is off by default. Start the daemon with
`KOTO_MCP_GATEWAY=1` to enable it. While it is off, the registry can
still be edited, but no group can reach any connection: the daemon opens
no MCP listener and passes no MCP servers to the harness.

When it is on, each running group gets its own gateway listener on
vsock port 9005. The guest agent bridges `127.0.0.1:18889` to that port.
A group started before you set the flag gets its listener on its next
restart.

### Harness configuration

For every turn the daemon sends only the names of the connections the
group is whitelisted on. The guest checks each name again and points every
server at the in-guest bridge, `http://127.0.0.1:18889/mcp/<conn>`. The
upstream URL and auth header are never part of any harness config, for any
harness. A turn with no grants gets no MCP configuration at all, and the
harness runs exactly as before.

claude:

- The guest passes
  `--mcp-config '{"mcpServers":{"<conn>":{"type":"http","url":"http://127.0.0.1:18889/mcp/<conn>"}}}'`
  and `--strict-mcp-config`, so an `.mcp.json` in the workspace cannot add
  servers the operator never granted.

codex (0.169.0):

- Each server is a runtime override:
  `-c mcp_servers.<conn>.url="http://127.0.0.1:18889/mcp/<conn>"` plus
  `-c mcp_servers.<conn>.default_tools_approval_mode="approve"`.
- `codex exec` (and `exec resume`) gets `--ignore-user-config`, so a
  `$CODEX_HOME/config.toml` written by the agent (CODEX_HOME is under
  `/workspace`) is not read. Login and thread history still live in
  CODEX_HOME, so resume keeps working.
- A project `.codex/config.toml` is never loaded: codex only loads it for a
  trusted directory, and with the user config ignored and no `--trust`
  nothing can mark the workspace trusted.
- `-c features.plugins=false`, `-c features.apps=false` and
  `-c features.skill_mcp_dependency_install=false` switch off the three
  features that would add servers of their own.
- Together these leave the `-c` overrides as the only source of MCP
  servers, which is the codex equivalent of `--strict-mcp-config`.
- `approve` is needed because koto runs codex with approval policy Never,
  under which a tool that asks for approval is denied outright. The
  gateway decides each call instead.
- Context compaction (`codex app-server`) gets no MCP overrides and has no
  `--ignore-user-config` flag. The compaction turn exposes no tools to the
  model. A server the agent planted in its own config could still be
  connected during compaction, but it would run only with rights the agent
  already has, and it can never reach a koto connection without going
  through the gateway.

pi (1.5.2):

- MCP is pi's built-in `builtin:mcp` extension, shipped in the pinned
  package. koto keeps `--no-extensions` and adds only `-e builtin:mcp`, and
  only on turns that have a grant. No other extension, skill or package
  loads.
- Before the agent starts, koto writes `mcp.json` into the fresh per-turn
  agent directory (`PI_CODING_AGENT_DIR`), with one
  `{"type":"http","url":"http://127.0.0.1:18889/mcp/<conn>","exposure":"direct"}`
  entry per granted connection. That directory is created new for each
  turn and removed afterwards, so nothing the agent writes there carries
  over to the next turn.
- A project `.pi/mcp.json` needs project trust. koto runs pi with
  `--no-approve`, which ignores trust-gated project configuration, and the
  trust store lives in the fresh agent directory, so no saved trust exists.
- pi treats names that differ only in `-` and `_` as the same server. If
  two granted connections collide this way, the first in sorted order is
  used.

Restricted modes and the gateway:

- plan and confirm modes exist only for claude. codex and pi always run in
  yolo mode: the harness does not ask before calling a tool.
- This does not widen MCP access. The gateway is the enforcement point for
  every harness. It decides on each request which tools are listed and
  which calls are allowed, using the group's grants, ceilings and rate
  limits. A harness, in any mode, can only reach the connections the
  gateway lets the group reach, with the tools the gateway lets it call.
- The harness strictness above only keeps the agent from adding other MCP
  servers to its own tool list. It is not what protects a koto connection.

Follow-ups, not in this release: interactive OAuth consent (browser + loopback listener), stdio upstreams; write-class tools with operator approval;
pins that survive a daemon restart; translations of this page.
