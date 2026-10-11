译自 ../protocol.md；以英文版为准。

# 线上协议

koto 有两份线上契约（wire contract），都是 protobuf，都位于 `protocol/`：

- `protocol/koto.proto`：**客户端 ↔ 守护进程**的 gRPC API（`service Koto`），
  由 `koto ctl`、TUI 和 Android 客户端使用。
- `protocol/guest.proto`：**守护进程 ↔ microVM guest** 之间经 vsock 的契约，
  只由守护进程和 guest 的 PID‑1 agent（`fcguest/`）使用。它从不离开
  host↔guest 这对 vsock。

生成的 Go 代码提交在 `protocol/pb` 下（`go_package
"koto-protocol/pb;pb"`）。用 `make proto-gen` 重新生成；`make
proto-verify` 重新生成后若 `protocol/pb` 有变化则失败（`git diff
--exit-code -- protocol/pb`，`Makefile`）。代码生成工具链在 `Makefile`
中被固定（pinned）：`PROTOC_GEN_GO_VER := v1.36.12`、`PROTOC_GEN_GO_GRPC_VER :=
v1.6.2`。

另见：[architecture.md](architecture.md)（各信道位于何处）、
[acl-and-auth.md](acl-and-auth.md)（谁可以调用什么）、
[ctl-reference.md](ctl-reference.md)（基于这些消息构建的 CLI 和 guest 内
动词）、[firecracker-vsock.md](../firecracker-vsock.md)（vsock 背景知识）。

---

## 1. 客户端 ↔ 守护进程：`service Koto`（gRPC）

### 1.1 传输层

在 `daemonMain`（`daemon/daemon.go`）中搭建：

- 普通 TCP 监听器（`net.Listen("tcp", addr)`），外面包一层来自
  `serverTLSConfig` 的 TLS（`grpc.Creds(credentials.NewTLS(tlsCfg))`）。
  客户端必须出示在允许清单（allowlist）内的证书（mTLS）。
- 每个调用都经过拦截器 `authUnary` / `authStream`。bearer token 从
  `authorization` 元数据键读取，可选的 `Bearer ` 前缀会被剥去
  （`authFromCtx`，`daemon/auth.go`）。token 的身份必须与客户端证书的 CN
  相同。失败返回 gRPC `Unauthenticated`，错误信息为 `invalid or missing
  token`、`no client certificate` 或 `client certificate and token name
  different identities` 之一。
- Keepalive：`keepalive.ServerParameters{Time: 30 * time.Second, Timeout:
  10 * time.Second}`，以及强制参数 `MinTime: 10 * time.Second,
  PermitWithoutStream: true`。
- `daemonMain` 中没有覆盖 `MaxRecvMsgSize` / `MaxSendMsgSize`，
  因此采用 grpc‑go 的默认值（具体是哪个 grpc‑go 版本的默认值未经核实；
  见 `daemon/go.mod` 中固定的运行时版本）。

完整方法名为 `/koto.Koto/<Rpc>`（`package koto;`，`service Koto`）。
一个 RPC 的 ACL 动词是它的 snake_case 名称：`verbFromMethod`
（`daemon/acl.go`）在每个内部大写字母前插入 `_` 并转小写，于是
`SchedToggle` → `sched_toggle`、`RunScript` → `run_script`、`AttachShell` →
`attach_shell`。ACL 的细节见 [acl-and-auth.md](acl-and-auth.md)。

### 1.2 约定（`protocol/koto.proto` 头部）

- 字段名为 snake_case。打印 JSON 的客户端使用
  `protojson.MarshalOptions{UseProtoNames: true}`（`daemon/ctl_cli.go`、
  `tui/daemon.go`），所以 JSON 键等于 proto 字段名。
- proto3 的零值不参与序列化。
- 每个 `*Resp` 都以 `bool ok = 1; string error = 2;` 开头。应用层错误
  以 `ok=false` **带内**（in band）放在 `error` 中传递；认证/ACL 失败和
  非法的流打开则是 gRPC status 错误。
- 时间戳（`ts`、`before`、调度时间）是 `double` 秒数。
- 自由形态的 JSON 对象（config、metrics）使用 `google.protobuf.Struct`。

