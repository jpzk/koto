译自 ../ctl-reference.md；以英文版为准。

# `koto ctl` 与 guest 内 ctl 平面 — 参考

koto 有两个都叫 "ctl" 的命令界面：

1. **`koto ctl`**：宿主机侧 CLI。它是守护进程 API 的 gRPC 客户端
   （`daemon/main.go`，`case "ctl"` → `ctlCliMain`；`daemon/ctl_cli.go`）。
2. **guest 内 ctl 平面**：microVM 内的 agent 每条命令向
   `/workspace/.cs/ctl` 写入一行 JSON，并从追加到
   `/workspace/.cs/ctl.out` 的回复中读取结果（`fcguest/ctl.go` 头部注释）。
   guest 把这些行经由 vsock 端口 9002 转发给守护进程（`fcguest/main.go`
   `portCtl`，`daemon/fc.go` `fcPortCtl`），守护进程在
   `ctlDispatch`（`daemon/ctl.go`）中应答。

两个界面各有自己的动词集合和规则。下面分别描述。

---

## 第 1 部分 — `koto ctl`（宿主机 CLI）

### 1.1 连接与认证

`koto ctl` 使用 mTLS 加 bearer token 连接守护进程（`ctlClient`、
`newKotoClient`、`tokenCreds`，位于 `daemon/ctl_cli.go`）。涉及的全部环境
变量见 [configuration.md](configuration.md) §2.3。角色、ACL 和 PKI 见
[acl-and-auth.md](acl-and-auth.md)。简述如下：

- `KOTO_ADDR`：默认 `127.0.0.1:8443`（`ctlClient`）。
- `KOTO_CLIENT`：客户端身份。默认 `agent`（`ctlClient`）。
- `KOTO_CREDS_DIR`：若未设置，`ctlCredsDir` 依次尝试 `./creds`（若存在）、
  `$KOTO_HOME/creds`（若设置了 `KOTO_HOME`）、`/var/lib/koto/creds`
  （若存在），最后回退到 `creds`。
- 该目录内的默认文件为 `client-<name>.crt`、`client-<name>.key`、
  `ca.crt` 和 `token-<name>`。`KOTO_CERT`、`KOTO_KEY`、`KOTO_CA` 和
  `KOTO_TOKEN` 可逐一覆盖（`newKotoClient`）。
- `KOTO_SERVER_NAME` 覆盖 TLS 服务器名（`newKotoClient`）。
- 最低为 TLS 1.3（`MinVersion: tls.VersionTLS13`，`newKotoClient`）。
- token 放在 `authorization: Bearer <token>` 元数据头中
  （`tokenCreds.GetRequestMetadata`）。
- 如果既没有 `KOTO_TOKEN` 也没有 token 文件，则失败并输出
  ``no token: set KOTO_TOKEN or provide <dir>/token-<name> (mint with `make pki-client NAME=<name> ROLE=agent`)``
  （`ctlClient`）。

对已安装守护进程的只读示例：

```sh
HOME=/var/lib/koto KOTO_CLIENT=tui KOTO_TOKEN=$(cat /var/lib/koto/creds/token-tui) koto ctl list
```

### 1.2 输出与退出码

- 一元（unary）动词将响应打印为**一行 protojson**，使用 proto 字段名
  （`ctlPrint`，`protojson.MarshalOptions{UseProtoNames: true}`）。
- RPC 失败时：`koto ctl: rpc: <err>`，退出码 1（`ctlPrint`）。
- 响应中 `"ok": false` 时：`koto ctl: daemon: <error>`，退出码 1
  （`ctlPrint`）。这是带内（in-band）应用错误契约。
- 用法错误退出码为 2（`ctlCliMain` 中各处的 `ctlFatal(2, …)`）。flag 解析
  错误同样退出 2，因为每个 flag set 都用 `flag.ExitOnError`。
- 流式动词（`tail`、`logs`、`watch`）每帧打印一行 protojson。
  干净的 EOF 退出 0；其他流错误打印 `koto ctl: stream: <err>`
  并退出 1（`ctlStreamJSON`）。
