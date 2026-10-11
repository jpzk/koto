译自 ../groups-goals-schedules.md；以英文版为准。

# 会话、后台任务、目标、计划任务与 report/reply

本页讲一个组存在之后其内部如何运转：聊天会话、turn 队列、`cs-job`
后台任务、目标循环、cron 计划任务，以及 `main` 与对端组之间的一次性
报告窗口。动词本身（参数、错误字符串、谁可以调用）见
[ctl-reference.md](ctl-reference.md)。本页解释它们做什么。

像 `daemon/goals.go` 这样的路径相对于仓库根目录。每条论断都给出可供
grep 的函数或常量名。

## 1. 会话

一个组（一个 microVM、一个 workspace）可以承载多个独立对话，称为
**会话**。

- **默认会话。** 线上以 `""` 表示组的默认会话。`"-"` 和 `"default"`
  是它的别名（`normalizeSession`，`daemon/sessions.go`）。
- **命名规则。** 任何其他名字必须匹配
  `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`（`sessionNameRE`）。不匹配时错误为
  `invalid session name (must match [A-Za-z0-9][A-Za-z0-9_-]{0,31})`
  （`normalizeSession`）。
- **并发。** 会话内 turn 逐个运行，会话之间并行。一个组最多同时运行
  `groupSlots = 10` 个 turn（`daemon/queue.go`）。
- **会话状态。** 每个会话保有自己的对话。guest 在
  `/workspace/.cs/sessions/<name>.id` 下为每个名字固定一个 claude 会话
  id（`daemon/sessions.go` 中的注释）。做固定的脚本
  （`entrypoint.sh`）不在本仓库跟踪范围内，所以这个细节在此
  **未经验证**。
- **注册表。** 宿主机在 `groups/<g>/.cs/sessions.json` 中保存具名会话
  列表（`sessionRegPath`）。
  - 向一个会话发送会把它加入列表（`registerSession`）。
  - 列表最多保存 `sessionRegMax = 256` 个名字。此后新名字不再列出，
    守护进程记录
    `session registry is full (256); "<name>" will not be listed`。
  - 注册表是建议性的 UI 状态。guest 的 id 文件才是真正的记录
    （`sessionRegPath` 上方的注释）。
- **保留的 `goal-` 前缀。** 任何以 `goal-` 开头的名字属于目标循环
  （`goalSessionPrefix`、`isReservedSession`）。客户端和 ctl 平面不能
  向这些会话发送或清除它们：
  - ctl `send` 发入其中会失败：
    `ctl: session <s> is reserved for the goal loop`（`ctlDispatch`，
    `daemon/ctl.go`）。
  - 指向保留会话的 `job_done` 和 `notify` 改为投递到默认会话
    （`daemon/notify.go` 中的 `recordJobDone`、`ctlDispatch` 中的
    `notify` 分支）。

### turn 队列限制（`daemon/queue.go`，`enqueue`）

每个 turn 都经过 `enqueue`：操作员发送、计划任务触发、任务回调、目标
turn 和报告。

- **消息大小：** 最多 `sendMsgMax = 1 << 20` 字节（1 MiB）。更大的消息
  失败：`message is <n> bytes; the limit is <max>`。
- **每组排队字节数：** 最多 `sendQueuedBytesMax = 16 << 20`
  （16 MiB）。超过时：
  `group "<g>" already has <n> bytes queued (max <max>); retry later`。
- **每会话排队消息数：** 最多 `sendQueueDepth = 64`。超过时：
  `group "<g>" send queue full (64 pending); retry later`。
- **组正在停止：** 错误为
  `group "<g>" is stopping; retry once it is down`。
- **卡死（wedged）的会话：** 若会话的上一个 turn 卡住且组未能恢复，
  错误为 `errSessionWedged`。其文本以
  `this conversation's previous turn is wedged and the group could not
  self-heal` 开头。

## 2. 后台任务（`cs-job`）

