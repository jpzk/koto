../groups-goals-schedules.md의 번역본입니다. 영어 원문이 기준입니다.

# 세션, 백그라운드 작업, 목표, 스케줄, report/reply

이 페이지는 그룹이 존재하게 된 뒤 그 안에서 작업이 어떻게 돌아가는지를
다룹니다: 채팅 세션, 턴 큐, `cs-job` 백그라운드 작업, 목표 루프, cron
스케줄, 그리고 `main`과 피어 사이의 일회성 보고 윈도. 동사 자체(인자,
오류 문자열, 호출 권한)는 [ctl-reference.md](ctl-reference.md)에 있습니다.
이 페이지는 그것들이 무엇을 하는지 설명합니다.

`daemon/goals.go` 같은 경로는 저장소 루트 기준입니다. 각 주장은 grep으로
찾을 함수나 상수의 이름을 명시합니다.

## 1. 세션

그룹(하나의 microVM, 하나의 워크스페이스)은 **세션**이라 부르는 여러 개의
독립적인 대화를 가질 수 있습니다.

- **기본 세션.** 와이어에서 `""`는 그룹의 기본 세션을 뜻합니다. `"-"`와
  `"default"`는 그 별칭입니다 (`normalizeSession`, `daemon/sessions.go`).
- **이름 규칙.** 그 외의 이름은
  `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`와 일치해야 합니다 (`sessionNameRE`).
  아니면 오류는 `invalid session name (must match [A-Za-z0-9][A-Za-z0-9_-]{0,31})`
  입니다 (`normalizeSession`).
- **동시성.** 턴은 세션 안에서는 한 번에 하나씩, 세션 간에는 병렬로
  실행됩니다. 그룹은 한 번에 최대 `groupSlots = 10`개의 턴을 실행할 수
  있습니다 (`daemon/queue.go`).
- **세션 상태.** 각 세션은 자기 대화를 유지합니다. 게스트는 이름별로
  claude 세션 id 하나를 `/workspace/.cs/sessions/<name>.id`에 고정합니다
  (`daemon/sessions.go`의 주석). 고정을 수행하는 스크립트
  (`entrypoint.sh`)는 이 저장소에서 추적되지 않으므로 그 세부 사항은
  여기서 **미검증**입니다.
- **레지스트리.** 호스트는 이름 있는 세션 목록을
  `groups/<g>/.cs/sessions.json`에 유지합니다 (`sessionRegPath`).
  - 세션으로 보내면 목록에 추가됩니다 (`registerSession`).
  - 목록은 최대 `sessionRegMax = 256`개의 이름을 담습니다. 그 뒤의 새
    이름은 목록에 오르지 않으며, 데몬은
    `session registry is full (256); "<name>" will not be listed`를 로그에
    남깁니다.
  - 레지스트리는 참고용 UI 상태입니다. 게스트의 id 파일이 실제 기록입니다
    (`sessionRegPath` 위의 주석).
- **예약된 `goal-` 접두사.** `goal-`로 시작하는 모든 이름은 목표 루프의
  소유입니다 (`goalSessionPrefix`, `isReservedSession`). 이 세션들은
  클라이언트나 ctl 플레인이 보내거나 지울 수 없습니다:
  - 그런 세션으로의 ctl `send`는
    `ctl: session <s> is reserved for the goal loop`로 실패합니다
    (`ctlDispatch`, `daemon/ctl.go`).
  - 예약된 세션을 겨냥한 `job_done`과 `notify`는 대신 기본 세션으로
    전달됩니다 (`daemon/notify.go`의 `recordJobDone`, `ctlDispatch`의
    `notify` case).

### 턴 큐 제한 (`daemon/queue.go`, `enqueue`)

모든 턴은 `enqueue`를 거칩니다: 운영자 send, 스케줄 발화, 작업 콜백, 목표
턴, 보고.

- **메시지 크기:** 최대 `sendMsgMax = 1 << 20` 바이트 (1 MiB). 더 큰
  메시지는 `message is <n> bytes; the limit is <max>`로 실패합니다.
- **그룹당 큐 바이트:** 최대 `sendQueuedBytesMax = 16 << 20` (16 MiB).
  초과 시:
  `group "<g>" already has <n> bytes queued (max <max>); retry later`.
