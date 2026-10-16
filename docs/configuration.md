# Configuration

Every setting below was checked against the source in this tree. Each line
carries a reference (`file` + function/const) so it can be grepped. Where the
code does not make behaviour clear, the text says **unverified**.

There are three configuration surfaces:

1. `/etc/koto/koto.env` — service environment for the installed daemon.
2. Process environment variables (`KOTO_*` and a few legacy names) read by
   the daemon, `koto ctl`, `koto tui`, `koto update` and the installer.
3. Per-group `config.json` keys (`model`, `provider`, `network`, …) set with
   `koto ctl config`, the TUI `/config`, or the ctl-plane `config_set` verb.

---

## 1. `/etc/koto/koto.env`

- Path: `/etc/koto/koto.env` — `daemon/install.go` const `envFilePath`.
- Loaded by systemd via `EnvironmentFile=` in the rendered unit
  `/etc/systemd/system/koto.service` — `daemon/install.go` `renderUnit`,
  const `unitPath`.
- Written by `koto install` (`daemon/install.go` `writeEnvFile`). Re-running
  install **preserves** values already present in the file (the merge loop
  over `existing` in `writeEnvFile`). If the file exists but cannot be read,
  install refuses with `refusing to rewrite it: re-running install must
  preserve the values set there`.
- Header the installer writes: ``Edit and `sudo systemctl restart koto` to apply.`` (`writeEnvFile`).
- Parsing: `KEY=VALUE` per line, blanks and `#` comments ignored, key and
  value whitespace-trimmed — `daemon/install.go` `parseEnvFile`.

Keys written by `writeEnvFile` (the `defaults` and `optional` slices of `envFileKeys`):

Always written (value preserved if already set):

- `KOTO_HOME` — default = the install `-state` dir, which defaults to
  `/var/lib/koto` (`daemon/install.go` const `defaultStateDir`, flag `state`).
- `KOTO_BIND=127.0.0.1`
- `KOTO_PORT=8443`

Optional (written commented-out as `#KEY=` when empty):

- `KOTO_HOST_CPUS`
- `KOTO_HOST_MEM_MIB`
- `ANTHROPIC_API_KEY` — deliberately **not** seeded from `<state>/creds`
  (comment in `writeEnvFile`).
- `KOTO_CLAUDE_BIN` — set to the claude CLI path resolved at install time
  (`installClaudeBin`); unlike the other keys, the freshly resolved value is
  written rather than the file's old value (`kv[0] != claudeBinEnv` branch).
- `KOTO_DEFAULT_PROVIDER` — the host default provider (see 2.1). Written by
  `koto setup`'s `provider` step; listed in `envFileKeys` so a reinstall
  preserves it.

The unit also sets `Environment=HOME=<state dir>`, `WorkingDirectory=<state
dir>` and `ExecStart=/usr/local/bin/koto daemon` (`renderUnit`).

---

## 2. Environment variables

### 2.1 Daemon (`koto daemon`)

- `KOTO_HOME` — state root. When set it is made absolute; when unset the
  daemon uses its **current working directory** — `daemon/daemon.go`
  `kotoHome`, `here`. Derived paths (`initPaths`): `groups/`, `groups.json`,
  `schedules.json`, `goals.json`, `run/`, `metrics.jsonl`.
- `KOTO_BIND` — gRPC listen address. Default `127.0.0.1` —
  `daemon/daemon.go` `daemonMain`. A non-loopback value produces a posture
  warning `KOTO_BIND=<v> exposes the control plane beyond this host…` —
  `daemon/posture.go`.
- `KOTO_PORT` — gRPC listen port. Default `8443` — `daemon/daemon.go`
  `daemonMain`. Guest egress to this port (and to `8443` unconditionally)
  is refused — `daemon/proxy.go` `egressTargetAllowed`.
- `KOTO_PORTS_BIND` — host address that a group's published `ports` bind
  on. Default `127.0.0.1` — `daemon/fc.go` (`portBind := envOr(...)`).
