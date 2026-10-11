../protocol.md의 번역본입니다. 영어 원문이 기준입니다.

# 와이어 프로토콜

koto에는 와이어 계약이 두 개 있으며, 둘 다 protobuf이고 둘 다
`protocol/`에 있습니다:

- `protocol/koto.proto`: **클라이언트 ↔ 데몬** gRPC API
  (`service Koto`). `koto ctl`, TUI, Android 클라이언트가 사용합니다.
- `protocol/guest.proto`: vsock 위의 **데몬 ↔ microVM 게스트** 계약.
  데몬과 게스트 PID‑1 에이전트(`fcguest/`)만 사용합니다. 호스트↔게스트
  vsock 쌍을 절대 벗어나지 않습니다.

생성된 Go 코드는 `protocol/pb`에 커밋되어 있습니다(`go_package
"koto-protocol/pb;pb"`). `make proto-gen`으로 재생성하고, `make
proto-verify`는 재생성한 뒤 `protocol/pb`가 바뀌었으면 실패합니다
(`git diff --exit-code -- protocol/pb`, `Makefile`). 코드 생성 툴체인은
`Makefile`에 고정되어 있습니다: `PROTOC_GEN_GO_VER := v1.36.12`,
`PROTOC_GEN_GO_GRPC_VER := v1.6.2`.

함께 보기: [architecture.md](architecture.md) (각 채널의 위치),
[acl-and-auth.md](acl-and-auth.md) (누가 무엇을 호출할 수 있는지),
[ctl-reference.md](ctl-reference.md) (이 메시지들 위에 만들어진 CLI와
게스트 내 동사), [firecracker-vsock.md](../firecracker-vsock.md) (vsock
배경지식).

---

## 1. 클라이언트 ↔ 데몬: `service Koto` (gRPC)

### 1.1 전송

`daemonMain`(`daemon/daemon.go`)에서 설정됩니다:

- 일반 TCP 리스너(`net.Listen("tcp", addr)`)를 `serverTLSConfig`의
  TLS로 감쌉니다(`grpc.Creds(credentials.NewTLS(tlsCfg))`).
  클라이언트는 허용 목록에 있는 인증서를 제시해야 합니다(mTLS).
- 모든 호출은 인터셉터 `authUnary` / `authStream`을 거칩니다. bearer
  토큰은 `authorization` 메타데이터 키에서 읽으며, 선택적인 `Bearer `
  접두사는 제거됩니다(`authFromCtx`, `daemon/auth.go`). 토큰의 신원은
  클라이언트 인증서 CN과 같아야 합니다. 실패하면 gRPC
  `Unauthenticated`가 `invalid or missing token`, `no client
  certificate`, `client certificate and token name different
  identities` 중 하나와 함께 반환됩니다.
- Keepalive: `keepalive.ServerParameters{Time: 30 * time.Second,
  Timeout: 10 * time.Second}`, 그리고 강제 조건 `MinTime: 10 *
  time.Second, PermitWithoutStream: true`.
- `daemonMain`에는 `MaxRecvMsgSize` / `MaxSendMsgSize` 재정의가 없어
  grpc‑go 기본값이 적용됩니다(어느 grpc‑go 버전의 기본값인지는 미검증;
  `daemon/go.mod`에 고정된 런타임 참조).

전체 메서드 이름은 `/koto.Koto/<Rpc>`입니다(`package koto;`,
`service Koto`). RPC의 ACL 동사는 그 이름의 snake_case입니다:
`verbFromMethod`(`daemon/acl.go`)가 안쪽의 모든 대문자 앞에 `_`를 넣고
소문자화하므로 `SchedToggle` → `sched_toggle`, `RunScript` →
`run_script`, `AttachShell` → `attach_shell`이 됩니다. ACL의 세부
사항은 [acl-and-auth.md](acl-and-auth.md)에 있습니다.

