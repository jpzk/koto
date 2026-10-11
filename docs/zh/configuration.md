译自 ../configuration.md；以英文版为准。

# 配置

以下每一项设置都已对照本仓库源码核实。每行都带引用（`文件` +
函数/常量名），可直接 grep。代码未能说明行为之处，文中标注
**unverified（未验证）**。

配置共有三个层面：

1. `/etc/koto/koto.env` — 已安装 daemon 的服务环境。
2. 进程环境变量（`KOTO_*` 及少数历史遗留名称），由 daemon、`koto ctl`、
   `koto tui`、`koto update` 和安装器读取。
3. 每个 group 的 `config.json` 键（`model`、`provider`、`network` 等），
   通过 `koto ctl config`、TUI 的 `/config`，或 ctl 平面的 `config_set`
   动词设置。

---

## 1. `/etc/koto/koto.env`

- 路径：`/etc/koto/koto.env` — `daemon/install.go` 常量 `envFilePath`。
- 由 systemd 通过渲染出的 unit `/etc/systemd/system/koto.service` 中的
  `EnvironmentFile=` 加载 — `daemon/install.go` `renderUnit`、常量
  `unitPath`。
- 由 `koto install` 写出（`daemon/install.go` `writeEnvFile`）。重跑
  install 会**保留**文件中已有的值（`writeEnvFile` 中对 `existing` 的
  合并循环）。文件存在但不可读时，install 拒绝执行：`refusing to
  rewrite it: re-running install must preserve the values set there`。
- 安装器写入的文件头：``Edit and `sudo systemctl restart koto` to apply.``（`writeEnvFile`）。
- 解析：每行 `KEY=VALUE`，空行和 `#` 注释忽略，键和值去除首尾空白 —
  `daemon/install.go` `parseEnvFile`。

`writeEnvFile` 写入的键（`defaults` 与 `optional` 两个切片）：

总是写入（已设置的值被保留）：

- `KOTO_HOME` — 默认 = install 的 `-state` 目录，其默认为
  `/var/lib/koto`（`daemon/install.go` 常量 `defaultStateDir`、标志
  `state`）。
- `KOTO_BIND=127.0.0.1`
- `KOTO_PORT=8443`

可选（为空时以注释形式写成 `#KEY=`）：

- `KOTO_HOST_CPUS`
- `KOTO_HOST_MEM_MIB`
- `ANTHROPIC_API_KEY` — 有意**不**从 `<state>/creds` 预填
  （`writeEnvFile` 中的注释）。
- `KOTO_CLAUDE_BIN` — 设为安装时解析出的 claude CLI 路径
  （`installClaudeBin`）；与其他键不同，这里写入的是新解析的值而不是
  文件里的旧值（`kv[0] != claudeBinEnv` 分支）。

该 unit 还设置 `Environment=HOME=<state dir>`、`WorkingDirectory=<state
dir>` 和 `ExecStart=/usr/local/bin/koto daemon`（`renderUnit`）。

---

## 2. 环境变量

### 2.1 Daemon（`koto daemon`）

- `KOTO_HOME` — 状态根目录。设置时会转为绝对路径；未设置时 daemon 使用
  其**当前工作目录** — `daemon/daemon.go` `kotoHome`、`here`。派生路径
  （`initPaths`）：`groups/`、`groups.json`、`schedules.json`、
  `goals.json`、`run/`、`metrics.jsonl`。
- `KOTO_BIND` — gRPC 监听地址。默认 `127.0.0.1` — `daemon/daemon.go`
  `daemonMain`。非环回地址会产生姿态（posture）警告
  `KOTO_BIND=<v> exposes the control plane beyond this host…` —
  `daemon/posture.go`。
- `KOTO_PORT` — gRPC 监听端口。默认 `8443` — `daemon/daemon.go`
  `daemonMain`。guest 对该端口（以及无条件对 `8443`）的出站访问会被
  拒绝 — `daemon/proxy.go` `egressTargetAllowed`。
- `KOTO_PORTS_BIND` — group 发布的 `ports` 在宿主上绑定的地址。默认
  `127.0.0.1` — `daemon/fc.go`（`portBind := envOr(...)`）。