- `KOTO_HOST_MEM_MIB` — fleet memory cap in MiB. `0` = unlimited; unset =
  90% of `/proc/meminfo` MemTotal (`fcHostMemDefaultPct = 90`); invalid
  values log `KOTO_HOST_MEM_MIB=%q invalid; using derived default` —
  `daemon/fchostmem.go` `fcHostMemInit`. If capacity cannot be discovered at
  all and the variable is unset, the daemon refuses VM spawns (`REFUSING VM
  spawns until KOTO_HOST_MEM_MIB is set (use 0 for explicitly unlimited)`).
- `KOTO_CLAUDE_BIN` — claude CLI executed for OAuth token refresh. Default
  bare `claude` (PATH lookup) — `daemon/claudebin.go` `claudeBin`, const
  `claudeBinEnv`.
- `KOTO_DEFAULT_PROVIDER` — provider written into a group's `config.json` when
  it has none or an invalid one: every new group (main included) and
  pre-provider groups on their next ensure(). `claudesdk` | `venice` |
  `openai` | `openrouter` (case-insensitive); unset or unknown = `claudesdk` —
  `daemon/providers.go` `defaultProvider()`. Groups that already name a valid
  provider are never rewritten. Host-only: it is not a `config.json` key, not
  a ctl field and not settable by a guest; set it with `koto setup --only
  provider` (or edit koto.env) and restart the daemon.
- `ANTHROPIC_API_KEY` — if set, outranks the key file
  `<state>/creds/anthropic-api-key` — `daemon/proxy.go` `currentAPIKey`,
  `proxyInitPaths`. The file is re-read per request.
- `KOTO_OPENROUTER_DATA_COLLECTION` — `deny` (default when unset) or `allow`.
  With `deny` the proxy forces `provider.data_collection: "deny"` into every
  `openrouter` chat request, so OpenRouter only routes to vendors that do not
  store or train on prompts, whatever the guest sent. `allow` turns the
  rewrite off. Any other value fails closed (the request gets a 400 and is
  never sent) — `daemon/proxy_openrouter.go` `openrouterPrivacy`. Read per
  request. See section 2.7.
- `KOTO_OPENROUTER_ZDR` — `1` additionally forces `provider.zdr: true` (Zero
  Data Retention endpoints only); `0` or unset = off. Same fail-closed rule.
- `KOTO_OPENROUTER_CREDITS` — `1` opts in to polling `GET /api/v1/key` for
  the key's credits (koto-context line, `koto budget`, OTel credit gauges).
  Unset or anything else = OFF (default): no extra request to openrouter.ai,
  no credits line, no credit gauges — `daemon/budget_openrouter.go`
  `openrouterCreditsEnabled`. See section 2.7.
- `CRED_PATH` (legacy name, no `KOTO_` prefix) — OAuth credentials file.
  Default `<state>/.claude/.credentials.json` if it exists, else
  `$HOME/.claude/.credentials.json` — `daemon/proxy.go` `proxyInitPaths`.
- `PROXY_PORT` (legacy) — base for the per-group proxy port allocation.
  Default `8787` — `daemon/daemon.go` var `PORT_BASE`, `initPaths`.
- `BIND` (legacy) — bind address for the in-process proxy listeners.
  Default `127.0.0.1` — `daemon/daemon.go` `daemonMain` → `proxyStart`.
- `INVOCATION_ID` (set by systemd) — only checked to word a posture finding
  (`the daemon is not running under koto.service`) — `daemon/posture.go`.

### 2.2 Installer (`koto install`)

- `KOTO_HOST_CPUS` — fleet CPU ceiling in whole cores, rendered into the
  unit as `CPUQuota=<n×100>%`. Default `nproc-1` (minimum 1); `0` =
  unlimited (no directive) — `daemon/install.go` `hostCPUs`,
  `installCPUQuota`. It is read from the **installer's** process
  environment (`os.Getenv` in `hostCPUs`); the running daemon does not read
  it. Whether a value only present in `koto.env` reaches the rendered unit
  is **unverified** (no code path loads koto.env into the installer env was
  found).
- `KOTO_HOME` — default state dir for `stateDirOf` (`daemon/install.go`).

### 2.3 Client (`koto ctl`, and the wizard smoke test)