### 1.3 RPC 列表

下列全部 36 个 RPC 都实现为 `daemon/grpc_server.go` 中 `kotoServer`
上的方法（通过 `pb.RegisterKotoServer` 注册）。

一元调用——group 生命周期与聊天：

- `Spawn(SpawnReq) → SpawnResp`。`SpawnReq{group, main, provider, model,
  size}`；`SpawnResp` 额外带 `int32 port`。
- `Send(SendReq) → BaseResp`。`SendReq{group, msg, image, image_mime,
  audio, audio_mime, session}`。按 proto 注释，图片会写入
  `groups/<g>/.cs/uploads/img-<ts>.<ext>` 并在消息中引用；
  音频 "CURRENTLY UNSUPPORTED"，带内拒绝。`session` 取 ""/`-`/
  `default` 表示默认会话。
- `List(ListReq) → ListResp`。
- `Stop`、`Interrupt`、`Destroy`、`Clear` `(GroupReq) → BaseResp`；
  `Restart(GroupReq) → SpawnResp`。`GroupReq{group, session}`；按 proto
  注释，`session` 只被 `Clear` 读取（"" = 整个 group，`-`/`default`
  = 默认会话，具体名字 = 该会话）。`Drain` 也按 `session` 定界（见下）。
- `Drain(GroupReq) → DrainResp{ok, error, dropped}`。只丢弃排队中
  （等待中）的提示；VM、记忆和进行中的 turn 不受影响。goal 会话从不被
  drain（`Drain` 上的 proto 注释）。
- `History(HistoryReq) → HistoryResp`。`HistoryReq{group, limit, before}`：
  `limit` 0 = 服务端默认值，`before` 0 = 末尾一页，否则返回
  `ts < before` 的事件。
- `Config(ConfigReq) → ConfigResp`。每项设置都是 proto3 `optional`，
  因此有三种状态：缺席 = 不变，出现且为 `""` = 清除，出现且有值 = 设置。
  字段：`model`、`effort`、`ports`、`provider`、`internet`
  （已弃用；"daemon maps full→network=wan"）、`size`、`root`、`network`、
  `autostart`、`kvm`。字段号 4 和 8 为 `reserved`（已移除的 skills
  功能）。允许的取值见 [configuration.md](configuration.md)。
- `Metrics(MetricsReq{group}) → MetricsResp`。

一元调用——job 与调度：

- `Jobs(JobsReq{group}) → JobsResp`。`group` 为 "" = 所有运行中的 group
  （需要 `*` 目标）。
- `JobLogs(JobLogsReq{group, id, tail}) → JobLogsResp{job, output,
  truncated}`。`tail` 0 ⇒ `jobLogsDefaultTail = int64(4096)`，上限为
  `jobLogsMaxTail = int64(65536)`（`daemon/jobs.go`）。
- `SchedAdd(SchedAddReq{group, cron, msg}) → SchedAddResp{item}`、
  `SchedList(SchedListReq{group}) → SchedListResp`、
  `SchedDel(SchedIDReq{id})`、`SchedToggle(SchedToggleReq{id, enabled})`、
  `SchedRun(SchedIDReq{id})`，后三者返回 `BaseResp`。

一元调用——goal（`daemon/goals.go`）：

- `GoalSet(GoalSetReq) → GoalResp`。`GoalSetReq{group, text, criteria,
  max_iterations, plan, name}`。`max_iterations` ≤ 0 ⇒
  `goalDefaultMaxIter = 20`，上限 `goalMaxIterCeil = 200`（`goalSetBy`）。
  `plan` 是 `optional bool`；缺席 = 先做计划。空 `name` 会从 `text`
  生成 slug。
- `GoalList(GoalListReq{group}) → GoalListResp`。`group` 为 "" = 所有
  group（需要 `*` 目标）。