- `KOTO_HOST_MEM_MIB` — 整个 fleet 的内存上限，单位 MiB。`0` = 不限；
  未设置 = `/proc/meminfo` MemTotal 的 90%（`fcHostMemDefaultPct =
  90`）；非法值记录日志 `KOTO_HOST_MEM_MIB=%q invalid; using derived
  default` — `daemon/fchostmem.go` `fcHostMemInit`。若容量完全无法探知
  且变量未设置，daemon 拒绝 VM 生成（`REFUSING VM spawns until
  KOTO_HOST_MEM_MIB is set (use 0 for explicitly unlimited)`）。
- `KOTO_CLAUDE_BIN` — 执行 OAuth 令牌刷新的 claude CLI。默认裸名
  `claude`（PATH 查找） — `daemon/claudebin.go` `claudeBin`、常量
  `claudeBinEnv`。
- `ANTHROPIC_API_KEY` — 设置时优先于密钥文件
  `<state>/creds/anthropic-api-key` — `daemon/proxy.go` `currentAPIKey`、
  `proxyInitPaths`。文件每个请求都会重读。
- `CRED_PATH`（历史遗留名称，无 `KOTO_` 前缀）— OAuth 凭据文件。默认
  `<state>/.claude/.credentials.json`（若存在），否则
  `$HOME/.claude/.credentials.json` — `daemon/proxy.go` `proxyInitPaths`。
- `PROXY_PORT`（遗留）— 每 group proxy 端口分配的基准。默认 `8787` —
  `daemon/daemon.go` 变量 `PORT_BASE`、`initPaths`。
- `BIND`（遗留）— 进程内 proxy 监听器的绑定地址。默认 `127.0.0.1` —
  `daemon/daemon.go` `daemonMain` → `proxyStart`。
- `INVOCATION_ID`（由 systemd 设置）— 仅用于措辞一条 posture 发现
  （`the daemon is not running under koto.service`）—
  `daemon/posture.go`。

### 2.2 安装器（`koto install`）

- `KOTO_HOST_CPUS` — fleet 的 CPU 上限（整核数），渲染进 unit 为
  `CPUQuota=<n×100>%`。默认 `nproc-1`（最小 1）；`0` = 不限（不写该
  指令）— `daemon/install.go` `hostCPUs`、`installCPUQuota`。它读取的
  是**安装器**的进程环境（`hostCPUs` 中的 `os.Getenv`）；运行中的
  daemon 不读它。仅写在 koto.env 里的值是否能进入渲染出的 unit 为
  **unverified**（未找到把 koto.env 加载进安装器环境的代码路径）。
- `KOTO_HOME` — `stateDirOf` 的默认状态目录（`daemon/install.go`）。

### 2.3 客户端（`koto ctl`，以及向导冒烟测试）

来自 `daemon/ctl_cli.go`（`ctlUsage` 的 env 行、`ctlClient`、
`ctlCredsDir`、`newKotoClient`）：

- `KOTO_ADDR` — 连接目标。默认 `127.0.0.1:8443`。
- `KOTO_CREDS_DIR` — PKI 目录。解析顺序（`ctlCredsDir`）：
  `KOTO_CREDS_DIR`，否则 `./creds`（若存在），否则 `$KOTO_HOME/creds`，
  否则 `/var/lib/koto/creds`（若存在），否则 `creds`。
- `KOTO_CLIENT` — 身份名。默认 `agent`。选择 creds 目录中的
  `client-<name>.crt`、`client-<name>.key`、`token-<name>`。
- `KOTO_CERT`、`KOTO_KEY`、`KOTO_CA` — 覆盖各个文件路径（默认
  `client-<name>.crt`、`client-<name>.key`、`ca.crt`）。
- `KOTO_TOKEN` — bearer 令牌的**值**（不是路径）。未设置时读
  `token-<name>`；该文件也缺失时：``no token: set KOTO_TOKEN or provide <path> (mint with `make pki-client NAME=<name> ROLE=agent`)``。
- `KOTO_SERVER_NAME` — 当连接目标不在服务器证书 SAN 中时的 TLS
  server-name 覆盖。