### 1.2 규약 (`protocol/koto.proto` 머리말)

- 필드 이름은 snake_case입니다. JSON을 출력하는 클라이언트는
  `protojson.MarshalOptions{UseProtoNames: true}`를 쓰므로
  (`daemon/ctl_cli.go`, `tui/daemon.go`) JSON 키는 proto 필드 이름과
  같습니다.
- proto3의 0 값은 직렬화되지 않습니다.
- 모든 `*Resp`는 `bool ok = 1; string error = 2;`로 시작합니다.
  애플리케이션 오류는 `ok=false`와 함께 `error`에 실려 **대역 내**로
  전달되고, 인증/ACL 실패와 잘못된 스트림 오픈은 gRPC 상태 오류입니다.
- 타임스탬프(`ts`, `before`, 스케줄 시각)는 `double` 초입니다.
- 자유 형식 JSON 객체(config, metrics)는
  `google.protobuf.Struct`입니다.

### 1.3 RPC

아래 36개 RPC 전부가 `daemon/grpc_server.go`의 `kotoServer` 메서드로
구현되어 있습니다(`pb.RegisterKotoServer`로 등록).

단항, 그룹 수명주기와 채팅:

- `Spawn(SpawnReq) → SpawnResp`. `SpawnReq{group, main, provider,
  model, size}`; `SpawnResp`에는 `int32 port`가 추가됩니다.
- `Send(SendReq) → BaseResp`. `SendReq{group, msg, image, image_mime,
  audio, audio_mime, session}`. proto 주석에 따르면 이미지는
  `groups/<g>/.cs/uploads/img-<ts>.<ext>`에 기록되어 메시지에서
  참조되고, 오디오는 "CURRENTLY UNSUPPORTED"이며 대역 내에서
  거부됩니다. `session`의 ""/`-`/`default`는 기본 세션입니다.
- `List(ListReq) → ListResp`.
- `Stop`, `Interrupt`, `Destroy`, `Clear` `(GroupReq) → BaseResp`;
  `Restart(GroupReq) → SpawnResp`. `GroupReq{group, session}`; proto
  주석에 따르면 `session`은 `Clear`만 읽습니다("" = 그룹 전체,
  `-`/`default` = 기본 세션, 이름 = 해당 세션). `Drain`도 `session`으로
  범위를 정합니다(아래).
- `Drain(GroupReq) → DrainResp{ok, error, dropped}`. 대기 중(큐에 있는)
  프롬프트만 버립니다. VM, 메모리, 진행 중인 턴은 건드리지 않습니다.
  goal 세션은 절대 drain되지 않습니다(`Drain`의 proto 주석).
- `History(HistoryReq) → HistoryResp`. `HistoryReq{group, limit,
  before}`: `limit` 0 = 서버 기본값, `before` 0 = 꼬리 페이지, 그 외 =
  `ts < before`인 이벤트.
- `Config(ConfigReq) → ConfigResp`. 각 설정은 proto3 `optional`이므로
  상태가 셋입니다: 없음 = 변경 없음, `""` 있음 = 지우기, 값 있음 =
  설정. 필드: `model`, `effort`, `ports`, `provider`, `internet`
  (폐기 예정; "daemon maps full→network=wan"), `size`, `root`,
  `network`, `autostart`, `kvm`. 필드 번호 4와 8은 `reserved`입니다
  (제거된 skills 기능). 허용 값은
  [configuration.md](configuration.md)에 있습니다.
- `Metrics(MetricsReq{group}) → MetricsResp`.

단항, 잡과 스케줄:

- `Jobs(JobsReq{group}) → JobsResp`. `group` "" = 실행 중인 모든 그룹
  (`*` 타깃 필요).
- `JobLogs(JobLogsReq{group, id, tail}) → JobLogsResp{job, output,
  truncated}`. `tail` 0 ⇒ `jobLogsDefaultTail = int64(4096)`, 상한은
  `jobLogsMaxTail = int64(65536)`(`daemon/jobs.go`).