- `GoalApprove`、`GoalPause`、`GoalInterrupt`、`GoalResume`、`GoalCancel`
  `(GoalGroupReq{group, name}) → GoalResp`。`name` 为 "" 时解析到唯一的
  候选 goal；"" 有歧义时报错。proto 注释称 `GoalApprove` "deliberately NO
  ctl-plane counterpart"（刻意不设 ctl 面对应项）。该注释已过时：
  `CtlRequest` 有 `goal_approve`（14），且 `ctlDispatch`（`daemon/ctl.go`）
  只对发起调用的 group 自身接受它。否则返回 `ctl: goal_approve is
  self-only (a plan set on a peer needs a human)`。

一元调用——机群与 ACL：

- `Resources(ResourcesReq{}) → ResourcesResp`。无目标（没有 group 字段）：
  仅凭动词授权（`targetOf`，`daemon/acl.go`）。在宿主机侧采集
  （`daemon/resources.go`）。
- `AclGet`、`AclSetRole`、`AclDelRole` → `AclResp`。硬编码为仅限管理员
  （`adminOnlyVerbs`，`daemon/acl.go`）。

流式调用：

- `RunScript(RunScriptReq{group, script, raw}) → stream ScriptEvent`。
  硬编码为仅限管理员。在 guest 内以 `node`（uid 1000）身份、cwd
  `/workspace`，用 `/bin/sh -c` 运行 `script`（proto 注释）。除非
  `raw=true`，输出会被清洗（sanitize）。`ScriptEvent.event` 为 `data`
  （字节在 `chunk` 中）… `end`；失败是一个 `error` 帧。不携带退出码。
- `JobTail(JobTailReq{group, id, tail, parsed}) → stream ScriptEvent`。
  `tail` ≤ 0 ⇒ 初始窗口为 `jobLogsMaxTail`（65536）字节，也是上限；随后
  通过 guest 内的 `exec tail -c <window> -f` 持续送出实时行
  （`kotoServer.JobTail`）。`parsed=true` 时守护进程发出携带
  `ScriptEvent.parsed`（一个 `Event`）的 `event` 帧，而非 `data` 块。
- `AttachShell(stream ShellInput) ↔ stream ShellFrame`。双向流。
  第一条消息必须是 `open`（`ShellOpen{session, cols, rows}`；
  空 `session` = `koto-shell`），之后是任意数量的 `data` / `resize`，
  可选地以 `close` 结束。`group` 在每条 `ShellInput` 上重复出现，使每条
  消息都被 `aclStream.RecvMsg`（`daemon/auth.go`）重新授权；后续消息若
  指名另一个 group 则属协议错误。`ShellFrame.event` 为
  `data | end | error`；`end` 表示这条连接在 guest 一侧的管路已拆除，
  而不是 tmux 会话已死亡。
- `SubscribeGroup(SubscribeReq{group, since_seq}) → stream Event`。每条
  实时 `Event` 带有按 group 计的 `seq`（从 1 开始，守护进程重启时重置）。
  `since_seq > 0` 时从内存环形缓冲重放 `seq > since_seq` 的事件；若环形
  缓冲覆盖不了缺口，流以一条合成的 `Event{event:"gap"}` 开始，客户端应
  通过 `History` 重新拉取。来自 `History` 的事件 `seq` 为 0。
- `SubscribeLogs(LogsReq{}) → stream LogEvent`。`LogEvent{event:"log",
  level, msg, ts, subsystem, group}`。
- `WatchState(WatchReq{}) → stream StateFrame{groups, ts,
  global_tok_per_sec}`。第一帧立即发送，之后仅在变化时发送。每帧都是
  完整快照。

流在打开期间会被周期性地重新检查授权。见 `aclStreamRecheck`
（`daemon/auth.go`）和 [acl-and-auth.md](acl-and-auth.md)。

---

## 2. 守护进程 ↔ guest：经 vsock 的 `protocol/guest.proto`

### 2.1 帧格式（`daemon/fcframe.go`、`fcguest/frame.go`）

每个 protobuf 信道使用同一种帧：**一个 `uint32` 大端序长度，后跟一条
序列化消息**。`fcReadFrame` 读取 4 字节头部，在分配消息体内存**之前**就
拒绝 `n > max`，然后 `io.ReadFull` 恰好读满 `n` 字节。`fcWriteFrame`
序列化后拒绝大于 `fcFrameMaxWrite` 的消息体，并在一次 `Write` 中写出
头部和消息体。两种超限情形都产生错误 `frame of %d bytes exceeds the
%d-byte channel maximum`。guest 侧的副本（`fcguest/frame.go` 中的
`writeFrame` / `readFrame`）被描述为相同的代码。

