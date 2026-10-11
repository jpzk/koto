../ctl-reference.md의 번역본입니다. 영어 원문이 기준입니다.

# `koto ctl`과 게스트 내부 ctl 플레인 — 레퍼런스

koto에는 둘 다 "ctl"이라 불리는 두 개의 명령 표면이 있습니다:

1. **`koto ctl`**: 호스트 측 CLI. 데몬 API의 gRPC 클라이언트입니다
   (`daemon/main.go`, `case "ctl"` → `ctlCliMain`; `daemon/ctl_cli.go`).
2. **게스트 내부 ctl 플레인**: microVM 안의 에이전트가 명령마다 JSON 한 줄을
   `/workspace/.cs/ctl`에 쓰고, `/workspace/.cs/ctl.out`에 덧붙여지는 응답을
   읽습니다 (`fcguest/ctl.go` 헤더 주석). 게스트는 이 줄들을 vsock 포트 9002로
   데몬에 전달하고 (`fcguest/main.go`의 `portCtl`, `daemon/fc.go`의
   `fcPortCtl`), 데몬은 `ctlDispatch`에서 응답합니다 (`daemon/ctl.go`).

각 표면은 고유한 동사(verb) 집합과 고유한 규칙을 갖습니다. 아래에서 둘 다
설명합니다.

---

## Part 1 — `koto ctl` (호스트 CLI)

### 1.1 연결과 인증

`koto ctl`은 mTLS와 베어러 토큰으로 데몬에 접속합니다 (`ctlClient`,
`newKotoClient`, `tokenCreds` — `daemon/ctl_cli.go`). 관련된 모든 환경 변수는
[configuration.md](configuration.md) §2.3을 보세요. 역할(role), ACL, PKI는
[acl-and-auth.md](acl-and-auth.md)에서 다룹니다. 요약:

- `KOTO_ADDR`: 기본값 `127.0.0.1:8443` (`ctlClient`).
- `KOTO_CLIENT`: 클라이언트 신원. 기본값 `agent` (`ctlClient`).
- `KOTO_CREDS_DIR`: 설정하지 않으면 `ctlCredsDir`가 순서대로 `./creds`(존재할
  때), `$KOTO_HOME/creds`(`KOTO_HOME`이 설정된 경우), `/var/lib/koto/creds`
  (존재할 때)를 시도하고, 마지막으로 `creds`로 폴백합니다.
- 그 디렉터리 안의 기본 파일명은 `client-<name>.crt`, `client-<name>.key`,
  `ca.crt`, `token-<name>`입니다. `KOTO_CERT`, `KOTO_KEY`, `KOTO_CA`,
  `KOTO_TOKEN`이 각각 하나씩 재정의합니다 (`newKotoClient`).
- `KOTO_SERVER_NAME`은 TLS 서버 이름을 재정의합니다 (`newKotoClient`).
- TLS 1.3이 최소 버전입니다 (`MinVersion: tls.VersionTLS13`, `newKotoClient`).
- 토큰은 `authorization: Bearer <token>` 메타데이터 헤더에 실립니다
  (`tokenCreds.GetRequestMetadata`).
- `KOTO_TOKEN`도 토큰 파일도 없으면 다음 오류로 실패합니다:
  ``no token: set KOTO_TOKEN or provide <dir>/token-<name> (mint with `make pki-client NAME=<name> ROLE=agent`)``
  (`ctlClient`).

설치된 데몬을 상대로 한 읽기 전용 예시:

```sh
HOME=/var/lib/koto KOTO_CLIENT=tui KOTO_TOKEN=$(cat /var/lib/koto/creds/token-tui) koto ctl list
```

### 1.2 출력과 종료 코드

- 단항(unary) 동사는 응답을 proto 필드 이름을 쓰는 **protojson 한 줄**로
  출력합니다 (`ctlPrint`, `protojson.MarshalOptions{UseProtoNames: true}`).
