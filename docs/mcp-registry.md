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
header, read-class connections, calls decided automatically by policy.
Parked approvals, OAuth and stdio servers are not supported yet.

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
        [--class read|write] [--rules <file.json>] [--auth-file <file>|-]
    koto ctl mcp rm <conn>
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
- The auth header value is never written to logs or audit lines.

## Main's read-only view (`mcp_view`)

A coordinator needs to know what its peers can reach in order to plan
work, but it must not be able to change it. The `main` group therefore
gets one read-only ctl verb, `mcp_view` (`daemon/mcp_view.go`), on the
in-guest ctl plane:

    {"cmd":"mcp_view","op":"ls"}
    {"cmd":"mcp_view","op":"show","conn":"<conn>"}
    {"cmd":"mcp_view","op":"effective","group":"<group>"}

- Only `main` may call it. Every other group gets
  `ctl: verb not allowed for non-main groups: mcp_view`.
- Only the three ops exist. Anything else (`grant`, `allow`, ...) is
  refused with `mcp_view is read-only (ls | show | effective); registry
  changes are operator-only (koto ctl mcp)`. The registry file is not
  touched. Changes stay with the operator (`koto ctl mcp`), as before.
- `ls` lists every connection, `show` one connection with its grants,
  `effective` the connections a group is whitelisted on, with the
  effective tools, the rules from both layers and the rate limit.

The answer is built field by field from an allowlist. A field that is
added to the registry later does not show up here until someone adds it
on purpose. What the answer can contain:

- connection name, enabled, class;
- URL as `scheme://host[:port]/path` (no user info, query or fragment);
- ceiling tool names and their argument rules;
- whitelisted groups, and per group the granted tools, rules and rate;
- the effective set;
- the auth kind: `none`, `header` (the header name only) or `pat` (an
  `Authorization: Bearer` header), plus a status (`ok` for a static
  header);
- `stale: true` when the registry file failed to reload and the daemon
  is using the last good version (the error text is not included);
- `truncated: true` when `ls` had to drop connections to stay under the
  size cap.

What it never contains: header values, tokens, client IDs and secrets,
token endpoints, issuers, file paths, upstream tool descriptions, load
error text.

The encoded answer is capped at 64 KiB. `ls` drops trailing connections
and sets `truncated`. `show` and `effective` refuse with
`mcp_view: answer exceeds 65536 bytes`. Every call is audited like any
other ctl verb.

## Audit

MCP activity goes into the daemon's hash-chained audit log, which
`koto audit verify` checks. Record layout:
[audit-events.md](audit-events.md#what-is-recorded).

- `mcp_call`: one record per gateway decision (group, connection,
  method, tool, duration, and a fixed reason code: `ok`, `policy`,
  `rate_limited`, `not_whitelisted`, `schema_changed`, `upstream`).
- `mcp_admin`: one record per `koto ctl mcp` op (op, connection, group,
  tool). URL, auth and rules are not recorded.
- `mcp_view` calls are recorded by the ctl audit, like every ctl verb.

## Enforcement (gateway)

Each group reaches MCP only through a host-side gateway (`daemon/mcpgw`).
Inside the guest the endpoint is `http://127.0.0.1:18889/mcp/<conn>`; the
guest agent relays it over vsock to the host, which knows which group the
VM belongs to. The guest never chooses its own group name.

For every request the gateway:

1. reads the current registry (no cache, so a `revoke` or `deny` applies
   to the next request);
2. answers 404 if the group is not whitelisted for the connection;
3. on `tools/list`, returns only the effective tools;
4. on `tools/call`, refuses tools outside the effective set, arguments
   that fail the ceiling or grant rules, and calls over the rate limit;
5. adds the auth header only on the host-to-upstream request;
6. writes one line to the group log (group, connection, method, tool,
   allowed or refused, reason, duration), and one `mcp_call` record to
   the daemon's hash-chained audit log with a fixed reason code instead
   of the free-text reason. Arguments, results and credentials are in
   neither. See [Audit](#audit).

All groups on one connection share one upstream MCP session.

Tool schema pinning: the first time the gateway sees a tool's definition
during a daemon run, it records a hash. If the upstream later changes that
definition, the tool is hidden and refused until the daemon restarts or the
connection is re-added. This protects against a server that swaps a tool's
description or schema after it was approved.

### On by default, and the kill switch

The gateway is on by default. It does nothing until the operator grants
a tool: a group whose effective set is empty (no whitelisted connection
with at least one effective tool) gets no MCP listener and no MCP
configuration, and its harness runs with the same arguments and files
as a daemon without MCP support. A golden test compares these against
develop for every launch path (claude, codex exec, codex app-server,
pi json, pi rpc).

- `KOTO_MCP_GATEWAY=0` (also `false`, `off`, `no`) is the kill switch.
  Set it in the daemon's environment and restart the daemon. No group
  gets a listener or any MCP configuration, whatever the registry says.
  The registry can still be edited while the switch is off.
- Any other value, or no value, means on.

When it is on, a group with at least one effective tool gets its own
gateway listener on vsock port 9005, and the guest agent bridges
`127.0.0.1:18889` to that port. The listener follows the grants while the
group runs:

- `allow` + `grant` on a group that had nothing: the listener opens, and
  the next turn gets the MCP configuration. No restart is needed.
- `revoke`, `deny` or `rm` that leaves the group with nothing: the
  listener closes, and the next turn runs without MCP again.

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
- Goal work turns use the same claude command line, so the same flags
  apply there.

codex (0.171.0):

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
- Goal work turns run `codex app-server` (live mode, so the operator can
  steer the turn). They get the same `-c` layer as `codex exec`: the
  gateway entries and the three features off. `codex app-server` has no
  `--ignore-user-config` flag, so on this path the strict half comes from
  codex's system requirements file instead:
  - Before every codex turn (exec and app-server) the guest writes
    `/etc/codex/requirements.toml` with one
    `[mcp_servers.<conn>.identity] url = "http://127.0.0.1:18889/mcp/<conn>"`
    entry per granted connection. codex disables every configured server
    whose name is not listed or whose URL differs, so a server the agent
    defines in `$CODEX_HOME/config.toml` or a project config is not
    started.
  - `/etc/codex` is root-owned. The agent runs as `node` and cannot change
    the file.
  - A turn without grants removes the file again, so nothing is left
    behind.
  - The file starts with a koto header. A `requirements.toml` that koto
    did not write is never replaced or removed. If the file cannot be
    written, the app-server turn runs without any MCP configuration
    rather than next to servers koto cannot switch off.
- Context compaction also uses `codex app-server`. It gets no MCP
  overrides and exposes no tools to the model. The requirements file of
  the last turn still applies, so a planted server stays switched off for
  a group that has grants. Without grants there are no koto connections
  to protect.

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
- Goal work turns run `pi --mode rpc` (live mode). They get the same
  `-e builtin:mcp` and the same per-turn `mcp.json` as `pi --mode json`,
  so granted tools work in goal work turns too. Compaction (`pi --mode
  rpc` without a turn) gets no MCP extension.
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

Follow-ups, not in this release: OAuth and stdio upstreams; write-class tools with operator approval;
pins that survive a daemon restart; translations of this page.
