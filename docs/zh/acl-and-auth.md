译自 ../acl-and-auth.md；以英文版为准。

# 认证与授权（gRPC 控制平面）

本页讲谁可以调用守护进程的 gRPC API（TUI、Android、`koto ctl`、脚本），
以及每个调用方可以做什么。guest 内 ctl 平面（`/workspace/.cs/ctl`）是
**另一条**授权轴，基于组身份，硬编码在 `daemon/ctl.go` 中。它不受
`acl.json` 管辖（`daemon/acl.go` 头部注释）。其规则见
[ctl-reference.md 第 2 部分](ctl-reference.md)。

下文所有路径都相对于状态根 `HERE`：设置了 `KOTO_HOME` 时为其值
（已安装模式，通常是 `/var/lib/koto`），否则为守护进程的工作目录
（`kotoHome`，`daemon/daemon.go`）。凭据文件位于
`<HERE>/creds/`（`credFile`，`daemon/auth.go`）。

## 1. 层次

每个调用按顺序通过三道检查（`daemon/auth.go` 头部注释、
`authFromCtx`、`aclCheck`）：

1. **mTLS**：服务器证书加必需的客户端证书。
2. **Bearer token**：认证，与客户端证书绑定。
3. **ACL**：按角色、动词和目标组授权。

该注释还提到这些层之下有一个 WireGuard overlay。它运行在 koto 进程
之外。

### 1.1 监听器

- 默认监听地址是 `127.0.0.1:8443`。`KOTO_BIND` 覆盖地址，`KOTO_PORT`
  覆盖端口（`daemon/daemon.go`，在 `go autostartGroups()` 之后）。见
  [configuration.md](configuration.md)。
- gRPC keepalive：服务器 `Time: 30s`、`Timeout: 10s`。强制策略为
  `MinTime: 10s` 加 `PermitWithoutStream: true`（`daemon/daemon.go`，
  `grpc.KeepaliveParams`）。
- 启动时守护进程记录 `kotod ready grpc=<addr> (mTLS+token)`，然后运行
  `authBindingPreflight`。

### 1.2 mTLS（`serverTLSConfig`，`daemon/auth.go`）

- 服务器出示 `creds/server.crt` 和 `creds/server.key`。
- 客户端证书是必需的，且必须链到 `creds/ca.crt`
  （`tls.RequireAndVerifyClientCert`）。最低版本为 TLS 1.3
  （`MinVersion: tls.VersionTLS13`）。
- **白名单（allowlist）。** 客户端叶子证书的 SHA-256 指纹（DER，小写
  十六进制）还必须出现在 `creds/clients.allow` 中
  （`VerifyPeerCertificate`、`loadClientAllow`）。
  - 行格式：`<sha256hex> <name>`。
  - 第一个字段以 `#` 开头的行被忽略。
- **每次握手重新读取。** 删除一行即可吊销该设备，无需重启守护进程。
- 握手错误：
  - `client cert <fp> not in allowlist`
  - `clients.allow: <err>`（例如文件缺失）
  - `no client certificate`

### 1.3 Bearer token（`authFromCtx`、`tokenIdentity`）

- token 从 `authorization` gRPC 元数据中读取。`Bearer ` 前缀被剥去。
- token 经 SHA-256 哈希后在 `creds/tokens.json` 中查找，该文件每次调用
  都重新读取（`loadTokenIdentities`）。
- **证书与 token 绑定（审计 L6）。** 客户端证书的 CommonName 必须等于
  `tokens.json` 中匹配到该 token 的键（`certIdentity`）。因此任一侧的
  吊销就足够：删掉 `clients.allow` 中的行，或删掉 `tokens.json` 中的
  条目。
- 失败返回 gRPC `Unauthenticated`：
  - `invalid or missing token`（日志记录为 `rejected call from <addr> (bad/missing token)`）
  - `no client certificate`
  - `client certificate and token name different identities`
- **启动预检**（`authBindingPreflight`）。对只出现在一侧的每个名字记录
  一条警告：
  - `identity "<n>" has an allowlisted cert but no tokens.json entry — its calls will be refused (mint one: koto pki client <n>)`
  - `identity "<n>" has a token but no cert named "<n>" in clients.allow — its calls will be refused`

### 1.4 `tokens.json` 的取值形态（`parseTokens`，`daemon/acl.go`）