`cs-job` 是一个在 guest 内运行的 POSIX shell 脚本
（`sidecar/cs-job`）。它在自己的进程会话（`setsid`）中启动命令，
使命令在 turn 结束后仍然存活。守护进程只读取任务状态并接收完成回调
（`daemon/jobs.go`、`daemon/notify.go`）。

### 命令

脚本中的用法行列出了这些命令：

- **`run [--no-notify] -- <cmd...>`**
  - 打印任务 id 并立即返回。
  - 回调默认开启。`--no-notify` 关闭它们。`--notify` 被接受但不做
    任何事。
  - 不带命令时失败：`cs-job run: no command`。
- **`spawn [--no-notify] <prompt...>`**：等同于
  `run -- cs-subagent <prompt...>`。`cs-subagent` 做一次 LLM 调用：
  它把最终答案打印到 stdout，进度打印到 stderr
  （`sidecar/cs-subagent`）。
- **`list`**：打印列 `ID STATUS RC SESSION CMD`。命令被截到 80 字节。
- **`wait <id> [secs]`**：等到任务完成。默认预算为 600 秒。它删除任务
  的 `notify` 标记，因此**等待会取消回调**。超时时：`cs-job: wait
  timeout (<budget> s) — job <id> still running`（退出 1）。
- **`status <id>`**：打印任务状态。状态文件保存 `running` 或 `done`。
  没有状态文件时打印 `unknown`。
- **`peek [-n N|-new] <id>`**：立即返回。先打印一行头部
  `<id> <status> rc=<rc> bytes=<n> age=<s>s`，然后是输出：
  - 默认最后 40 行。
  - 带 `-new` 时，只有自上次 peek 以来新增的字节。偏移保存在
    `<jobdir>/peek`。
  - 无论哪种，输出上限为 `CS_PEEK_MAX` 字节（默认 8000）。
- **`logs <id>`**：打印任务的 stdout 和 stderr 合并输出。
- **`rm <id>`**：向任务的进程组发送 SIGTERM，等待最多约 2 秒
  （20 × 0.1 秒），若仍存活则发送 SIGKILL，然后删除任务目录。
- **`clean`**：删除所有任务目录，运行中的任务和回调仍在发送中
  （`notifying` 标记）的任务除外。

### 规则与磁盘布局

- **任务 id。** id 是 `mktemp` 的基名。任何非字母数字的 id 都被拒绝：
  `cs-job: invalid job id: <id>`（`chkid`）。守护进程的检查是
  `jobIDRE = ^[A-Za-z0-9]{1,32}$`（`daemon/jobs.go`）。
- **并发上限。** 最多 `CS_MAX_JOBS` 个任务同时运行（默认 8，
  `MAX=${CS_MAX_JOBS:-8}`）。超过时：
  `cs-job: job cap reached (<n>/<max> running); wait or raise CS_MAX_JOBS`。
- **任务目录。** 每个任务得到 `/workspace/.cs/jobs/<id>/`，包含
  `cmd`、`started`、`session`、`status`、`pgid`、`out`（stdout 和
  stderr 合并）、`rc`，以及 `notify` / `notifying` 标记。
- **会话归属。** 任务属于其启动时 `$KOTO_SESSION` 中的会话，未设置时
  为 `default`。
- **孤儿任务。** 脚本头部说被 VM 重启杀死的任务随后会被
  `entrypoint.sh` 重新标记为 `orphaned`。该文件不在本仓库中，所以这点
  **未经验证**。

### 完成回调

1. 命令退出时，任务运行 `cs-job _notify <id>`。这会向
   `/workspace/.cs/ctl` 写一行 `job_done`，带 `id`、`rc`、`total`
   （输出大小）、`session`，以及输出最后 1500 字节的 base64。
2. 守护进程最多保留该尾部的 `ctlMaxJobOut = 8 << 10` 字节
   （`daemon/ctl.go`）。它记录结果（`recordJobDone`）并立即刷新任务
   列表（`kickJobsRefresh`）。
