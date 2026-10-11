../operations.md의 번역본입니다. 영어 원문이 기준입니다.

# 운영

Day-2 참조 문서: 로그가 어디로 가는지, 자원 모니터링과 알림이 어떻게
동작하는지, `koto update`로 업그레이드하기, `koto uninstall`로 koto
제거하기, 그리고 가장 자주 만나게 될 오류 메시지들. 모든 주장은 읽어 온
소스 파일과 심볼을 명시하므로, 심볼을 grep해서 확인하세요.

관련 문서: [configuration.md](configuration.md) (`KOTO_REPO`,
`KOTO_REPO_URL`, `KOTO_HOST_MEM_MIB` 등 환경 변수),
[ctl-reference.md](ctl-reference.md) (`logs`, `resources` 동사),
[getting-started.md](getting-started.md) (설치, 사전 점검 오류),
[groups-goals-schedules.md](groups-goals-schedules.md) (큐와 goal
오류), [acl-and-auth.md](acl-and-auth.md) (거부 오류).

## 1. 로그

로그 표면은 세 가지로 구분됩니다.

### 1.1 데몬 로그

- 모든 데몬 로그 라인은 하나의 깔때기 `logDeliver`
  (`daemon/events.go`)를 거칩니다. `emitLog` / `emitLogG` / `emitLogf`
  / `emitLogfG` / `emitLogfQuiet`가 호출합니다.
- 각 라인은 **stderr**에 `[<level>] <subsystem>: <msg>`로 기록됩니다
  (`logDeliver`의 `fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", ...)`).
  설치된 systemd 유닛 아래에서 그 stderr는 저널로 들어갑니다. 마법사의
  마지막 화면은 `journalctl -u koto -f`("daemon logs")를, 스모크 테스트
  실패는 `journalctl -u koto -n 50`을 가리킵니다(`stepHandoff`,
  `daemon/setup_steps.go`).
- 메시지는 `sanitize`로 정제된 뒤 `flattenInline`을 거치므로 로그 레코드
  하나는 항상 한 줄입니다(`logDeliver`).
- 데몬은 마지막 **200**줄을 메모리에 유지합니다(`logRingMax`,
  `daemon/events.go`). 새 `SubscribeLogs` 스트림(`koto ctl logs`, TUI
  로그 창)은 먼저 그 링을 받고, 이후 라이브 라인을 받습니다
  (`SubscribeLogs`, `daemon/grpc_server.go`). 구독자마다 256 항목
  채널이 있으며, 뒤처진 구독자는 `log` 서브시스템의 `warn` 라인
  `log stream fell behind: %d line(s) were not delivered to this
  subscriber`를 받습니다(`logDeliver`).
- 범위가 제한된(비관리자) 신원은 자기가 볼 수 있는 그룹에 귀속된
  라인만 봅니다. 귀속되지 않은 라인(그룹 `""`)은 감춰집니다
  (`logVisible`, `daemon/grpc_server.go`).
  [acl-and-auth.md](acl-and-auth.md)를 보세요.

### 1.2 error 라인은 운영자 알림이 됩니다

(`daemon/logalert.go`)

- `emitLogG`를 통해 나온 모든 `error` 수준 라인은 `main` 그룹으로
  **high** 심각도 알림으로도 전달됩니다(`forwardLogAlert`,
  `notifyDeliver(ctlMainGroup, "high", ...)`가 배달). `warn` 이하는
  로그에만 남습니다.
- 제목 형식: `<LEVEL> <subsystem>`에 귀속 시 ` [<group>]`을 더한 것,
  예: `ERROR fc [dev]` (`logAlertTitle`).
- 속도 제한: (subsystem, group)별 토큰 버킷은 버스트 5, 분당 1 재충전
  (`logAlertBurst`, `logAlertRefill`), 플릿 전역 버킷은 버스트 20,
  10초당 1(`logAlertGlobalBurst`, `logAlertGlobalRefill`). 억제된
  라인도 로그에는 남고, 배너만 떨어집니다.