- **세션당 큐 메시지:** 최대 `sendQueueDepth = 64`. 초과 시:
  `group "<g>" send queue full (64 pending); retry later`.
- **그룹 중지 중:** 오류는
  `group "<g>" is stopping; retry once it is down`.
- **꽉 막힌 세션(wedged):** 세션의 이전 턴이 멈춰 있고 그룹이 복구하지
  못했다면 오류는 `errSessionWedged`입니다. 그 텍스트는
  `this conversation's previous turn is wedged and the group could not
  self-heal`로 시작합니다.

## 2. 백그라운드 작업 (`cs-job`)

`cs-job`은 게스트 안에서 실행되는 POSIX 셸 스크립트입니다
(`sidecar/cs-job`). 명령을 자체 프로세스 세션(`setsid`)에서 시작하므로
명령은 턴이 끝나도 살아남습니다. 데몬은 작업 상태를 읽고 완료 콜백을 받을
뿐입니다 (`daemon/jobs.go`, `daemon/notify.go`).

### 명령

스크립트의 usage 줄에 다음 명령들이 있습니다:

- **`run [--no-notify] -- <cmd...>`**
  - 작업 id를 출력하고 즉시 반환합니다.
  - 콜백은 기본으로 켜져 있습니다. `--no-notify`가 끕니다. `--notify`는
    받아들이지만 아무것도 하지 않습니다.
  - 명령이 없으면 `cs-job run: no command`로 실패합니다.
- **`spawn [--no-notify] <prompt...>`**:
  `run -- cs-subagent <prompt...>`와 같습니다. `cs-subagent`는 LLM 호출을
  한 번 수행합니다: 최종 답을 stdout에, 진행 상황을 stderr에 출력합니다
  (`sidecar/cs-subagent`).
- **`list`**: `ID STATUS RC SESSION CMD` 열을 출력합니다. 명령은
  80바이트로 잘립니다.
- **`wait <id> [secs]`**: 작업이 끝날 때까지 기다립니다. 기본 예산은
  600초입니다. 작업의 `notify` 마커를 지우므로 **기다리면 콜백이
  취소됩니다**. 타임아웃 시: `cs-job: wait timeout (<budget> s) — job <id> still
  running` (종료 코드 1).
- **`status <id>`**: 작업 상태를 출력합니다. 상태 파일은 `running` 또는
  `done`을 담습니다. 상태 파일이 없으면 `unknown`을 출력합니다.
- **`peek [-n N|-new] <id>`**: 즉시 반환합니다. 헤더 줄
  `<id> <status> rc=<rc> bytes=<n> age=<s>s`를 출력한 뒤 출력을 보여줍니다:
  - 기본은 마지막 40줄.
  - `-new`를 주면 마지막 peek 이후 추가된 바이트만. 오프셋은
    `<jobdir>/peek`에 유지됩니다.
  - 어느 쪽이든 출력은 `CS_PEEK_MAX` 바이트(기본 8000)로 제한됩니다.
- **`logs <id>`**: 작업의 stdout과 stderr를 합쳐 출력합니다.
- **`rm <id>`**: 작업의 프로세스 그룹에 SIGTERM을 보내고, 약 2초
  (20 × 0.1초)까지 기다린 뒤, 아직 살아 있으면 SIGKILL을 보내고 작업
  디렉터리를 삭제합니다.
- **`clean`**: 실행 중인 작업과 콜백 전송 중(`notifying` 마커)인 작업을
  제외한 모든 작업 디렉터리를 삭제합니다.

### 규칙과 디스크 배치

- **작업 id.** id는 `mktemp` 베이스네임입니다. 영숫자가 아닌 id는
  `cs-job: invalid job id: <id>`로 거부됩니다 (`chkid`). 데몬의 검사는
  `jobIDRE = ^[A-Za-z0-9]{1,32}$`입니다 (`daemon/jobs.go`).
- **동시성 상한.** 한 번에 최대 `CS_MAX_JOBS`개 작업이 실행됩니다 (기본 8,
  `MAX=${CS_MAX_JOBS:-8}`). 초과 시:
  `cs-job: job cap reached (<n>/<max> running); wait or raise CS_MAX_JOBS`.