各信道的上限（`daemon/fcframe.go`）：

- `fcFrameMaxCtl = 1 << 20`（1 MiB）：guest→host 的 ctl 请求，端口 9002
  （`fcCtlConn`）。
- `fcFrameMaxGuest = 16 << 20`（16 MiB）：guest→host 的 agent 响应与
  10000 上的流帧（`fcAgentCall`、`fcReadAgentFrame`），以及 9004 上的
  turn 帧（`fcTurnSink`）。
- `fcFrameMaxHost = 64 << 20`（64 MiB）：host→guest 的请求（附件随
  `MsgReq` 传输）。guest 以 `frameMaxHost = 64 << 20` 读取宿主机帧
  （`fcguest/frame.go`）。
- `fcFrameMaxWrite = fcFrameMaxHost`：守护进程写出的帧从不超过 64 MiB。

慢速消息体防御：在 guest 发起的信道（9002、9004）上，守护进程通过
`fcReadFrameBounded`（`daemon/fc.go`）读取。它无限期等待头部，然后对
帧的剩余部分应用 `fcFrameBodyWait = 60 * time.Second`。

host→guest 的连接建立（`fcHostDial`，`daemon/fc.go`）：连接到
Firecracker 的 vsock UDS，写入 `CONNECT <port>\n`，然后逐字节读取回复
直到 `\n`。回复必须以 `OK ` 开头，否则错误为
`vsock CONNECT <port>: <line>`。

### 2.2 端口 9002：ctl 控制面（guest → host）

- 消息：`CtlRequest`（一个 `oneof cmd`）→ `CtlResponse{ok, error, oneof
  result}`。
- 一条连接可以承载多对请求/响应。`fcCtlConn` 循环执行
  `fcReadFrameBounded` → `ctlDispatchPB` → `fcWriteFrame` 直到 EOF；
  坏帧记日志为 `[<g>] ctl frame: <err>`。
- 授权依据是**请求到达于哪个 group 的 vsock 监听器**，从不依据任何字段
  （guest.proto 头部；`daemon/ctl.go`）。
- `CtlRequest.cmd` 的变体及其字段号：`spawn`(1)
  `SpawnReq`、`send`(2) `CtlSendReq{group, msg, session, reply,
  from_session}`、`stop`(3)、`list`(4)、`resources`(5)、`config_set`(6)
  `ConfigReq`、`tail`(7) `CtlTailReq{group, n}`、`job_done`(8)
  `JobDoneReq{id, rc, out, total, session}`、`notify`(9) `NotifyReq{severity,
  title, msg, session}`、`report`(10) `ReportReq{msg}`、`goal_done`(11)
  `GoalDoneReq{id, note}`、`goal_verdict`(12) `GoalVerdictReq{id, met,
  reasons}`、`goal_set`(13)、`goal_approve`(14)、`goal_status`(15)
  `GoalListReq`、`goal_pause`(16)、`goal_interrupt`(17)、`goal_resume`(18)、
  `goal_cancel`(19)、`sched_add`(20)、`sched_list`(21)、`sched_del`(22)、
  `sched_toggle`(23)、`sched_run`(24)。上面没有专属类型的消息复用
  `koto.proto` 中同名的请求类型。
- `CtlResponse.result` 的变体：`port`（spawn）、`list`、`resources`、
  `config`、`tail`（`CtlTailResp{text}`）、`goal`、`goals`（goal_status）、
  `sched`（sched_add）、`scheds`（sched_list）。
- guest 一侧（`fcguest/ctl.go`）：agent 向 `/workspace/.cs/ctl` 写一行
  JSON。agent 用 `protojson.UnmarshalOptions{DiscardUnknown: false}` 把它
  解析成 `CtlRequest`，因此未知动词、未知字段或类型错误都会在 guest 内
  被拒绝。回复被压平成一个 JSON 对象（`UseProtoNames: true`）追加到
  `/workspace/.cs/ctl.out`。各动词语义见 [ctl-reference.md](ctl-reference.md)
  第 2 部分。

