../architecture.md의 번역본입니다. 영어 원문이 기준입니다.

# 아키텍처

운영자의 gRPC 클라이언트에서 microVM 내부의 에이전트 프로세스까지, koto의
구성 요소가 어떻게 맞물려 있는지를 설명합니다. 모든 주장은 출처가 된 파일
(그리고 대개 함수나 상수)을 명시하므로 `grep`으로 확인할 수 있습니다.
상위 수준 다이어그램은 저장소 `README.md`의 `## Architecture` 아래에
있습니다. 이 페이지는 그 다이어그램의 코드 검증 버전입니다.

관련 문서: [getting-started.md](getting-started.md) (설치),
[configuration.md](configuration.md) (설정 항목),
[acl-and-auth.md](acl-and-auth.md) (gRPC 신뢰 경계),
[firecracker-vsock.md](../firecracker-vsock.md) (최초의 vsock/jailer
설계 노트).

## 1. 계층 한눈에 보기

1. **1계층, 호스트 사용자.** gRPC 클라이언트(`koto tui`, `koto ctl`)와
   상태 디렉터리의 `creds/` 아래 자격 증명 저장소.
   [acl-and-auth.md](acl-and-auth.md)를 보세요.
2. **2계층, koto 데몬.** 하나의 Go 프로세스(`koto daemon`,
   `daemon/daemon.go`의 `daemonMain`)입니다. 설치한 사용자 권한으로
   systemd 유닛 아래에서 rootless로 실행되며, gRPC 서버, 그룹별 LLM
   프록시, gVisor 네트워크 게이트웨이, cron, goal, ctl 플레인을 담고
   있습니다.
3. **3계층, 그룹마다 하나씩 jail에 가둔 Firecracker VMM.** 각 VMM은
   `koto fcjail`(`daemon/fcjail.go`)을 통해 시작되고, PID 1이
   `fc-agent`(`fcguest/main.go`)인 microVM을 부팅합니다.

각 계층은 자기 안쪽 계층이 이미 침해당했다고 가정합니다. 이것이
README의 "defense in depth"(심층 방어) 서술입니다. 아래 절들은 각
계층이 코드에서 실제로 무엇을 하는지 설명합니다.

## 2. 2계층: 데몬

### 2.1 시작 순서

순서는 `daemon/daemon.go`의 `daemonMain`입니다:

1. `usernsEnsure()`(`daemon/userns.go`)가 데몬을 새 사용자 네임스페이스
   안으로 재실행(re-exec)합니다. `newuidmap`/`newgidmap`이
   `/etc/subuid`와 `/etc/subgid`의 매핑을 설치합니다. 이는 무조건
   수행됩니다. 실패하면 데몬은 ``koto: run `koto userns-check` to
   diagnose``를 출력하고 종료 코드 1로 끝납니다. 이유는 `userns.go`
   머리말에 있습니다: fcjail은 VMM마다 두 항목짜리 `uid_map`을 써야
   하고, `fcJailFixupPerms`는 파일을 VM별 uid로 chown해야 합니다. 둘 다
   userns 밖의 비특권 프로세스로는 불가능합니다.
2. `initPaths()`가 `HERE`(`kotoHome()`)에서 상태 경로를 파생합니다:
   - `groups/` (`ROOT`)
   - `groups.json`, `schedules.json`, `goals.json`
   - `run/` (`SOCK_DIR`)
   - `metrics.jsonl`

   `PORT_BASE`의 기본값은 `8787`이고 `PROXY_PORT`로 재정의할 수
   있습니다.
3. `hardenStatePaths()`와 `fcClearStalePids()`가 실행되고, 이어서
   `fcHostMemInit()`과 `fcCgroupInit()`(§4.3 참조), 그 다음
   `postureStartup()`(`daemon/posture.go`)이 실행됩니다. posture 단계는
   호스트 전역 제한 중 적용되어 있지 *않은* 것을 모두 로그로 남깁니다.
4. `proxyStart(bind)`가 프로세스 내 LLM 프록시(§3)를 시작합니다.
5. SIGTERM/SIGINT는 첫 VM이 부팅되기 *전에* 등록됩니다
   (`daemon/daemon.go`의 "audit 2026-09-11 L146" 주석). 이렇게 해야
   종료 시에도 `fcStopAll`이 반드시 실행됩니다.
6. `spawnEnsure("main", true)`가 `main` 그룹을 띄웁니다. 그 뒤
   `go autostartGroups()`가 `autostart=yes`로 설정된 그룹들을
   백그라운드에서 부팅합니다.