- RPC가 실패하면: `koto ctl: rpc: <err>`, 종료 코드 1 (`ctlPrint`).
- 응답에 `"ok": false`가 있으면: `koto ctl: daemon: <error>`, 종료 코드 1
  (`ctlPrint`). 이것이 대역 내(in-band) 애플리케이션 오류 규약입니다.
- 사용법 오류는 종료 코드 2입니다 (`ctlCliMain` 전반의 `ctlFatal(2, …)`).
  플래그 파싱 오류도 2인데, 모든 플래그 셋이 `flag.ExitOnError`를 쓰기
  때문입니다.
- 스트리밍 동사(`tail`, `logs`, `watch`)는 프레임마다 protojson 한 줄을
  출력합니다. 깨끗한 EOF는 0으로 종료하고, 그 외 스트림 오류는
  `koto ctl: stream: <err>`를 출력하고 1로 종료합니다 (`ctlStreamJSON`).
- 모든 단항 호출에는 30초 데드라인이 있습니다 (`ctlCtx`). 스트림과 `ask`는
  아래에 설명하는 자체 컨텍스트를 씁니다.
- 동사 없이 `koto ctl`을 실행하면 사용법 텍스트를 출력하고 2로 종료합니다.
  알 수 없는 동사는 사용법 텍스트에 더해 `koto ctl: unknown verb: <verb>`를
  출력하고 2로 종료합니다 (`ctlCliMain`).

### 1.3 메시지 인자 (`-` = stdin)

`send`, `ask`, `runscript`에서 메시지나 스크립트는 뒤따르는 인자들을 공백
하나로 이어 붙인 것입니다. 대신 `-` 하나를 주면 stdin을 읽고, 끝의 개행을
제거하며, 입력을 `ctlStdinMax = 1 << 20` 바이트(1 MiB, 데몬의
`daemon/queue.go`에 있는 `sendMsgMax`와 동일)로 제한합니다. 상한을 넘는
입력은 `stdin is larger than 1048576 bytes, which is more than the daemon
accepts for one message` 오류와 함께 종료 코드 2로 실패합니다 (`ctlMsgArg`).

### 1.4 동사

동사는 사용법 텍스트(`ctlUsage`)의 순서대로 나열합니다. 여기서 언급하는 모든
`case`는 별도 헬퍼를 명시하지 않는 한 `ctlCliMain`의 `switch verb` 안에
있습니다.

#### 그룹 수명 주기

- **`list`**: 모든 그룹과 상태. RPC `List`.
- **`spawn [-provider P] [-model M] <group>`**: 그룹을 만들거나 시작합니다.
  `-provider`(도움말 텍스트 `claudesdk|venice`), `-model`(모델 재정의).
  RPC `Spawn`.
- **`stop <group>`**: RPC `Stop`.
- **`interrupt <group>`**: RPC `Interrupt`. `drain`의 주석에 따르면, 실행 중인
  턴을 중단하는 것은 `interrupt`의 역할입니다.
- **`destroy <group>`**: RPC `Destroy`.
- **`restart <group>`**: RPC `Restart`.
- **`clear [-session S] <group>`**: RPC `Clear`. `-session`은 그 채팅 세션만
  지웁니다(`-` 또는 `default` = 기본 세션). 없으면 그룹 전체를 지웁니다.
  사용법 텍스트는 플래그 없이 `clear <group>`만 보여주지만, 플래그는
  존재합니다 (`case "clear"`).
- **`drain [-session S] <group>`**: 대기 중인 프롬프트를 버립니다. 사용법
  텍스트에 따르면 VM, 대화, 진행 중인 턴은 건드리지 않습니다. `-session`은
  그 세션만 비웁니다(`-`/`default` = 기본). 없으면 그룹의 모든 세션을
  비웁니다. RPC `Drain`.

`stop`, `interrupt`, `destroy`, `restart`는 정확히 인자 하나를 받습니다:
``usage: koto ctl <verb> <group>`` (`groupVerb`).

#### 대화