- **작업 디렉터리.** 각 작업은 `/workspace/.cs/jobs/<id>/`를 갖고, 그 안에
  `cmd`, `started`, `session`, `status`, `pgid`, `out`(stdout과 stderr
  합본), `rc`, 그리고 `notify` / `notifying` 마커가 있습니다.
- **세션 귀속.** 작업은 시작 시점의 `$KOTO_SESSION` 세션에 속하며, 그것이
  없으면 `default`입니다.
- **고아(orphan).** 스크립트 헤더에 따르면 VM 재시작으로 죽은 작업은
  나중에 `entrypoint.sh`가 `orphaned`로 다시 라벨링합니다. 그 파일은 이
  저장소에 없으므로 이것은 **미검증**입니다.

### 완료 콜백

1. 명령이 종료되면 작업은 `cs-job _notify <id>`를 실행합니다. 이는
   `id`, `rc`, `total`(출력 크기), `session`, 그리고 출력 마지막
   1500바이트의 base64를 담은 `job_done` 줄을 `/workspace/.cs/ctl`에
   씁니다.
2. 데몬은 그 꼬리를 최대 `ctlMaxJobOut = 8 << 10` 바이트까지 유지합니다
   (`daemon/ctl.go`). 결과를 기록하고(`recordJobDone`) 즉시 작업 목록을
   갱신합니다 (`kickJobsRefresh`).
3. 결과는 (group, session)별로 묶입니다. `notifyDebounce = 3s` 타이머가
   새 완료마다 다시 시작됩니다 (`armNotifyTimerLocked`,
   `daemon/notify.go`). 타이머가 발화하면 대기 중인 모든 결과가 **하나의**
   턴으로 시작한 세션에 전송됩니다. 그 턴은 `Background job(s) completed:`
   로 시작하고 각 꼬리를 `> `로 인용합니다 (`flushNotify`).
4. 그 턴을 큐에 넣을 수 없으면 `notifyRetryDelay = 30s` 뒤에
   재시도합니다.
5. 버퍼 상한:
   - 키당 `notifyMaxPending = 64`개 결과. 가장 오래된 것부터 버려집니다.
   - 그룹당 대기 세션 `notifyMaxKeysPerGroup = 32`개. 초과 세션의 결과는
     기본 세션으로 갑니다.

### 호스트가 작업을 보는 방법

호스트는 게스트의 워크스페이스를 마운트할 수 없습니다. 게스트 에이전트를
통해 작은 셸 스크립트를 실행해 작업 상태를 읽습니다 (`fcReadJobs`,
`jobsListScript`).

- **캐싱:** 결과는 `jobsRefreshTTL = 3s` 동안 캐시됩니다.
- **실패:** 읽기 실패 후 호스트는 `jobsFailBackoff = 60s`를 기다렸다가
  재시도합니다. 연속 `jobsFailWarnAfter = 3`회 실패하면 경고를 로그에
  남깁니다.
- **목록 크기:** 최대 `jobsMaxPerGroup = 256`개 작업이 나열됩니다.
- **로그:** `JobLogs`는 기본으로 마지막 `jobLogsDefaultTail` 바이트를
  반환합니다 (`fcJobLogs`). 제한은 [protocol.md](protocol.md)에 있습니다.

## 3. 목표 (목표 루프)

목표는 독립적인 심판(judge)이 수용 기준을 승인하거나 반복 상한에 도달할
때까지 그룹에서 턴을 계속 실행하라고 데몬에 지시합니다. 이 모든 것은
`daemon/goals.go`에 있습니다.

### 목표 생성 (`goalSetBy`)

- **필수 필드:** group, text, criteria. 하나라도 없으면 오류는
  `group is required`, `goal text is required` 또는
  `acceptance criteria are required`입니다.
- **크기 제한:** text와 criteria는 각각 `goalTextMax = 16 << 10` 바이트로
  제한됩니다. 더 큰 입력은 잘리는 게 아니라 거부됩니다:
  `goal text is <n> bytes; the limit is <max>`.
- **main에는 불가:** `main`에 대한 목표는
  `cannot set a goal on main`으로 거부됩니다.