- `SchedAdd(SchedAddReq{group, cron, msg}) → SchedAddResp{item}`,
  `SchedList(SchedListReq{group}) → SchedListResp`,
  `SchedDel(SchedIDReq{id})`, `SchedToggle(SchedToggleReq{id,
  enabled})`, `SchedRun(SchedIDReq{id})`, 마지막 셋은 `BaseResp`를
  반환합니다.

단항, goal(`daemon/goals.go`):

- `GoalSet(GoalSetReq) → GoalResp`. `GoalSetReq{group, text, criteria,
  max_iterations, plan, name}`. `max_iterations` ≤ 0 ⇒
  `goalDefaultMaxIter = 20`, 상한 `goalMaxIterCeil = 200`
  (`goalSetBy`). `plan`은 `optional bool`이며 없음 = 계획 우선. 빈
  `name`은 `text`에서 슬러그가 생성됩니다.
- `GoalList(GoalListReq{group}) → GoalListResp`. `group` "" = 모든 그룹
  (`*` 타깃 필요).
- `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume`,
  `GoalCancel` `(GoalGroupReq{group, name}) → GoalResp`. `name` ""는
  유일한 후보 goal로 해석되며, 모호한 ""는 오류입니다. proto 주석은
  `GoalApprove`에 "deliberately NO ctl-plane counterpart"가 있다고
  말합니다. 그 주석은 낡았습니다: `CtlRequest`에는 `goal_approve`(14)가
  있고, `ctlDispatch`(`daemon/ctl.go`)는 호출한 그룹 자신에 대해서만
  이를 받아들입니다. 그 외에는 `ctl: goal_approve is self-only (a plan
  set on a peer needs a human)`을 반환합니다.

단항, 플릿과 ACL:

- `Resources(ResourcesReq{}) → ResourcesResp`. 타깃 없음(group 필드
  없음): 동사만으로 인가됩니다(`targetOf`, `daemon/acl.go`). 호스트
  쪽에서 수집됩니다(`daemon/resources.go`).
- `AclGet`, `AclSetRole`, `AclDelRole` → `AclResp`. 하드코딩된 관리자
  전용(`adminOnlyVerbs`, `daemon/acl.go`).

스트리밍:

- `RunScript(RunScriptReq{group, script, raw}) → stream ScriptEvent`.
  하드코딩된 관리자 전용. 게스트에서 `script`를 `/bin/sh -c`로 `node`
  (uid 1000) 권한, cwd `/workspace`에서 실행합니다(proto 주석). 출력은
  `raw=true`가 아니면 정제(sanitize)됩니다. `ScriptEvent.event`는
  `data`(`chunk`의 바이트) … `end`이며, 실패는 `error` 프레임
  하나입니다. 종료 코드는 전달되지 않습니다.
- `JobTail(JobTailReq{group, id, tail, parsed}) → stream ScriptEvent`.
  `tail` ≤ 0 ⇒ 초기 윈도는 `jobLogsMaxTail`(65536)바이트이며 상한도
  같습니다. 이후 게스트의 `exec tail -c <window> -f`로 라이브 라인을
  받습니다(`kotoServer.JobTail`). `parsed=true`면 데몬은 `data` 청크
  대신 `ScriptEvent.parsed`(하나의 `Event`)를 실은 `event` 프레임을
  내보냅니다.
- `AttachShell(stream ShellInput) ↔ stream ShellFrame`. 양방향. 첫
  메시지는 `open`이어야 하고(`ShellOpen{session, cols, rows}`; 빈
  `session` = `koto-shell`), 이후 임의 개수의 `data` / `resize`, 선택적
  `close`가 옵니다. `group`은 모든 `ShellInput`에 반복되므로 메시지마다
  `aclStream.RecvMsg`(`daemon/auth.go`)가 재인가합니다. 나중 메시지가
  다른 그룹을 지목하면 프로토콜 오류입니다. `ShellFrame.event`는
  `data | end | error`이며, `end`는 이 연결의 게스트 쪽 배관이 해체됐다는
  뜻이지 tmux 세션이 죽었다는 뜻이 아닙니다.