- **`send [-session S] <group> <msg...>`**: 메시지를 큐에 넣고 즉시
  반환합니다. `-session`은 채팅 세션을 지정합니다(`""` = 기본). 이 플래그는
  사용법 텍스트에 나오지 않습니다 (`case "send"`). RPC `Send`.
- **`ask [-timeout D] [-json] [-session S] <group> <msg...>`**: 보내고, 답을
  스트리밍하고, 턴이 끝나면 종료합니다 (`ctlAsk`).
  - `-timeout`: 기본값 `30m` (`30*time.Minute`). 만료 시:
    `timed out after <D> waiting for turn end`, 종료 코드 1.
  - `-json`: 캡처된 이벤트를 일반 텍스트 대신 protojson 줄들로 출력합니다.
  - `-session`: 메인 사용법 텍스트에는 없습니다. `ask` 자체의 사용법 오류에는
    표시됩니다.
  - 동작 (`ctlAsk`): `ask`는 보내기 *전에* 그룹을 구독합니다
    (`SubscribeGroup`). 다른 세션의 이벤트는 무시하며, `""`, `-`, `default`는
    모두 같은 세션을 뜻합니다 (`normalizeAskSession`). 텍스트가 메시지와
    일치하는 첫 `prompt` 이벤트부터 캡처를 시작합니다. 일반 모드에서는
    `done` 텍스트를 stdout에, `err` 텍스트를 stderr에 출력합니다.
    `turn_end`에서 0으로 종료합니다.
- **`history [-limit N] [-before TS] <group>`**: `-limit`(int, 최대 이벤트 수;
  `0`은 데몬에 제한을 보내지 않으며, 서버 기본값은 미검증), `-before`(float
  타임스탬프 커서, 미포함). RPC `History`.

#### 구성

- **`config <group>`**: 플래그가 없으면 유효 구성(effective config)을
  출력합니다 (`ctlConfig`).
- **`config <group> [flags]`**: 키를 설정합니다. Go의 flag 패키지는 첫 번째
  비(非)플래그에서 멈추므로 그룹이 먼저 와야 합니다. 플래그 뒤에 다른 것이
  오면 ``config: unexpected args after group: [...] (flags follow the group)``
  으로 실패합니다. 실제로 지정한 플래그만 전송되므로 `-model ""`은 모델을
  *지우는* 반면 플래그를 생략하면 그대로 둡니다 (`ctlConfig`의 `fs.Visit`).
  RPC `Config`. 플래그는:
  - `-model M` (`""`은 지움)
  - `-provider claudesdk|venice`
  - `-network none|wan|lan|full`
  - `-size small|medium|large|xlarge`
  - `-root yes|no`
  - `-effort E`
  - `-ports P` (쉼표 목록)
  - `-autostart yes|no`
  - `-kvm yes|no`: 중첩 가상화(게스트 안의 `/dev/kvm`). **`ctlUsage`에는
    없지만** `ctlConfig`에 정의되어 있습니다.

  값 검증, 기본값, 변경이 언제 적용되는지는 모두
  [configuration.md](configuration.md) §4에 있습니다. `ctlConfig` 위의 주석에
  따르면 `network`/`size`/`root`/`ports`는 `/restart` 시에,
  `model`/`provider`/`effort`는 다음 메시지에 적용되고, `autostart`는 데몬
  시작 시에만 읽힙니다.

#### 스트림과 점검

- **`metrics [group]`**: RPC `Metrics`. 그룹이 없으면 요청의 group이
  비어 있습니다.
- **`resources`**: 그룹별 호스트 측 디스크·메모리·CPU와 플릿(fleet) 집계.
  사용법 텍스트에는 `(admin)` 표시가 있습니다. 인자를 받지 않습니다. RPC
  `Resources`.
- **`tail [-since N] <group>`**: 그룹 이벤트 스트림. `-since N`(uint64)은
  라이브로 가기 전에 `seq > N`인 링 프레임을 재생합니다. RPC
  `SubscribeGroup`.
- **`logs`**: 데몬 자체의 로그 스트림. 인자 없음. RPC `SubscribeLogs`.
- **`watch`**: 변경이 있을 때마다 그룹 상태 스냅숏. 인자 없음. RPC
  `WatchState`.