7. gRPC 서버가 TCP `KOTO_BIND:KOTO_PORT`에서 수신 대기합니다. 기본값은
   `127.0.0.1`과 `8443`입니다. mTLS(`serverTLSConfig`)와
   `authUnary`/`authStream` 인터셉터를 사용합니다. 자세한 내용은
   [acl-and-auth.md](acl-and-auth.md)에 있습니다.

호스트 쪽 ctl FIFO는 존재하지 않습니다. 그룹의 ctl 플레인은 VM이 올라온
뒤 vsock으로 서비스됩니다. `daemon/daemon.go`의 gRPC 리스너 위 주석이
그렇게 말합니다.

### 2.2 systemd 샌드박스

유닛 텍스트는 `daemon/install.go`가 렌더링합니다. 다음 지시문을
담습니다:

- `Type=exec`와 `ExecStart=/usr/local/bin/koto daemon`.
- `ProtectSystem=strict`.
- `ProtectHome=yes`, 또는 `claude` 바이너리가 홈 디렉터리 아래에 있으면
  `ProtectHome=tmpfs`에 읽기 전용 바인드를 더한 구성
  (`installHomeScoping`).
- `DevicePolicy=closed`와 `DeviceAllow=/dev/kvm rw`.
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`.
- `RestrictNamespaces=user mnt pid net ipc uts cgroup`.
- `LockPersonality=yes`와 `RestrictRealtime=yes`.
- `NoNewPrivileges=no`. 반드시 `no`로 유지해야 합니다: 그렇지 않으면
  setuid `newuidmap`이 무시된다고 `installCapBounding`의 주석이
  설명합니다.
- `CapabilityBoundingSet=…`, `installCapBounding`이 구성.
- `CPUQuota`, `KOTO_HOST_CPUS`가 요구할 때만(`installCPUQuota`).
- `WantedBy=multi-user.target`.

## 3. LLM 프록시 (자격 증명 주입)

프록시는 데몬 프로세스 안에서 실행됩니다. 별도 바이너리가 아닙니다
(`daemon/proxy.go`의 `proxyStart`, `proxyListen`).

- **TCP가 아니라 그룹마다 unix 소켓 하나.** 각 그룹의 리스너는
  소유자 전용(`0700`) 디렉터리 안의 `run/proxy/p<port>.sock`
  (`proxySockDir`, `proxySockPath`)입니다. 이유는 `proxySockDir` 위
  주석(audit M1)에 있습니다: 루프백 TCP 포트였다면 어떤 로컬 uid든
  자격 증명이 주입된 LLM 접근을 얻을 수 있었을 것입니다. 그룹의 포트
  번호(`PORT_BASE`에서 할당, `daemon/groups.go`의 `allocPort`,
  `groups.json`에 영속화)는 여전히 존재합니다. 그룹을 식별하고 소켓
  파일 이름을 정하는 데 쓰입니다.
- **일대일 불변식.** 이미 다른 그룹에 묶인 포트로 `proxyListen`을
  호출하면 `port %d already bound for group %q (requested for %q)`로
  실패합니다.
- **게스트가 도달하는 방법.** 게스트에는 호스트로 가는 TCP 경로가
  없습니다. 게스트 안의 `ANTHROPIC_BASE_URL`은
  `http://127.0.0.1:18888`(`daemon/fc.go`의 `fcGuestProxyTCP`)입니다.
  fc-agent가 거기서 수신하고 각 연결을 호스트 vsock 포트 9000으로
  전달합니다. 데몬은 그 연결을 그룹의 프록시 소켓으로 이어 붙입니다
  (`fcSpliceToProxy`).
- **업스트림.** 상수는 `upstream = "https://api.anthropic.com"`과
  `veniceUpstream = "https://api.venice.ai"`입니다. 그룹의 provider는
  요청마다 `config.json`에서 읽습니다(`groupProvider`).
- **자격 증명.** 요청마다 읽습니다(`currentAPIKey`, `readCreds`).
  Venice 키는 자격 증명 파일 옆 `venice.key`에 있습니다
  (`veniceKeyPath`). 게스트는 둘 다 결코 보지 못합니다.
- **한도.** 버퍼링되는 요청 본문은 `proxyMaxBody = 64 << 20`(64 MiB)로
  제한됩니다. `http.Server`는 `ReadHeaderTimeout` 30초와 `IdleTimeout`
  90초를 씁니다. 이것이 중요한 이유는 `proxyListen` 주석에 있습니다:
  연결을 거둬들여 VM의 vsock 연결 테이블이 가득 차지 않게 합니다.