- `emitLogfQuiet`는 전달을 건너뜁니다. 자원 알림은 자체 알림을 큐에
  넣기 때문에 이것을 씁니다(`resNotifyOperator`).
- 그룹별 미배달 알림 마커는 32개로 제한됩니다(`notifyQueueMax`,
  `daemon/logtail.go`). 큐는 메모리에 있으므로 데몬이 재시작하면
  미배달 마커는 사라집니다(`notifyQueueMax` 위 주석).

### 1.3 그룹별 기록(transcript)

(`daemon/logtail.go`)

- 각 그룹은 `<workspace>/.cs/log`(`groupLogPath`)의 그룹 스트림과 동시성
  슬롯마다 하나씩인 `<workspace>/.cs/log.<n>`(`slotLogPath`) 스트림을
  가집니다. `logPaths`는 그룹 스트림과 `groupSlots`개의 슬롯 스트림을
  나열합니다. `vol(g)`는 호스트에 있는 그룹의 워크스페이스
  마운트입니다.
- History는 각 스트림의 마지막 4 MiB까지만 읽고(`historyTailCap`),
  응답당 최대 8 MiB를 반환하며(`historyBytesMax`), 페이지 크기를 5000
  이벤트로 제한합니다(`historyLimitMax`). 4 MiB 창을 지난 스크롤백은 빈
  페이지로 끝납니다.
- 부분(종결되지 않은) 라인은 8 MiB까지 버퍼링되며(`tailMaxPartial`),
  그것을 넘으면 살아남은 꼬리에 `…[line truncated] `가 접두됩니다
  (`tailTruncMark`).

### 1.4 프록시 메트릭

- LLM 프록시는 완료된 요청마다 JSON 레코드 하나를
  `<state>/metrics.jsonl`에 덧붙입니다(`METRICS`, `daemon/daemon.go`
  에서 설정; `metricsAppend`, `daemon/proxy.go`).
- 256 MiB(`metricsMaxBytes`)에서 회전하며 이전 세대 하나를
  `metrics.jsonl.1`로 보관하므로, 디스크 최악의 경우는 상한의 약
  두 배입니다. 회전 실패는 error로 로그됩니다: `metrics rotation failed
  (%v): %s is past its %d MiB ceiling and still growing`.

## 2. 자원 모니터링

(`daemon/resources.go`, 와이어 타입은 `daemon/wire/wire.go`)

### 2.1 수집

- `resourcesLoop`는 시작 시 한 번, 이후 **30초**마다 샘플링합니다
  (`resSampleInterval`). 수치는 호스트에서 옵니다(`stat`, `statfs`,
  `/proc/<pid>/stat`). 따라서 멈췄거나 중지된 게스트도 보고됩니다.
- 예외: 게스트 메모리와 게스트 자신의 디스크 사용량은 **실행 중인**
  게스트 내부에서 `resGuestProbe`의 제한된 exec로 읽습니다
  (`stat -f ... /workspace; cat /proc/meminfo`). exec에는 5초 타임아웃
  (`resGuestExecTimeout`)이 있고, 한 번에 최대 8개 실행
  (`resGuestExecPar`), 전체 스윕에는 20초 기한(`resSweepDeadline`)이
  있습니다. 중지된 VM은 이를 위해 절대 부팅되지 않습니다. 실행 중인
  게스트가 갱신을 놓치면 마지막 판독값이 최대 3 샘플 간격 동안
  유지됩니다(`resGuestMaxAge`). 중지된 VM의 판독값은 즉시 버려집니다.
  어느 쪽이든 그 뒤에는 미지(0)로 읽힙니다(`resGuestRetain`).
- 그룹별 이력: 샘플 120개(`resRingLen`), 즉 30초 간격으로 1시간.
- 스냅샷은 1초 동안 캐시되므로(`resSnapTTL`) 더 빨리 폴링해도 같은
  답이 돌아옵니다.
- 호스트 파일시스템 수치는 `ROOT`(`<state>/groups`)에 대한 `statfs`
  이며, 여유 수치로 `Bavail`(비 root 사용자에게 가용한 블록)을 씁니다
  (`hostFSStats`).