- **반복 상한:** `max_iterations`가 `<= 0`이면
  `goalDefaultMaxIter = 20`이 됩니다. `goalMaxIterCeil = 200`을 넘는 값은
  200으로 낮춰집니다.
- **활성 목표 제한:** 그룹은 한 번에 최대 `goalMaxActive = 8`개 목표를
  실행할 수 있습니다. 초과 시:
  `group "<g>" already has <n> active goals; finish or cancel one first`.
- **옛 레코드:** 새 목표를 설정하면 그룹의 끝난 목표 레코드(met,
  cancelled, exhausted)가 삭제됩니다.
- **Id:** 목표 id는 16진수 12자입니다 (`newSchedID`, `goalIDLen`).
- **영속성:** 목표는 `<state>/goals.json`에 저장됩니다 (`GOALS_FILE`,
  `daemon/daemon.go`).

### 이름과 세션

각 목표 실행은 목표의 **이름**을 따서 지은 자체 세션 두 개를 받습니다
(`goalSessionSlug`). 이름이 없는 옛 레코드는 대신 id를 씁니다.

- 워커는 `goal-<name>`에서 실행됩니다 (`goalWorkSessionFor`).
- 심판은 `goal-<name>-judge`에서 실행됩니다 (`goalJudgeSessionFor`).

이름이 정해지는 방식 (`resolveGoalNameLocked`):

- 이름이 없으면 목표 텍스트의 슬러그를 씁니다 (`slugifyGoalName`).
- **길이:** 최대 `goalNameMax = 16`자. 아니면:
  `goal name must be at most 16 characters`.
- **세션 안전:** 파생된 두 세션 이름 모두 `sessionNameRE`를 통과해야
  합니다. 아니면: `goal name "<n>" is not usable as a session name`.
- **id처럼 보이면 안 됨:** `goalIDRE`(16진수 12자)와 일치하는 이름은
  거부됩니다. 오류는
  `goal name "<n>" looks like a goal id`로 시작합니다.
- **충돌:** 이름이 채팅 세션이나 다른 목표의 세션에 이미 쓰이고 있으면
  데몬은 결과가 16자에 들어가도록 베이스를 다듬으며 `-2`, `-3` 등을
  시도합니다. `-99` 이후에는
  `could not find a free name near "<base>"`로 포기합니다.

### 수명 주기

상태는 `goalStatus*` 상수들입니다: `planning`, `awaiting_approval`,
`running`, `paused`, `met`, `cancelled`, `exhausted`. `met`, `cancelled`,
`exhausted`는 종결 상태입니다 (`goalTerminal`).

1. **계획 (선택, 기본으로 켜짐).**
   - `plan`이 있으면 목표는 `planning`으로 시작합니다. 데몬은 계획 턴을 한
     번 실행하고 (`goalPlanPhase`, 프롬프트 `goalPlanMsg`) 목표를
     `awaiting_approval`로 옮깁니다.
   - 알림 `goal plan ready for review`를 보냅니다.
   - **승인 주체:** 호스트의 `GoalApprove` RPC, 또는 ctl 플레인의
     `goal_approve`. ctl 플레인은 그룹이 스스로 목표를 설정했을 때만
     동작합니다 (`ctlGoalSelfSet`). main이나 운영자가 그룹에 설정한
     계획은 사람이 승인해야 합니다.
   - main이 ctl 플레인으로 `plan:false` 목표를 설정하면 plan-first로
     바뀝니다 (`ctlDispatch`,
     [ctl-reference.md](ctl-reference.md) 참고).
2. **반복.**
   - 각 `running` 반복은 `Iteration`을 증가시킵니다.
   - 워커 세션의 컨텍스트를 지웁니다 (`clearGoalSession` →
     `clearSessionContext`). 호스트 트랜스크립트는 유지됩니다.
   - 워커 프롬프트(`goalWorkerMsg`)를 보냅니다. 프롬프트에는 다음이
     들어갑니다:
     - 목표 디렉터리 `/workspace/goal/<name>` (`goalDirFor`);
     - 이전 턴의 마감 보고 (`LastHandoff`, `goalCaptureHandoff`가
       트랜스크립트에서 가져오며, **마지막** `goalNoteMax = 4000`자로
       잘림);
     - 심판의 최근 피드백이 있다면 그것 (`LastFeedback`).