- **`jobs [group]`**: 백그라운드 작업(`cs-job`)을 게스트에서 새로 읽어
  나열합니다. 그룹이 없으면 모든 그룹. RPC `Jobs`.
- **`job-logs [-tail N] <group> <id>`**: 한 작업의 메타데이터와 출력 끝부분.
  플래그 도움말에 따르면 `-tail`은 끝에서부터의 바이트 수로, 기본 4096, 최대
  65536. CLI는 `0`을 보내고 데몬이 기본값을 적용합니다
  (`daemon/jobs.go`의 `jobLogsDefaultTail = 4096`, `jobLogsMaxTail = 65536`).
  RPC `JobLogs`.
- **`job-tail [-tail N] <group> <id>`**: 작업 출력을 라이브로 따라갑니다.
  플래그 도움말에 따르면 `-tail`은 초기 윈도 바이트 수로, 기본 65536. 0
  이하의 값은 `jobLogsMaxTail`이 되고, 더 큰 값은 그 값으로 잘립니다
  (`daemon/grpc_server.go`, JobTail 핸들러). 원시 `data` 청크를 stdout에
  쓰고, `end`에서 0으로, `error`에서 1로 종료합니다. RPC `JobTail`.

#### 스케줄 (`ctlSched`)

- **`sched list [group]`**: RPC `SchedList`.
- **`sched add <group> <cron...> <msg...>`**: `cron`은 `@alias` 토큰 하나
  또는 정확히 5개 필드입니다. 그 뒤 전부가 메시지입니다. 5필드 형식인데
  남은 인자가 7개 미만이면: `sched add: need 5 cron fields (or one
  @alias) + msg`. 빈 메시지는: `sched add: empty message`. RPC `SchedAdd`.
  데몬이 아는 별칭(`cronAliases`, `daemon/cron.go`)은 `@yearly`,
  `@annually`, `@monthly`, `@weekly`, `@daily`, `@midnight`, `@hourly`
  입니다.
- **`sched del|on|off|run <id>`**: RPC `SchedDel`, `SchedToggle`
  (`Enabled: sub == "on"`), `SchedRun`.

#### 목표 (`ctlGoal`)

- **`goal set [-max N] [-plan=no] [-name x] <group> <goal> :: <criteria>`**.
  목표 텍스트와 기준은 리터럴 `::` 인자로 구분하며, 양쪽 모두 비어 있으면
  안 됩니다.
  - `-max`: 최대 반복 횟수. `0`은 기본값을 뜻하며, 기본값은
    `goalDefaultMaxIter = 20`입니다. 값은 `goalMaxIterCeil = 200`으로
    잘립니다 (`daemon/goals.go`, `goalSetBy`).
  - `-plan`: `yes`(기본) 또는 `no`. 그 외 값은
    `goal set: -plan takes yes or no`로 실패합니다.
  - `-name`: 짧은 실행 이름이며 세션 이름으로도 쓰입니다. 기본값은 목표의
    슬러그(slug)입니다.
  - RPC `GoalSet`을 보냅니다. `main`에 대한 목표는 `cannot set a goal
    on main`으로 거부됩니다 (`goalSetBy`).
- **`goal list [group]`**: RPC `GoalList`.
- **`goal approve|pause|interrupt|resume|cancel <group> [name]`**: RPC
  `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume`, `GoalCancel`.
  `[name]`은 그룹의 동시 목표 중 하나를 고르며, 후보가 하나뿐이면 생략할
  수 있습니다. `[name]`은 `ctlUsage`에는 없지만 해당 동사의 사용법 오류에는
  있습니다. `interrupt`는 지금 일시정지하고 진행 중인 목표 턴을 중단한다는
  뜻입니다(사용법 텍스트).

#### 공유 셸 (`ctlShell`)