- `SubscribeGroup(SubscribeReq{group, since_seq}) → stream Event`. 모든
  라이브 `Event`는 그룹별 `seq`(1에서 시작, 데몬 재시작 시 초기화)를
  지닙니다. `since_seq > 0`은 메모리 내 링에서 `seq > since_seq`인
  이벤트를 재생합니다. 링이 그 간극을 메울 수 없으면 스트림은 합성
  `Event{event:"gap"}`으로 시작하고 클라이언트는 `History`로 다시
  가져와야 합니다. `History`의 이벤트는 `seq`가 0입니다.
- `SubscribeLogs(LogsReq{}) → stream LogEvent`. `LogEvent{event:"log",
  level, msg, ts, subsystem, group}`.
- `WatchState(WatchReq{}) → stream StateFrame{groups, ts,
  global_tok_per_sec}`. 첫 프레임은 즉시 전송되고 이후 프레임은 변경
  시에만 옵니다. 각 프레임은 전체 스냅샷입니다.

스트림 인가는 스트림이 열려 있는 동안 주기적으로 재검사됩니다.
`aclStreamRecheck`(`daemon/auth.go`)와
[acl-and-auth.md](acl-and-auth.md)를 보세요.

---

## 2. 데몬 ↔ 게스트: vsock 위의 `protocol/guest.proto`

### 2.1 프레이밍 (`daemon/fcframe.go`, `fcguest/frame.go`)

모든 protobuf 채널은 같은 프레임을 씁니다: **`uint32` 빅엔디언 길이
하나, 그다음 직렬화된 메시지 하나**. `fcReadFrame`은 4바이트 헤더를
읽고, 본문을 할당하기 **전에** `n > max`를 거부한 뒤, 정확히 `n`바이트를
`io.ReadFull`합니다. `fcWriteFrame`은 마샬링하고
`fcFrameMaxWrite`보다 큰 본문을 거부하며, 헤더와 본문을 한 번의
`Write`로 씁니다. 두 초과 사례 모두 `frame of %d bytes exceeds the
%d-byte channel maximum` 오류를 냅니다. 게스트 쪽 복사본
(`fcguest/frame.go`의 `writeFrame` / `readFrame`)은 같은 코드로
설명됩니다.

채널별 최대치(`daemon/fcframe.go`):

- `fcFrameMaxCtl = 1 << 20` (1 MiB): 게스트→호스트 ctl 요청, 포트 9002
  (`fcCtlConn`).
- `fcFrameMaxGuest = 16 << 20` (16 MiB): 10000의 게스트→호스트
  에이전트 응답 및 스트림 프레임(`fcAgentCall`, `fcReadAgentFrame`),
  그리고 9004의 턴 프레임(`fcTurnSink`).
- `fcFrameMaxHost = 64 << 20` (64 MiB): 호스트→게스트 요청(첨부물이
  `MsgReq` 안에 실림). 게스트는 호스트 프레임을 `frameMaxHost = 64 <<
  20`(`fcguest/frame.go`)으로 읽습니다.
- `fcFrameMaxWrite = fcFrameMaxHost`: 데몬은 64 MiB보다 큰 프레임을
  절대 쓰지 않습니다.

느린 본문 방어: 게스트가 시작하는 채널(9002, 9004)에서 데몬은
`fcReadFrameBounded`(`daemon/fc.go`)로 읽습니다. 헤더는 기한 없이
기다리고, 프레임의 나머지에는 `fcFrameBodyWait = 60 * time.Second`를
적용합니다.