3. 结果按（组，会话）分组。一个 `notifyDebounce = 3s` 的计时器在每个
   新完成时重新开始计时（`armNotifyTimerLocked`，`daemon/notify.go`）。
   触发时，所有待发结果作为**一个** turn 发往启动任务的会话。该 turn
   以 `Background job(s) completed:` 开头，并用 `> ` 引用每个尾部
   （`flushNotify`）。
4. 若该 turn 无法入队，`notifyRetryDelay = 30s` 后重试。
5. 缓冲上限：
   - 每个键最多 `notifyMaxPending = 64` 个结果。最旧的被丢弃。
   - 每组最多 `notifyMaxKeysPerGroup = 32` 个待发会话。多出来的会话的
     结果转入默认会话。

### 宿主机如何看到任务

宿主机不能挂载 guest 的 workspace。它通过 guest agent 运行一个小 shell
脚本来读取任务状态（`fcReadJobs`、`jobsListScript`）。

- **缓存：** 结果缓存 `jobsRefreshTTL = 3s`。
- **失败：** 读取失败后，宿主机等待 `jobsFailBackoff = 60s` 再重试。
  连续 `jobsFailWarnAfter = 3` 次失败后记录一条警告。
- **列表大小：** 最多列出 `jobsMaxPerGroup = 256` 个任务。
- **日志：** `JobLogs` 默认返回最后 `jobLogsDefaultTail` 字节
  （`fcJobLogs`）。各上限见 [protocol.md](protocol.md)。

## 3. 目标（目标循环）

目标告诉守护进程在一个组中持续运行 turn，直到一个独立的裁判（judge）
接受验收标准，或达到迭代上限。这些全部在 `daemon/goals.go` 中。

### 创建目标（`goalSetBy`）

- **必填字段：** 组、文本和标准。缺一个时错误为
  `group is required`、`goal text is required` 或
  `acceptance criteria are required`。
- **大小限制：** 文本和标准各限 `goalTextMax = 16 << 10` 字节。更大的
  输入被拒绝而非截断：`goal text is <n> bytes; the limit is <max>`。
- **不能在 main 上：** 在 `main` 上设目标被拒绝：
  `cannot set a goal on main`。
- **迭代上限：** `max_iterations` 为 `<= 0` 时变为
  `goalDefaultMaxIter = 20`。高于 `goalMaxIterCeil = 200` 的值被降到
  200。
- **活跃目标上限：** 一个组最多同时运行 `goalMaxActive = 8` 个目标。
  超过时：
  `group "<g>" already has <n> active goals; finish or cancel one first`。
- **旧记录：** 设置新目标会删除该组已结束的目标记录（met、cancelled、
  exhausted）。
- **id：** 目标 id 为 12 个十六进制字符（`newSchedID`、`goalIDLen`）。
- **持久化：** 目标保存在 `<state>/goals.json`（`GOALS_FILE`，
  `daemon/daemon.go`）。

### 名字与会话

每次目标运行得到两个专属会话，以目标的**名字**命名
（`goalSessionSlug`）。没有名字的旧记录用 id 代替。

- worker 在 `goal-<name>` 中运行（`goalWorkSessionFor`）。
- judge 在 `goal-<name>-judge` 中运行（`goalJudgeSessionFor`）。

名字如何选定（`resolveGoalNameLocked`）：

- 未给名字时，使用目标文本的 slug（`slugifyGoalName`）。
- **长度：** 最多 `goalNameMax = 16` 个字符。否则：
  `goal name must be at most 16 characters`。
- **须可用作会话名：** 两个派生会话名都必须通过 `sessionNameRE`。
  否则：`goal name "<n>" is not usable as a session name`。
- **不能长得像 id：** 匹配 `goalIDRE`（12 个十六进制字符）的名字会被
  拒绝。错误以 `goal name "<n>" looks like a goal id` 开头。
- **冲突：** 若名字已被某个聊天会话或另一个目标的会话占用，守护进程
  尝试 `-2`、`-3` 等，并裁剪基名使结果不超过 16 个字符。到 `-99` 后
  放弃：`could not find a free name near "<base>"`。

### 生命周期