### 2.2 필드 (`koto ctl resources`, 게스트 내 `resources` 동사)

두 플레인 모두 같은 `resourcesSnapshot`에서 만들어집니다
(`resourcesCtlResp`). JSON 이름은 `wire.GroupResources` /
`wire.HostResources`에서:

- 그룹별: `group`, `running`, `alloc_bytes`, `declared_bytes`,
  `alloc_pct`, `growth_bytes_per_hour`, `growth_span_seconds`,
  `rss_bytes`, `cpu_pct`, `vcpus`, `mem_mib`, `guest_disk_total_bytes`,
  `guest_disk_avail_bytes`, `guest_disk_used_bytes`,
  `guest_disk_used_pct`, `guest_mem_total_bytes`,
  `guest_mem_avail_bytes`, `guest_mem_used_pct`, `mem_committed_mib`.
- 호스트: `fs_total_bytes`, `fs_free_bytes`, `fs_used_pct`,
  `alloc_total_bytes`, `provisioned_bytes`, `groups`, `running_groups`,
  `mem_cap_mib`, `mem_committed_mib`, `mem_host_total_mib`.

의미:

- `alloc_bytes`는 워크스페이스 이미지의 할당된 블록입니다
  (`st_blocks × 512`, `statAllocBytes`). 이미지는 희소(sparse)하고
  Firecracker의 virtio-blk에는 discard가 없으므로 이 숫자는 사실상
  **커지기만** 합니다. 호스트의 비용이지 게스트가 얼마나 찼는지가
  아닙니다(`resources.go` 머리말 주석).
- `guest_disk_used_pct`는 게스트 파일시스템이 얼마나 찼는지이며 `df`
  처럼 계산됩니다: used ÷ (used + avail). 따라서 ext4의 예약 블록은
  어느 쪽에도 들어가지 않습니다(`resGuestDiskPct`). 그룹이 가득 차기
  직전인지 판단할 때는 이것을 쓰세요.
- `growth_bytes_per_hour`는 직전 10분 창에서 측정됩니다
  (`resGrowthWindow`). 샘플 이력이 창 전체를 덮기 전까지는(예: 데몬
  재시작 직후) **`growth_span_seconds` 0과 함께 0**으로 읽히며, 절대
  0 아래로 내려가지 않습니다(`resGrowth`).
- `provisioned_bytes`는 모든 그룹의 `size` 프리셋 합계입니다.
  `fs_total_bytes`를 초과하는 것은 정상입니다
  (`HostResources.ProvisionedBytes`의 주석).
- `mem_cap_mib` 0은 플릿 메모리 상한이 없다는 뜻입니다
  (`wire.HostResources`의 주석).

### 2.3 임계값 알림

(`resCheckThresholds`, `resNotifyOperator`)

- 임계값은 하드코딩되어 있습니다: 80%에서 경고(심각도 `normal`),
  90%에서 치명(심각도 `high`)(`resWarnPct`, `resCritPct`,
  `resAlertSeverity`).
- 알림은 수준이 **올라갈 때만** 발화합니다. 발화시킨 임계값보다 5
  포인트 아래로 내려가면 그 수준이 다시 장전됩니다(`resClearMargin`,
  `resArmedLevel`).
- 대상은 셋:
  - 호스트 파일시스템(`resSubjectHostFS`). 제목 `Host disk N% full`.
  - 실행 중인 각 그룹의 **게스트** 파일시스템(`resSubjectDisk`). 제목
    `Group <g> disk N% full`. 게스트 디스크 크기를 알 수 없는 그룹
    (중지됐거나 도달 불가)은 아무것도 올리지 않습니다.
  - 각 그룹의 마지막 10 샘플 평균 CPU(`resCPUAvgWindow`, ≈5분), 자기
    vCPU 대비 백분율(`resSubjectCPU`). 제목
    `Group <g> CPU N% sustained`.