3. **완료 주장(claim).** 워커는 자기 턴이 실행 중일 때 `goal_done`을 보내
   완료를 주장합니다 (`recordGoalDone`).
   - 주장은 목표의 턴 동안에만 받아들여집니다. 아니면:
     `no goal turn in flight for "<g>"`.
   - 첫 번째 주장이 이깁니다.
   - id 없는 주장은 그룹에 열린 목표 턴이 정확히 하나일 때만 받아들여집니다
     (`resolveGoalWindowLocked`).
4. **심판.** 심판은 주장 뒤에만 실행됩니다 (`goalJudgeCheck`):
   - 컨텍스트가 지워지고 `goalJudgeMsg`를 받습니다.
   - `goal_verdict`로 답해야 합니다.
   - 평결 없는 심판 턴은 한 번 재시도됩니다.
   - 평결 없는 주장이 연속 `goalSilentJudgeMax = 3`회가 되면 목표는 사유
     `judge`로 일시정지됩니다.
   - 평결이 미달이면 사유가 `LastFeedback`으로 저장되고 루프가
     계속됩니다.
5. **달성** (`goalMarkMet`).
   - 상태가 `met`이 됩니다.
   - 알림 `goal met`이 전송됩니다.
   - 워커의 노트를 인용한 `[koto] goal finished: ...` 턴이 그룹의 **기본**
     세션으로 갑니다 (`goalInformCoordinatorMsg`).
6. **소진** (`goalMarkExhausted`).
   - `Iteration >= MaxIterations`인데 아무것도 수용되지 않았으면 목표는
     `exhausted`로 끝납니다. 일시정지하지 않습니다.
   - 높은 심각도의 알림 `goal exhausted`가 전송됩니다.
   - `[koto] goal ended WITHOUT completing: ...` 턴이 기본 세션으로 갑니다
     (`goalCoordinatorExhaustedMsg`).

### 일시정지, 재개, 취소

- **Pause** (`goalPause`): `running`에서만. 사유는 `operator`.
- **Interrupt** (`goalInterrupt`): `running`에서만. 사유 `interrupted`로
  일시정지하고 워커의 현재 턴도 취소합니다.
- **Resume** (`goalResume`): `paused`에서만. 목표는 이전 상태로
  돌아갑니다 (`running`, 또는 계획 중에 멈췄다면 `planning`).
- **Approve** (`goalApprove`): `awaiting_approval`에서만. 목표는
  `running`이 됩니다.
- **Cancel** (`goalCancel`): 종결이 아닌 모든 상태에서. 목표는
  `cancelled`가 됩니다.

데몬이 스스로 적용하는 일시정지 (`goalPauseWith`, `goalPauseOnStop`):

- **`stalled`:** 컨텍스트 리셋, 큐 삽입 또는 턴이 실패했거나, 턴이
  `turn_end`를 끝내 내지 못했을 때. 포기 전에 큐 삽입은
  `goalEnqueueRetries = 3`회, `goalRetrySleep = 30s` 간격으로
  재시도됩니다 (`goalEnqueue`).
- **`judge`:** 위의 4단계 참고.
- **`stopped`:** 그룹이 중지되었습니다. 알림은
  `goal paused (group stopped)`라고 말합니다.

목표가 더는 `running`이나 `planning`이 아닌데 큐에 남아 있던 목표 턴은
실행되지 않고 버려집니다 (`goalTurnShouldRun`). 오류 텍스트는
`goal turn skipped (goal no longer active)`입니다 (`daemon/queue.go`).

**Destroy:** 그룹을 파괴하면 그 목표들이 취소되고 레코드가 삭제됩니다
(`goalCancelOnDestroy`, `delGoalsFor`).

**데몬 재시작:** `running`이나 `planning` 상태의 목표는 데몬이 시작할 때
드라이버를 되찾습니다 (`resumeGoalDrivers`, `daemon/daemon.go`에서 호출).

### 동사가 가리키는 목표의 해석

approve, pause, interrupt, resume, cancel 동사는 선택적 이름을 받습니다
(`resolveGoalLocked`). 이름은 목표의 이름 또는 id와 일치합니다. 생략하면:

- 가능한 목표가 정확히 하나면 그 목표가 사용됩니다;
- 목표가 없으면: `group "<g>" has no goal`;
- 여럿이면: `group "<g>" has <n> goals — name one of: ...`.

### 보안 노트

`daemon/goals.go` 맨 위의 주석에 따르면 주장 윈도는 **악의가 아니라 사고**
를 막기 위한 것입니다. 그룹의 모든 세션은 하나의 uid, 하나의 워크스페이스,
하나의 ctl FIFO를 공유하므로 어떤 턴이든 주장을 쓸 수 있습니다. 분리가
적대적 에이전트를 상대로도 유지되어야 한다면 별도의 그룹을 쓰세요.

## 4. 스케줄 (cron)

스케줄은 `daemon/schedules.go`에, 파서는 `daemon/cron.go`에 있습니다.
스케줄은 `<state>/schedules.json`에 모드 `0600`으로 저장됩니다
(`SCHED_FILE`, `saveSched`).

### cron 문법 (`parseCron`, `parseField`)

- **다섯 필드:** 분 `0-59`, 시 `0-23`, 일 `1-31`, 월 `1-12`, 요일 `0-6`.
  - 요일 `7`은 범위 밖입니다.
  - 월과 요일 이름은 허용되지 않는데, 값이 `strconv.Atoi`를 거치기
    때문입니다.
- **별칭** (`cronAliases`): `@yearly` / `@annually` → `0 0 1 1 *`,
  `@monthly` → `0 0 1 * *`, `@weekly` → `0 0 * * 0`, `@daily` /
  `@midnight` → `0 0 * * *`, `@hourly` → `0 * * * *`. `@reboot`은
  없습니다.
- **필드 문법:** `*`, `N`, `A-B`, `,`를 쓰는 목록, `/S`를 쓰는 스텝.
  - `S`는 1과 그 필드 범위 크기 사이여야 합니다.
  - `N/S`는 N부터 필드 최댓값까지 S 간격이라는 뜻입니다.
- **일과 요일:** 둘 다 제한되어 있으면(어느 쪽도 `*`가 아니면) **어느 한
  쪽**이라도 일치할 때 날짜가 일치합니다. 고전 cron 규칙입니다
  (`dateMatches`).
- **시간대:** 데몬의 로컬 시간대. `time.Now()`가 명시적 시간대 없이
  쓰입니다.
- **오류 문자열:**
  - `cron: expected 5 fields, got <n> (...)`
  - `bad step "<s>" (must be 1..<n>)`
  - `range <a>-<b> out of bounds <lo>-<hi>`
  - 필드 오류에는 접두사가 붙습니다: `minute: `, `hour: `,
    `day-of-month: `, `month: ` 또는 `day-of-week: `.

### 제한 (`addSched`)

- **필수 필드:** `group is required`, `msg is required`.
- **cron 표현식:** 최대 `schedCronMax = 256` 바이트.
- **메시지:** 최대 `schedMaxMsg = 16 << 10` 바이트.
  초과 시: `msg too long (<n> bytes; max <max>)`.
- **그룹당:** 최대 `schedMaxPerGroup = 100`개 스케줄.
  초과 시: `schedule cap reached for <g> (100)`.
- **데몬 전체:** 최대 `schedMaxTotal = 2000`개 스케줄.
  초과 시: `daemon-wide schedule cap reached (2000)`.
- **언젠가는 발화해야 함:** 향후 4년 안에 일치가 없는 표현식은
  거부됩니다: `cron "<expr>" has no fire within 4 years` (`next`).
- **Id:** 스케줄 id는 16진수 12자입니다 (`newSchedID`).

### 발화

- **타이밍:** `cronLoop`는 매 벽시계 분 경계마다 깨어나 `NextDueAt`이
  지난, 활성화된 각 스케줄을 발화합니다.
- **도착지:** 발화는 메시지를 그룹의 **기본 세션**에 큐잉합니다
  (`fireSchedule` → `enqueueSend(it.Group, "", it.Msg)`). `sched_fired`
  이벤트를, 또는 `sched run`으로 수동 시작했다면 `sched_run`을 냅니다
  (`runSchedNow`).