该文件把 `clientid → credential` 做映射。接受三种取值形态：

- `"<sha256hex>"`：遗留的裸哈希。角色为 `admin`（`defaultRole`）。
- `{"hash": "<hex>", "role": "agent"}`：单个角色。
- `{"hash": "<hex>", "roles": ["reader", "ops"]}`：多个角色。两者都在时
  `roles` 胜过 `role`。

条目在两种情况下被跳过（它不为任何人提供认证）：

- 不匹配任何一种形态，或 `hash` 为空；
- 去除空白后没有任何角色。例如
  `koto pki client -role "" x` 写出 `"roles": null`，会被跳过。

哈希在查找前被转为小写。

## 2. PKI：铸造凭据

有两条等价路径。两者产出相同的工件（`daemon/pki.go` 头部注释）：

- **Go：** `koto pki`，被 `koto setup` 和 `koto install` 使用。不需要
  openssl 或 jq。
- **Makefile：** `make pki-init` 和 `make pki-client`，使用 openssl 和
  jq。

### 2.1 `koto pki`（`pkiCliMain`，`daemon/pki.go`）

```
usage: koto pki init   [-creds DIR] [-san DNS:a,IP:1.2.3.4]
       koto pki client [-creds DIR] [-role r1,r2] <name>
       koto pki server [-creds DIR] -san DNS:a,IP:1.2.3.4
```

默认值：`-creds creds`。`init -san` 默认为 `defaultServerSANs` =
`DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`。`client -role` 默认为
`admin`。

#### `init`（`pkiInit`）

- 仅当 CA 不存在时才创建它（`pkiEnsureCA`）：EC P-256，
  `CN=koto-ca`，有效期 3650 天。
- 若 `ca.crt` 存在而 `ca.key` 不存在，则拒绝并输出
  `<crt> exists but <key> does not — refusing to mint a new CA over it.`，
  后跟恢复/移开的建议。
- 签发服务器证书（`pkiServerCert`）：`CN=koto-daemon`，有效期
  825 天，ExtKeyUsage ServerAuth。SAN 必须是 `DNS:…` 或 `IP:…`。其他
  任何形式都失败：`bad SAN "<s>" (want DNS:… or IP:…)`。
- 服务器证书在以下情况重新签发：不存在、`server.key` 缺失、不可读或
  已过期、处于距到期 `pkiRenewWindow`（30 天）以内、尚未生效，或未链
  到 CA（`pkiServerCertNeedsReissue`）。SAN 不做比较。
- 仅当 `acl.json` 文件不存在时才播种它（`pkiCreateNew`，权限 0600），
  内容为：

  ```json
  {
    "agent": {
      "list": "*", "send": "*", "history": "*", "metrics": "*",
      "sched_list": "*",
      "subscribe_group": "*", "watch_state": "*"
    }
  }
  ```

- creds 目录以权限 0750 创建。

#### `client <name>`（`pkiClient`）

- **命名规则：** `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` 且不含 `..`
  （`pkiClientNameRE`、`pkiClientNameOK`）。否则失败：
  `invalid client name "<name>": letters, digits and ._- only, no path separators …`。
- **写出：**
  - `client-<name>.key` 和 `client-<name>.crt`：EC P-256，`CN=<name>`，
    825 天，ClientAuth。
  - 向 `clients.allow` 追加一行 `<fp> <name>`，除非该指纹已存在。
  - `token-<name>` 中的 32 字节随机十六进制 token（0600）。
  - `tokens.json` 中 `<name>` 之下的 `{"hash", "roles"}`。
- 更新 `tokens.json` 时持有 creds 目录上的 flock
  （`pkiLockCreds`），并经由临时文件加 rename 原子写入。
- 损坏的 `tokens.json` 失败并输出 `tokens.json is corrupt — fix it on disk first: <err>`。
- CA key 缺失时失败并输出 ``ca.key (run `koto pki init` first): <err>``。

#### `server -san …`

- 只重新签发服务器证书。CA 不动，所以现有客户端继续可用。
- 打印 `server cert reissued for <san> — restart the daemon to apply`。
- 不带 `-san` 时失败：`pki server: -san is required (e.g. -san …)`
  （退出 2）。

### 2.2 Makefile 路径（`Makefile`，目标 `pki-init` / `pki-client`）