- 배달: `resources` 서브시스템의 로그 라인(수준 1은 `warn`, 수준 2는
  `error`, `emitLogfQuiet`로 전송)과 `main`으로의 알림. main의 알림
  큐가 가득 차면 데몬은 `notification backlog full, alert dropped:
  <title>`을 로그합니다.

## 3. 업그레이드: `koto update`

(`daemon/update.go`)

```
usage: koto update [--check] [--version X] [-y]
```

플래그(`updateMain`):

- `--check`: 보고만 합니다. 최신이면 0으로, 업데이트가 있으면 **3**으로
  종료합니다(`updateExitAvailable`).
- `--version X`: 최신 대신 릴리스 X를 설치합니다. 다운그레이드는 이
  방식으로 버전을 지명할 때만 허용됩니다.
- `-y`: 데몬을 중지하라는 것을 포함해 프롬프트를 수락합니다. 설치
  프로그램에는 `--yes`로 전달됩니다.

단계:

1. **설치된 버전.** `/usr/local/bin/koto`가 있으면
   (`installedKotoBinPath`) 그것의 `version` 출력을 씁니다. 없으면
   실행 중인 바이너리 자신의 `kotoVersion`을 씁니다
   (`updateInstalledVersion`).
2. **최신 릴리스.** `HEAD <repo>/releases/latest` 요청을 보내고,
   태그는 리다이렉트를 따라가지 않고 `Location` 헤더에서 읽습니다
   (`updateLatestRelease`). 저장소는 `https://github.com/<KOTO_REPO>`
   (기본 `jpzk/koto`)이거나, 설정되어 있으면 `KOTO_REPO_URL`입니다
   (`updateRepoURL`). 명령 전체에 30초 컨텍스트가 있고, 컨텍스트가
   없으면 HTTP 클라이언트에 20초 타임아웃이 걸립니다.
3. **결정**(`updateDecide`; 버전은 `parseSemver`가
   `MAJOR.MINOR.PATCH`로 비교하며, 앞의 `v`와 `-`/`+` 접미사는
   무시합니다). 출력되는 메시지:
   - `koto %s is available (installed: %s)`: 더 새로우므로
     업데이트합니다.
   - `up to date: koto %s is the latest release`
   - `up to date: development build %s is based on the latest release
     %s`: `1.0.0-6-gd5150df-dirty` 같은 빌드는 기반 릴리스로 칩니다.
   - `release %s replaces development build %s (named explicitly)`
   - `%s is a downgrade from %s (named explicitly)`
   - `installed %s is newer than the latest release %s; nothing to do`
   - `installed %s is not a release build; %s is available`
   - `%q is not a release version`: 업데이트하지 않습니다.
4. **설치**(`updateRun`). root로 실행하면 거부합니다(`run as your
   normal user, not root — koto installs as you and uses sudo where it
   must`). 바이너리에 **임베드된** `install.sh`(`embeddedInstaller`,
   `//go:embed installer/install.sh`)를 임시 파일에 쓰고 터미널에서
   `sh <file> --version <target> --no-attach [--yes]`를 실행합니다. 그
   설치 프로그램이 릴리스를 검증하고 새 릴리스의 `koto install`과
   `koto setup`을 실행합니다(파일 머리말 주석). 설치는 데몬을 중지하며,
   그와 함께 실행 중인 모든 microVM도 중지됩니다.
   실패 시: `koto update: the installer failed: <err>`.

설치 프로그램을 다운로드하지 않고 임베드하는 이유는, 다운로드한
설치 프로그램이라면 자신이 검사하는 릴리스와 같은 채널로 고정된 서명 키
지문을 가져오게 되기 때문입니다(`update.go` 머리말 주석). 서명 검사
자체는 `install.sh`에 있으며
[getting-started.md](getting-started.md)에 설명되어 있습니다.

## 4. koto 제거: `koto uninstall`

(`daemon/uninstall.go`)

```
usage: koto uninstall [flags]
```

플래그(`uninstallMain`):

- `-state DIR`: 상태 디렉터리(기본 `/var/lib/koto`,
  `daemon/install.go`의 `defaultStateDir`).