- **L7 이그레스 게이트.** `daemon/proxy.go`의 `egressTargetAllowed`가
  프록시의 CONNECT/절대 형식 대상에 그룹의 네트워크 정책을 적용합니다.
  `proxySetBootNetwork`가 VM 스폰 시점의 정책을 기록합니다. `fcnet.go`
  머리말에 따르면 이 게이트는 L3 필터(§5)와 `fcClassifyDst`를
  공유합니다.

## 4. 3계층: jail에 가둔 VMM

### 4.1 fcjail

`daemon/fcjail.go`에서:

- **시작 방식.** 데몬은 자신을 `koto fcjail <spec>`으로
  `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS`와 함께
  재실행합니다. 자식(`fcjailMain`)은 다음을 순서대로 수행합니다:
  1. Firecracker에 필요한 것만 VM별 chroot에 바인드 마운트: 바이너리,
     커널, rootfs, 이 VM의 워크스페이스 이미지, `/dev/kvm`,
     `/dev/urandom`, 그리고 VM의 vsock 소켓 디렉터리.
  2. 그 안으로 chroot.
  3. `PR_SET_NO_NEW_PRIVS` 설정.
  4. VM별 uid로 강등.
  5. Firecracker를 exec.

  koto는 `--no-seccomp`를 절대 넘기지 않으므로 Firecracker의 내장
  seccomp 필터는 계속 켜져 있습니다.
- **VM별 uid 대역.** `fcJailBaseUID = 30000`에서 시작해
  `fcJailMaxUID = 60000`에서 끝납니다. `/etc/subuid`가 그보다 적게
  허용하면 끝이 잘리고, 잘린 값이 재실행된 단계들에
  `KOTO_JAIL_MAX_UID`(`jailMaxEnv`)로 전달됩니다.
- **uid 도출.** `fcJailUID`는
  `uid = fcJailBaseUID + (proxyPort − PORT_BASE)`를 계산합니다. 그룹마다
  프록시 포트가 다르므로 그룹마다 uid가 다릅니다. 대역을 벗어난 결과는
  폴백이 아니라 오류입니다("audit 2026-09-11 L6").
- **경로.** chroot는 `run/fc/<g>.jail`(`fcJailDir`)입니다. 소켓은
  `run/fc/<g>.sock/`(`fcSockDir`)에 있습니다. 실행 디렉터리 `run/fc`는
  `0700`(`fcEnsureRunDir`)입니다.

### 4.2 VM

`daemon/fc.go`에서:

- **애셋.** 바이너리, 커널, rootfs는 `HERE` 아래
  `fcassets/firecracker`, `fcassets/vmlinux`, `fcassets/rootfs.img`
  입니다(`fcBinPath`, `fcKernelPath`, `fcRootfsPath`).
- **영속 디스크.** 게스트의 `/workspace`는
  `groups/<g>/workspace.img`(`fcWorkspaceImg`)입니다. virtio-blk로
  연결되는 그룹별 ext4 이미지입니다. VM이 실행 중일 때 호스트에서 절대
  마운트하지 마세요.
- **크기 프리셋**(`fcSizePresets`). 각 항목은 vCPU / RAM / 디스크 /
  IO MiB/s / IO ops/s입니다:
  - `small`(기본값): 2 / 1024 MiB / 8 GiB / 100 / 15000
  - `medium`: 2 / 2048 MiB / 12 GiB / 150 / 22500
  - `large`: 4 / 4096 MiB / 16 GiB / 200 / 30000
  - `xlarge`: 8 / 8192 MiB / 24 GiB / 250 / 37500

  모든 VM은 또한 `fcIOBurstBytes = 256 << 20`의 일회성 IO 버스트를
  받습니다. VMM은 nice `fcVMNice = 10`으로 실행됩니다.
- **게스트 워커.** uid/gid 1000(`fcWorkerUID`/`fcWorkerGID`, `node`
  사용자)으로 실행됩니다.
- **vsock 장치.** `"guest_cid": 3`으로 구성됩니다(`fcVMConfig`).
  게스트는 CID 2의 호스트로 다이얼합니다(`fcguest/main.go`의
  `hostCID`).

### 4.3 VMM 주위의 자원 한도

