# Authentication and authorization (gRPC control plane)

This page covers who may call the daemon's gRPC API (TUI, Android, `koto ctl`,
scripts) and what each caller may do. The in-guest ctl plane
(`/workspace/.cs/ctl`) is a **separate** authorization axis keyed on group
identity, hardcoded in `daemon/ctl.go`. It is not governed by `acl.json`
(header comment of `daemon/acl.go`). For its rules, see
[ctl-reference.md, Part 2](ctl-reference.md).

All paths below are relative to the state root `HERE`: `KOTO_HOME` when set
(installed mode, normally `/var/lib/koto`), otherwise the daemon's working
directory (`kotoHome`, `daemon/daemon.go`). Credential files live in
`<HERE>/creds/` (`credFile`, `daemon/auth.go`).

## 1. Layers

Every call passes three checks in order (`daemon/auth.go` header comment,
`authFromCtx`, `aclCheck`):

1. **mTLS**: server cert plus a required client cert.
2. **Bearer token**: authentication, bound to the client cert.
3. **ACL**: authorization by role, verb and target group.

The comment also mentions a WireGuard overlay below these layers. It runs
outside the koto process.

### 1.1 Listener

- The default listen address is `127.0.0.1:8443`. `KOTO_BIND` overrides the
  address and `KOTO_PORT` the port (`daemon/daemon.go`, after
  `go autostartGroups()`). See [configuration.md](configuration.md).
- gRPC keepalive: server `Time: 30s`, `Timeout: 10s`. The enforcement policy
  is `MinTime: 10s` with `PermitWithoutStream: true` (`daemon/daemon.go`,
  `grpc.KeepaliveParams`).
- On startup the daemon logs `kotod ready grpc=<addr> (mTLS+token)`, then runs
  `authBindingPreflight`.

### 1.2 mTLS (`serverTLSConfig`, `daemon/auth.go`)

- The server presents `creds/server.crt` and `creds/server.key`.
- Client certs are required and must chain to `creds/ca.crt`
  (`tls.RequireAndVerifyClientCert`). The minimum version is TLS 1.3
  (`MinVersion: tls.VersionTLS13`).
- **Allowlist.** The SHA-256 fingerprint of the client's leaf cert (DER,
  lowercase hex) must also appear in `creds/clients.allow`
  (`VerifyPeerCertificate`, `loadClientAllow`).
  - Line format: `<sha256hex> <name>`.
  - Lines whose first field starts with `#` are ignored.
- **Re-read on every handshake.** Deleting a line revokes that device without
  a daemon restart.
- Handshake errors:
  - `client cert <fp> not in allowlist`
  - `clients.allow: <err>` (for example, the file is missing)
  - `no client certificate`

### 1.3 Bearer token (`authFromCtx`, `tokenIdentity`)

- The token is read from the `authorization` gRPC metadata. The `Bearer `
  prefix is stripped.
- The token is SHA-256 hashed and looked up in `creds/tokens.json`, which is
  re-read on every call (`loadTokenIdentities`).
- **Cert and token are bound (audit L6).** The client cert's CommonName must
  equal the `tokens.json` key that matched the token (`certIdentity`). Either
  revocation alone is therefore enough: removing the `clients.allow` line, or
  removing the `tokens.json` entry.
- Failures return gRPC `Unauthenticated`:
  - `invalid or missing token` (logged as `rejected call from <addr> (bad/missing token)`)
  - `no client certificate`
  - `client certificate and token name different identities`
- **Startup preflight** (`authBindingPreflight`). It logs a warning for each
  name present on only one side:
  - `identity "<n>" has an allowlisted cert but no tokens.json entry — its calls will be refused (mint one: koto pki client <n>)`
  - `identity "<n>" has a token but no cert named "<n>" in clients.allow — its calls will be refused`

### 1.4 `tokens.json` shapes (`parseTokens`, `daemon/acl.go`)

The file maps `clientid → credential`. Three value shapes are accepted:

- `"<sha256hex>"`: legacy bare hash. The role is `admin` (`defaultRole`).
- `{"hash": "<hex>", "role": "agent"}`: a single role.
- `{"hash": "<hex>", "roles": ["reader", "ops"]}`: several roles. `roles`
  wins over `role` if both are present.

An entry is skipped (it authenticates nobody) in two cases:

- it matches none of these shapes, or has an empty `hash`;
- it ends up with no roles after trimming. For example,
  `koto pki client -role "" x` writes `"roles": null`, which is skipped.

The hash is lowercased before lookup.

## 2. PKI: minting credentials

There are two equivalent routes. Both produce the same artifacts (`daemon/pki.go`
header comment):

- **Go:** `koto pki`, used by `koto setup` and `koto install`. Needs no
  openssl or jq.
- **Makefile:** `make pki-init` and `make pki-client`, using openssl and jq.

### 2.1 `koto pki` (`pkiCliMain`, `daemon/pki.go`)

```
usage: koto pki init   [-creds DIR] [-san DNS:a,IP:1.2.3.4]
       koto pki client [-creds DIR] [-role r1,r2] <name>
       koto pki server [-creds DIR] -san DNS:a,IP:1.2.3.4
```

Defaults: `-creds creds`. `init -san` defaults to `defaultServerSANs` =
`DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`. `client -role` defaults to
`admin`.

#### `init` (`pkiInit`)

- Creates the CA only if it is absent (`pkiEnsureCA`): EC P-256,
  `CN=koto-ca`, valid for 3650 days.
- If `ca.crt` exists without `ca.key`, it refuses with
  `<crt> exists but <key> does not — refusing to mint a new CA over it.`,
  followed by restore/move-aside advice.
- Issues the server cert (`pkiServerCert`): `CN=koto-daemon`, valid for
  825 days, ExtKeyUsage ServerAuth. SANs must be `DNS:…` or `IP:…`. Anything
  else fails with `bad SAN "<s>" (want DNS:… or IP:…)`.
- The server cert is reissued when it is absent, `server.key` is missing,
  it is unreadable or expired, it is within `pkiRenewWindow` (30 days) of
  expiry, it is not yet valid, or it does not chain to the CA
  (`pkiServerCertNeedsReissue`). SANs are not compared.
- Seeds `acl.json` only if the file is absent (`pkiCreateNew`, mode 0600) with:

  ```json
  {
    "agent": {
      "list": "*", "send": "*", "history": "*", "metrics": "*",
      "sched_list": "*",
      "subscribe_group": "*", "watch_state": "*"
    }
  }
  ```

- The creds directory is created with mode 0750.

#### `client <name>` (`pkiClient`)

- **Name rule:** `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` and no `..`
  (`pkiClientNameRE`, `pkiClientNameOK`). Otherwise it fails with
  `invalid client name "<name>": letters, digits and ._- only, no path separators …`.
- **Writes:**
  - `client-<name>.key` and `client-<name>.crt`: EC P-256, `CN=<name>`,
    825 days, ClientAuth.
  - A line `<fp> <name>` appended to `clients.allow`, unless that fingerprint
    is already present.
  - A 32-byte random hex token in `token-<name>` (0600).
  - `{"hash", "roles"}` under `<name>` in `tokens.json`.
- The `tokens.json` update holds an flock on the creds directory
  (`pkiLockCreds`) and writes atomically via a temp file and rename.
- A corrupt `tokens.json` fails with `tokens.json is corrupt — fix it on disk first: <err>`.
- A missing CA key fails with ``ca.key (run `koto pki init` first): <err>``.

#### `server -san …`

- Reissues only the server cert. The CA is untouched, so existing clients
  keep working.
- Prints `server cert reissued for <san> — restart the daemon to apply`.
- Without `-san` it fails with `pki server: -san is required (e.g. -san …)`
  (exit 2).

### 2.2 Makefile route (`Makefile`, targets `pki-init` / `pki-client`)

- `make pki-init [SERVER_SAN=…]`:
  - `SERVER_SAN` defaults to `DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`.
  - It refuses if `creds/ca.key` exists:
    `creds/ca.key exists — refusing to regenerate the CA …`.
  - It seeds the same `agent` role into `creds/acl.json` if that file is
    absent.
