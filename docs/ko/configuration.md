../configuration.md의 번역본입니다. 영어 원문이 기준입니다.

# 구성

아래의 모든 설정은 이 트리의 소스와 대조해 확인했습니다. 각 행에는
참조(`file` + 함수/상수)가 붙어 있어 grep으로 확인할 수 있습니다. 코드가
동작을 분명히 하지 않는 곳에서는 본문에 **unverified(미검증)** 라고
적었습니다.

구성 표면(surface)은 세 가지입니다:

1. `/etc/koto/koto.env` — 설치된 daemon의 서비스 환경.
2. 프로세스 환경 변수(`KOTO_*`와 몇 가지 레거시 이름) — daemon,
   `koto ctl`, `koto tui`, `koto update`, 설치 스크립트가 읽습니다.
3. group별 `config.json` 키(`model`, `provider`, `network`, …) —
   `koto ctl config`, TUI `/config`, 또는 ctl 플레인의 `config_set`
   동사로 설정합니다.

---

## 1. `/etc/koto/koto.env`

- 경로: `/etc/koto/koto.env` — `daemon/install.go` 상수 `envFilePath`.
- systemd가 렌더링된 유닛 `/etc/systemd/system/koto.service`의
  `EnvironmentFile=`을 통해 로드합니다 — `daemon/install.go`
  `renderUnit`, 상수 `unitPath`.
- `koto install`이 씁니다 (`daemon/install.go` `writeEnvFile`). install을
  다시 실행해도 파일에 이미 있는 값은 **보존**됩니다 (`writeEnvFile`의
  `existing`에 대한 병합 루프). 파일이 존재하는데 읽을 수 없으면 install은
  `refusing to rewrite it: re-running install must preserve the values set
  there`와 함께 거부합니다.
- 설치 스크립트가 쓰는 헤더: ``Edit and `sudo systemctl restart koto` to apply.`` (`writeEnvFile`).
- 파싱: 한 행에 `KEY=VALUE`, 빈 행과 `#` 주석은 무시, 키와 값의 공백은
  트리밍 — `daemon/install.go` `parseEnvFile`.

`writeEnvFile`이 쓰는 키 (`defaults`와 `optional` 슬라이스):

항상 쓰이는 키 (이미 설정된 값은 보존):

- `KOTO_HOME` — 기본값 = install의 `-state` 디렉터리이며, 그 기본값은
  `/var/lib/koto` (`daemon/install.go` 상수 `defaultStateDir`, 플래그
  `state`).
- `KOTO_BIND=127.0.0.1`
- `KOTO_PORT=8443`

선택적 키 (비어 있으면 `#KEY=`로 주석 처리되어 쓰임):

- `KOTO_HOST_CPUS`
- `KOTO_HOST_MEM_MIB`
- `ANTHROPIC_API_KEY` — 의도적으로 `<state>/creds`에서 시딩하지
  **않습니다** (`writeEnvFile`의 주석).
- `KOTO_CLAUDE_BIN` — 설치 시점에 해석된 claude CLI 경로로 설정됩니다
  (`installClaudeBin`); 다른 키와 달리 파일의 옛 값이 아니라 새로 해석된
  값이 쓰입니다 (`kv[0] != claudeBinEnv` 분기).

유닛은 또한 `Environment=HOME=<state dir>`, `WorkingDirectory=<state
dir>`, `ExecStart=/usr/local/bin/koto daemon`을 설정합니다 (`renderUnit`).

---

## 2. 환경 변수

### 2.1 Daemon (`koto daemon`)

- `KOTO_HOME` — 상태 루트. 설정되면 절대 경로로 변환됩니다. 설정되지
  않으면 daemon은 **현재 작업 디렉터리**를 사용합니다 — `daemon/daemon.go`
  `kotoHome`, `here`. 파생 경로 (`initPaths`): `groups/`, `groups.json`,
  `schedules.json`, `goals.json`, `run/`, `metrics.jsonl`.
- `KOTO_BIND` — gRPC 수신 주소. 기본값 `127.0.0.1` — `daemon/daemon.go`
  `daemonMain`. 루프백이 아닌 값은 보안 자세(posture) 경고를 냅니다:
  `KOTO_BIND=<v> exposes the control plane beyond this host…` —
  `daemon/posture.go`.
- `KOTO_PORT` — gRPC 수신 포트. 기본값 `8443` — `daemon/daemon.go`
  `daemonMain`. 이 포트(그리고 무조건 `8443`)로 향하는 guest 이그레스는
  거부됩니다 — `daemon/proxy.go` `egressTargetAllowed`.
- `KOTO_PORTS_BIND` — group의 게시(published) `ports`가 바인드되는
  호스트 주소. 기본값 `127.0.0.1` — `daemon/fc.go` (`portBind :=
  envOr(...)`).
