../acl-and-auth.md의 번역본입니다. 영어 원문이 기준입니다.

# 인증과 권한 부여 (gRPC 컨트롤 플레인)

이 페이지는 누가 데몬의 gRPC API를 호출할 수 있고(TUI, Android, `koto ctl`,
스크립트) 각 호출자가 무엇을 할 수 있는지를 다룹니다. 게스트 내부 ctl 플레인
(`/workspace/.cs/ctl`)은 그룹 신원을 키로 하는 **별도의** 권한 부여 축이며,
`daemon/ctl.go`에 하드코딩되어 있습니다. `acl.json`의 지배를 받지 않습니다
(`daemon/acl.go` 헤더 주석). 그 규칙은
[ctl-reference.md, Part 2](ctl-reference.md)를 보세요.

아래의 모든 경로는 상태 루트 `HERE`에 대한 상대 경로입니다: `KOTO_HOME`이
설정되어 있으면 그 값(설치 모드, 보통 `/var/lib/koto`), 아니면 데몬의 작업
디렉터리 (`kotoHome`, `daemon/daemon.go`). 자격 증명 파일은 `<HERE>/creds/`에
있습니다 (`credFile`, `daemon/auth.go`).

## 1. 계층

모든 호출은 세 가지 검사를 순서대로 통과합니다 (`daemon/auth.go` 헤더 주석,
`authFromCtx`, `aclCheck`):

1. **mTLS**: 서버 인증서와 필수 클라이언트 인증서.
2. **베어러 토큰**: 인증(authentication), 클라이언트 인증서에 바인딩.
3. **ACL**: 역할·동사·대상 그룹에 의한 권한 부여(authorization).

주석은 이 계층들 아래의 WireGuard 오버레이도 언급합니다. 이는 koto 프로세스
밖에서 동작합니다.

### 1.1 리스너

- 기본 수신 주소는 `127.0.0.1:8443`입니다. `KOTO_BIND`가 주소를,
  `KOTO_PORT`가 포트를 재정의합니다 (`daemon/daemon.go`,
  `go autostartGroups()` 뒤). [configuration.md](configuration.md)를 보세요.
- gRPC keepalive: 서버 `Time: 30s`, `Timeout: 10s`. 집행 정책은
  `MinTime: 10s`에 `PermitWithoutStream: true` (`daemon/daemon.go`,
  `grpc.KeepaliveParams`).
- 시작 시 데몬은 `kotod ready grpc=<addr> (mTLS+token)`을 로그에 남기고
  `authBindingPreflight`를 실행합니다.

### 1.2 mTLS (`serverTLSConfig`, `daemon/auth.go`)

- 서버는 `creds/server.crt`와 `creds/server.key`를 제시합니다.
- 클라이언트 인증서는 필수이며 `creds/ca.crt`로 체인이 이어져야 합니다
  (`tls.RequireAndVerifyClientCert`). 최소 버전은 TLS 1.3입니다
  (`MinVersion: tls.VersionTLS13`).
- **허용 목록.** 클라이언트 리프 인증서의 SHA-256 지문(DER, 소문자 16진수)이
  `creds/clients.allow`에도 있어야 합니다
  (`VerifyPeerCertificate`, `loadClientAllow`).
  - 줄 형식: `<sha256hex> <name>`.
  - 첫 필드가 `#`로 시작하는 줄은 무시됩니다.
- **핸드셰이크마다 다시 읽습니다.** 줄 하나를 지우면 데몬 재시작 없이 그
  기기가 폐지(revoke)됩니다.
- 핸드셰이크 오류:
  - `client cert <fp> not in allowlist`
  - `clients.allow: <err>` (예: 파일 없음)
  - `no client certificate`

### 1.3 베어러 토큰 (`authFromCtx`, `tokenIdentity`)

- 토큰은 `authorization` gRPC 메타데이터에서 읽습니다. `Bearer ` 접두사는
  제거됩니다.
- 토큰은 SHA-256 해시되어 `creds/tokens.json`에서 조회되며, 이 파일은
  호출마다 다시 읽힙니다 (`loadTokenIdentities`).