状态是 `goalStatus*` 常量：`planning`、`awaiting_approval`、
`running`、`paused`、`met`、`cancelled` 和 `exhausted`。
`met`、`cancelled` 和 `exhausted` 是终态（`goalTerminal`）。

1. **计划（可选，默认开启）。**
   - 带 `plan` 时，目标从 `planning` 开始。守护进程运行一个计划 turn
     （`goalPlanPhase`，提示词 `goalPlanMsg`）并把目标移到
     `awaiting_approval`。
   - 它发送一条通知：`goal plan ready for review`。
   - **谁能批准：** 宿主机的 `GoalApprove` RPC，或 ctl 平面的
     `goal_approve`。ctl 平面只在组自己给自己设的目标上有效
     （`ctlGoalSelfSet`）。由 main 或操作员设在某组上的计划需要人类
     批准。
   - main 通过 ctl 平面以 `plan:false` 设目标时，会被改为先出计划
     （`ctlDispatch`，见 [ctl-reference.md](ctl-reference.md)）。
2. **迭代。**
   - 每个 `running` 迭代使 `Iteration` 增加。
   - 它清除 worker 会话的上下文（`clearGoalSession` →
     `clearSessionContext`）。宿主机转写（transcript）保留。
   - 它发送 worker 提示词（`goalWorkerMsg`）。提示词包含：
     - 目标目录 `/workspace/goal/<name>`（`goalDirFor`）；
     - 上一个 turn 的收尾报告（`LastHandoff`，由 `goalCaptureHandoff`
       从转写中提取，裁剪到其**最后** `goalNoteMax = 4000` 个字符）；
     - judge 上一次的反馈，若有（`LastFeedback`）。
3. **声明完成。** worker 在其 turn 运行期间发送 `goal_done` 来声明完成
   （`recordGoalDone`）。
   - 声明只在该目标的 turn 期间被接受。否则：
     `no goal turn in flight for "<g>"`。
   - 第一个声明胜出。
   - 不带 id 的声明只有当组内恰好有一个目标 turn 打开时才被接受
     （`resolveGoalWindowLocked`）。
4. **裁判。** judge 只在声明之后运行（`goalJudgeCheck`）：
   - 它的上下文被清除，然后得到 `goalJudgeMsg`。
   - 它必须用 `goal_verdict` 作答。
   - 没有给出裁决的 judge turn 重试一次。
   - 连续 `goalSilentJudgeMax = 3` 次声明都没有裁决后，目标以原因
     `judge` 被暂停。
   - 若裁决为未达标，理由被保存为 `LastFeedback`，循环继续。
5. **达标**（`goalMarkMet`）。
   - 状态变为 `met`。
   - 发送通知 `goal met`。
   - 一条 `[koto] goal finished: ...` turn 发往组的**默认**会话，
     引用 worker 的说明（`goalInformCoordinatorMsg`）。
6. **耗尽**（`goalMarkExhausted`）。
   - 当 `Iteration >= MaxIterations` 且没有任何声明被接受时，目标以
     `exhausted` 结束。它不会暂停。
   - 发送一条高严重级别通知 `goal exhausted`。
   - 一条 `[koto] goal ended WITHOUT completing: ...` turn 发往默认
     会话（`goalCoordinatorExhaustedMsg`）。

### 暂停、恢复与取消

- **暂停**（`goalPause`）：只能从 `running`。原因为 `operator`。
- **中断**（`goalInterrupt`）：只能从 `running`。它以原因
  `interrupted` 暂停，并同时取消 worker 当前的 turn。
- **恢复**（`goalResume`）：只能从 `paused`。目标回到之前的状态
  （`running`，或在计划期间被暂停则为 `planning`）。
- **批准**（`goalApprove`）：只能从 `awaiting_approval`。目标变为
  `running`。
- **取消**（`goalCancel`）：从任何非终态。目标变为 `cancelled`。

守护进程自行施加的暂停（`goalPauseWith`、`goalPauseOnStop`）：