- TLS 最低版本为 1.3（`newKotoClient` 中的 `tls.VersionTLS13`）。

### 2.4 `koto tui`

`daemon/tui_cmd.go`：标志 `-state` 默认 `$KOTO_HOME`，否则
`/var/lib/koto`。它读取 `<state>/creds/token-tui` 并以如下环境 exec
`koto-tui`：

- `KOTO_CA=<state>/creds/ca.crt`
- `KOTO_CERT=<state>/creds/client-tui.crt`
- `KOTO_KEY=<state>/creds/client-tui.key`
- `KOTO_TOKEN=<contents of token-tui>`
- `KOTO_ENDPOINT=$KOTO_ADDR`（默认 `127.0.0.1:8443`）
- `KOTO_SERVER_NAME=koto-daemon`
- `KOTO_PROMPTS_DIR=<state>/prompts`
- `SOCK_PATH=<state>/run/tui/koto.sock`
- `KOTO_TUI_TERM=$TERM`（默认 `xterm-256color`）
- `KOTO_SCRIPTS_DIR=<state>/scripts` — 仅当该目录存在时。

令牌缺失的错误：``no TUI token in <creds> — mint one with `koto pki client -creds <creds> tui` ``。

### 2.5 `koto update`

- `KOTO_REPO_URL` — 完整仓库 URL（去掉尾部 `/`）。
- `KOTO_REPO` — `owner/name`，默认 `jpzk/koto`；URL 变为
  `https://github.com/<repo>`。
— `daemon/update.go` `updateRepoURL`（与 `install.sh` 一致）。

`install.sh` 本身另外读取 `KOTO_KEY_FPR`、`KOTO_KEYSERVER`、
`KOTO_ARCH`（默认 `x86_64`）、`KOTO_VERSION`（默认 `latest`）、
`KOTO_DEST`、`KOTO_RELEASE_PUBKEY` — `install.sh` 文件顶部的赋值。

### 2.6 内部变量（非运维设置）

这些变量出现在 `daemon/` 代码中，但由 koto 自己设置；请勿手动设置。

- `KOTO_USERNS_READY` — userns 引导阶段标记（`unset`/`1`/`2`）—
  `daemon/userns.go` 常量 `usernsEnv`。
- `KOTO_JAIL_MAX_UID` — 跨引导 re-exec 传递的 per-VM uid 区段上限
  （钳位后）；仅接受 `(30000, 60000]` 区间 — `daemon/fcjail.go` 常量
  `jailMaxEnv`、`fcJailBaseUID`、`fcJailMaxUID`。
- `KOTO_DEFAULT_CLAUDE_MODEL`、`KOTO_DEFAULT_VENICE_MODEL` — 以
  `defaultClaudeModel` / `defaultVeniceModel` 的值注入 guest init 环境 —
  `daemon/fc.go`（init 的 `env` map）、`daemon/groups.go`。同时注入：
  `ANTHROPIC_BASE_URL=http://127.0.0.1:18888`（`fcGuestProxyTCP`），
  以及当 group 的 network 不是 `none` 时的
  `NO_PROXY`/`no_proxy=127.0.0.1,localhost`。
- `KOTO_SESSION` — 由 `entrypoint.sh` 在 guest 内按 turn 导出；daemon
  从 guest 进程环境中读取它来归属 job — `daemon/jobs.go`、
  `daemon/send.go`。
- `KOTO_ENDPOINT`、`KOTO_PROMPTS_DIR`、`KOTO_SCRIPTS_DIR`、
  `KOTO_TUI_TERM` — 由 `koto tui` 为 TUI 子进程设置（见 2.4 节）。

---

## 3. 每 group 配置（`config.json`）

- 位置：`<state>/groups/<group>/.cs/config.json` —
  `daemon/config.go` `groupConfigPath`。在每 group 锁保护下原子写入
  （临时文件 + rename，mode 0600）— `updateGroupConfig`。
- group 必须存在：`no such group: <g> (spawn it first)`；非法名称：
  `invalid group name` — `daemon/config.go` `configCmd`。
- 未知/非法的值被**静默忽略**（保留旧值）— `daemon/config.go`
  `applyConfig`。