- `make pki-client NAME=<client> [ROLE=r1,r2]`:
  - `ROLE` defaults to `admin`.
  - It requires `creds/ca.key` and uses `jq` to edit `tokens.json`.
  - It writes the same files as `koto pki client`.
- Keys are generated under `umask 077` and then `chmod 600`.

### 2.3 Identities created by setup (`daemon/setup_steps.go`)

- The setup PKI step runs `pkiInit` with `defaultServerSANs` plus any extra
  SANs the operator types in.
- It then mints two clients, **both with role `admin`**, each only if its
  cert is absent:
  - `tui`
  - `agent`, the default `KOTO_CLIENT` of `koto ctl`.
- The `agent` *client* is therefore admin by design. The comment cites audit
  M11, decided 2026-09-05.
- The *role* named `agent` in the seeded `acl.json` is a different thing: it
  exists for scoped identities minted with `koto pki client -role agent <name>`.

## 3. Authorization: roles, verbs, targets

### 3.1 Model (`daemon/acl.go`)

- A user (clientid) has one or more roles, and each role grants verbs on
  targets. Effective permission is the **union** over the user's roles
  (`rolesAllowed`).
- `creds/acl.json` maps `role → {verb → targets}`. `daemon/acl.go` is the
  source; the Makefile comment above `pki-init` says the same.
  - Targets are group names, `"*"` or `["*"]` (any group), or a list.
  - A verb key of `"*"` grants every verb on its targets, **except** the
    admin-only verbs (`grantFor`).
  - The legacy flat form `{"agent": ["list", "send"]}` means those verbs on
    any target (`parseACL`).
- `acl.json` is re-read on every call (`loadACL`). Editing roles or grants
  needs no restart.

### 3.2 The `admin` role

`admin` is hardcoded (`grantFor`, `defaultRole = "admin"`):

- It may call every verb on every target.
- Any `admin` key in `acl.json` is ignored.
- A missing or corrupt `acl.json` cannot lock admin out.
- `acl set admin …` fails with
  `the admin role is hardcoded and cannot be defined in the ACL`.
- `acl del admin` fails with `the admin role is hardcoded and cannot be deleted`
  (`aclSetRoleCmd`, `aclDelRoleCmd`).

### 3.3 Failing closed

Requests are denied in each of these cases (`grantFor`, `roleAllowed`,
`parseACL`, `parseTargets`):

- **Unknown role:** the role is absent from `acl.json`.
- **Missing file:** with no `acl.json`, no non-admin role exists.
- **Corrupt file:** malformed `acl.json` (`parseACL` returns nil) denies every
  non-admin role.
- **Malformed role:** a role whose value is malformed grants nothing.
- **Bad grant value:** a single bad grant value, or one that is `null` or
  `""`, yields an empty target set (audit M35).
- **Unlisted verb:** a verb that is neither listed nor covered by `"*"`. New
  RPCs are therefore denied by default.

### 3.4 Verbs

- A verb is the snake_case form of the gRPC method name, for example
  `/koto.Koto/SchedAdd` → `sched_add` (`verbFromMethod`).
- For the list of RPCs, see `service Koto` in `protocol/koto.proto`.

The full verb set, derived mechanically from the RPCs (so names like `goal_list` or `job_logs` need not appear literally in Go source):

- **Lifecycle:** `spawn`, `send`, `list`, `stop`, `interrupt`, `destroy`,
  `restart`, `history`, `config`, `metrics`, `clear`, `drain`
- **Jobs:** `jobs`, `job_logs`, `job_tail`
- **Schedules:** `sched_add`, `sched_list`, `sched_del`, `sched_toggle`,
  `sched_run`
- **Goals:** `goal_set`, `goal_list`, `goal_approve`, `goal_pause`,
  `goal_interrupt`, `goal_resume`, `goal_cancel`