- `KOTO_HOST_MEM_MIB` — fleet 메모리 상한(MiB). `0` = 무제한; 미설정 =
  `/proc/meminfo` MemTotal의 90% (`fcHostMemDefaultPct = 90`); 잘못된
  값은 `KOTO_HOST_MEM_MIB=%q invalid; using derived default`를 로그에
  남깁니다 — `daemon/fchostmem.go` `fcHostMemInit`. 용량을 전혀 알아낼 수
  없고 변수도 미설정이면 daemon은 VM 스폰을 거부합니다 (`REFUSING VM
  spawns until KOTO_HOST_MEM_MIB is set (use 0 for explicitly
  unlimited)`).
- `KOTO_CLAUDE_BIN` — OAuth 토큰 갱신을 위해 실행되는 claude CLI. 기본값은
  꾸밈없는 `claude` (PATH 조회) — `daemon/claudebin.go` `claudeBin`, 상수
  `claudeBinEnv`.
- `ANTHROPIC_API_KEY` — 설정되어 있으면 키 파일
  `<state>/creds/anthropic-api-key`보다 우선합니다 — `daemon/proxy.go`
  `currentAPIKey`, `proxyInitPaths`. 파일은 요청마다 다시 읽힙니다.
- `CRED_PATH` (레거시 이름, `KOTO_` 접두사 없음) — OAuth 자격 증명 파일.
  기본값은 `<state>/.claude/.credentials.json`이 존재하면 그것, 아니면
  `$HOME/.claude/.credentials.json` — `daemon/proxy.go` `proxyInitPaths`.
- `PROXY_PORT` (레거시) — group별 프록시 포트 할당의 기준값. 기본값
  `8787` — `daemon/daemon.go` 변수 `PORT_BASE`, `initPaths`.
- `BIND` (레거시) — 프로세스 내 프록시 리스너의 바인드 주소. 기본값
  `127.0.0.1` — `daemon/daemon.go` `daemonMain` → `proxyStart`.
- `INVOCATION_ID` (systemd가 설정) — 보안 자세 소견의 문구를 정하는 데만
  확인됩니다 (`the daemon is not running under koto.service`) —
  `daemon/posture.go`.

### 2.2 설치 스크립트 (`koto install`)

- `KOTO_HOST_CPUS` — fleet CPU 상한(전체 코어 수)으로, 유닛에
  `CPUQuota=<n×100>%`로 렌더링됩니다. 기본값 `nproc-1` (최소 1); `0` =
  무제한 (지시어 없음) — `daemon/install.go` `hostCPUs`,
  `installCPUQuota`. 이 값은 **설치 스크립트의** 프로세스 환경에서
  읽힙니다 (`hostCPUs`의 `os.Getenv`); 실행 중인 daemon은 읽지 않습니다.
  koto.env에만 있는 값이 렌더링된 유닛에 도달하는지는
  **unverified(미검증)** 입니다 (koto.env를 설치 스크립트 환경으로
  로드하는 코드 경로를 찾지 못함).
- `KOTO_HOME` — `stateDirOf`의 기본 상태 디렉터리 (`daemon/install.go`).

### 2.3 클라이언트 (`koto ctl`, 그리고 마법사의 smoke test)

`daemon/ctl_cli.go`에서 (`ctlUsage`의 env 행, `ctlClient`, `ctlCredsDir`,
`newKotoClient`):

- `KOTO_ADDR` — 접속 대상. 기본값 `127.0.0.1:8443`.
- `KOTO_CREDS_DIR` — PKI 디렉터리. 해석 순서 (`ctlCredsDir`):
  `KOTO_CREDS_DIR`, 없으면 `./creds`가 존재하면 그것, 없으면
  `$KOTO_HOME/creds`, 없으면 `/var/lib/koto/creds`가 존재하면 그것,
  없으면 `creds`.
- `KOTO_CLIENT` — 신원(identity) 이름. 기본값 `agent`. creds 디렉터리의
  `client-<name>.crt`, `client-<name>.key`, `token-<name>`을 선택합니다.
- `KOTO_CERT`, `KOTO_KEY`, `KOTO_CA` — 개별 파일 경로 오버라이드 (기본값
  `client-<name>.crt`, `client-<name>.key`, `ca.crt`).
- `KOTO_TOKEN` — bearer 토큰의 **값** (경로가 아님). 미설정이면
  `token-<name>`을 읽습니다. 그 파일이 없으면: ``no token: set KOTO_TOKEN or provide <path> (mint with `make pki-client NAME=<name> ROLE=agent`)``.
- `KOTO_SERVER_NAME` — 서버 인증서 SAN에 없는 접속 대상을 위한 TLS
  server-name 오버라이드.