- `make pki-init [SERVER_SAN=…]`：
  - `SERVER_SAN` 默认为 `DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`。
  - 若 `creds/ca.key` 存在则拒绝：
    `creds/ca.key exists — refusing to regenerate the CA …`。
  - 若 `creds/acl.json` 文件不存在，它播种同样的 `agent` 角色。
- `make pki-client NAME=<client> [ROLE=r1,r2]`：
  - `ROLE` 默认为 `admin`。
  - 它要求 `creds/ca.key` 存在，并用 `jq` 编辑 `tokens.json`。
  - 它写出与 `koto pki client` 相同的文件。
- 密钥在 `umask 077` 下生成，然后 `chmod 600`。

### 2.3 setup 创建的身份（`daemon/setup_steps.go`）

- setup 的 PKI 步骤以 `defaultServerSANs` 加上操作员输入的额外 SAN 运行
  `pkiInit`。
- 然后铸造两个客户端，**角色都是 `admin`**，各自仅在其证书不存在时
  创建：
  - `tui`
  - `agent`，即 `koto ctl` 的默认 `KOTO_CLIENT`。
- 因此 `agent` *客户端*在设计上就是 admin。注释引用了审计 M11，
  决定于 2026-09-05。
- 播种的 `acl.json` 中名为 `agent` 的*角色*是另一回事：它为用
  `koto pki client -role agent <name>` 铸造的受限身份而存在。

## 3. 授权：角色、动词、目标

### 3.1 模型（`daemon/acl.go`）

- 一个用户（clientid）有一个或多个角色，每个角色在目标上授予动词。
  生效权限是该用户所有角色的**并集**（`rolesAllowed`）。
- `creds/acl.json` 映射 `role → {verb → targets}`。`daemon/acl.go` 是
  依据；`pki-init` 上方的 Makefile 注释说法相同。
  - 目标是组名、`"*"` 或 `["*"]`（任意组），或一个列表。
  - 动词键为 `"*"` 时在其目标上授予所有动词，**但不包括**仅限 admin
    的动词（`grantFor`）。
  - 遗留的扁平形态 `{"agent": ["list", "send"]}` 表示这些动词作用于
    任意目标（`parseACL`）。
- `acl.json` 每次调用都重新读取（`loadACL`）。编辑角色或授权无需重启。

### 3.2 `admin` 角色

`admin` 是硬编码的（`grantFor`，`defaultRole = "admin"`）：

- 它可以在任何目标上调用任何动词。
- `acl.json` 中的任何 `admin` 键都被忽略。
- `acl.json` 缺失或损坏不可能把 admin 锁在门外。
- `acl set admin …` 失败并输出
  `the admin role is hardcoded and cannot be defined in the ACL`。
- `acl del admin` 失败并输出 `the admin role is hardcoded and cannot be deleted`
  （`aclSetRoleCmd`、`aclDelRoleCmd`）。

### 3.3 失败即关闭（fail closed）

以下每种情况请求都会被拒绝（`grantFor`、`roleAllowed`、
`parseACL`、`parseTargets`）：

- **未知角色：** 该角色不在 `acl.json` 中。
- **文件缺失：** 没有 `acl.json` 时，非 admin 角色一概不存在。
- **文件损坏：** 格式错误的 `acl.json`（`parseACL` 返回 nil）拒绝所有
  非 admin 角色。
- **角色格式错误：** 取值格式错误的角色不授予任何权限。
- **坏的授权值：** 单个坏的授权值，或值为 `null` 或 `""` 的授权，产出
  空目标集（审计 M35）。
- **未列出的动词：** 既未列出也不被 `"*"` 覆盖的动词。因此新 RPC 默认
  被拒绝。

### 3.4 动词

- 动词是 gRPC 方法名的 snake_case 形式，例如
  `/koto.Koto/SchedAdd` → `sched_add`（`verbFromMethod`）。
- RPC 列表见 `protocol/koto.proto` 中的 `service Koto`。

完整动词集，从 RPC 机械推导（因此像 `goal_list` 或 `job_logs` 这样的
名字不必逐字出现在 Go 源码中）：

- **生命周期：** `spawn`、`send`、`list`、`stop`、`interrupt`、`destroy`、
  `restart`、`history`、`config`、`metrics`、`clear`、`drain`