- **인증서와 토큰은 바인딩됩니다 (감사 L6).** 클라이언트 인증서의
  CommonName이 토큰과 일치한 `tokens.json` 키와 같아야 합니다
  (`certIdentity`). 따라서 둘 중 하나의 폐지만으로 충분합니다:
  `clients.allow` 줄 삭제, 또는 `tokens.json` 항목 삭제.
- 실패는 gRPC `Unauthenticated`를 반환합니다:
  - `invalid or missing token` (로그: `rejected call from <addr> (bad/missing token)`)
  - `no client certificate`
  - `client certificate and token name different identities`
- **시작 시 사전 점검** (`authBindingPreflight`). 한쪽에만 존재하는 이름마다
  경고를 로그에 남깁니다:
  - `identity "<n>" has an allowlisted cert but no tokens.json entry — its calls will be refused (mint one: koto pki client <n>)`
  - `identity "<n>" has a token but no cert named "<n>" in clients.allow — its calls will be refused`

### 1.4 `tokens.json`의 형태 (`parseTokens`, `daemon/acl.go`)

이 파일은 `clientid → credential`을 매핑합니다. 세 가지 값 형태가
허용됩니다:

- `"<sha256hex>"`: 레거시 단독 해시. 역할은 `admin`입니다 (`defaultRole`).
- `{"hash": "<hex>", "role": "agent"}`: 단일 역할.
- `{"hash": "<hex>", "roles": ["reader", "ops"]}`: 복수 역할. 둘 다 있으면
  `roles`가 `role`을 이깁니다.

다음 두 경우 항목은 건너뜁니다(아무도 인증하지 못합니다):

- 이 형태들 중 어느 것에도 맞지 않거나 `hash`가 비어 있는 경우;
- 공백 제거 후 역할이 하나도 남지 않는 경우. 예를 들어
  `koto pki client -role "" x`는 `"roles": null`을 쓰는데, 이는
  건너뜁니다.

해시는 조회 전에 소문자로 변환됩니다.

## 2. PKI: 자격 증명 발급

동등한 경로가 둘 있습니다. 둘 다 같은 산출물을 만듭니다 (`daemon/pki.go`
헤더 주석):

- **Go:** `koto pki`. `koto setup`과 `koto install`이 사용합니다. openssl이나
  jq가 필요 없습니다.
- **Makefile:** `make pki-init`과 `make pki-client`. openssl과 jq를
  사용합니다.

### 2.1 `koto pki` (`pkiCliMain`, `daemon/pki.go`)

```
usage: koto pki init   [-creds DIR] [-san DNS:a,IP:1.2.3.4]
       koto pki client [-creds DIR] [-role r1,r2] <name>
       koto pki server [-creds DIR] -san DNS:a,IP:1.2.3.4
```

기본값: `-creds creds`. `init -san`의 기본값은 `defaultServerSANs` =
`DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`. `client -role`의 기본값은
`admin`.

#### `init` (`pkiInit`)

- CA가 없을 때만 만듭니다 (`pkiEnsureCA`): EC P-256, `CN=koto-ca`, 3650일
  유효.
- `ca.crt`는 있는데 `ca.key`가 없으면
  `<crt> exists but <key> does not — refusing to mint a new CA over it.` 뒤에
  복원/옮겨두기 조언을 덧붙이며 거부합니다.
- 서버 인증서를 발급합니다 (`pkiServerCert`): `CN=koto-daemon`, 825일 유효,
  ExtKeyUsage ServerAuth. SAN은 `DNS:…` 또는 `IP:…`여야 합니다. 그 외는
  `bad SAN "<s>" (want DNS:… or IP:…)`로 실패합니다.
- 서버 인증서는 다음의 경우 재발급됩니다: 없거나, `server.key`가 없거나,
  읽을 수 없거나 만료되었거나, 만료까지 `pkiRenewWindow`(30일) 이내이거나,
  아직 유효하지 않거나, CA로 체인이 이어지지 않을 때
  (`pkiServerCertNeedsReissue`). SAN은 비교하지 않습니다.
- `acl.json`은 파일이 없을 때만 시드합니다 (`pkiCreateNew`, 모드 0600):

  ```json
  {
    "agent": {
      "list": "*", "send": "*", "history": "*", "metrics": "*",
      "sched_list": "*",
      "subscribe_group": "*", "watch_state": "*"
    }
  }
  ```