- 每个一元调用有 30 秒截止时间（`ctlCtx`）。流和 `ask` 使用自己的
  context，见下文。
- 不带动词运行 `koto ctl` 打印用法文本并退出 2。未知动词打印用法文本外加
  `koto ctl: unknown verb: <verb>` 并退出 2（`ctlCliMain`）。

### 1.3 消息参数（`-` = 标准输入）

对于 `send`、`ask` 和 `runscript`，消息或脚本是用单个空格连接的尾随参数。
单独一个 `-` 则改为读取标准输入，去除尾部换行，并把输入上限定为
`ctlStdinMax = 1 << 20` 字节（1 MiB，与守护进程 `daemon/queue.go` 中的
`sendMsgMax` 相同）。超过上限的输入会失败，输出 `stdin is larger than
1048576 bytes, which is more than the daemon accepts for one message`，
退出 2（`ctlMsgArg`）。

### 1.4 动词

动词按用法文本（`ctlUsage`）的顺序列出。除非另行给出 helper，这里提到的
每个 `case` 都在 `ctlCliMain` 的 `switch verb` 中。

#### 组生命周期

- **`list`**：所有组及其状态。RPC `List`。
- **`spawn [-provider P] [-model M] <group>`**：创建或启动一个组。
  `-provider`（帮助文本 `claudesdk|venice|openai`），`-model`（模型覆盖）。
  RPC `Spawn`。
- **`stop <group>`**：RPC `Stop`。
- **`interrupt <group>`**：RPC `Interrupt`。按 `drain` 上的注释，中止
  正在运行的 turn 是 `interrupt` 的职责。
- **`destroy <group>`**：RPC `Destroy`。
- **`restart <group>`**：RPC `Restart`。
- **`clear [-session S] <group>`**：RPC `Clear`。`-session` 只清除该聊天
  会话（`-` 或 `default` = 默认会话）。不带该 flag 时清除整个组。用法文本
  显示的是不带 flag 的 `clear <group>`，但该 flag 存在（`case "clear"`）。
- **`drain [-session S] <group>`**：丢弃排队的提示。用法文本说 VM、会话
  和进行中的 turn 均不受影响。`-session` 只排空该会话（`-`/`default` =
  默认）。不带它时，组内每个会话都被排空。RPC `Drain`。

`stop`、`interrupt`、`destroy` 和 `restart` 恰好接受一个参数：
``usage: koto ctl <verb> <group>``（`groupVerb`）。

#### 会话消息

- **`send [-session S] <group> <msg...>`**：消息入队并立即返回。
  `-session` 设定聊天会话（`""` = 默认）。该 flag 未出现在用法文本中
  （`case "send"`）。RPC `Send`。
- **`ask [-timeout D] [-json] [-session S] <group> <msg...>`**：发送、
  流式输出回复，并在 turn 结束时退出（`ctlAsk`）。
  - `-timeout`：默认 `30m`（`30*time.Minute`）。超时时输出
    `timed out after <D> waiting for turn end`，退出 1。
  - `-json`：以 protojson 行打印捕获到的事件，而不是纯文本。
  - `-session`：未出现在主用法文本中。出现在 `ask` 自己的用法错误里。
  - 行为（`ctlAsk`）：`ask` 在发送*之前*订阅该组（`SubscribeGroup`）。
    它忽略其他会话的事件，其中 `""`、`-` 和 `default` 指同一个会话
    （`normalizeAskSession`）。它从第一个文本等于该消息的 `prompt` 事件
    开始捕获。纯文本模式下，`done` 文本打印到 stdout，`err` 文本打印到
    stderr。在 `turn_end` 时以 0 退出。
- **`history [-limit N] [-before TS] <group>`**：`-limit`（int，最大事件
  数；`0` 表示不向守护进程发送限制，服务端默认值未经验证）、`-before`
  （浮点时间戳游标，不含端点）。RPC `History`。

#### 配置