호스트→게스트 연결 수립(`fcHostDial`, `daemon/fc.go`): Firecracker
vsock UDS에 연결하고 `CONNECT <port>\n`을 쓴 뒤, `\n`까지 응답을 한
바이트씩 읽습니다. `OK `로 시작해야 하며, 아니면 오류는
`vsock CONNECT <port>: <line>`입니다.

### 2.2 포트 9002: ctl 플레인 (게스트 → 호스트)

- 메시지: `CtlRequest`(`oneof cmd`) → `CtlResponse{ok, error, oneof
  result}`.
- 한 연결이 여러 요청/응답 쌍을 나를 수 있습니다. `fcCtlConn`은 EOF까지
  `fcReadFrameBounded` → `ctlDispatchPB` → `fcWriteFrame`을 반복하며,
  잘못된 프레임은 `[<g>] ctl frame: <err>`로 로그됩니다.
- 인가는 요청이 **어느 그룹의 vsock 리스너**로 도착했는지에 따르며,
  필드로는 절대 하지 않습니다(guest.proto 머리말; `daemon/ctl.go`).
- `CtlRequest.cmd` 변형과 필드 번호: `spawn`(1) `SpawnReq`, `send`(2)
  `CtlSendReq{group, msg, session, reply, from_session}`, `stop`(3),
  `list`(4), `resources`(5), `config_set`(6) `ConfigReq`, `tail`(7)
  `CtlTailReq{group, n}`, `job_done`(8) `JobDoneReq{id, rc, out, total,
  session}`, `notify`(9) `NotifyReq{severity, title, msg, session}`,
  `report`(10) `ReportReq{msg}`, `goal_done`(11) `GoalDoneReq{id,
  note}`, `goal_verdict`(12) `GoalVerdictReq{id, met, reasons}`,
  `goal_set`(13), `goal_approve`(14), `goal_status`(15) `GoalListReq`,
  `goal_pause`(16), `goal_interrupt`(17), `goal_resume`(18),
  `goal_cancel`(19), `sched_add`(20), `sched_list`(21),
  `sched_del`(22), `sched_toggle`(23), `sched_run`(24). 위에 자체 타입이
  없는 메시지는 같은 이름의 `koto.proto` 요청 타입을 재사용합니다.
- `CtlResponse.result` 변형: `port`(spawn), `list`, `resources`,
  `config`, `tail`(`CtlTailResp{text}`), `goal`, `goals`(goal_status),
  `sched`(sched_add), `scheds`(sched_list).
- 게스트 쪽(`fcguest/ctl.go`): 에이전트는 `/workspace/.cs/ctl`에 JSON
  한 줄을 씁니다. 에이전트는 이를
  `protojson.UnmarshalOptions{DiscardUnknown: false}`로 `CtlRequest`로
  파싱하므로, 알 수 없는 동사·알 수 없는 필드·잘못된 타입은 게스트에서
  거부됩니다. 응답은 하나의 JSON 객체로 평탄화되어
  (`UseProtoNames: true`) `/workspace/.cs/ctl.out`에 덧붙습니다. 동사별
  의미는 [ctl-reference.md](ctl-reference.md) 2부를 보세요.

### 2.3 포트 10000: 에이전트 RPC (호스트 → 게스트)

- 연결당 `AgentRequest` 프레임 하나(`fcAgentOpen` / `fcAgentCall`,
  `daemon/fc.go`). `AgentRequest.op` 변형: `init` `InitReq{ports, env,
  net, root, group, kvm}`, `msg` `MsgReq{msg, system_prompt,
  config_json, uploads_tar, session, slot}`, `exec` `ExecReq{script}`,
  `exec_stream` `ExecStreamReq{script}`, `run_script`
  `GuestScriptReq{script}`, `shell_attach` `ShellAttachReq{session,
  cols, rows}`, `shutdown` `ShutdownReq{}`.