- **任务：** `jobs`、`job_logs`、`job_tail`
- **计划任务：** `sched_add`、`sched_list`、`sched_del`、`sched_toggle`、
  `sched_run`
- **目标：** `goal_set`、`goal_list`、`goal_approve`、`goal_pause`、
  `goal_interrupt`、`goal_resume`、`goal_cancel`
- **检查：** `resources`
- **ACL 管理：** `acl_get`、`acl_set_role`、`acl_del_role`
- **执行与流：** `run_script`、`attach_shell`、`subscribe_group`、
  `subscribe_logs`、`watch_state`

### 3.5 仅限 admin 的动词（`adminOnlyVerbs`）

这些动词不能通过 `acl.json` 授予，即使用 `"*"` 动词也不行。
只有 `admin` 能通过：

- `acl_get`、`acl_set_role`、`acl_del_role`。ACL 管理绝不能通过 ACL
  自身被委派出去。
- `run_script`：在 guest VM 中直接执行代码。
- `config_posture`：一个合成动词，见下一节。

### 3.6 `config` 与 `config_posture`（`postureVerb`，`daemon/auth.go`）

- 设置任何 **posture** 键的 `Config` 请求被重新标记为
  `config_posture`，它仅限 admin。posture 键为 `network`、
  `internet`、`root`、`ports`、`size`、`autostart` 和 `kvm`。
- 因此 `config` 授权只覆盖其余设置：`model`、`effort` 和 `provider`。
- 目标检查仍然适用。guest 内 ctl 平面自己就拒绝 posture 键
  （`daemon/ctl.go` 中的 `configPostureKey`；见
  [configuration.md](configuration.md)）。

### 3.7 定向与仅动词（`targetOf`）

一个动词当其请求消息携带组时为组作用域（"定向"）。

定向的请求类型及使用它们的 RPC：

- `SpawnReq`：Spawn
- `SendReq`：Send
- `GroupReq`：Stop、Interrupt、Destroy、Restart、Clear、Drain
- `HistoryReq`：History
- `ConfigReq`：Config
- `MetricsReq`：Metrics
- `JobsReq`：Jobs
- `JobLogsReq`：JobLogs
- `JobTailReq`：JobTail
- `SchedAddReq`：SchedAdd
- `SchedListReq`：SchedList
- `GoalSetReq`：GoalSet
- `GoalListReq`：GoalList
- `GoalGroupReq`：GoalApprove、GoalPause、GoalInterrupt、GoalResume、
  GoalCancel
- `SubscribeReq`：SubscribeGroup
- `RunScriptReq`：RunScript
- `ShellInput`：AttachShell。流上的每条消息都携带 `group` 并受检查。

规则：

- 定向动词只有当授权含 `"*"` 或点名该组时才通过
  （`roleAllowed`）。
- **组为空**的定向请求需要 `"*"` 授权，因为它跨所有组读取。例子：
  全局 `metrics`、不加过滤的 `sched_list`，以及不带组的 `jobs` /
  `goal_list`。空名字永远不会被存为目标（`parseTargets`）。
- **仅动词**（没有目标维度）：`list`、`watch_state`、`resources`、
  `subscribe_logs`、`sched_del`、`sched_toggle`、`sched_run` 和 `acl_*`
  动词。对该动词的任何授权，无论目标集如何，都放行调用。

### 3.8 仅动词读取的投影（`visibleTargets`）

对以下动词，授权的目标集收窄的是**答案**而非调用本身
（`daemon/grpc_server.go`）：

- **`list` 和 `watch_state`。** 只返回授权覆盖的组
  （`projectGroups`）。除非 `jobs` 授权覆盖该组，各组的任务记录也会
  被去掉。
- **`resources`。** 其他组被移除；除非授权是 `"*"`，宿主机机群总计
  （组数、运行中的组、已分配/预配字节、已承诺内存）只对可见的组重新
  计算（`projectResources`）。机器级字段（文件系统大小与剩余空间、
  内存上限和 MemTotal）保留。
- **`subscribe_logs`。** 只投递归属于被覆盖组的事件。守护进程级事件
  （组为空）被丢弃，除非授权是 `"*"`（`logVisible`）。
- **`metrics`。** `GlobalMetric` 字段（任意组中最新的记录）只为
  `metrics` 授权是 `"*"` 的调用方填充（`Metrics` handler，审计 M30）。