- **`config <group>`**：不带 flag 时打印生效配置（`ctlConfig`）。
- **`config <group> [flags]`**：设置配置键。组必须放在最前，因为 Go 的
  flag 包在第一个非 flag 参数处停止解析。flag 之后的任何内容都会失败：
  ``config: unexpected args after group: [...] (flags follow the group)``。
  只有实际给出的 flag 才会被发送，因此 `-model ""` 会*清除*模型，而缺省
  的 flag 保持不变（`ctlConfig` 中的 `fs.Visit`）。RPC `Config`。flag
  如下：
  - `-model M`（`""` 清除）
  - `-provider claudesdk|venice|openai`
  - `-network none|wan|lan|full`
  - `-size small|medium|large|xlarge`
  - `-root yes|no`
  - `-effort E`
  - `-ports P`（逗号分隔列表）
  - `-autostart yes|no`
  - `-kvm yes|no`：嵌套虚拟化（guest 内的 `/dev/kvm`）。**未列在
    `ctlUsage` 中**，但定义于 `ctlConfig`。

  取值校验、默认值以及变更何时生效全部见
  [configuration.md](configuration.md) §4。`ctlConfig` 上方的注释说
  `network`/`size`/`root`/`ports` 在 `/restart` 时生效，
  `model`/`provider`/`effort` 在下一条消息生效，而 `autostart` 只在守护
  进程启动时读取。

#### 流与检查

- **`metrics [group]`**：RPC `Metrics`。不带组时请求中的组为空。
- **`resources`**：每组的宿主机侧磁盘、内存和 CPU，外加机群汇总。用法
  文本标注为 `(admin)`。不接受参数。RPC `Resources`。
- **`tail [-since N] <group>`**：组事件流。`-since N`（uint64）先重放
  `seq > N` 的环形缓冲帧再转为实时。RPC `SubscribeGroup`。
- **`logs`**：守护进程自身的日志流。无参数。RPC `SubscribeLogs`。
- **`watch`**：每次变化时输出组状态快照。无参数。RPC `WatchState`。
- **`jobs [group]`**：列出后台任务（`cs-job`），从 guest 实时读取。
  不带组表示所有组。RPC `Jobs`。
- **`job-logs [-tail N] <group> <id>`**：单个任务的元数据加输出尾部。
  按 flag 帮助，`-tail` 是从末尾起算的字节数，默认 4096，最大 65536。
  CLI 发送 `0`，守护进程应用默认值（`daemon/jobs.go` 中的
  `jobLogsDefaultTail = 4096`、`jobLogsMaxTail = 65536`）。RPC `JobLogs`。
- **`job-tail [-tail N] <group> <id>`**：实时跟踪任务输出。按 flag 帮助，
  `-tail` 是初始窗口字节数，默认 65536。0 或更小的值变为
  `jobLogsMaxTail`，更大的值被钳制到它（`daemon/grpc_server.go`，
  JobTail handler）。它把原始 `data` 块写到 stdout，收到 `end` 时退出 0，
  收到 `error` 时退出 1。RPC `JobTail`。

#### 计划任务（`ctlSched`）

- **`sched list [group]`**：RPC `SchedList`。
- **`sched add <group> <cron...> <msg...>`**：`cron` 要么是一个 `@alias`
  记号，要么恰好 5 个字段。其后的所有内容都是消息。5 字段形式下剩余参数
  不足 7 个时：`sched add: need 5 cron fields (or one @alias) + msg`。
  消息为空时：`sched add: empty message`。RPC `SchedAdd`。守护进程认识
  的别名（`cronAliases`，`daemon/cron.go`）是 `@yearly`、`@annually`、
  `@monthly`、`@weekly`、`@daily`、`@midnight` 和 `@hourly`。
- **`sched del|on|off|run <id>`**：RPC `SchedDel`、`SchedToggle`
  （`Enabled: sub == "on"`）和 `SchedRun`。

#### 目标（`ctlGoal`）