- **`shell <group> [session]`**: RPC `AttachShell`로 그룹의 microVM에
  대화형 터미널을 연결합니다.
  - stdin이 터미널이어야 하며, 아니면 `shell: stdin is not a terminal: …`.
  - 터미널은 `ISIG`를 끈 raw 모드가 되어 ctrl-C가 게스트에 전달됩니다.
  - **ctrl-]**(바이트 `0x1d`)는 분리(detach)하고 `[detached — session left
    running]`을 출력합니다.
  - SIGWINCH는 리사이즈를 보냅니다. TTY 크기가 없으면 80×24입니다
    (`ctlWinsize`).
  - 출력은 `shellFilter`를 거칩니다.
  - 사용법 텍스트에 따르면 기본 tmux 세션은 `koto-shell`입니다. 이름 있는
    채팅 세션의 tmux 세션은 `koto-shell-<name>`입니다.

#### admin 역할 전용

사용법 텍스트는 아래 동사들을 "admin role only" 아래에 둡니다. `ctlAcl`의
주석에 따르면 `acl_*` 동사는 서버 측에서 하드코딩된 admin 전용입니다.
권한 부여 모델은 [acl-and-auth.md](acl-and-auth.md)를 보세요.

- **`runscript [-raw] <group> <script...>`**: 그룹의 microVM에서 POSIX
  스크립트를 `node`로 실행합니다. 출력은 stdout으로 라이브 스트리밍됩니다.
  RPC `RunScript`.
  - `-raw`는 **첫 번째** 인자일 때만 인식됩니다(flag 패키지가 파싱하지
    않음).
  - `-raw` 없이는 데몬이 출력을 정화(sanitize)합니다 (`case "runscript"`의
    주석).
  - stdout이 TTY인 상태의 `-raw`는 **거부됩니다**: ``-raw writes the guest's bytes
    to your terminal unfiltered; redirect stdout to a file (koto ctl runscript
    -raw <group> ... > out.bin) or drop -raw to get the sanitized stream``,
    종료 코드 2 (`stdoutIsTTY`).
- **`acl get`**: ACL 문서를 출력합니다. RPC `AclGet`.
- **`acl set <role> <verb>[:<groups>] ...`**: 역할의 권한(grant)을
  *교체*합니다 (RPC `AclSetRole`).
  - 각 토큰은 `verb` 또는 `verb:g1,g2`입니다. 단독 `verb`나 `verb:*`는
    `"*"`가 됩니다. 그 외에는 공백을 정리한 그룹 이름 목록이 됩니다.
  - 빈 동사는 `acl set: empty verb in "<tok>"`로 실패합니다.
  - 사용법 텍스트의 예시:
    `acl set ops stop:ghost restart:ghost list`.
- **`acl del <role>`**: RPC `AclDelRole`.
- 알 수 없는 하위 명령은 `unknown acl subcommand: <sub> (get|set|del)`을
  출력합니다.

### 1.5 사용법 텍스트와 코드의 간극

다음 플래그와 인자는 `daemon/ctl_cli.go`에 존재하지만 `ctlUsage`에는
없습니다:

- `config -kvm`
- `send -session`
- `ask -session`
- `clear -session`
- `goal approve|pause|interrupt|resume|cancel`의 선택적 `[name]`

---

## Part 2 — 게스트 내부 ctl 플레인 (`/workspace/.cs/ctl`)

### 2.1 와이어 형태

- 각 요청은 한 줄에 JSON 객체 하나입니다. `cmd` 필드가 동사를 고릅니다
  (`ctlDispatch`의 `cmdEnvelope`). 잘못된 JSON은 `json: <err>`를 받습니다.
- 게스트는 각 줄을 `CtlRequest`로 변환하고, 응답을 `ctl.out`의 평탄한
  `ok`/`error` JSON 줄로 되돌립니다 (`fcguest/ctl.go`의
  `ctlRequestFromJSON`, `ctlResponseJSON`). 프레임은 vsock 포트 9002를
  씁니다. [firecracker-vsock.md](../firecracker-vsock.md)를 보세요.