### 2.3 端口 10000：agent RPC（host → guest）

- 每条连接一个 `AgentRequest` 帧（`fcAgentOpen` /
  `fcAgentCall`，`daemon/fc.go`）。`AgentRequest.op` 的变体：`init`
  `InitReq{ports, env, net, root, group, kvm}`、`msg` `MsgReq{msg,
  system_prompt, config_json, uploads_tar, session, slot}`、`exec`
  `ExecReq{script}`、`exec_stream` `ExecStreamReq{script}`、`run_script`
  `GuestScriptReq{script}`、`shell_attach` `ShellAttachReq{session, cols,
  rows}`、`shutdown` `ShutdownReq{}`。
- 请求之后各操作的后续内容（guest.proto 头部与 `fcAgentOpen` 注释）：
  - `init`、`msg`、`exec`、`shutdown`：一个 `AgentResponse{ok, error, rc,
    out}` 帧，以 `fcFrameMaxGuest` 为上限读取。非 ok 的 `error` 在使用前
    会被清洗并压平为一行（`fcAgentCall` 中的
    `flattenInline(sanitize(...))`）。
  - `exec_stream`：**原始字节**，没有响应帧；宿主机关闭管道。
  - `run_script`：guest→host 的 `AgentFrame` 流（`data`、`end`、
    `error`）。
  - `shell_attach`：**双向**的 `AgentFrame`：guest→host 为 `data`/`end`/
    `error`，host→guest 为 `input`/`resize`（`fcShellDial`）。
    关闭连接只是让客户端脱离（detach），不会杀死 tmux 会话。
- `AgentFrame.end` 是一个显式终止符，因为在 hybrid vsock 上 guest 侧的
  close 不会在宿主机上表现为 EOF（guest.proto 注释）。

陈旧注释：`protocol/koto.proto` 的 `ShellInput` 一节仍描述一种
`[type byte][uint32 BE len][payload]` 帧，类型为 `'D'/'E'/'X'/'I'/'R'`，
并提及 `fcReadScriptFrame` / `writeScriptFrame`。这两个函数在代码树中
都不存在。代码使用的是 protobuf `AgentFrame`
（`daemon/fc.go` 中的 `fcReadAgentFrame`、`fcShellDial`）。

### 2.4 端口 9004：turn 流（guest → host）

- 每个 turn 一条连接。第一帧必须是
  `TurnFrame{open: TurnOpen{slot}}`。然后是事件帧，最后是 `turn_end`。
- `TurnFrame.kind` 的变体：`open`、`text`（字节）、`think_begin`、
  `think_end`（词数）、`tool`（`ToolUse{name, input}`）、
  `tool_out_begin`、`tool_out_end`（字节数）、`err`、`turn_end`。
- `fcTurnSink`（`daemon/fcturn.go`）在两种情形下拒绝流。
  其一是 slot 缺失或越界（`slot < 0 || slot >=
  groupSlots`），日志为 `turn stream opened without a valid slot`。
  其二是该 slot 上没有待处理的 turn（`fcConsumeExpectedTurn`），日志为
  `turn stream opened on slot %d with no outstanding turn —
  refused`。
- 帧被以 `[[marker]]` 语法渲染进该 slot 的宿主机日志
  （`daemon/logparse.go`）。会被解析成标记的文本将被转义，因此 guest
  无法伪造标记。工具体被截断到 `fcMarkerMaxBody =
  64 << 10`，工具名截断到 `fcMarkerMaxName = 256`（`truncateBytes`）。
- 若转写（transcript）无法写入，该 turn 会被判定失败而非一直等待：
  守护进程记日志 `turn stream slot %d abandoned: the transcript is
  unwritable, so this turn cannot complete` 并调用 `failInflightTurn`。

### 2.5 非 protobuf 的 vsock 端口

按 guest.proto 头部，9000（API 出口 → 代理，HTTP）和 9003（L3 以太网帧
→ gVisor）是各有自己协议的字节管道。9001（旧日志流）已退役。见
[architecture.md](architecture.md) §6。