- **`stalled`：** 上下文重置、入队或 turn 失败，或某个 turn 从未产出
  `turn_end`。放弃之前，入队会重试 `goalEnqueueRetries = 3` 次，间隔
  `goalRetrySleep = 30s`（`goalEnqueue`）。
- **`judge`：** 见上面第 4 步。
- **`stopped`：** 组被停止。通知说 `goal paused (group stopped)`。

排队的目标 turn 若其目标已不再是 `running` 或 `planning`，会被丢弃而
不是运行（`goalTurnShouldRun`）。错误文本为
`goal turn skipped (goal no longer active)`（`daemon/queue.go`）。

**销毁：** 销毁一个组会取消其目标并删除其记录
（`goalCancelOnDestroy`、`delGoalsFor`）。

**守护进程重启：** 处于 `running` 或 `planning` 的目标在守护进程启动
时取回它们的驱动器（`resumeGoalDrivers`，由 `daemon/daemon.go` 调用）。

### 解析动词指的是哪个目标

approve、pause、interrupt、resume 和 cancel 动词接受一个可选名字
（`resolveGoalLocked`）。名字匹配目标的名字或其 id。若省略：

- 恰好一个可能的目标时，使用该目标；
- 没有目标时：`group "<g>" has no goal`；
- 有多个时：`group "<g>" has <n> goals — name one of: ...`。

### 安全说明

`daemon/goals.go` 顶部的注释说，声明窗口防的是**意外，而不是恶意**。
组内所有会话共享一个 uid、一个 workspace 和一个 ctl FIFO，所以任何
turn 都能写出一个声明。当隔离必须对抗敌意 agent 时，请使用分开的组。

## 4. 计划任务（cron）

计划任务在 `daemon/schedules.go` 中，解析器在 `daemon/cron.go` 中。
计划任务保存在 `<state>/schedules.json`（`SCHED_FILE`），权限 `0600`
（`saveSched`）。

### cron 语法（`parseCron`、`parseField`）

- **五个字段：** 分钟 `0-59`、小时 `0-23`、月内日 `1-31`、月份
  `1-12`、星期 `0-6`。
  - 星期 `7` 超出范围。
  - 月份和星期的名字不被接受，因为取值要经过 `strconv.Atoi`。
- **别名**（`cronAliases`）：`@yearly` / `@annually` → `0 0 1 1 *`、
  `@monthly` → `0 0 1 * *`、`@weekly` → `0 0 * * 0`、`@daily` /
  `@midnight` → `0 0 * * *`、`@hourly` → `0 * * * *`。没有
  `@reboot`。
- **字段语法：** `*`、`N`、`A-B`、用 `,` 的列表，以及用 `/S` 的步长。
  - `S` 必须在 1 与该字段范围大小之间。
  - `N/S` 表示从 N 到字段最大值，每 S 一次。
- **月内日与星期：** 两者都受限（都不是 `*`）时，**任一**匹配即算当天
  匹配。这是经典 cron 规则（`dateMatches`）。
- **时区：** 守护进程的本地时区。`time.Now()` 未带显式时区使用。
- **错误字符串：**
  - `cron: expected 5 fields, got <n> (...)`
  - `bad step "<s>" (must be 1..<n>)`
  - `range <a>-<b> out of bounds <lo>-<hi>`
  - 字段错误带前缀：`minute: `、`hour: `、`day-of-month: `、
    `month: ` 或 `day-of-week: `。

### 限制（`addSched`）

- **必填字段：** `group is required`、`msg is required`。
- **cron 表达式：** 最多 `schedCronMax = 256` 字节。
- **消息：** 最多 `schedMaxMsg = 16 << 10` 字节。
  超过时：`msg too long (<n> bytes; max <max>)`。
- **每组：** 最多 `schedMaxPerGroup = 100` 个计划任务。
  超过时：`schedule cap reached for <g> (100)`。
- **全守护进程：** 最多 `schedMaxTotal = 2000` 个计划任务。
  超过时：`daemon-wide schedule cap reached (2000)`。
- **必须终会触发：** 未来 4 年内没有任何匹配的表达式被拒绝：
  `cron "<expr>" has no fire within 4 years`（`next`）。