- **Inspection:** `resources`
- **ACL management:** `acl_get`, `acl_set_role`, `acl_del_role`
- **Execution and streams:** `run_script`, `attach_shell`, `subscribe_group`,
  `subscribe_logs`, `watch_state`

### 3.5 Admin-only verbs (`adminOnlyVerbs`)

These verbs cannot be granted through `acl.json`, not even with the `"*"` verb.
Only `admin` passes:

- `acl_get`, `acl_set_role`, `acl_del_role`. ACL management must not be
  delegatable through the ACL itself.
- `run_script`: direct code execution in a guest VM.
- `config_posture`: a synthetic verb, covered in the next section.
- `spawn_posture`: the same rule on `Spawn`, also covered in the next section.

### 3.6 `config` vs `config_posture` (`postureVerb`, `daemon/auth.go`)

- A `Config` request that sets any **posture** key is re-labelled
  `config_posture`, which is admin-only. The posture keys are `network`,
  `internet`, `root`, `ports`, `size`, `autostart`, `kvm` and `rootfs`.
- A `config` grant therefore covers only the remaining settings: `model`,
  `effort` and `provider`.
- A `Spawn` request carrying `size`, `rootfs` or `root` seeds those keys into
  the new group's config before its first boot, so it is re-labelled
  `spawn_posture`, also admin-only. A plain `spawn` grant still covers a spawn
  with only `provider` and `model`.
- The target check still applies. The in-guest ctl plane refuses posture keys
  on its own (`configPostureKey` in `daemon/ctl.go`; see
  [configuration.md](configuration.md)).

### 3.7 Targeted vs verb-only (`targetOf`)

A verb is group-scoped ("targeted") when its request message carries a group.

Targeted request types, with the RPCs that use them:

- `SpawnReq`: Spawn
- `SendReq`: Send
- `GroupReq`: Stop, Interrupt, Destroy, Restart, Clear, Drain
- `HistoryReq`: History
- `ConfigReq`: Config
- `MetricsReq`: Metrics
- `JobsReq`: Jobs
- `JobLogsReq`: JobLogs
- `JobTailReq`: JobTail
- `SchedAddReq`: SchedAdd
- `SchedListReq`: SchedList
- `GoalSetReq`: GoalSet
- `GoalListReq`: GoalList
- `GoalGroupReq`: GoalApprove, GoalPause, GoalInterrupt, GoalResume,
  GoalCancel
- `SubscribeReq`: SubscribeGroup
- `RunScriptReq`: RunScript
- `ShellInput`: AttachShell. Every message on the stream carries `group` and
  is checked.

Rules:

- A targeted verb passes only if the grant has `"*"` or names the group
  (`roleAllowed`).
- A targeted request with an **empty** group needs the `"*"` grant, because it
  reads across all groups. Examples: global `metrics`, unfiltered `sched_list`,
  and `jobs` / `goal_list` without a group. An empty name is never stored as a
  target (`parseTargets`).
- **Verb-only** (no target dimension): `list`, `watch_state`, `resources`,
  `subscribe_logs`, `sched_del`, `sched_toggle`, `sched_run` and the `acl_*`
  verbs. Any grant of the verb, with any target set, lets the call through.

### 3.8 Projection of verb-only reads (`visibleTargets`)

For these verbs the grant's target set narrows the **answer** rather than the
call (`daemon/grpc_server.go`):

- **`list` and `watch_state`.** Only groups covered by the grant are returned
  (`projectGroups`). Each group's job records are also dropped unless the
  `jobs` grant covers that group.
- **`resources`.** Other groups are removed, and the host fleet totals
  (group count, running groups, alloc/provisioned bytes, committed memory) are
  recomputed over the visible groups only, unless the grant is `"*"` (`projectResources`). Machine-level fields such as filesystem size and free space, memory cap and MemTotal are kept.
- **`subscribe_logs`.** Only events attributed to a covered group are delivered.
  Daemon-wide events (empty group) are dropped unless the grant is `"*"`
  (`logVisible`).
- **`metrics`.** The `GlobalMetric` field (the newest record from any group) is
  filled only for callers whose `metrics` grant is `"*"` (`Metrics` handler,
  audit M30).