- 호출 그룹(`owner`)은 페이로드가 아니라 연결에서 데몬이 결정합니다
  (`ctlDispatch(owner, line)`, `daemon/fc.go`의 `fcCtlConn`).
- 알 수 없는 동사는 `ctl: verb not allowed: <cmd>`를 받습니다
  (`ctlDispatch`의 default case).

### 2.2 동사와 사용 권한

여기서 "main"은 `main`이라는 이름의 그룹입니다 (`ctlMainGroup`). 그 외 모든
그룹은 non-main입니다. non-main 그룹이 main 전용 동사를 호출하면 `ctl: verb
not allowed for non-main groups: <verb>`를 받습니다.

**main 전용 오케스트레이션**

- **`spawn`** `{"group"}`. 그룹 이름은
  `^[a-z0-9][a-z0-9_-]{0,31}$`와 일치해야 합니다 (`ctlGroupRE`). 아니면:
  `ctl: invalid group name (must match [a-z0-9][a-z0-9_-]{0,31})`.
  - `main`은 스폰할 수 없습니다: `ctl: cannot spawn 'main'`.
  - 등록 그룹은 최대 `ctlMaxSpawn = 100`개:
    `ctl: spawn cap reached (100 groups)`. 이미 존재하는 그룹의 재스폰은
    허용됩니다.
  - 항상 `main:false`로 스폰합니다.
  - 응답에는 `port`가 있습니다.
- **`send`** `{"group","msg","session"?,"reply"?,"from_session"?}`.
  - `main`으로 보내면 `ctl: cannot send to self`로 실패합니다.
  - 메시지는 큐에 들어가고 즉시 확인 응답됩니다.
  - 목표 루프 세션은
    `ctl: session <s> is reserved for the goal loop`로 거부됩니다.
  - `reply:true`는 큐 삽입과 동시에 대상의 일회성 보고 윈도를
    장전(arm)하고, 메시지에 `reportRequestNote`를 덧붙입니다
    (`armReportAndEnqueue`).
  - 잘못되었거나 예약된 `from_session`은 기본 세션으로 폴백합니다.
- **`stop`** `{"group"}`. `main`은 중지할 수 없습니다: `ctl: cannot stop 'main'`
  (`stopGroup`).
- **`list`**: 응답에 `groups`가 있습니다 (`listGroups`).
- **`resources`**: 캐시된 호스트 측 샘플, 읽기 전용 (`resourcesCtlResp`).
- **`config_set`** `{"group"?, model/effort/provider}`.
  - 기본 대상은 호출자 자신의 그룹입니다.
  - **자세(posture) 키는 거부됩니다**. 해당 키는 `network`, `internet`,
    `root`, `ports`, `size`, `autostart`, `kvm`입니다 (`configPostureKey`).
    오류는
    ``ctl: <key> is an operator-only setting — ask the operator (TUI /config, or koto ctl config)``.
  - 여기서 설정할 수 있는 것은 `model`, `effort`, `provider`뿐입니다
    (`configCmd`).
- **`tail`** `{"group","n"}`. 피어의 `.cs/log`에서 마지막 `n`줄을
  돌려줍니다:
  - `n`은 기본 50이고 200으로 잘립니다.
  - 파일 끝에서 최대 256 KiB까지 읽습니다 (`readTail(…, 256*1024)`).
  - 속도 제한: 버스트 `ctlPollBurst = 30`, `ctlPollRefill = 500ms`마다 토큰
    하나가 채워지는 그룹별 토큰 버킷. 한도를 넘으면:
    `ctl: tail is being polled too fast — slow down (this is rate limited per group)`.

**모든 그룹, 자기 대상**

- **`job_done`** `{"id","rc","out"(base64),"total","session"}`. 작업이 끝날
  때 `cs-job`이 보냅니다.
  - 출력 꼬리는 `ctlMaxJobOut = 8<<10`(8 KiB)으로 제한됩니다.
  - `id`는 64바이트, `rc`는 8바이트 안에 들어가야 합니다 (`ctlJobField`).
  - 잘못된 세션은 기본 세션으로 폴백합니다.
  - 결과를 기록하고(`recordJobDone`) 작업 목록을 갱신합니다.