- 요청 뒤에 오는 것, op별(guest.proto 머리말과 `fcAgentOpen` 주석):
  - `init`, `msg`, `exec`, `shutdown`: `AgentResponse{ok, error, rc,
    out}` 프레임 하나, `fcFrameMaxGuest` 한도로 읽음. ok가 아닌
    `error`는 사용 전에 정제되어 한 줄로 평탄화됩니다
    (`fcAgentCall`의 `flattenInline(sanitize(...))`).
  - `exec_stream`: **원시 바이트**, 응답 프레임 없음; 호스트가 파이프를
    닫습니다.
  - `run_script`: 게스트→호스트 `AgentFrame` 스트림(`data`, `end`,
    `error`).
  - `shell_attach`: **양방향** `AgentFrame`: 게스트→호스트
    `data`/`end`/`error`, 호스트→게스트 `input`/`resize`
    (`fcShellDial`). 연결을 닫으면 클라이언트가 분리될 뿐 tmux 세션이
    죽지는 않습니다.
- `AgentFrame.end`가 명시적 종결자인 이유는, 하이브리드 vsock에서는
  게스트 쪽 close가 호스트 EOF로 드러나지 않기 때문입니다(guest.proto
  주석).

낡은 주석: `protocol/koto.proto`의 `ShellInput` 절은 아직
`[type byte][uint32 BE len][payload]` 프레임과 `'D'/'E'/'X'/'I'/'R'`
타입을 설명하며 `fcReadScriptFrame` / `writeScriptFrame`을 지목합니다.
둘 다 트리에 존재하지 않습니다. 코드는 protobuf `AgentFrame`을
사용합니다(`daemon/fc.go`의 `fcReadAgentFrame`, `fcShellDial`).

### 2.4 포트 9004: 턴 스트림 (게스트 → 호스트)

- 턴마다 연결 하나. 첫 프레임은 반드시
  `TurnFrame{open: TurnOpen{slot}}`이어야 합니다. 그다음 이벤트
  프레임들이 오고, 마지막이 `turn_end`입니다.
- `TurnFrame.kind` 변형: `open`, `text`(바이트), `think_begin`,
  `think_end`(단어 수), `tool`(`ToolUse{name, input}`),
  `tool_out_begin`, `tool_out_end`(바이트 수), `err`, `turn_end`.
- `fcTurnSink`(`daemon/fcturn.go`)는 두 경우에 스트림을 거부합니다.
  첫째는 슬롯이 없거나 범위를 벗어난 경우(`slot < 0 || slot >=
  groupSlots`)로, `turn stream opened without a valid slot`으로
  로그됩니다. 둘째는 미결 턴이 없는 슬롯(`fcConsumeExpectedTurn`)으로,
  `turn stream opened on slot %d with no outstanding turn — refused`로
  로그됩니다.
- 프레임은 `[[marker]]` 문법(`daemon/logparse.go`)으로 슬롯의 호스트
  로그에 렌더링됩니다. 마커로 파싱될 법한 텍스트는 이스케이프되므로
  게스트는 마커를 위조할 수 없습니다. 툴 본문은 `fcMarkerMaxBody =
  64 << 10`으로, 툴 이름은 `fcMarkerMaxName = 256`으로 잘립니다
  (`truncateBytes`).
- 기록(transcript)을 쓸 수 없으면 턴을 기다리게 두지 않고 실패
  처리합니다: 데몬은 `turn stream slot %d abandoned: the transcript is
  unwritable, so this turn cannot complete`를 로그하고
  `failInflightTurn`을 호출합니다.

### 2.5 protobuf가 아닌 vsock 포트

guest.proto 머리말에 따르면 9000(API 이그레스 → 프록시, HTTP)과
9003(L3 이더넷 프레임 → gVisor)은 자체 프로토콜을 가진 바이트
파이프입니다. 9001(옛 로그 스트림)은 퇴역했습니다.
[architecture.md](architecture.md) §6을 보세요.