### 3.9 Streams

- **Verb gate.** At stream open, the caller must hold the verb in some role
  (`anyGrant`, in `authStream`). The target check runs when each request
  message is received (`aclStream.RecvMsg`).
- **Re-check.** Long-lived streams are re-authorized on send at most every
  `aclStreamRecheck` = 5s (method `revalidate` on `aclStream`, audit M25).
  - The identity is re-resolved by name from `tokens.json`. If it is gone, the
    stream closes with `Unauthenticated` `identity <name> has been revoked`.
  - If the roles no longer allow the verb, it closes with `PermissionDenied`
    `roles <r> may no longer call <verb> on "<group>"`.
- **Admission caps.** `streamMaxPerIdentity = 512` and
  `streamMaxGlobal = 2048` (`streamAdmit`). Over either cap the call returns
  `ResourceExhausted` `too many live streams (per-identity 512, global 2048)`.

### 3.10 Denial errors and logs (`aclCheck`)

- Denials return gRPC `PermissionDenied`:
  - `roles <r1,r2> may not call <verb> on "<group>"` (targeted)
  - `roles <r1,r2> may not call <verb>` (verb-only)
- Every decision is logged under the `acl` source:
  `<name> (roles …) allowed|denied <verb> [on "<group>"] from <addr>`.
- Targeted decisions are attributed to the group's log when the group name is
  valid (`aclLogGroup`).

## 4. Managing the ACL

There are three admin-only RPCs: `AclGet`, `AclSetRole` and `AclDelRole`
(`daemon/grpc_server.go`). From the CLI they are `koto ctl acl get|set|del`
(`ctlAcl`, `daemon/ctl_cli.go`; see [ctl-reference.md](ctl-reference.md)).

```sh
koto ctl acl get
koto ctl acl set ops stop:ghost restart:ghost list 'metrics:*'
koto ctl acl del ops
```

- **`acl set` replaces the role's whole grant object** (`aclSetRoleCmd`).
- **Role names** follow `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$` (`roleNameRE`).
  Otherwise: `invalid role name "<r>"`.
- **Grant validation** (`validateGrants`) rejects the whole request on the
  first bad entry:
  - A verb key must be `"*"` or match `roleNameRE`, else `invalid verb "<v>"`.
  - A target must be `"*"` or a list of names matching `roleNameRE` (or
    `"*"`). Errors: `verb "<v>": target must be "*" or a list of groups` and
    `verb "<v>": invalid target <t>`.
- **Writes** hold `aclFileLock` and go atomically through
  `creds/.acl.json.tmp` and a rename, with mode 0600 (`writeACLDoc`).
- **A corrupt file blocks mutation:**
  `acl.json is corrupt — fix or remove it on disk first` (`readACLDoc`).
- **`acl del` on a missing role** fails with `no such role: <role>`.

## 5. Recipes

**Scoped identity for a script.** Mint a client and point `koto ctl` at it:

```sh
koto pki client -creds /var/lib/koto/creds -role agent ci
KOTO_CLIENT=ci koto ctl list
```

`KOTO_CLIENT` selects `client-<name>.{crt,key}` and `token-<name>`
(`newKotoClient`, `daemon/ctl_cli.go`).

**Confine a role to one group** by replacing `"*"` values with lists:

```sh
koto ctl acl set agent list:main send:main history:main subscribe_group:main watch_state:main
```

**Revoke a device.** Delete its line in `creds/clients.allow`, **or** its
entry in `creds/tokens.json`. Either is enough because of the cert/token
binding. New calls fail right away, and open streams close within 5s
(`aclStreamRecheck`).

**Reach the daemon from another machine.** Add a SAN without touching the CA,
then restart the daemon:

```sh
koto pki server -creds /var/lib/koto/creds -san DNS:koto-daemon,DNS:localhost,IP:127.0.0.1,IP:192.168.1.20
```

The daemon only listens on `127.0.0.1` unless `KOTO_BIND` is set. See
[configuration.md](configuration.md).