- **VM별 cgroup**(`daemon/fccgroup.go`). 각 VM은 `vms/<group>`에서
  실행됩니다. `cpu.weight`는 vCPU 수에 비례합니다
  (`fcCgroupWeightPerVCPU = 50`). `memory.high`는
  `mem_mib + fcCgroupMemMarginMiB`(512)로 설정됩니다. 의도적으로
  `memory.max`는 절대 쓰지 않습니다. 쓸 수 있는 cgroup 트리가 없으면
  데몬은 한 줄을 로그로 남기고 cgroup 없이 VM을 실행하며, 스폰 로그에
  `cgroup=off`가 찍힙니다.
- **플릿 메모리 상한**(`daemon/fchostmem.go`). `vms/`에 `memory.max`가
  걸립니다. `fcHostMemAdmit`은 들어갈 자리가 없는 VM의 부팅을
  거부합니다. 상한은 `KOTO_HOST_MEM_MIB`에서 오며, `0`은 무제한을
  뜻합니다. 설정하지 않으면 상한은 MemTotal의 90%가 기본입니다
  (`fcHostMemDefaultPct = 90`).
- **연결 상한.** 한 그룹은 모든 리스너를 합쳐 최대
  `fcMaxConnsPerGroup = 64`개의 게스트→호스트 vsock 연결을 열 수
  있습니다. 그 이상의 연결은
  `refusing guest vsock connection: %d already open (per-group cap)`
  으로 거부됩니다(`fcAcceptLoop`).

## 5. 네트워킹

- **기본값 `network=none`.** VM에는 NIC이 없습니다. 유일한 이그레스는
  vsock 9000 위의 L7 프록시입니다(`fc.go` 머리말, `fcnet.go` 머리말).
- **`network=wan|lan|full`.** `fcSpawn`이 사용자 공간 gVisor
  게이트웨이(`containers/gvisor-tap-vsock`, `daemon/fcnet.go`의
  `fcNetGateway`)를 붙이고, `groupNetwork(g) != fcNetNone`일 때만 9003
  리스너를 엽니다. 정책은 스폰 시점에 캡처되므로 바꾸려면 그룹을
  재시작해야 합니다.
- **토폴로지**(`daemon/fcnet.go`의 상수):
  - 게스트 `192.168.127.2` (`fcNetGuestIP`)
  - 게이트웨이 `192.168.127.1` (`fcNetGatewayIP`), DNS도 서비스
  - 서브넷 `192.168.127.0/24`, MTU 1500

  이더넷 프레임은 vsock 9003 위에서 프레임당 4바이트 빅엔디언 길이
  접두사가 붙는 Qemu 프레이밍으로 전달됩니다.
- **프레임 필터.** `fcEgressConn`, `fcClassifyDst`, `fcDstAllowed`가
  netstack이 패킷을 보기 전에 실행됩니다. 목적지 클래스는 네 가지:
  - `ctl`: 루프백, 링크 로컬, 호스트 자신의 인터페이스 IP. 항상 차단.
  - `gw`: `192.168.127.0/24`. 항상 허용.
  - `lan`: RFC1918, IPv6 ULA, 멀티캐스트, limited broadcast, CGNAT
    `100.64/10`. `lan`과 `full`에서 허용.
  - `wan`: 그 외 전부. `wan`과 `full`에서 허용.

  netstack은 `169.254.169.254`도 차단합니다
  (`Ec2MetadataAccess=false`).
- **주의 사항**(`fcnet.go` 머리말에 명시). DNS는 프레임 필터 바깥인
  호스트에서 해석됩니다. 따라서 `lan` 게스트도 공개 이름을 해석할 수는
  있습니다. 데이터 프레임은 여전히 목적지 IP로 필터링됩니다.

낡은 주석: `daemon/fc.go`의 `fcPortNet` 주석은 9003이 "only for
internet=full"에 붙는다고 말합니다. `fcSpawn`의 코드는 `none`이 아닌
모든 네트워크 값에 대해 붙입니다. 이 페이지는 코드를 따릅니다.

## 6. vsock 포트 맵

권위 있는 맵은 `daemon/fc.go`의 머리말 주석과 상수 `fcPortProxy`,
`fcPortLog`, `fcPortCtl`, `fcPortNet`, `fcPortLogSlot`,
`fcPortAgent`입니다.

Firecracker의 하이브리드 vsock은 이렇게 동작합니다. 게스트→호스트
연결에서는 Firecracker가 `<uds>_<port>`로 연결하고 데몬이 거기서
수신합니다. 호스트→게스트 연결에서는 데몬이 `<uds>`로 연결해
`CONNECT <port>\n`을 보냅니다(`fcHostDial`). `<uds>`는
`run/fc/<g>.sock/v`(`fcUDS`)입니다.