- creds 디렉터리는 모드 0750으로 만듭니다.

#### `client <name>` (`pkiClient`)

- **이름 규칙:** `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`이고 `..` 금지
  (`pkiClientNameRE`, `pkiClientNameOK`). 아니면
  `invalid client name "<name>": letters, digits and ._- only, no path separators …`
  로 실패합니다.
- **쓰는 파일:**
  - `client-<name>.key`와 `client-<name>.crt`: EC P-256, `CN=<name>`,
    825일, ClientAuth.
  - `clients.allow`에 `<fp> <name>` 줄 추가(그 지문이 이미 있으면 제외).
  - `token-<name>`에 32바이트 랜덤 16진수 토큰 (0600).
  - `tokens.json`의 `<name>` 아래 `{"hash", "roles"}`.
- `tokens.json` 갱신은 creds 디렉터리에 flock을 잡고 (`pkiLockCreds`) 임시
  파일과 rename으로 원자적으로 씁니다.
- 손상된 `tokens.json`은 `tokens.json is corrupt — fix it on disk first: <err>`로 실패합니다.
- CA 키가 없으면 ``ca.key (run `koto pki init` first): <err>``로 실패합니다.

#### `server -san …`

- 서버 인증서만 재발급합니다. CA는 건드리지 않으므로 기존 클라이언트는
  계속 동작합니다.
- `server cert reissued for <san> — restart the daemon to apply`를
  출력합니다.
- `-san` 없이는 `pki server: -san is required (e.g. -san …)`로 실패합니다
  (종료 코드 2).

### 2.2 Makefile 경로 (`Makefile`, 타깃 `pki-init` / `pki-client`)

- `make pki-init [SERVER_SAN=…]`:
  - `SERVER_SAN`의 기본값은 `DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`.
  - `creds/ca.key`가 존재하면 거부합니다:
    `creds/ca.key exists — refusing to regenerate the CA …`.
  - `creds/acl.json`이 없으면 같은 `agent` 역할을 시드합니다.
- `make pki-client NAME=<client> [ROLE=r1,r2]`:
  - `ROLE`의 기본값은 `admin`.
  - `creds/ca.key`가 필요하며 `tokens.json` 편집에 `jq`를 씁니다.
  - `koto pki client`와 같은 파일을 씁니다.
- 키는 `umask 077` 아래에서 생성된 뒤 `chmod 600`됩니다.

### 2.3 setup이 만드는 신원 (`daemon/setup_steps.go`)

- setup의 PKI 단계는 `defaultServerSANs`에 운영자가 입력한 추가 SAN을 더해
  `pkiInit`을 실행합니다.
- 그다음 클라이언트 둘을 발급하는데, **둘 다 역할이 `admin`**이며, 각각
  인증서가 없을 때만 발급합니다:
  - `tui`
  - `agent` — `koto ctl`의 기본 `KOTO_CLIENT`.
- 따라서 `agent` *클라이언트*는 설계상 admin입니다. 주석은 감사 M11을
  인용하며, 2026-09-05에 결정되었습니다.
- 시드된 `acl.json`의 `agent`라는 *역할*은 별개의 것입니다:
  `koto pki client -role agent <name>`으로 발급한 범위 제한 신원을 위해
  존재합니다.

## 3. 권한 부여: 역할, 동사, 대상

### 3.1 모델 (`daemon/acl.go`)

- 사용자(clientid)는 하나 이상의 역할을 갖고, 각 역할은 대상에 대한 동사를
  부여합니다. 유효 권한은 사용자의 역할들에 대한 **합집합**입니다
  (`rolesAllowed`).
- `creds/acl.json`은 `role → {verb → targets}`를 매핑합니다.
  `daemon/acl.go`가 근거이며, `pki-init` 위의 Makefile 주석도 같은 내용을
  말합니다.
  - 대상은 그룹 이름, `"*"` 또는 `["*"]`(모든 그룹), 또는 목록입니다.
  - 동사 키 `"*"`는 그 대상에 대해 모든 동사를 부여하되, admin 전용 동사는
    **예외**입니다 (`grantFor`).
  - 레거시 평탄 형식 `{"agent": ["list", "send"]}`는 그 동사들을 모든
    대상에 부여한다는 뜻입니다 (`parseACL`).