- TLS 최소 버전은 1.3입니다 (`newKotoClient`의 `tls.VersionTLS13`).

### 2.4 `koto tui`

`daemon/tui_cmd.go`: 플래그 `-state`의 기본값은 `$KOTO_HOME`, 없으면
`/var/lib/koto`. `<state>/creds/token-tui`를 읽고 다음 환경으로
`koto-tui`를 exec합니다:

- `KOTO_CA=<state>/creds/ca.crt`
- `KOTO_CERT=<state>/creds/client-tui.crt`
- `KOTO_KEY=<state>/creds/client-tui.key`
- `KOTO_TOKEN=<contents of token-tui>`
- `KOTO_ENDPOINT=$KOTO_ADDR` (기본값 `127.0.0.1:8443`)
- `KOTO_SERVER_NAME=koto-daemon`
- `KOTO_PROMPTS_DIR=<state>/prompts`
- `SOCK_PATH=<state>/run/tui/koto.sock`
- `KOTO_TUI_TERM=$TERM` (기본값 `xterm-256color`)
- `KOTO_SCRIPTS_DIR=<state>/scripts` — 그 디렉터리가 존재할 때만.

토큰 누락 오류: ``no TUI token in <creds> — mint one with `koto pki client -creds <creds> tui` ``.

### 2.5 `koto update`

- `KOTO_REPO_URL` — 전체 저장소 URL (끝의 `/`는 제거).
- `KOTO_REPO` — `owner/name`, 기본값 `jpzk/koto`; URL은
  `https://github.com/<repo>`가 됩니다.
— `daemon/update.go` `updateRepoURL` (`install.sh`를 그대로 반영).

`install.sh` 자체는 추가로 `KOTO_KEY_FPR`, `KOTO_KEYSERVER`,
`KOTO_ARCH` (기본값 `x86_64`), `KOTO_VERSION` (기본값 `latest`),
`KOTO_DEST`, `KOTO_RELEASE_PUBKEY`를 읽습니다 — `install.sh` 맨 위의
할당문.

### 2.6 내부 변수 (운영자 설정 아님)

`daemon/` 코드에 나오지만 koto 자신이 설정하는 변수입니다. 설정하지
마세요.

- `KOTO_USERNS_READY` — userns 부트스트랩 단계 마커 (`unset`/`1`/`2`) —
  `daemon/userns.go` 상수 `usernsEnv`.
- `KOTO_JAIL_MAX_UID` — 부트스트랩 re-exec들 사이에서 전달되는 VM별 uid
  대역의 클램프된 끝값; `(30000, 60000]`에서만 수용 — `daemon/fcjail.go`
  상수 `jailMaxEnv`, `fcJailBaseUID`, `fcJailMaxUID`.
- `KOTO_DEFAULT_CLAUDE_MODEL`, `KOTO_DEFAULT_VENICE_MODEL` —
  `defaultClaudeModel` / `defaultVeniceModel`의 값으로 guest init 환경에
  주입됩니다 — `daemon/fc.go` (init `env` 맵), `daemon/groups.go`. 함께
  주입되는 것: `ANTHROPIC_BASE_URL=http://127.0.0.1:18888`
  (`fcGuestProxyTCP`), 그리고 group의 network가 `none`이 아닐 때
  `NO_PROXY`/`no_proxy=127.0.0.1,localhost`.
- `KOTO_SESSION` — guest 내부에서 `entrypoint.sh`가 턴마다 export합니다;
  daemon은 작업(job) 귀속을 위해 guest 프로세스 환경에서 이를 읽습니다 —
  `daemon/jobs.go`, `daemon/send.go`.
- `KOTO_ENDPOINT`, `KOTO_PROMPTS_DIR`, `KOTO_SCRIPTS_DIR`,
  `KOTO_TUI_TERM` — `koto tui`가 TUI 자식 프로세스를 위해 설정합니다
  (2.4절).

---

## 3. group별 구성 (`config.json`)

- 위치: `<state>/groups/<group>/.cs/config.json` — `daemon/config.go`
  `groupConfigPath`. group별 잠금 아래에서 원자적으로(임시 파일 + rename,
  모드 0600) 쓰입니다 — `updateGroupConfig`.
- group이 존재해야 합니다: `no such group: <g> (spawn it first)`; 잘못된
  이름: `invalid group name` — `daemon/config.go` `configCmd`.
- 알 수 없거나 잘못된 값은 **조용히 무시**됩니다 (이전 값 유지) —
  `daemon/config.go` `applyConfig`.
- `null`, `""` 또는 `[]` 값은 그 키를 **지웁니다** — `isClear`.
- 다시 읽으면 기본값이 채워진 *유효(effective)* 구성이 반환됩니다 —
  `effectiveConfig`. 읽기 전용 `io` 필드(`<MiB>/s / <ops>ops`)도
  보고합니다.