- **`goal set [-max N] [-plan=no] [-name x] <group> <goal> :: <criteria>`**。
  目标文本与验收标准由一个字面 `::` 参数分隔，两侧都必须非空。
  - `-max`：最大迭代次数。`0` 表示默认值，即
    `goalDefaultMaxIter = 20`。该值被钳制到
    `goalMaxIterCeil = 200`（`daemon/goals.go`，`goalSetBy`）。
  - `-plan`：`yes`（默认）或 `no`。其他值失败并输出
    `goal set: -plan takes yes or no`。
  - `-name`：简短的运行名，同时用作会话名。默认为目标文本的 slug。
  - 发送 RPC `GoalSet`。在 `main` 上设目标会被拒绝：`cannot set a goal
    on main`（`goalSetBy`）。
- **`goal list [group]`**：RPC `GoalList`。
- **`goal approve|pause|interrupt|resume|cancel <group> [name]`**：RPC
  `GoalApprove`、`GoalPause`、`GoalInterrupt`、`GoalResume` 和
  `GoalCancel`。`[name]` 从组的并发目标中选出一个，当只有一个候选时可以
  省略。`[name]` 未出现在 `ctlUsage` 中，但出现在该动词的用法错误里。
  `interrupt` 的意思是立即暂停并中止进行中的目标 turn（用法文本）。

#### 共享终端（`ctlShell`）

- **`shell <group> [session]`**：通过 RPC `AttachShell` 把一个交互式终端
  接到该组的 microVM 上。
  - stdin 必须是终端，否则 `shell: stdin is not a terminal: …`。
  - 终端被置于 raw 模式且清除 `ISIG`，因此 ctrl-C 能到达 guest。
  - **ctrl-]**（字节 `0x1d`）脱离并打印 `[detached — session left
    running]`。
  - SIGWINCH 发送 resize。没有 TTY 尺寸时，尺寸为 80×24
    （`ctlWinsize`）。
  - 输出经过 `shellFilter`。
  - 按用法文本，默认 tmux 会话是 `koto-shell`。具名聊天会话的 tmux
    会话是 `koto-shell-<name>`。

#### 仅限 admin 角色

用法文本把这些放在 "admin role only" 之下。`ctlAcl` 上的注释说 `acl_*`
动词在服务端硬编码为仅限 admin。授权模型见
[acl-and-auth.md](acl-and-auth.md)。

- **`runscript [-raw] <group> <script...>`**：以 `node` 身份在组的
  microVM 中运行一个 POSIX 脚本。输出实时流向 stdout。RPC `RunScript`。
  - `-raw` 只有作为**第一个**参数时才被识别（它不经 flag 包解析）。
  - 不带 `-raw` 时，守护进程会净化输出（`case "runscript"` 中的注释）。
  - `-raw` 且 stdout 是 TTY 时会被**拒绝**：``-raw writes the guest's bytes
    to your terminal unfiltered; redirect stdout to a file (koto ctl runscript
    -raw <group> ... > out.bin) or drop -raw to get the sanitized stream``，
    退出 2（`stdoutIsTTY`）。
- **`acl get`**：打印 ACL 文档。RPC `AclGet`。
- **`acl set <role> <verb>[:<groups>] ...`**：*替换*一个角色的授权（RPC
  `AclSetRole`）。
  - 每个记号是 `verb` 或 `verb:g1,g2`。裸 `verb` 或 `verb:*` 变为
    `"*"`。否则变为去除空白后的组名列表。
  - 空动词失败并输出 `acl set: empty verb in "<tok>"`。
  - 用法文本中的示例：
    `acl set ops stop:ghost restart:ghost list`。
- **`acl del <role>`**：RPC `AclDelRole`。
- 未知子命令打印 `unknown acl subcommand: <sub> (get|set|del)`。

### 1.5 用法文本与代码之间的差距

这些 flag 和参数存在于 `daemon/ctl_cli.go`，但缺失于 `ctlUsage`：

- `config -kvm`
- `send -session`
- `ask -session`
- `clear -session`
- `goal approve|pause|interrupt|resume|cancel` 上可选的 `[name]`