- **`notify`** `{"severity","title"(b64),"msg"(b64),"session"?}`.
  - `high`가 아닌 모든 심각도는 `normal`이 됩니다.
  - 제목은 200바이트, 메시지는 2000바이트로 잘립니다
    (`notifyTitleMax`, `notifyMsgMax`).
  - 속도 제한: 버스트 5, 이후 15초당 1건 (`notifyRateBurst`,
    `notifyRateRefill`).
  - 오류: `ctl: notify: empty title and message`,
    `ctl: notify: rate limited`, `ctl: notify: backlog full`.
  - 예약된 세션은 기본 세션으로 옮겨집니다.
- **`report`** `{"msg"(b64)}`. non-main 그룹 전용. main은
  `ctl: main has no delegator to report to`를 받습니다.
  - 장전된 윈도가 필요하며, 없으면 ``no reply pending: main must delegate
    with reply:true first (one report per delegation)``.
  - 메시지는 정화되고 `reportMsgMax = 4000`바이트로 잘립니다
    (`daemon/report.go`).
  - 빈 메시지는: `ctl: report: empty message`.
- **`goal_done`** `{"id","note"(b64)}` 및 **`goal_verdict`**
  `{"id","met","reasons"(b64)}`. note와 reasons는
  `goalNoteMax = 4000`으로 잘립니다 (`recordGoalDone`,
  `recordGoalVerdict`).
- **`goal_set`** `{"group"?,"name"?,"text","criteria","plan"?,"max_iterations"?}`.
  - non-main 호출자는 항상 자신을 대상으로 강제됩니다. 빈 group은 호출자
    자신의 그룹을 뜻합니다.
  - `main`에 대한 목표는 거부됩니다: `ctl: cannot set a goal on main`.
  - **main이 `plan:false`로 목표를 설정하면 plan-first로 강제되는데**, 계획
    승인은 사람의 몫이기 때문입니다 (`ctlDispatch`의 로그 줄).
- **`goal_approve`** `{"name"?}`. 자기 자신 전용. 다른 그룹을 지정하면
  ``ctl: goal_approve is self-only (a plan set on a peer needs a human)``으로
  실패합니다. 또한 그룹이 스스로 설정한, 승인 대기 중인 목표가 있어야
  합니다. 아니면: ``ctl: goal "<slug>" was set by <who> — its plan needs a human
  to approve`` (`ctlGoalSelfSet`, `daemon/goals.go`).
- **`goal_status`** `{"group"?}`. non-main 호출자는 자신의 목표만 봅니다.
- **`goal_pause` / `goal_interrupt` / `goal_resume` / `goal_cancel`**
  `{"group"?,"name"?}`. non-main 호출자는 자신으로 강제됩니다. main은 어떤
  그룹이든 조종할 수 있습니다.
- **`sched_add`** `{"group"?,"cron","msg"}`.
  - non-main 호출자는 자신으로 강제됩니다.
  - main이 `group`을 비우면 기본값은 `main`입니다 (`addSched`).
- **`sched_list`**: non-main 호출자는 자신의 스케줄만 봅니다.
- **`sched_del` / `sched_toggle` (`"enabled"`) / `sched_run`** `{"id"}`.
  non-main 호출자는 그 스케줄을 소유해야 하며, 아니면 `ctl: not your
  schedule` (`ownsSched`). 알 수 없는 id는 하부의 "not found" 오류로
  떨어집니다.

### 2.3 호스트 CLI와의 차이

- 호스트의 `koto ctl config`는 ACL이 허용하면 posture 키를 설정할 수
  있습니다. ctl 플레인의 `config_set`은 불가능합니다 (`configPostureKey`).
- ctl 플레인에는 `destroy`, `restart`, `runscript`, `acl`, `shell` 동사가
  없습니다. 어느 것도 `ctlDispatch` switch에 나타나지 않습니다.