- `--purge`: 상태 디렉터리와 `/etc/koto/koto.env`도 삭제합니다.
- `-n`: 드라이 런. 각 특권 명령은 `would: sudo …`로 출력만 되고 아무
  것도 바뀌지 않습니다(`uninstallOpts.step`).
- `-y`: 기본값을 수락합니다. **`--purge`와 결합하면 프롬프트 없이
  삭제합니다.**
- `-no-color`: 코드는 받아들이지만 usage 텍스트에는 나오지 않습니다.

위치 인자는 거부됩니다: `unexpected argument %q — this command takes
flags only`. root로 실행하면 거부됩니다: `run as your normal user, not
root (sudo is used only for the system files)` (`runUninstall`).

순서(`runUninstall`):

1. 유닛, `/usr/local/bin/koto`, `/usr/local/bin/koto-tui`,
   `/etc/koto/koto.env` 중 아무것도 없으면 `nothing installed — …`를
   출력하고 멈춥니다. 단 `--purge`가 주어졌으면 계속합니다.
2. **먼저 중지.** 유닛 파일 `/etc/systemd/system/koto.service`
   (`unitPath`)가 있거나 **또는** `systemctl is-active koto`가
   `active`라고 하면(`systemctlIsActive`), `systemctl stop koto`,
   `disable`, 유닛 제거, `daemon-reload`, `reset-failed`를 실행합니다.
   중지 실패는 중단시킵니다: `stop koto: … (fix, or `sudo systemctl
   kill koto`, then re-run)`. 유닛은 `TimeoutStopSec=25`를 설정합니다
   (`daemon/install.go`).
3. **라이브 데몬 검사.** `/proc`를 스캔해 상태 루트(환경의
   `KOTO_HOME`, 없으면 cwd)가 같은 디렉터리인 `koto daemon` 프로세스를
   찾습니다(`runningKotoDaemon`, `procKotoHome`). 아직 실행 중이면
   `a koto daemon is still running against …`로 거부합니다. `-n`이면
   경고만 합니다.
4. `/usr/local/bin/koto`와 `/usr/local/bin/koto-tui`를 제거합니다.
5. `--purge`가 없으면 보존한 것들을 나열하고(`ANTHROPIC_API_KEY`가
   들어 있을 수 있는 env 파일, 그룹 수와 `du -sh` 크기가 표시되는 상태
   디렉터리) 멈춥니다. 나중의 `koto install`이 이를 다시 집어 듭니다.
6. `--purge`가 있으면(`uninstallPurge`):
   - 상태 디렉터리는 `purgeRefusal`로 검사됩니다. 경로가 절대 경로가
     아니거나, 파일시스템 루트거나, 홈 디렉터리거나, `.git`이나
     `go.work`를 담고 있으면("that is a koto clone") 거부합니다
     (`refusing to delete <dir>: …`). 경로에 `.koto-version`,
     `groups`, `creds`, `fcassets`, `groups.json` 중 아무것도 없어도
     거부합니다(`no koto state found there …`).
   - `-y`가 아니면 `Delete it?`을 묻습니다(기본은 아니오).
   - (장치, 아이노드)가 프롬프트 전에 기록되고 삭제 직전에 다시
     검사됩니다(`dirIdentity`). 다르면 `… is not the directory that
     was checked (it was replaced while the confirmation was open) —
     refusing to delete it`으로 거부합니다.
   - 상태 디렉터리는 **sudo 없이** 제거됩니다(`os.RemoveAll`); 사용자
     소유이기 때문입니다.
   - `/etc/koto/koto.env`를 제거(sudo)한 뒤
     `rmdir --ignore-fail-on-non-empty /etc/koto`를 실행합니다.
   
중단된 제거는 다시 실행하면 마무리됩니다. 각 항목은 이미 없으면
감지되어 건너뛰기 때문입니다(머리말 주석).