From `daemon/ctl_cli.go` (`ctlUsage` env line, `ctlClient`, `ctlCredsDir`,
`newKotoClient`):

- `KOTO_ADDR` — dial target. Default `127.0.0.1:8443`.
- `KOTO_CREDS_DIR` — PKI directory. Resolution order (`ctlCredsDir`):
  `KOTO_CREDS_DIR`, else `./creds` if it exists, else `$KOTO_HOME/creds`,
  else `/var/lib/koto/creds` if it exists, else `creds`.
- `KOTO_CLIENT` — identity name. Default `agent`. Selects
  `client-<name>.crt`, `client-<name>.key`, `token-<name>` in the creds dir.
- `KOTO_CERT`, `KOTO_KEY`, `KOTO_CA` — override the individual file paths
  (defaults `client-<name>.crt`, `client-<name>.key`, `ca.crt`).
- `KOTO_TOKEN` — the bearer token **value** (not a path). If unset,
  `token-<name>` is read; if that file is missing: ``no token: set KOTO_TOKEN or provide <path> (mint with `make pki-client NAME=<name> ROLE=agent`)``.
- `KOTO_SERVER_NAME` — TLS server-name override for dial targets not in the
  server cert SAN.
- TLS minimum version is 1.3 (`tls.VersionTLS13` in `newKotoClient`).

### 2.4 `koto tui`

`daemon/tui_cmd.go`: flag `-state` defaults to `$KOTO_HOME`, else
`/var/lib/koto`. It reads `<state>/creds/token-tui` and execs `koto-tui`
with this environment:

- `KOTO_CA=<state>/creds/ca.crt`
- `KOTO_CERT=<state>/creds/client-tui.crt`
- `KOTO_KEY=<state>/creds/client-tui.key`
- `KOTO_TOKEN=<contents of token-tui>`
- `KOTO_ENDPOINT=$KOTO_ADDR` (default `127.0.0.1:8443`)
- `KOTO_SERVER_NAME=koto-daemon`
- `KOTO_PROMPTS_DIR=<state>/prompts`
- `SOCK_PATH=<state>/run/tui/koto.sock`
- `KOTO_TUI_TERM=$TERM` (default `xterm-256color`)
- `KOTO_SCRIPTS_DIR=<state>/scripts` — only if that directory exists.

Missing token error: ``no TUI token in <creds> — mint one with `koto pki client -creds <creds> tui` ``.

### 2.5 `koto update`

- `KOTO_REPO_URL` — full repo URL (trailing `/` trimmed).
- `KOTO_REPO` — `owner/name`, default `jpzk/koto`; URL becomes
  `https://github.com/<repo>`.
— `daemon/update.go` `updateRepoURL` (mirrors `install.sh`).

`install.sh` itself additionally reads `KOTO_KEY_FPR`, `KOTO_KEYSERVER`,
`KOTO_ARCH` (default `x86_64`), `KOTO_VERSION` (default `latest`),
`KOTO_DEST`, `KOTO_RELEASE_PUBKEY` — `install.sh` top-of-file assignments.

### 2.6 Internal variables (not operator settings)

These appear in `daemon/` code but are set by koto itself; do not set them.

- `KOTO_USERNS_READY` — userns bootstrap stage marker (`unset`/`1`/`2`) —
  `daemon/userns.go` const `usernsEnv`.
- `KOTO_JAIL_MAX_UID` — clamped end of the per-VM uid band carried across
  the bootstrap re-execs; accepted only in `(30000, 60000]` —
  `daemon/fcjail.go` const `jailMaxEnv`, `fcJailBaseUID`, `fcJailMaxUID`.
- `KOTO_DEFAULT_CLAUDE_MODEL`, `KOTO_DEFAULT_VENICE_MODEL`,
  `KOTO_DEFAULT_OPENROUTER_MODEL` — injected into the guest init env with the
  values of `defaultClaudeModel` / `defaultVeniceModel` /
  `defaultOpenRouterModel` — `daemon/fc.go` (init `env` map),
  `daemon/groups.go`. Also injected: `ANTHROPIC_BASE_URL=http://127.0.0.1:18888`
  (`fcGuestProxyTCP`), and `NO_PROXY`/`no_proxy=127.0.0.1,localhost` when
  the group's network is not `none`.