### 3.9 流

- **动词门。** 打开流时，调用方必须在某个角色中持有该动词
  （`authStream` 中的 `anyGrant`）。目标检查在收到每条请求消息时运行
  （`aclStream.RecvMsg`）。
- **复查。** 长寿流在发送时最多每 `aclStreamRecheck` = 5s 重新授权一次
  （`aclStream` 的 `revalidate` 方法，审计 M25）。
  - 身份按名字从 `tokens.json` 重新解析。若已消失，流以
    `Unauthenticated` `identity <name> has been revoked` 关闭。
  - 若角色不再允许该动词，流以 `PermissionDenied`
    `roles <r> may no longer call <verb> on "<group>"` 关闭。
- **准入上限。** `streamMaxPerIdentity = 512` 和
  `streamMaxGlobal = 2048`（`streamAdmit`）。超过任一上限，调用返回
  `ResourceExhausted` `too many live streams (per-identity 512, global 2048)`。

### 3.10 拒绝错误与日志（`aclCheck`）

- 拒绝返回 gRPC `PermissionDenied`：
  - `roles <r1,r2> may not call <verb> on "<group>"`（定向）
  - `roles <r1,r2> may not call <verb>`（仅动词）
- 每个决定都以 `acl` 来源记录日志：
  `<name> (roles …) allowed|denied <verb> [on "<group>"] from <addr>`。
- 组名有效时，定向决定归属到该组的日志（`aclLogGroup`）。

## 4. 管理 ACL

有三个仅限 admin 的 RPC：`AclGet`、`AclSetRole` 和 `AclDelRole`
（`daemon/grpc_server.go`）。CLI 上是 `koto ctl acl get|set|del`
（`ctlAcl`，`daemon/ctl_cli.go`；见 [ctl-reference.md](ctl-reference.md)）。

```sh
koto ctl acl get
koto ctl acl set ops stop:ghost restart:ghost list 'metrics:*'
koto ctl acl del ops
```

- **`acl set` 替换该角色的整个授权对象**（`aclSetRoleCmd`）。
- **角色名**遵循 `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`（`roleNameRE`）。
  否则：`invalid role name "<r>"`。
- **授权校验**（`validateGrants`）在第一个坏条目处拒绝整个请求：
  - 动词键必须是 `"*"` 或匹配 `roleNameRE`，否则 `invalid verb "<v>"`。
  - 目标必须是 `"*"` 或由匹配 `roleNameRE` 的名字（或 `"*"`）组成的
    列表。错误：`verb "<v>": target must be "*" or a list of groups` 和
    `verb "<v>": invalid target <t>`。
- **写入**持有 `aclFileLock`，经由 `creds/.acl.json.tmp` 加 rename
  原子完成，权限 0600（`writeACLDoc`）。
- **损坏的文件阻止变更：**
  `acl.json is corrupt — fix or remove it on disk first`（`readACLDoc`）。
- **对不存在的角色执行 `acl del`** 失败并输出 `no such role: <role>`。

## 5. 配方

**为脚本铸造受限身份。** 铸造一个客户端并让 `koto ctl` 指向它：

```sh
koto pki client -creds /var/lib/koto/creds -role agent ci
KOTO_CLIENT=ci koto ctl list
```

`KOTO_CLIENT` 选择 `client-<name>.{crt,key}` 和 `token-<name>`
（`newKotoClient`，`daemon/ctl_cli.go`）。

**把一个角色限制到单个组**，把 `"*"` 的值换成列表：

```sh
koto ctl acl set agent list:main send:main history:main subscribe_group:main watch_state:main
```

**吊销设备。** 删除它在 `creds/clients.allow` 中的行，**或**它在
`creds/tokens.json` 中的条目。由于证书/token 绑定，任一侧就足够。
新调用立即失败，打开的流在 5 秒内关闭（`aclStreamRecheck`）。

**从另一台机器访问守护进程。** 在不动 CA 的前提下添加 SAN，然后重启
守护进程：

```sh
koto pki server -creds /var/lib/koto/creds -san DNS:koto-daemon,DNS:localhost,IP:127.0.0.1,IP:192.168.1.20
```

除非设置了 `KOTO_BIND`，守护进程只监听 `127.0.0.1`。见
[configuration.md](configuration.md)。