### 키

- `provider` — `claudesdk` | `venice`. 기본값 `claudesdk`
  (`daemon/groups.go` 상수 `defaultProvider`, `ensureProviderConfig`).
- `model` — 자유 문자열, 문자 집합 `^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`,
  최대 128자 (`configIdentRE`, `configMaxIdent`). 미설정 시 기본값:
  claudesdk에서는 `claude-sonnet-5`, venice에서는 `kimi-k2.5`
  (`daemon/groups.go` `defaultClaudeModel`, `defaultVeniceModel`).
- `effort` — `model`과 같은 문자 집합/길이 규칙. 미설정이면 `(default)`로
  표시됩니다 (`effectiveConfig`).
- `network` — `none` | `wan` | `lan` | `full` (`daemon/fcnet.go` 상수
  `fcNetNone`…`fcNetFull`). 기본값 `none` (NIC 없음). 저장된 다른
  문자열은 `none`으로 읽힙니다 (`daemon/groups.go`
  `groupConfig.network`). 다음 스폰(`/restart`)에 적용됩니다; `none`으로
  낮추는 것은 프록시의 바로 다음 요청에도 적용됩니다 (`applyConfig`의
  주석). 레거시 키 `internet`: `full` → `wan`, `none` → `none`,
  `network`로 이전됩니다.
- `size` — `small` | `medium` | `large` | `xlarge`. 기본값 `small`. 다음
  스폰에 적용됩니다. 디스크는 커지기만 하고 절대 줄어들지 않습니다.
  프리셋 (`daemon/fc.go` `fcSizePresets`; vCPU, RAM MiB, workspace 디스크,
  IO MiB/s, IO ops):
  - `small`: 2, 1024, 8 GiB, 100, 15000 (`fcDefaultVcpus`,
    `fcDefaultMemMiB`, `fcWorkspaceBytes`)
  - `medium`: 2, 2048, 12 GiB, 150, 22500
  - `large`: 4, 4096, 16 GiB, 200, 30000
  - `xlarge`: 8, 8192, 24 GiB, 250, 37500
- `root` — `yes` | `no`. 기본값 `no`. guest 내 암호 없는 sudo + 영구 root
  오버레이; 다음 스폰에 적용됩니다 (`daemon/fc.go` `groupRoot`).
- `kvm` — `yes` | `no`. 기본값 `no`. guest에 VMX/SVM(중첩 가상화)을
  노출합니다; 다음 스폰에 적용됩니다 (`daemon/fc.go` `groupKVM`). 참고:
  `koto ctl` usage 텍스트에는 나오지 않지만 플래그 `-kvm`은 존재합니다
  (`daemon/ctl_cli.go`, `fs.String("kvm", …)`).
- `autostart` — `yes` | `no`. 기본값 `no`. daemon 기동 시에만 읽힙니다
  (`autostartGroups`); group의 `/restart`는 이를 적용하지 않습니다.
- `ports` — JSON int 배열 또는 쉼표 문자열 (`8080,3000`). 각 포트는
  `[1024, 65535]` 범위여야 합니다; 중복과 범위 밖 항목은 버려지며 최대
  32개까지 유지됩니다 (`configMaxPorts`). 호스트에서 `KOTO_PORTS_BIND`
  (기본값 `127.0.0.1`)에 게시됩니다.

yes/no 키는 실패 시 닫힙니다(fail closed): 없거나, 파싱 불가능하거나,
다른 어떤 값이든 `no`입니다; 손으로 편집한 파일을 위해 JSON 불리언은
수용됩니다 — `daemon/groups.go` `groupConfigBool`.

### 누가 무엇을 설정할 수 있는가

- **자세(posture) 키** = `network`, `internet`, `root`, `ports`, `size`,
  `autostart`, `kvm`. gRPC에서 이들 중 하나라도 담은 Config 요청은 합성
  관리자 전용 동사 `config_posture`로 인가됩니다 — `daemon/auth.go`
  `postureVerb`, `daemon/acl.go` `adminOnlyVerbs`.
- guest 내 ctl 플레인에서 `config_set`은 main 전용이며 (`ctl: verb not
  allowed for non-main groups: config_set`) 모든 자세 키를 `ctl: <key> is
  an operator-only setting — ask the operator (TUI /config, or koto ctl
  config)`로 거부합니다 — `daemon/ctl.go` `configPostureKey`. 따라서
  main이 거기서 설정할 수 있는 것은 `model`, `effort`, `provider`
  뿐입니다.
- 역할은 [acl-and-auth.md](acl-and-auth.md)를 참조하세요.