- **id：** 计划任务 id 为 12 个十六进制字符（`newSchedID`）。

### 触发

- **时机：** `cronLoop` 在每个挂钟分钟边界醒来，触发每个已启用且
  `NextDueAt` 已过的计划任务。
- **去向：** 触发把消息排入该组的**默认会话**（`fireSchedule` →
  `enqueueSend(it.Group, "", it.Msg)`）。它发出一个 `sched_fired`
  事件，用 `sched run` 手动启动时则是 `sched_run`（`runSchedNow`）。
- **不补发：** 守护进程停机期间错过的触发被跳过。加载时，`NextDueAt`
  从当前时间重新计算（`loadSched`）。
- **被拒绝的触发：** 若组的队列拒绝一次计划触发，该计划任务被重新置为
  到期并在下一个 tick 重试（`rearmSchedule`）。手动的 `sched run` 不
  重试。
- **没有未来匹配：** 若计划任务不再有任何匹配，它被禁用
  （`Enabled = false`，`cronLoop`）。
- **坏的保存条目：** `schedules.json` 中 cron 不再可解析的条目在加载
  时被丢弃，并带警告 `load: drop <id> (bad cron ...)`。
- **销毁：** 销毁一个组会删除其计划任务（`delSchedsFor`）。

谁可以添加、列出或更改哪些计划任务见
[ctl-reference.md](ctl-reference.md) §2.2。

## 5. main 与对端之间的 report/reply

这是 `daemon/report.go`。它让 `main` 把工作交给一个对端组并拿回
**一个**答复。

1. **武装。** `main` 以 `"reply":true` 发送（ctl `send`）。守护进程把
   消息附加 `reportRequestNote` 后入队。该说明告诉对端如何发送
   `report`。同一步中守护进程武装该组的报告窗口
   （`armReportAndEnqueue`）。
   - 若入队失败，什么都不会被武装。
   - 窗口打开期间再次发送会替换它（"最新者胜"）。
   - `from_session` 决定 main 的哪个会话收到报告。无效或保留的值表示
     main 的默认会话。
2. **报告。** 对端向 `/workspace/.cs/ctl` 写
   `{"cmd":"report","msg":"<base64>"}`（`ctlDispatch` 中的 `report`
   分支）。
   - 文本被净化并修剪。结果为空时失败：
     `ctl: report: empty message`。
   - 它在 UTF-8 边界上被截到 `reportMsgMax = 4000` 字节
     （`truncateRunes`）。被截断时，投递的 turn 以
     `(report clipped to 4000 bytes of <n>)` 结尾。
3. **投递。** `deliverReport` 向 main 排入一个以 `[report from <g>]`
   开头的 turn。对端的文本被逐行用 `> ` 引用（`reportQuoteBody`），
   随后是一条警告，说明内容由对端撰写。
4. **规则：**
   - **每次武装一个报告。**
     - 没有打开的窗口时，错误为
       `no reply pending: main must delegate with reply:true first (one report per delegation)`。
     - 一个报告正在投递中时，错误为
       `a report for this delegation is already being delivered (one report per delegation)`。
   - **main 积压。** 若 main 的队列拒绝该报告，窗口重新打开，对端
     得到 `main is backlogged, report not delivered — retry later: ...`。
   - **过期。** 窗口在武装后 `reportArmedMax = 24h` 过期。
   - **main 不能报告：** main 得到
     `ctl: main has no delegator to report to`。
   - **停止与销毁：** 停止或销毁对端会关闭其窗口
     （`daemon/groups.go` 中的 `disarmReport`）。

## 另见

- [ctl-reference.md](ctl-reference.md)：`sched*`、`goal*`、`send`、
  `report`、`job_done` 和 `notify` 的确切动词语法与错误。
- [protocol.md](protocol.md)：`Goal*`、`Sched*`、`Jobs`、`JobLogs` 和
  `JobTail` RPC。
- [architecture.md](architecture.md)：队列、guest 和 ctl FIFO 的位置。