---

## 第 2 部分 — guest 内 ctl 平面（`/workspace/.cs/ctl`）

### 2.1 线上格式

- 每个请求是一行一个 JSON 对象。其 `cmd` 字段选择动词
  （`ctlDispatch` 中的 `cmdEnvelope`）。格式错误的 JSON 得到
  `json: <err>`。
- guest 把每一行翻译成一个 `CtlRequest`，并把回复译回 `ctl.out` 中一行
  扁平的 `ok`/`error` JSON（`fcguest/ctl.go` 中的 `ctlRequestFromJSON`、
  `ctlResponseJSON`）。帧走 vsock 端口 9002。见
  [../firecracker-vsock.md](../firecracker-vsock.md)。
- 调用方组（`owner`）由守护进程根据连接判定，而不是来自载荷
  （`ctlDispatch(owner, line)`，`daemon/fc.go` 中的 `fcCtlConn`）。
- 未知动词得到 `ctl: verb not allowed: <cmd>`（`ctlDispatch` 的
  default 分支）。

### 2.2 动词及其使用者

这里 "main" 指名为 `main` 的组（`ctlMainGroup`）。其他组都是非 main。
非 main 组调用仅限 main 的动词会得到 `ctl: verb not allowed for
non-main groups: <verb>`。

**仅限 main 的编排动词**

- **`spawn`** `{"group"}`。组名必须匹配
  `^[a-z0-9][a-z0-9_-]{0,31}$`（`ctlGroupRE`）。否则：
  `ctl: invalid group name (must match [a-z0-9][a-z0-9_-]{0,31})`。
  - 不能 spawn `main`：`ctl: cannot spawn 'main'`。
  - 最多 `ctlMaxSpawn = 100` 个注册的组：
    `ctl: spawn cap reached (100 groups)`。重新 spawn 已存在的组是允许的。
  - 总是以 `main:false` spawn。
  - 响应中带有 `port`。
- **`send`** `{"group","msg","session"?,"reply"?,"from_session"?}`。
  - 发给 `main` 失败：`ctl: cannot send to self`。
  - 消息入队并立即确认。
  - 目标为目标循环会话时被拒绝：
    `ctl: session <s> is reserved for the goal loop`。
  - `reply:true` 在入队的同时武装目标的一次性报告窗口，并把
    `reportRequestNote` 追加到消息（`armReportAndEnqueue`）。
  - 无效或保留的 `from_session` 回退到默认会话。
- **`stop`** `{"group"}`。不能停止 `main`：`ctl: cannot stop 'main'`
  （`stopGroup`）。
- **`list`**：响应中带有 `groups`（`listGroups`）。
- **`resources`**：缓存的宿主机侧采样，只读（`resourcesCtlResp`）。
- **`config_set`** `{"group"?, model/effort/provider}`。
  - 默认为调用方自己的组。
  - **posture 键会被拒绝**。它们是 `network`、`internet`、`root`、
    `ports`、`size`、`autostart` 和 `kvm`（`configPostureKey`）。错误为
    ``ctl: <key> is an operator-only setting — ask the operator (TUI /config, or koto ctl config)``。
  - 这里只能设置 `model`、`effort` 和 `provider`（`configCmd`）。
- **`tail`** `{"group","n"}`。返回对端 `.cs/log` 的最后 `n` 行：
  - `n` 默认 50，钳制到 200。
  - 它最多从文件末尾读取 256 KiB（`readTail(…, 256*1024)`）。
  - 速率限制：每组一个令牌桶，突发量 `ctlPollBurst = 30`，每
    `ctlPollRefill = 500ms` 补充一个令牌。超限时：
    `ctl: tail is being polled too fast — slow down (this is rate limited per group)`。
  - 日志由对端撰写，因此不会原样返回（`ctlTailFence`，
    `daemon/report.go`）。它先被清理（去除控制字符和 bidi 覆盖字符），然后
    每一行都像 report 正文一样加上 `> ` 引用前缀。最前面是
    `[tail of <group>]` 头部，最后是 report turn 所用的同一段对端内容尾注
    （`peerTrailer`）。只有带 `> ` 引用前缀的行才是对端的文本。