게스트 → 호스트:

- **9000, API 이그레스**(`fcPortProxy`). 원시 TCP가 그룹의 프록시
  소켓으로 이어 붙여집니다(`fcSpliceToProxy`).
- **9001, 퇴역**(`fcPortLog`). 원시 로그 스트림이었습니다. `fcSpawn`은
  이 포트의 리스너를 열지 않습니다.
- **9002, ctl 플레인**(`fcPortCtl`). 프레이밍된 protobuf `CtlRequest` /
  `CtlResponse`(`fcCtlConn`). 게스트 안에서 셸 인터페이스는
  `/workspace/.cs/ctl` 파일이고, 응답은 `/workspace/.cs/ctl.out`에
  덧붙습니다(`fcguest/ctl.go`).
- **9003, L3 이더넷 프레임**(`fcPortNet`). 네트워크가 있는 그룹 전용
  (§5).
- **9004, 턴 스트림**(`fcPortLogSlot`). 턴마다 연결 하나.
  `TurnOpen{slot}`이 먼저 오고, 그 턴의 이벤트들, 마지막에 `TurnEnd`.
  `fcTurnSink`(`daemon/fcturn.go`)가 이를 호스트 로그 `.cs/log.<slot>`
  으로 렌더링합니다. 게스트발 마커 모양 텍스트는 이스케이프되므로
  게스트는 마커를 위조할 수 없습니다.

호스트 → 게스트:

- **10000, 에이전트 RPC**(`fcPortAgent`). 연결당 프레이밍된
  `AgentRequest` 하나. `fc.go` 머리말은 op로 `init`, `msg`, `exec`,
  `exec_stream`, `shutdown`을 나열하고, `AgentFrame`으로 응답하는
  스트리밍 op로 `run_script`와 `shell_attach`를 지목합니다.

프레이밍(`daemon/fcframe.go`, `fcguest/frame.go`에 바이트 단위로 동일
미러링)은 `uint32` 빅엔디언 길이 뒤에 protobuf 메시지 하나입니다.
최대 길이는 할당 전에 검사됩니다:

- `fcFrameMaxCtl = 1 << 20` (1 MiB), 9002용
- `fcFrameMaxGuest = 16 << 20` (16 MiB), 게스트→호스트 에이전트 프레임용
- `fcFrameMaxHost = 64 << 20` (64 MiB), 호스트→게스트 요청용

더 자세한 내용은 [protocol.md](protocol.md)와
[firecracker-vsock.md](../firecracker-vsock.md)에 있습니다.

낡은 주석: `fcguest/main.go`의 머리말은 아직 9001을 살아 있는 로그
FIFO로 설명하고 에이전트 RPC를 "JSON line"이라고 부릅니다. 데몬 쪽
(`fc.go` 머리말, `fcframe.go`)은 protobuf 프레이밍이며 9001 리스너가
없습니다. 둘이 어긋나는 곳에서는 `daemon/fc.go`를 믿으세요.

## 7. 게스트 내부

- **PID 1은 `fc-agent`**(`fcguest/main.go`)입니다. 초기 부팅(마운트,
  루프백 활성화, 그리고 장치가 확실히 비어 있을 때만 포맷하는
  `/dev/vdb` 워크스페이스 마운트, `mountWorkspace` 참조)을 처리합니다.
  좀비 프로세스도 거둬들입니다.
- **턴.** `msg` op가 턴 하나를 실행합니다. 에이전트가 파일들을 쓰고
  게스트 내 `.cs/in` FIFO에 공급하면 `entrypoint.sh`가 읽습니다
  (`fc.go` 머리말, "msg" op). 주석들은 이를 `sidecar/entrypoint.sh`라
  부르지만, 이 저장소에는 추적되는 `entrypoint.sh`가 없으므로
  (`git ls-files`) 그 출처는 **미검증**입니다.
- **컨테이너.** rootfs는 rootless podman을 탑재하며, KVM 뒤의 게스트
  커널에서 실행됩니다(`fcguest/Dockerfile.rootfs`).
- **호스트 쪽 파일.** 호스트는 자신이 읽는 그룹별 파일(로그,
  `config.json`, `prompt.md`)을 `groups/<g>/` 아래 일반 파일로
  보관합니다(`fc.go` 머리말).