- `acl.json`은 호출마다 다시 읽힙니다 (`loadACL`). 역할이나 권한 편집에
  재시작이 필요 없습니다.

### 3.2 `admin` 역할

`admin`은 하드코딩되어 있습니다 (`grantFor`, `defaultRole = "admin"`):

- 모든 대상에 대해 모든 동사를 호출할 수 있습니다.
- `acl.json`의 `admin` 키는 무시됩니다.
- `acl.json`이 없거나 손상되어도 admin은 잠기지 않습니다.
- `acl set admin …`은
  `the admin role is hardcoded and cannot be defined in the ACL`로
  실패합니다.
- `acl del admin`은 `the admin role is hardcoded and cannot be deleted`로
  실패합니다 (`aclSetRoleCmd`, `aclDelRoleCmd`).

### 3.3 실패 시 닫힘(failing closed)

다음 각각의 경우 요청은 거부됩니다 (`grantFor`, `roleAllowed`,
`parseACL`, `parseTargets`):

- **알 수 없는 역할:** 역할이 `acl.json`에 없음.
- **파일 없음:** `acl.json`이 없으면 admin 이외의 역할은 존재하지 않음.
- **손상된 파일:** 잘못된 형식의 `acl.json`(`parseACL`이 nil 반환)은 모든
  비(非)admin 역할을 거부함.
- **잘못된 역할:** 값이 잘못된 역할은 아무것도 부여하지 않음.
- **잘못된 grant 값:** 잘못된 grant 값 하나, 또는 `null`이나 `""`인 값은 빈
  대상 집합이 됨 (감사 M35).
- **목록에 없는 동사:** 나열되지도 않고 `"*"`로 덮이지도 않는 동사. 따라서
  새 RPC는 기본적으로 거부됩니다.

### 3.4 동사

- 동사는 gRPC 메서드 이름의 snake_case 형태입니다. 예:
  `/koto.Koto/SchedAdd` → `sched_add` (`verbFromMethod`).
- RPC 목록은 `protocol/koto.proto`의 `service Koto`를 보세요.

RPC에서 기계적으로 도출한 전체 동사 집합 (따라서 `goal_list`나 `job_logs`
같은 이름이 Go 소스에 글자 그대로 나올 필요는 없습니다):

- **수명 주기:** `spawn`, `send`, `list`, `stop`, `interrupt`, `destroy`,
  `restart`, `history`, `config`, `metrics`, `clear`, `drain`
- **작업:** `jobs`, `job_logs`, `job_tail`
- **스케줄:** `sched_add`, `sched_list`, `sched_del`, `sched_toggle`,
  `sched_run`
- **목표:** `goal_set`, `goal_list`, `goal_approve`, `goal_pause`,
  `goal_interrupt`, `goal_resume`, `goal_cancel`
- **점검:** `resources`
- **ACL 관리:** `acl_get`, `acl_set_role`, `acl_del_role`
- **실행과 스트림:** `run_script`, `attach_shell`, `subscribe_group`,
  `subscribe_logs`, `watch_state`

### 3.5 admin 전용 동사 (`adminOnlyVerbs`)

다음 동사들은 `acl.json`을 통해 부여할 수 없으며, `"*"` 동사로도
불가능합니다. `admin`만 통과합니다:

- `acl_get`, `acl_set_role`, `acl_del_role`. ACL 관리는 ACL 자체를 통해
  위임할 수 없어야 합니다.
- `run_script`: 게스트 VM에서의 직접 코드 실행.
- `config_posture`: 합성 동사, 다음 절에서 다룹니다.

### 3.6 `config` 대 `config_posture` (`postureVerb`, `daemon/auth.go`)

- **posture** 키를 하나라도 설정하는 `Config` 요청은 `config_posture`로
  다시 라벨링되며, 이는 admin 전용입니다. posture 키는 `network`,
  `internet`, `root`, `ports`, `size`, `autostart`, `kvm`입니다.
- 따라서 `config` grant는 나머지 설정만 커버합니다: `model`, `effort`,
  `provider`.
- 대상 검사는 여전히 적용됩니다. 게스트 내부 ctl 플레인은 posture 키를
  자체적으로 거부합니다 (`daemon/ctl.go`의 `configPostureKey`;
  [configuration.md](configuration.md) 참고).

