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

For claude turns the daemon sends only the names of the connections the
group is whitelisted on. The guest turns these into
`--mcp-config '{"mcpServers":{"<conn>":{"type":"http","url":"http://127.0.0.1:18889/mcp/<conn>"}}}'`
and adds `--strict-mcp-config`, so an `.mcp.json` in the workspace cannot
add servers the operator never granted. The upstream URL and auth header
are never part of this config.

Follow-ups, not in this release: the same rendering for codex and pi;
OAuth and stdio upstreams; write-class tools with operator approval;
pins that survive a daemon restart; translations of this page.