- **보충 발화 없음:** 데몬이 꺼져 있는 동안 놓친 발화는 건너뜁니다. 로드
  시 `NextDueAt`은 현재 시각에서 다시 계산됩니다 (`loadSched`).
- **거부된 발화:** 그룹의 큐가 스케줄 발화를 거부하면 스케줄은 다시 기한이
  된 상태가 되어 다음 틱에 재시도됩니다 (`rearmSchedule`). 수동 `sched
  run`은 재시도되지 않습니다.
- **향후 일치 없음:** 스케줄에 더 이상의 일치가 없으면 비활성화됩니다
  (`Enabled = false`, `cronLoop`).
- **잘못 저장된 항목:** cron이 더는 파싱되지 않는 `schedules.json` 항목은
  로드 시 `load: drop <id> (bad cron ...)` 경고와 함께 버려집니다.
- **Destroy:** 그룹을 파괴하면 그 스케줄들이 삭제됩니다 (`delSchedsFor`).

누가 어떤 스케줄을 추가·조회·변경할 수 있는지는
[ctl-reference.md](ctl-reference.md) §2.2에서 다룹니다.

## 5. main과 피어 사이의 report/reply

이것은 `daemon/report.go`입니다. `main`이 피어에게 일을 맡기고 답을
**하나** 돌려받게 해줍니다.

1. **장전(arm).** `main`이 `"reply":true`로 보냅니다 (ctl `send`). 데몬은
   `reportRequestNote`를 덧붙여 메시지를 큐잉합니다. 이 노트는 피어에게
   `report`를 보내는 방법을 알려줍니다. 같은 단계에서 데몬은 그 그룹의
   보고 윈도를 장전합니다 (`armReportAndEnqueue`).
   - 큐 삽입이 실패하면 아무것도 장전되지 않습니다.
   - 윈도가 열린 상태에서 다시 보내면 교체됩니다("최신이 이김").
   - `from_session`은 main의 어느 세션이 보고를 받을지 고릅니다. 잘못되었
     거나 예약된 값은 main의 기본 세션을 뜻합니다.
2. **보고.** 피어가
   `{"cmd":"report","msg":"<base64>"}`를 `/workspace/.cs/ctl`에 씁니다
   (`ctlDispatch`의 `report` case).
   - 텍스트는 정화되고 다듬어집니다. 결과가 비면
     `ctl: report: empty message`로 실패합니다.
   - UTF-8 경계에서 `reportMsgMax = 4000` 바이트로 잘립니다
     (`truncateRunes`). 잘리면 전달된 턴은
     `(report clipped to 4000 bytes of <n>)`으로 끝납니다.
3. **전달.** `deliverReport`가 `[report from <g>]`로 시작하는 턴을 main에
   큐잉합니다. 피어의 텍스트는 줄마다 `> `로 인용되고
   (`reportQuoteBody`), 내용이 피어가 쓴 것이라는 경고가 뒤따릅니다.
4. **규칙:**
   - **장전당 보고 하나.**
     - 열린 윈도가 없으면 오류는
       `no reply pending: main must delegate with reply:true first (one report per delegation)`.
     - 보고가 이미 전달되는 중이면 오류는
       `a report for this delegation is already being delivered (one report per delegation)`.
   - **main 과부하.** main의 큐가 보고를 거부하면 윈도가 다시 열리고
     피어는
     `main is backlogged, report not delivered — retry later: ...`를
     받습니다.
   - **만료.** 윈도는 장전된 지 `reportArmedMax = 24h` 뒤에 만료됩니다.
   - **main은 보고할 수 없음:** main은
     `ctl: main has no delegator to report to`를 받습니다.
   - **중지와 파괴:** 피어를 중지하거나 파괴하면 그 윈도가 닫힙니다
     (`daemon/groups.go`의 `disarmReport`).

## 참고

- [ctl-reference.md](ctl-reference.md): `sched*`, `goal*`, `send`,
  `report`, `job_done`, `notify`의 정확한 동사 문법과 오류.
- [protocol.md](protocol.md): `Goal*`, `Sched*`, `Jobs`, `JobLogs`,
  `JobTail` RPC.
- [architecture.md](architecture.md): 큐, 게스트, ctl FIFO의 위치.