개발 체크아웃에서의 대응물은 `Makefile`에 있습니다: `make clean`은 빌드
산출물, `metrics.jsonl`, `proxy.log`, `run`을 제거합니다.
`make clean-groups`는 `yes`를 입력하라고 물은 뒤
`groups groups.json schedules.json goals.json`을 **영구 삭제**합니다
(`FORCE=1`은 프롬프트를 건너뜁니다). `make clean-creds`는 `creds`를
제거합니다.

## 5. 트러블슈팅

각 항목은 코드에 나타나는 그대로의 메시지, 그 출처, 그리고 대처법을
제시합니다.

### 설치 / 설정

- `daemon did not answer within 45s: … check `systemctl status koto`
  and `journalctl -u koto -n 50``: 마법사의 스모크 단계
  (`daemon/setup_steps.go`). 저널을 읽으세요.
- `no usable /dev/kvm on this host — groups will not boot until that
  is fixed`: 같은 단계의 경고(`checkKVM`). 사전 점검 항목과 메시지는
  [getting-started.md](getting-started.md)에 나열되어 있습니다.

### 자격 증명과 LLM 프록시 (`daemon/proxy.go`)

- `no credentials: run `koto claude-login` (or set
  ANTHROPIC_API_KEY)`: Anthropic 자격 증명을 찾을 수 없습니다.
- `oauth token refresh failed — turns will 401 until it works (check
  `koto claude-login --status`): …` 그리고 `… but the token on disk is
  still expired — the CLI did not rotate it; run `koto claude-login``:
  OAuth 갱신 문제. error 라인이므로 `main`에 배너로도 나타납니다.
- `the venice credential is not available on this host; ask the
  operator` (HTTP 503): venice provider인 그룹인데 키를 찾지 못했습니다
  (`no venice key: write to …`).
- `koto proxy: <METHOD> <path> is outside the endpoint allowlist for
  provider <p> (the proxy relays inference and model metadata only)`
  (HTTP 403, `llmRefuse`): 게스트가 프록시가 중계하지 않는 API 경로를
  호출했습니다.
- `koto proxy: too many concurrent requests for this group`
  (HTTP 503): 60초를 기다린 뒤에도 그룹당 32개 또는 전체 128개를
  초과하는 요청이 비행 중입니다(`proxyMaxInflightPerGroup`,
  `proxyMaxInflightGlobal`, `proxyInflightWait`).
- `koto proxy: request-body memory budget exhausted; retry shortly`
  (503), `koto proxy: request body stalled` (408),
  `request body too large or unreadable` (413): 요청 본문 읽기 문제.

### 이그레스 (`daemon/proxy.go`)

- `egress denied: this group's network profile is 'none' (a raised
  profile applies on /restart)` (403): `network`를 설정하고
  ([configuration.md](configuration.md) 참조) 그룹을 재시작하세요.
- `egress blocked: target not permitted under this group's network
  profile` (403): 대상이 그룹 프로필이 허용하는 범위 밖입니다.
- `koto proxy: too many concurrent egress connections for this group`
  (503).

### 그룹 부팅 (`daemon/fchostmem.go`)

- `group %s needs %d MiB (mem %d + %d VMM margin) but the fleet cap is
  %d MiB with %d MiB already committed to running VMs — stop a VM,
  pick a smaller size, or raise KOTO_HOST_MEM_MIB`: 플릿 메모리 상한이
  가득 찼습니다.
- `refusing to boot %s: the host's memory capacity could not be
  determined …`: 상한을 계산할 수 없었습니다.

### 턴

- `this conversation's previous turn is wedged and the group could not
  self-heal — /restart the group …` (`errSessionWedged`,
  `daemon/queue.go`). 그룹을 재시작하세요. 그 밖의 큐 한도는
  [groups-goals-schedules.md](groups-goals-schedules.md)에서 다룹니다.

### 제거

- `a koto daemon is still running against …`: 메시지대로
  `sudo systemctl stop koto`나 `kill <pid>`로 중지한 뒤 다시
  실행하세요.
- `refusing to delete …: that is a koto clone, not an installed state
  dir`: `-state`가 소스 체크아웃을 가리킵니다. 진짜 상태 디렉터리를
  가리키게 하세요.