- `KOTO_SESSION` — exported per turn inside the guest by `entrypoint.sh`;
  the daemon reads it from guest process environments to attribute jobs —
  `daemon/jobs.go`, `daemon/send.go`.
- `KOTO_ENDPOINT`, `KOTO_PROMPTS_DIR`, `KOTO_SCRIPTS_DIR`, `KOTO_TUI_TERM` —
  set by `koto tui` for the TUI child (section 2.4).

### 2.7 OpenRouter (`provider: openrouter`)

OpenRouter (https://openrouter.ai) is one OpenAI-compatible HTTPS API in
front of many model vendors (https://openrouter.ai/docs/api_reference/overview).
koto relays it exactly like Venice: direct HTTPS, host-side key.

- Key: an OpenRouter API key goes in `<state>/creds/openrouter.key` (mode
  0600; `koto setup --only auth` writes it). The daemon reads it per request
  and injects it as `Authorization: Bearer`; the guest only ever holds the
  placeholder `proxied` — `daemon/proxy_openrouter.go` `openrouterAuth`. No
  key = the guest gets a 503 and nothing is sent.
- Guest routes: `POST /api/v1/chat/completions`, `GET /api/v1/models[/…]`,
  relayed unchanged to `https://openrouter.ai/api/v1/…`
  (`llmRoutesOpenRouter`). Every other path is refused, including
  `/api/v1/key` and `/api/v1/credits`. The attribution headers
  `HTTP-Referer`, `Referer`, `X-Title` and `X-OpenRouter-Title` are dropped.
  The agent loop is the Venice one (`venice_stream.js`).
- Privacy: OpenRouter's default routing (`provider.data_collection:
  "allow"`) includes vendors that keep prompts and may train on them
  (https://openrouter.ai/docs/guides/routing/provider-selection). koto forces
  `"deny"` host side by default; see `KOTO_OPENROUTER_DATA_COLLECTION` and
  `KOTO_OPENROUTER_ZDR` (section 2.1). Account-level privacy settings on
  openrouter.ai still apply on top and can only tighten this.
- Default model `moonshotai/kimi-k3` (lists `tools` in OpenRouter's
  `supported_parameters`; the agent loop needs tool calls); override per
  group with `model`. The TUI model picker lists only models that support
  tools and text output.
- Budget key `openrouter/apikey`. OpenRouter sends no balance headers.
  Credits are OFF by default (no extra egress); with
  `KOTO_OPENROUTER_CREDITS=1` in koto.env
  the daemon polls `GET /api/v1/key` with the host key after OpenRouter
  requests (at most once a minute, backing off up to 15 minutes on errors,
  never blocking a turn) — `daemon/budget_openrouter.go`. That endpoint
  describes the KEY, not the account: koto-context shows
  `credits: $X left of $Y key limit` when the key has a credit limit, and
  `credits: $Z used, no key limit` otherwise. The account balance needs a
  management key (`/api/v1/credits`) that koto never holds, so it is not
  shown. OTel gauges: `koto.llm.credits.used`, `.limit`, `.remaining`
  (only when the key has a limit) and `.age`.
- Recommendation: give the key koto uses its own credit limit on
  openrouter.ai (https://openrouter.ai/settings/keys). That caps what a
  runaway agent can spend, and it is what makes the remaining balance
  visible to koto. koto's own `koto budget` caps work on top.

---

## 3. Per-group configuration (`config.json`)

- Location: `<state>/groups/<group>/.cs/config.json` —
  `daemon/config.go` `groupConfigPath`. Written atomically (temp file +
  rename, mode 0600) under a per-group lock — `updateGroupConfig`.
- The group must exist: `no such group: <g> (spawn it first)`; bad names:
  `invalid group name` — `daemon/config.go` `configCmd`.
- Unknown/invalid values are **silently ignored** (the previous value is
  kept) — `daemon/config.go` `applyConfig`.
- A value of `null`, `""` or `[]` **clears** the key — `isClear`.
- Reading back returns the *effective* config with defaults filled in —
  `effectiveConfig`. It also reports a read-only `io` field
  (`<MiB>/s / <ops>ops`).

### Keys

- `provider` — `claudesdk` | `venice` | `openai` | `openrouter`. Default: the host's
  `KOTO_DEFAULT_PROVIDER`, else `claudesdk` (`daemon/providers.go`
  `defaultProvider()`, `daemon/groups.go` `ensureProviderConfig`).
- `model` — free string, charset `^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`, max 128
  chars (`configIdentRE`, `configMaxIdent`). Default when unset:
  `claude-sonnet-5` under claudesdk, `kimi-k2.5` under venice,
  `moonshotai/kimi-k3` under openrouter (`daemon/groups.go`
  `defaultClaudeModel`, `defaultVeniceModel`,
  `daemon/proxy_openrouter.go` `defaultOpenRouterModel`).
- `effort` — same charset/length rule as `model`. Shown as `(default)`
  when unset (`effectiveConfig`).
- `network` — `none` | `wan` | `lan` | `full` (`daemon/fcnet.go` consts
  `fcNetNone`…`fcNetFull`). Default `none` (no NIC). Any other stored string
  reads back as `none` (`daemon/groups.go` `groupConfig.network`). Applies
  on next spawn (`/restart`); lowering to `none` also takes effect on the
  proxy's very next request (comment in `applyConfig`). Legacy key
  `internet`: `full` → `wan`, `none` → `none`, migrated to `network`.
- `size` — `small` | `medium` | `large` | `xlarge`. Default `small`.
  Applies on next spawn. Disk grows but never shrinks. Presets
  (`daemon/fc.go` `fcSizePresets`; vCPU, RAM MiB, workspace disk, IO MiB/s,
  IO ops):
  - `small`: 2, 1024, 8 GiB, 100, 15000 (`fcDefaultVcpus`,
    `fcDefaultMemMiB`, `fcWorkspaceBytes`)
  - `medium`: 2, 2048, 12 GiB, 150, 22500
  - `large`: 4, 4096, 16 GiB, 200, 30000
  - `xlarge`: 8, 8192, 24 GiB, 250, 37500
- `root` — `yes` | `no`. Default `no`. Passwordless sudo + persistent root
  overlay in the guest; applies on next spawn (`daemon/fc.go` `groupRoot`).
- `kvm` — `yes` | `no`. Default `no`. Exposes VMX/SVM (nested
  virtualization) to the guest; applies on next spawn (`daemon/fc.go`
  `groupKVM`). Note: not listed in the `koto ctl` usage text, but the flag
  `-kvm` exists (`daemon/ctl_cli.go`, `fs.String("kvm", …)`).
- `autostart` — `yes` | `no`. Default `no`. Read only at daemon start
  (`autostartGroups`); a group `/restart` does not apply it.
- `ports` — JSON int array or comma string (`8080,3000`). Each port must be
  in `[1024, 65535]`; duplicates and out-of-range entries are dropped; at
  most 32 kept (`configMaxPorts`). Published on `KOTO_PORTS_BIND` (default
  `127.0.0.1`) on the host.

The yes/no keys fail closed: absent, unparsable or any other value is
`no`; a JSON boolean is accepted for hand-edited files —
`daemon/groups.go` `groupConfigBool`.

### Who may set what

- **Posture keys** = `network`, `internet`, `root`, `ports`, `size`,
  `autostart`, `kvm`. On gRPC, a Config request carrying any of them is
  authorized as the synthetic admin-only verb `config_posture` —
  `daemon/auth.go` `postureVerb`, `daemon/acl.go` `adminOnlyVerbs`.
- On the in-guest ctl plane, `config_set` is main-only (`ctl: verb not
  allowed for non-main groups: config_set`) and refuses every posture key
  with `ctl: <key> is an operator-only setting — ask the operator (TUI
  /config, or koto ctl config)` — `daemon/ctl.go` `configPostureKey`.
  Main can therefore only set `model`, `effort`, `provider` there.
- See [acl-and-auth.md](acl-and-auth.md) for roles.