### 3.7 대상 지정 대 동사 전용 (`targetOf`)

요청 메시지가 그룹을 담고 있으면 그 동사는 그룹 범위("대상 지정,
targeted")입니다.

대상 지정 요청 타입과 이를 쓰는 RPC:

- `SpawnReq`: Spawn
- `SendReq`: Send
- `GroupReq`: Stop, Interrupt, Destroy, Restart, Clear, Drain
- `HistoryReq`: History
- `ConfigReq`: Config
- `MetricsReq`: Metrics
- `JobsReq`: Jobs
- `JobLogsReq`: JobLogs
- `JobTailReq`: JobTail
- `SchedAddReq`: SchedAdd
- `SchedListReq`: SchedList
- `GoalSetReq`: GoalSet
- `GoalListReq`: GoalList
- `GoalGroupReq`: GoalApprove, GoalPause, GoalInterrupt, GoalResume,
  GoalCancel
- `SubscribeReq`: SubscribeGroup
- `RunScriptReq`: RunScript
- `ShellInput`: AttachShell. 스트림의 모든 메시지가 `group`을 담고 있으며
  검사됩니다.

규칙:

- 대상 지정 동사는 grant가 `"*"`이거나 그 그룹을 명시할 때만 통과합니다
  (`roleAllowed`).
- group이 **비어 있는** 대상 지정 요청은 `"*"` grant가 필요한데, 모든
  그룹에 걸쳐 읽기 때문입니다. 예: 전역 `metrics`, 필터 없는 `sched_list`,
  그룹 없는 `jobs` / `goal_list`. 빈 이름은 대상으로 저장되지 않습니다
  (`parseTargets`).
- **동사 전용**(대상 차원 없음): `list`, `watch_state`, `resources`,
  `subscribe_logs`, `sched_del`, `sched_toggle`, `sched_run`, 그리고
  `acl_*` 동사들. 어떤 대상 집합이든 그 동사의 grant가 있으면 호출이
  통과합니다.

### 3.8 동사 전용 읽기의 투영 (`visibleTargets`)

다음 동사들에서는 grant의 대상 집합이 호출이 아니라 **응답**을 좁힙니다
(`daemon/grpc_server.go`):

- **`list`와 `watch_state`.** grant가 커버하는 그룹만 반환됩니다
  (`projectGroups`). `jobs` grant가 그 그룹을 커버하지 않으면 각 그룹의
  작업 레코드도 제거됩니다.
- **`resources`.** 다른 그룹은 제거되고, 호스트 플릿 합계(그룹 수, 실행 중
  그룹, 할당/프로비저닝 바이트, 커밋된 메모리)는 grant가 `"*"`가 아닌 한
  보이는 그룹만으로 다시 계산됩니다 (`projectResources`). 파일시스템
  크기·여유 공간, 메모리 상한, MemTotal 같은 머신 수준 필드는 유지됩니다.
- **`subscribe_logs`.** 커버된 그룹에 귀속되는 이벤트만 전달됩니다. 데몬
  전역 이벤트(빈 group)는 grant가 `"*"`가 아닌 한 버려집니다
  (`logVisible`).
- **`metrics`.** `GlobalMetric` 필드(어느 그룹이든 최신 레코드)는
  `metrics` grant가 `"*"`인 호출자에게만 채워집니다 (`Metrics` 핸들러,
  감사 M30).

### 3.9 스트림

- **동사 게이트.** 스트림을 열 때 호출자는 어떤 역할에서든 그 동사를 보유해야
  합니다 (`authStream`의 `anyGrant`). 대상 검사는 각 요청 메시지 수신 시
  수행됩니다 (`aclStream.RecvMsg`).
- **재검사.** 수명이 긴 스트림은 전송 시 최대 `aclStreamRecheck` = 5초마다
  다시 권한 검사됩니다 (`aclStream`의 `revalidate` 메서드, 감사 M25).
  - 신원은 `tokens.json`에서 이름으로 다시 해석됩니다. 사라졌으면 스트림은
    `Unauthenticated` `identity <name> has been revoked`로 닫힙니다.
  - 역할이 더는 그 동사를 허용하지 않으면 `PermissionDenied`
    `roles <r> may no longer call <verb> on "<group>"`로 닫힙니다.
- **수용 상한.** `streamMaxPerIdentity = 512`,
  `streamMaxGlobal = 2048` (`streamAdmit`). 어느 상한이든 넘으면 호출은
  `ResourceExhausted` `too many live streams (per-identity 512, global 2048)`
  를 반환합니다.

### 3.10 거부 오류와 로그 (`aclCheck`)

- 거부는 gRPC `PermissionDenied`를 반환합니다:
  - `roles <r1,r2> may not call <verb> on "<group>"` (대상 지정)
  - `roles <r1,r2> may not call <verb>` (동사 전용)
- 모든 결정은 `acl` 소스 아래에 로그로 남습니다:
  `<name> (roles …) allowed|denied <verb> [on "<group>"] from <addr>`.
- 대상 지정 결정은 그룹 이름이 유효할 때 그 그룹의 로그에 귀속됩니다
  (`aclLogGroup`).

## 4. ACL 관리

admin 전용 RPC가 셋 있습니다: `AclGet`, `AclSetRole`, `AclDelRole`
(`daemon/grpc_server.go`). CLI에서는 `koto ctl acl get|set|del`입니다
(`ctlAcl`, `daemon/ctl_cli.go`; [ctl-reference.md](ctl-reference.md) 참고).

```sh
koto ctl acl get
koto ctl acl set ops stop:ghost restart:ghost list 'metrics:*'
koto ctl acl del ops
```

- **`acl set`은 역할의 grant 객체 전체를 교체합니다** (`aclSetRoleCmd`).
- **역할 이름**은 `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`를 따릅니다
  (`roleNameRE`). 아니면: `invalid role name "<r>"`.
- **grant 검증**(`validateGrants`)은 첫 번째 잘못된 항목에서 요청 전체를
  거부합니다:
  - 동사 키는 `"*"`이거나 `roleNameRE`와 일치해야 하며, 아니면
    `invalid verb "<v>"`.
  - 대상은 `"*"`이거나 `roleNameRE`(또는 `"*"`)와 일치하는 이름 목록이어야
    합니다. 오류: `verb "<v>": target must be "*" or a list of groups` 및
    `verb "<v>": invalid target <t>`.
- **쓰기**는 `aclFileLock`을 잡고 `creds/.acl.json.tmp`와 rename을 통해
  원자적으로, 모드 0600으로 이루어집니다 (`writeACLDoc`).
- **손상된 파일은 변경을 막습니다:**
  `acl.json is corrupt — fix or remove it on disk first` (`readACLDoc`).
- **없는 역할에 대한 `acl del`**은 `no such role: <role>`로 실패합니다.

## 5. 레시피

**스크립트용 범위 제한 신원.** 클라이언트를 발급하고 `koto ctl`이 그것을
쓰게 합니다:

```sh
koto pki client -creds /var/lib/koto/creds -role agent ci
KOTO_CLIENT=ci koto ctl list
```

`KOTO_CLIENT`는 `client-<name>.{crt,key}`와 `token-<name>`을 선택합니다
(`newKotoClient`, `daemon/ctl_cli.go`).

**역할을 한 그룹으로 한정**하려면 `"*"` 값을 목록으로 바꿉니다:

```sh
koto ctl acl set agent list:main send:main history:main subscribe_group:main watch_state:main
```

**기기 폐지.** `creds/clients.allow`의 해당 줄을 지우거나, **또는**
`creds/tokens.json`의 항목을 지웁니다. 인증서/토큰 바인딩 덕분에 어느
쪽이든 충분합니다. 새 호출은 즉시 실패하고, 열린 스트림은 5초 안에
닫힙니다 (`aclStreamRecheck`).

**다른 머신에서 데몬에 접근.** CA를 건드리지 않고 SAN을 추가한 뒤 데몬을
재시작합니다:

```sh
koto pki server -creds /var/lib/koto/creds -san DNS:koto-daemon,DNS:localhost,IP:127.0.0.1,IP:192.168.1.20
```

데몬은 `KOTO_BIND`가 설정되지 않는 한 `127.0.0.1`에서만 수신합니다.
[configuration.md](configuration.md)를 보세요.