**任何组，仅限自身**

- **`job_done`** `{"id","rc","out"(base64),"total","session"}`。由
  `cs-job` 在任务完成时发送。
  - 输出尾部上限为 `ctlMaxJobOut = 8<<10`（8 KiB）。
  - `id` 必须不超过 64 字节，`rc` 不超过 8 字节（`ctlJobField`）。
  - 格式错误的会话回退到默认会话。
  - 记录结果（`recordJobDone`）并刷新任务列表。
- **`notify`** `{"severity","title"(b64),"msg"(b64),"session"?}`。
  - `high` 以外的任何严重级别都变为 `normal`。
  - 标题截断到 200 字节，消息截断到 2000 字节
    （`notifyTitleMax`、`notifyMsgMax`）。
  - 速率限制：突发量 5，之后每 15 秒一个（`notifyRateBurst`、
    `notifyRateRefill`）。
  - 错误：`ctl: notify: empty title and message`、
    `ctl: notify: rate limited`、`ctl: notify: backlog full`。
  - 保留会话被移到默认会话。
- **`report`** `{"msg"(b64)}`。仅限非 main 组。main 得到
  `ctl: main has no delegator to report to`。
  - 需要已武装的窗口，否则 ``no reply pending: main must delegate
    with reply:true first (one report per delegation)``。
  - 消息被净化并截断到 `reportMsgMax = 4000` 字节
    （`daemon/report.go`）。
  - 空消息：`ctl: report: empty message`。
- **`goal_done`** `{"id","note"(b64)}` 和 **`goal_verdict`**
  `{"id","met","reasons"(b64)}`。note 和 reasons 截断到
  `goalNoteMax = 4000`（`recordGoalDone`、`recordGoalVerdict`）。
- **`goal_set`** `{"group"?,"name"?,"text","criteria","plan"?,"max_iterations"?}`。
  - 非 main 调用方总是被强制指向自身。空的 group 表示调用方自己。
  - 在 `main` 上设目标会被拒绝：`ctl: cannot set a goal on main`。
  - **当 main 以 `plan:false` 设目标时，会被强制改为先出计划**，
    因为批准计划是人类的职责（`ctlDispatch` 中的日志行）。
- **`goal_approve`** `{"name"?}`。仅限自身。指名其他组会失败：
  ``ctl: goal_approve is self-only (a plan set on a peer needs a human)``。
  它还要求存在一个该组自己设置的、等待批准的目标。否则：
  ``ctl: goal "<slug>" was set by <who> — its plan needs a human
  to approve``（`ctlGoalSelfSet`，`daemon/goals.go`）。
- **`goal_status`** `{"group"?}`。非 main 调用方只能看到自己的目标。
- **`goal_pause` / `goal_interrupt` / `goal_resume` / `goal_cancel`**
  `{"group"?,"name"?}`。非 main 调用方被强制指向自身。main 可以操控
  任何组。
- **`sched_add`** `{"group"?,"cron","msg"}`。
  - 非 main 调用方被强制指向自身。
  - main 留空 `group` 时，默认指向 `main`（`addSched`）。
- **`sched_list`**：非 main 调用方只能看到自己的计划任务。
- **`sched_del` / `sched_toggle`（`"enabled"`）/ `sched_run`** `{"id"}`。
  非 main 调用方必须拥有该计划任务，否则 `ctl: not your
  schedule`（`ownsSched`）。未知 id 落入底层的 "not found" 错误。

### 2.3 与宿主机 CLI 的差异

- 宿主机的 `koto ctl config` 在 ACL 允许的前提下可以设置 posture 键。
  ctl 平面的 `config_set` 不能（`configPostureKey`）。
- ctl 平面没有 `destroy`、`restart`、`runscript`、`acl` 或 `shell`
  动词。它们都不出现在 `ctlDispatch` 的 switch 中。