- 值为 `null`、`""` 或 `[]` 时**清除**该键 — `isClear`。
- 读取返回填入默认值后的*生效*配置 — `effectiveConfig`。它还报告一个
  只读的 `io` 字段（`<MiB>/s / <ops>ops`）。

### 键

- `provider` — `claudesdk` | `venice`。默认 `claudesdk`
  （`daemon/groups.go` 常量 `defaultProvider`、`ensureProviderConfig`）。
- `model` — 自由字符串，字符集 `^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`，最长
  128 字符（`configIdentRE`、`configMaxIdent`）。未设置时的默认值：
  claudesdk 下 `claude-sonnet-5`，venice 下 `kimi-k2.5`
  （`daemon/groups.go` `defaultClaudeModel`、`defaultVeniceModel`）。
- `effort` — 字符集/长度规则与 `model` 相同。未设置时显示为
  `(default)`（`effectiveConfig`）。
- `network` — `none` | `wan` | `lan` | `full`（`daemon/fcnet.go` 常量
  `fcNetNone`…`fcNetFull`）。默认 `none`（无 NIC）。存储的其他任何
  字符串读回为 `none`（`daemon/groups.go` `groupConfig.network`）。下次
  spawn（`/restart`）生效；降为 `none` 还会在 proxy 的下一个请求立即
  生效（`applyConfig` 中的注释）。遗留键 `internet`：`full` → `wan`、
  `none` → `none`，迁移为 `network`。
- `size` — `small` | `medium` | `large` | `xlarge`。默认 `small`。下次
  spawn 生效。磁盘只增不减。预设（`daemon/fc.go` `fcSizePresets`；
  vCPU、RAM MiB、workspace 磁盘、IO MiB/s、IO ops）：
  - `small`：2、1024、8 GiB、100、15000（`fcDefaultVcpus`、
    `fcDefaultMemMiB`、`fcWorkspaceBytes`）
  - `medium`：2、2048、12 GiB、150、22500
  - `large`：4、4096、16 GiB、200、30000
  - `xlarge`：8、8192、24 GiB、250、37500
- `root` — `yes` | `no`。默认 `no`。guest 内免密 sudo + 持久 root
  overlay；下次 spawn 生效（`daemon/fc.go` `groupRoot`）。
- `kvm` — `yes` | `no`。默认 `no`。向 guest 暴露 VMX/SVM（嵌套虚拟
  化）；下次 spawn 生效（`daemon/fc.go` `groupKVM`）。注意：`koto ctl`
  usage 文本未列出，但标志 `-kvm` 存在（`daemon/ctl_cli.go`，
  `fs.String("kvm", …)`）。
- `autostart` — `yes` | `no`。默认 `no`。仅在 daemon 启动时读取
  （`autostartGroups`）；group 的 `/restart` 不会应用它。
- `ports` — JSON 整数数组或逗号分隔字符串（`8080,3000`）。每个端口须
  在 `[1024, 65535]` 内；重复和越界条目被丢弃；最多保留 32 个
  （`configMaxPorts`）。在宿主上发布到 `KOTO_PORTS_BIND`（默认
  `127.0.0.1`）。

yes/no 键按关闭失败（fail closed）处理：缺失、不可解析或其他任何值均
为 `no`；手工编辑的文件可用 JSON 布尔值 — `daemon/groups.go`
`groupConfigBool`。

### 谁能设置什么

- **姿态（posture）键** = `network`、`internet`、`root`、`ports`、
  `size`、`autostart`、`kvm`。在 gRPC 上，携带其中任何一个的 Config
  请求按合成的仅限管理员动词 `config_posture` 授权 —
  `daemon/auth.go` `postureVerb`、`daemon/acl.go` `adminOnlyVerbs`。
- 在 guest 内 ctl 平面上，`config_set` 仅限 main（`ctl: verb not
  allowed for non-main groups: config_set`），且拒绝所有姿态键：`ctl:
  <key> is an operator-only setting — ask the operator (TUI /config, or
  koto ctl config)` — `daemon/ctl.go` `configPostureKey`。因此 main 在
  那里只能设置 `model`、`effort`、`provider`。
- 角色详见 [acl-and-auth.md](acl-and-auth.md)。
