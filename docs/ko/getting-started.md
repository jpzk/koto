../getting-started.md의 번역본입니다. 영어 원문이 기준입니다.

# 시작하기

이 페이지는 아무것도 없는 상태에서 daemon이 돌아가는 상태까지 koto를
설치하는 과정을 다룹니다: 호스트에 필요한 것, 세 단계(획득, 통합, 구성),
설치를 가로막는 플랫폼 점검과 그 정확한 메시지, 그리고 마법사(wizard) 각
단계가 하는 일.

모든 주장에는 소스 참조(파일 + 함수, 상수 또는 Makefile 타깃)가 붙어 있어
grep으로 확인할 수 있습니다. 코드 주석이나 usage 텍스트가 코드와 다른
경우, 이 페이지는 코드를 따르며 그 사실을 명시합니다.

함께 보기: [configuration.md](configuration.md) (koto.env 키, `KOTO_*`
환경 변수, group 설정), [acl-and-auth.md](acl-and-auth.md) (마법사가
발급하는 PKI와 토큰), [ctl-reference.md](ctl-reference.md) (`koto ctl`).

## 1. 두 가지 설치 경로

### 1.1 파이프 설치 스크립트

```sh
curl -fsSL https://kotovm.com/install.sh | sh
curl -fsSL https://kotovm.com/install.sh | sh -s -- --version 1.0.0
```

`install.sh`는 네 단계를 순서대로 실행합니다 (`install.sh` 머리말 주석):

1. fetch: 릴리스 태그를 임시 디렉터리에 클론한 뒤, 서명된 아티팩트를 그곳에 다운로드하고 검증합니다 (`make fetch`와 동일).
2. install: `koto install` (`make install`과 동일).
3. setup: `koto setup` (`make wizard`와 동일).
4. tui: `koto tui`.

`curl | sh`에서는 stdin이 파이프이므로 2~4단계는 `/dev/tty`에서 읽습니다.
터미널이 없으면 스크립트는 2단계 후에 멈추고 남은 명령을 출력합니다
(`install.sh` 머리말 주석).

`install.sh` 플래그 (`install.sh` `usage()`):

- `--version X`: 설치할 릴리스. 기본값은 `latest`이며 `VERSION="${KOTO_VERSION:-latest}"`에서 옵니다.
- `--no-attach`: TUI를 여는 대신 `koto setup` 후에 멈춥니다. `koto update`가 실행하는 방식입니다.
- `--yes`: `koto install`과 `koto setup`의 기본값을 수락합니다. 업그레이드 시 daemon 중지도 포함합니다.
- `--download-only`, `--dest DIR`, `--manifest FILE`: fetch와 검증만 합니다. `--dest` 기본값은 현재 디렉터리. `--manifest`는 릴리스에서 오지 않은 sha256sum(1) 목록과의 대조 검사를 추가합니다.
- `--next TEXT`: `--download-only` 실행이 끝났을 때 제안할 명령.
- `-h`, `--help`.

`install.sh`가 거부하는 것 (각각 `install.sh`의 `die`):

- Linux/x86_64가 아닌 호스트: ``koto is Linux/x86_64 only (this is <os>/<arch>)``.
- 누락된 도구: ``<tool> is required but not installed``. 필요한 도구는 `curl gpg zstd sha256sum`이며, `--download-only`가 아니면 `git`도 필요합니다.
- 빈 키 지문(fingerprint): ``no release key fingerprint — refusing to accept a signature from whatever key turns up``.

검증 순서 (`install.sh` 머리말 주석, "WHAT THIS VERIFIES" 절):

1. 지문으로 서명 키를 가져옵니다.
2. `SHA256SUMS`의 분리 서명(detached signature)을 검증하고, `VALIDSIG` 행이 그 지문을 지목하는지 요구합니다. 서명된 파일은 또한 요청한 버전과 같은 `# koto-version: X.Y.Z` 행을 정확히 하나 담고 있어야 합니다. 그 외의 값은 롤백으로 간주되어 거부됩니다 (`install.sh`, `signed_ver`).
3. 서명된 `SHA256SUMS`에서 에셋 이름을 가져옵니다.
4. 압축 해제 전에 모든 에셋을 검사하고, 전체 세트가 통과할 때까지 모든 것을 목적지 옆에 스테이징합니다.

고정(pinned) 지문은 `A6E69ED6BC4779F3281721484CC1AFDE15B64EA3`입니다
(`install.sh` `KEY_FPR`; Makefile `RELEASE_KEY_FPR`). `install.sh`가 읽는
환경 변수 오버라이드는 [configuration.md](configuration.md) §2.5에
정리되어 있습니다.

### 1.2 클론에서, 세 단계로

Makefile 맨 위의 주석("GETTING koto RUNNING IS THREE STAGES")이 단계를
정의합니다:

1. 아티팩트 다섯 개를 획득(ACQUIRE)합니다. 두 가지 경로 중 하나: `make fetch` 또는 `make build`.
2. 시스템에 통합(INTEGRATE)합니다: `make install`.
3. 설치된 시스템을 구성(CONFIGURE)합니다: `make wizard`.

```sh
make fetch && make install && make wizard   # download — minutes
make build && make install && make wizard   # build it yourself — ~40 min
```

`make setup`은 `make build`, `make install`, `make wizard`를 그 순서로
실행하는 축약형입니다 (Makefile 타깃 `setup`).

각 단계는 이전 단계의 일을 대신하기를 거부하며, 그 일을 하는 명령의
이름을 알려줍니다. 모든 단계는 다시 실행해도 안전합니다 (Makefile 머리말
주석).

## 2. 요구 사항

### 2.1 플랫폼

- linux/amd64 전용. `checkPlatform` (`daemon/setup_checks.go`)은 다른 `GOOS/GOARCH`를 다음 해결책과 함께 실패시킵니다: ``koto needs linux/amd64: Firecracker microVMs need KVM, and the guest kernel and rootfs are built for x86_64.``
- `/dev/kvm`이 존재하고 모두에게 읽기/쓰기가 가능해야 합니다 (모드 `&0o006 == 0o006`). `checkKVMAt`와 §4.2 참조.
- 비특권 사용자 네임스페이스(unprivileged user namespaces), 그리고 사용자의 subuid/subgid 범위. §4.3과 §4.4 참조.

README는 빌드와 실행이 검증된 배포판을 나열합니다: Fedora 44, Ubuntu
24.04 LTS, Ubuntu 26.04 LTS, Arch (`README.md`, "From sources"). 이
페이지에서 그 목록을 재검증하지는 않았습니다.

### 2.2 빌드 시에만 필요 (`make build` 경로)

Docker 또는 podman. 둘 다 있으면 Makefile은 docker를 우선합니다 (Makefile
`CONTAINER ?= $(shell command -v docker … || command -v podman …)`;
`daemon/setup_checks.go`의 `containerEngine`도 같은 순서). Go 바이너리는
다이제스트 고정 이미지 `GO_IMAGE` 안에서 `CGO_ENABLED=0`으로 빌드되므로
호스트에 Go 툴체인이 필요 없습니다 (Makefile `GO_BUILD_RUN`). 두 엔진이
모두 없으면 컴파일 전에 빌드가 실패합니다 (Makefile `need-container`):

```
podman or docker is required to build koto (sudo dnf install podman / sudo apt install podman)
```

다른 빌드 도구는 `git`, `make`, `curl`입니다 (`checkBuildTools`). README의
설치 명령은 rootless podman을 위해 `passt`(Ubuntu에서는 `uidmap`도)를
추가합니다:

```sh
sudo dnf install -y git make podman                  # Fedora 44+
sudo apt install -y git make podman passt uidmap     # Ubuntu 24.04+
sudo pacman -S --needed git make podman passt        # Arch
```

### 2.3 런타임 (설치된 daemon이 셸로 호출하는 것)

`daemon/setup_checks.go`의 `checkRuntimeTools`에서 가져왔으며, Fedora와
Debian 패키지 이름을 함께 적습니다:

- `mkfs.ext4`: `e2fsprogs`. 각 group의 workspace 이미지를 생성합니다. 이미지 확장에는 호스트 도구가 필요 없습니다: 호스트가 파일을 늘리고 guest가 파일시스템 크기를 온라인으로 조정합니다.
- `tar`: `tar`. skill을 guest에 전달합니다.
- `newuidmap`, `newgidmap`: Fedora에서는 `shadow-utils`, Debian/Ubuntu에서는 `uidmap`. jail에 갇힌 microVM 모니터가 실행되는 VM별 uid/gid를 매핑합니다.
- `claude`: 경고만 합니다(warn-only). 프록시가 토큰 갱신을 위해 셸로 호출하는 OAuth(구독) 인증에만 필요합니다. 힌트는 `node >= 22`와 `npm i -g @anthropic-ai/claude-code`를 요구합니다. Debian 계열 호스트에서는 Ubuntu의 `nodejs` 패키지가 v18이라는 점도 언급하며 `sudo snap install node --classic --channel=22` 또는 nvm을 제안합니다.

Podman은 런타임 의존성이 아닙니다. koto가 실행하는 것 중 컨테이너는
없습니다 (`checkRuntimeTools` 주석; Makefile 머리말).

## 3. 1단계: 획득

### 3.1 아티팩트 다섯 개

Makefile `ARTIFACTS`와 `daemon/install.go`의 `artifacts`가 둘 다
나열합니다:

- `koto`
- `koto-tui`
- `fcassets/firecracker`
- `fcassets/vmlinux`
- `fcassets/rootfs.img`

두 경로 모두 정확히 이것들을 만들어 냅니다.

### 3.2 `make fetch`

`make fetch`는 `KOTO_BASE_URL="$(KOTO_DIST_URL)/$(DIST_VERSION)"`과 함께
`install.sh --version $(DIST_VERSION) --dest . --manifest artifacts.sha256
--download-only --next "make install"`을 실행합니다 (Makefile 타깃
`fetch`). 기본값:

- `KOTO_DIST_URL ?= https://github.com/jpzk/koto/releases/download`
- `DIST_VERSION ?= 1.7.4`
- `DIST_ARCH ?= x86_64`

릴리스 에셋은 zstd로 압축되고 `<basename>_<version>_<arch>.zst` 형식으로
명명됩니다. 예: `rootfs.img_fedora44_x86_64.zst` (Makefile의 `DIST_ARCH`
위 주석). 커밋된 `artifacts.sha256`은 압축 해제 후, 설치되는 상태의
아티팩트를 체크섬합니다 (Makefile의 `MANIFEST` 위 주석).

`DIST_VERSION`이 `unreleased`이면 fetch는 다음 메시지와 함께 1로
종료합니다: ``no koto release has been published yet, so there is nothing
to fetch.``

### 3.3 `make build`

`make build`는 하위 make들을 이 순서로 실행합니다: `koto`, `koto-tui`,
`firecracker`, `kernel`, `rootfs` (Makefile 타깃 `build`). 각각은 출력이
입력보다 새것이면 아무 일도 하지 않습니다(no-op).

- `koto`: `go build -trimpath -ldflags "-s -w -X main.kotoVersion=$(KOTO_VERSION)" -o koto ./daemon`. 먼저 `install.sh`를 `daemon/installer/install.sh`로 복사합니다. 설치 스크립트가 `koto update`를 위해 임베드되기 때문입니다. `KOTO_VERSION`은 `git describe --tags --always --dirty`이며, 실패하면 날짜로 대체됩니다. `koto version`이 이를 출력합니다 (`daemon/main.go`, `kotoVersion`, 기본값 `"dev"`).
- `firecracker`: `fcguest/build-firecracker.sh`.
- `kernel`: `fcguest/build-kernel.sh`.
- `rootfs`: `fcguest/build-rootfs.sh`. 입력에 `fcguest/*.go`, `sidecar/*`, `protocol/`이 포함되므로 (Makefile `FCGUEST_SRC`) 프로토콜 변경은 guest 이미지를 다시 빌드합니다.

빌드는 재현 가능(reproducible)하지 않습니다. 소스 빌드는 당신이 직접
빌드했기 때문에 신뢰되는 것이며, 릴리스 매니페스트와 대조 검사되지
않습니다 (Makefile의 `dist` 위 주석; `daemon/install.go`의
`requireArtifacts` 주석).

## 4. 2단계: `make install` / `koto install`

`make install`은 먼저 `require-artifacts`를 실행합니다. 다섯 파일 중
하나라도 없으면 ``missing artifact(s): …``와 fetch/build 힌트를 출력한
뒤 `./koto install`을 실행합니다 (Makefile 타깃 `install`,
`require-artifacts`).

### 4.1 `koto install` 플래그

`daemon/install.go`의 `installMain`과 `installUsage`에서:

- `-state DIR`: 상태 디렉터리. 기본값 `/var/lib/koto` (`defaultStateDir`). 값은 `unitSafeValue`를 통과해야 하며, 아니면 ``-state: …``와 함께 종료 코드 2.
- `-y`: 기본값을 수락하고 절대 묻지 않습니다.
- `-no-color`: 색상 비활성화. 이 플래그는 파싱되지만 usage 텍스트에는 나오지 않습니다.

usage 텍스트는 install이 "starts the service"라고 말합니다. 코드는
업그레이드 때만 시작합니다. 새 설치는 `systemctl enable koto`를 실행하고
``service enabled (not started — `koto setup` starts it)``를 출력합니다
(`runInstall`). 마법사가 `server.crt`를 발급하기 전에는 daemon이 올라올
수 없습니다.

### 4.2 install이 하는 일, 순서대로

`daemon/install.go`의 `runInstall`에서 가져온 단계입니다:

1. 업그레이드를 감지하고 (`installed()`: `/etc/systemd/system/koto.service`와 `/etc/koto/koto.env`가 둘 다 존재) ``existing installation detected — upgrading in place``를 출력합니다.
2. 클론 밖에서의 실행을 거부합니다: ``run this from a koto clone (no Makefile/daemon here)``. 아티팩트 누락도 거부합니다 (`requireArtifacts`).
3. root를 거부합니다: ``run as your normal user, not root — the daemon runs as you (sudo is used only for the system files)``.
4. 호스트 요구 사항 점검을 실행합니다 (`preflightGate`, §5).
5. 실행 중인 daemon을 중지합니다 (`installStopDaemon`).
6. 상태 디렉터리를 시딩합니다 (`seedStateDir`). 디렉터리가 없으면 `sudo install -d -o <uid> -g <gid> -m 0750`으로 생성합니다. 기존 디렉터리는 `stateDirTrusted`를 통과해야 합니다: 심볼릭 링크가 아니어야 하고, 디렉터리여야 하고, 당신 소유여야 하고, group/other 쓰기 비트가 없어야 합니다. 실패 메시지는 ``<dir>: <reason> — remove it or choose another -state``. 그 다음 install은 `groups/`, `creds/`, `fcassets/`, `prompts/`, `run/`을 생성하고 (모드 0750), `.claude -> creds` 링크를 만들고, 클론에서 `groups/`, `groups.json`, `schedules.json`, `goals.json`, `metrics.jsonl`을 이전(migrate)합니다. 절대 덮어쓰지 않습니다 (`migrateState`).
7. 바이너리를 설치합니다: `sudo install -m 0755 <self> /usr/local/bin/koto`, 그리고 `koto-tui`를 `/usr/local/bin/koto-tui`로. `koto-tui`가 없으면 경고만 합니다.
8. AppArmor가 userns를 제한하고 운영자가 제안을 수락했다면(또는 koto 프로파일이 이미 존재하면), AppArmor 프로파일을 설치하거나 갱신합니다 (`installAppArmorProfile`).
9. `claude` CLI를 해석하고 (`installClaudeBin`) `/etc/koto/koto.env`를 씁니다 (`writeEnvFile`; 키는 [configuration.md](configuration.md) 참조).
10. `/etc/systemd/system/koto.service`를 렌더링합니다 (`renderUnit`, `ExecStart=/usr/local/bin/koto daemon`). 유닛이 바뀌었으면 `systemctl daemon-reload`를 실행합니다.
11. 업그레이드에서는 `systemctl start koto`를 실행합니다. 새 설치에서는 `systemctl enable koto`를 실행합니다.
12. `git describe`에서 `<state>/.koto-version`을 씁니다. 실패하면 `kotoVersion`으로 대체.

모든 특권 작업은 실행 전에 에코되는 별도의 `sudo` exec입니다 (`sudoRun`;
`daemon/install.go` 머리말 주석).

## 5. 플랫폼 점검 (preflight)

`daemon/setup_checks.go`의 `runPreflight`는 이 순서로 점검합니다: 플랫폼,
`/dev/kvm`, 중첩 userns, subuid/subgid, 빌드 시 도구, 런타임 도구, 디스크
공간, 네트워크. `koto install`과 `koto setup --check` 둘 다 실행합니다.

빌드 시 그룹(컨테이너 엔진 + `git`, `make`, `curl`)은 `advisory()`를
거치며, 실패를 경고로 바꿉니다. fetch 경로에는 그 도구들이 필요 없기
때문입니다.

### 5.1 게이트의 결정 방식 (`preflightGate`)

- 하드 실패(hard failure)가 하나라도 있으면 설치가 멈춥니다. 실패가 하나 이상이면 ``<n> unmet requirement(s) — see the remediation above``를 보고합니다. 하드 실패와 `/dev/kvm`이 둘 다 실패하면 ``<n> unmet requirement(s), all shown above — fix all of them (<m> blocking, plus /dev/kvm)``를 보고합니다.
- `/dev/kvm`만 실패하는 것은 소프트(soft)입니다. 프롬프트 ``Continue without KVM?``의 기본값은 no입니다. yes로 답하면 API는 응답하지만 어떤 group도 부팅할 수 없는 daemon이 설치됩니다 (`preflightResult.noKVM`). no로 답하면 중단됩니다 (`errSetupAborted`, 텍스트 `aborted`).
- userns가 ``restricted by AppArmor``이고 `apparmor_parser`가 사용 가능하면, install은 ``Install an AppArmor profile for koto?``를 제안합니다 (기본값 yes). 거절하면 ``nested user namespaces are unavailable — see the remediation above, or re-run and accept the AppArmor profile``로 실패합니다.

### 5.2 `/dev/kvm` (`checkKVMAt`)

- 없음: 상세 ``not present``. 해결책은 BIOS/UEFI에서 가상화를 켜거나, koto 자체가 VM 안에서 돌 때는 중첩 가상화(nested virtualization)를 켜는 것입니다.
- 모드에 world rw가 없음: 상세 ``mode NNNN — not world-accessible``. kvm 그룹 멤버십은 도움이 되지 않습니다. jail에 갇힌 Firecracker VMM은 보조 그룹이 비어 있는 VM별 uid로 실행됩니다 (`kvmRemediation`; `daemon/fcjail.go` 참조). 가장 먼저 출력되는 해결책은 udev 규칙입니다:

  ```sh
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm
  ```

  대안은 jail uid 대역에 대한 uid별 POSIX ACL이며, `/etc/subuid`의 시작값에서 `fcJailHostUID(start, fcJailBaseUID)`로 계산해 `fcJailBaseUID+ctlMaxSpawn`까지입니다. 해결책 텍스트는 ACL의 두 가지 문제를 경고합니다: systemd-logind가 이를 다시 쓰고, 이 점검은 모드 비트를 읽으므로 계속 실패합니다.
- 장치는 열리지만 실패: 상세 ``present but not usable: <err>``. 해결책은 ``sudo usermod -aG kvm $USER``.
- 정상: ``usable (mode NNNN)``.

### 5.3 중첩 사용자 네임스페이스 (`checkUserns`)

- `/proc/sys/user/max_user_namespaces`가 `0`: 실패 ``user.max_user_namespaces = 0``. 해결책은 ``sudo sysctl -w user.max_user_namespaces=15000``이며, `/etc/sysctl.d/`에 영구 반영합니다.
- AppArmor가 비특권 userns를 제한함 (`apparmorUsernsRestricted`). 그러면 점검은 설치된 바이너리를 exec합니다 (`apparmorProfileWorks(installedKotoBin)`). 성공하면 결과는 ok, ``granted to koto by AppArmor profile (host-wide restriction still on)``. 아니면 ``restricted by AppArmor``로 실패합니다. 대체 해결책(`usernsRemediation`)은 호스트 전역의 `kernel.apparmor_restrict_unprivileged_userns=0`을 `/etc/sysctl.d/60-koto.conf`에 영구 반영하는 것입니다.
- 읽을 수 없는 파일: 경고 ``unknown``.
- 그 외에는 ok: ``allowed (<n>)``.

### 5.4 subuid/subgid (`checkSubuid`)

`/etc/subuid`와 `/etc/subgid` 둘 다에 사용자 이름 또는 uid에 대한 행이
필요합니다. 하나라도 없으면 점검은 ``no range for <user>``로 실패하며
해결책은 다음과 같습니다:

```sh
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>
podman system migrate
```

### 5.5 컨테이너 엔진 (`checkContainerEngine`, 권고 사항)

- 어느 엔진도 없음: ``neither found``.
- `<engine> info --format json` 실패: `` `<engine> info` failed: … ``.
- podman만 있음: podman이 rootless가 아니면 (``running as root``), 그리고 pasta가 없으면 (``not found``, `passt` 설치) 경고합니다. Docker에는 추가 점검이 없습니다.

### 5.6 디스크와 네트워크

- `checkDisk`는 현재 작업 디렉터리를 검사하고 여유 공간이 20 GiB 미만이면 경고합니다.
- `checkNetwork`는 5초 타임아웃으로 `HEAD https://github.com`을 보내고 경고만 합니다. 아티팩트가 이미 있으면 install은 오프라인에서도 동작합니다.

## 6. 3단계: `make wizard` / `koto setup`

`make wizard`는 `./koto`가 실행 가능한지 확인합니다. 아니면 ``./koto is
missing — acquire the artifacts first:``를 출력하고 1로 종료합니다.
가능하면 `./koto setup`을 실행합니다 (Makefile 타깃 `wizard`).

### 6.1 플래그

`daemon/setup.go`의 `setupMain`과 `setupUsage`에서:

- `--check`: 닥터 모드. 모든 preflight 프로브와 각 단계의 `detect`를 실행하며, 아무것도 바꾸지 않습니다. 준비되면 종료 코드 0, 아니면 1 (`setupCheck`). ``koto is fully set up``, ``<n> host requirement(s) unmet`` 또는 ``<n> step(s) pending — run `koto setup` to finish``를 출력합니다.
- `-y`: 기본값을 수락하고 절대 묻지 않습니다. 실제 입력이 필요한 단계는 실패합니다.
- `--only ID`: 한 단계만 실행합니다. 그 단계의 `detect`는 건너뛰므로 항상 실행됩니다. 알 수 없는 ID는 ``no such step "<id>" (see `koto setup --list`)``, 종료 코드 2. usage 텍스트의 예시 `--only rootfs`는 더 이상 존재하지 않는 단계를 가리킵니다.
- `--from ID`: 이 단계부터 시작합니다.
- `--provider P`: 새 group의 기본 provider (`claudesdk`, `venice` 또는 `openai`, 대소문자 무시). 그러면 `provider` 단계는 묻지 않고 그것을 저장합니다. 알 수 없는 값은 종료 코드 2로 끝납니다: ``unknown --provider "<p>" (want claudesdk, venice or openai)``. `koto setup --only provider --provider P`는 아무것도 묻지 않으므로 TTY 없이도 실행됩니다.
- `--providers L`: 설정할 provider들, 쉼표로 구분 (예: `claudesdk,openai`, 대소문자 무시, 중복 제거). 그러면 `provider` 단계는 아무것도 묻지 않고, `auth` 단계가 각각을 연결합니다. 기본값은 `--provider`가 주어지면 그것, 아니면 목록의 첫 번째입니다. 목록에 없는 `--provider`는 종료 코드 2로 끝납니다: ``--provider <p> is not in --providers <list>: the default must be one of the providers being set up``. 알 수 없는 이름은 종료 코드 2로 끝납니다: ``unknown provider "<p>" in --providers (want claudesdk, venice or openai)``.
- `--no-color`: 일반 출력. `NO_COLOR`가 설정되어 있거나 `TERM`이 `dumb` 또는 비어 있어도 스타일링이 꺼집니다 (`colorOK`, `daemon/setup_ui.go`).
- `--list`: 단계 id를 순서대로 출력합니다.

TTY가 없고, `-y`나 `--check`도 없으며, `--provider P` 또는 `--providers L`을 붙인 `--only provider`도 아니면 setup은 ``setup needs a terminal (or
pass -y / --check)``와 함께 2로 종료합니다.

상태 디렉터리는 `KOTO_HOME`, 설정되지 않았으면 `/var/lib/koto`입니다
(`setupCtx.stateDir`). 자격 증명은 `<state>/creds`로 갑니다.

### 6.2 단계

단계는 순서대로 (`daemon/setup_steps.go`의 `setupSteps`):

1. `installed` (Installed system). `installed()`를 감지합니다. 아무것도 설치하지 않습니다. koto가 설치되어 있지 않으면 "획득 후 `make install`" 힌트와 함께 실패합니다.
2. `pki` (TLS identities). 감지는 `creds/` 아래의 이것들을 모두 요구합니다: `ca.crt`, `ca.key`, `server.crt`, `client-tui.crt`, `token-tui`, `client-agent.crt`, `token-agent`. 다른 머신에서 daemon에 접근할지 묻고, 그렇다면 `IP:192.168.1.20,DNS:koto.lan` 같은 추가 SAN을 받습니다. 기본 SAN은 `DNS:koto-daemon`, `DNS:localhost`, `IP:127.0.0.1` (`defaultServerSANs`, `daemon/pki.go`). `pkiInit`를 실행한 뒤 `tui`와 `agent` 클라이언트를 둘 다 role `admin`으로 발급합니다. 기존 CA는 절대 재생성되지 않습니다. [acl-and-auth.md](acl-and-auth.md) 참조.
3. `provider` (Providers). 두 가지 질문입니다. 먼저 `knownProviders`에 대한 다중 선택입니다 (`claudesdk`, `venice`, `openai`, 각각 한 줄 설명이 붙고, 이미 자격 증명이 있는 provider는 `✓ connected`로 표시됩니다): 쉼표나 공백으로 구분한 번호 (`1,3`), provider 이름, 또는 `all`로 답합니다. Enter는 미리 선택된 집합을 유지합니다: 현재 기본값과 이미 연결된 모든 provider. 그 밖의 입력은 설명과 함께 다시 묻습니다 (`parseMultiChoice`, `daemon/setup_ui.go`). 그다음, 둘 이상이 선택된 경우에만, 새 group이 그중 어느 것을 기본으로 쓸지 묻습니다. 현재 기본값이 그 안에 있으면 미리 선택됩니다. 정확히 하나만 선택되면 그것이 기본값이 되고 두 번째 질문은 하지 않습니다. 기본값만 저장되며, `/etc/koto/koto.env`의 `KOTO_DEFAULT_PROVIDER=<p>`로 기록됩니다 (`setupPersistProvider`, `daemon/setup_provider.go`). 파일의 다른 모든 행은 유지되고, 선택 자체를 기록하는 다른 설정은 없습니다 (어떤 자격 증명이 존재하는지로 도출할 수 있습니다). koto.env가 유효한 provider를 지정하고 있으면 (그리고 `--provider` 또는 `--providers`의 첫 번째가 주어졌다면 그것과 일치하면) 완료입니다. `--provider`나 `--providers`가 있으면 묻지 않습니다. `-y`에서는 현재 집합과 기본값을 유지합니다 (없으면 `claudesdk`). 권고 단계: `--check`는 이 단계의 미완료를 실패가 아닌 경고로 보고합니다. 선택한 적 없는 설치는 이전과 똑같이 `claudesdk`로 동작하기 때문입니다.
4. `auth` (Provider credentials). 위에서 선택된 모든 provider에 대해 표시 순서대로 동작합니다 (provider 단계가 실행되지 않았다면: `--providers`, 없으면 `--provider` 하나, 없으면 기본값과 이미 연결된 모든 provider). 이미 로컬 자격 증명이 있는 provider는 `already connected, login skipped`로 보고됩니다. 나머지 각각은 자신의 "Connect … now?" 질문을 받고 `connected`, `skipped` (나중에 연결할 명령과 함께) 또는 `failed` (오류와 함께, 다음 provider는 여전히 제시됩니다)로 끝납니다. 이어서 요약이 선택된 각 provider와 그 상태를 나열하고 기본값을 표시합니다. 기본 provider가 자격 증명 없이 끝나면, 고칠 때까지 새 group의 모든 turn이 실패한다고 크게 알립니다. 기본이 아닌 provider는 더 부드러운 경고를 받습니다. 단계 통과 여부는 기본 provider만이 결정합니다: 기본 provider에 자격 증명이 있거나 그 로그인이 명시적으로 건너뛰어졌으면 통과하고, 기본 provider의 로그인이 실패하면 실패합니다. 선택된 모든 provider에 자격 증명이 있으면 이 단계는 완료이며 (재실행 시 건너뜀), 감지는 로컬에서 이루어집니다: claudesdk는 `creds/.credentials.json`, `creds/anthropic-api-key` 또는 `ANTHROPIC_API_KEY`, venice는 비어 있지 않은 `creds/venice.key`, openai는 `creds/openai-auth.json` 또는 비어 있지 않은 `creds/openai-api-key`. 로그인은 프로세스 안에서 실행됩니다: claudesdk는 `authConnect` (`koto claude-login`과 동일), venice와 minimax는 숨김 키 입력으로 `koto venice-login` / `koto minimax-login`처럼 provider에 확인하고 (거부된 키는 받지 않으며 아무것도 기록하지 않음) `creds/venice.key` 또는 `creds/minimax.key` (0600)에 저장합니다. openai는 `koto openai-login`의 device-code 또는 API 키 흐름입니다. `-y`에서는 아무것도 로그인하지 않습니다: 이미 연결된 provider는 통과하고, 빠진 각 provider는 비대화형 대안 (예: ``koto openai-login --api-key-stdin < keyfile``)과 함께 보고되며, 빠진 것이 기본 provider일 때만 단계가 실패합니다.
5. `service` (Start the daemon). `sudo systemctl restart koto`를 실행합니다. 이번 실행에서 자격 증명이나 기본 provider가 바뀌었다면 (`restartOwed`) 항상 미완료로 보고합니다. daemon은 `KOTO_DEFAULT_PROVIDER`를 시작할 때만 읽습니다. 마법사 주석은 그 이유를 프록시가 기동 시 한 번 자격 증명을 해석하기 때문이라고 설명합니다. 그 주석은 낡았습니다(stale): 프록시는 이제 요청마다 자격 증명을 읽습니다 (`daemon/proxy.go`의 `currentAPIKey`와 `readCreds`; `claudeLoginUsage`도 같은 말을 합니다). 따라서 파일 기반 자격 증명에서 재시작은 무해하지만 엄밀히 필수는 아닙니다. 환경 변수의 `ANTHROPIC_API_KEY`는 여전히 프로세스 수명 동안 고정됩니다 (`envAPIKey`).
6. `smoke` (Smoke test). `tui` 신원으로 `127.0.0.1:8443`에 대한 클라이언트를 만들어 `List` RPC를 호출합니다. 2초마다, 최대 45초까지 재시도한 뒤 ``daemon did not answer within 45s: … check `systemctl status koto` and `journalctl -u koto -n 50` ``로 실패합니다. `/dev/kvm`이 사용 불가능하면 ``no usable /dev/kvm on this host — groups will not boot until that is fixed``를 경고합니다. 그다음 기본 provider의 자격 증명을 로컬에서 확인하고 (LLM 요청 없음), 없으면 고치는 명령과 함께 경고합니다.
7. `done` (Ready). ``koto is installed and running.``과 다음 명령들을 출력합니다: `koto tui`, `koto ctl list`, `systemctl status koto`, `journalctl -u koto -f`. 상태 디렉터리와 `/etc/koto/koto.env`, 그리고 `koto pki client -creds <state>/creds <name>`으로 클라이언트를 추가하는 방법도 출력합니다.

재개는 상태 파일이 아니라 감지(detection)로 동작합니다. `koto setup`을
다시 실행하면 `detect`가 충족된 모든 단계를 건너뜁니다 (`daemon/setup.go`
머리말). 중단된 실행은 ``interrupted — re-run `koto setup` to resume from
here``를 출력합니다. 실패한 단계는 ``step "<id>" failed: …``를 출력합니다.

## 7. 첫 사용

```sh
koto tui            # /new <name> spawns your first agent, /exit detaches (README.md)
koto ctl list       # list groups; see ctl-reference.md
koto setup --check  # health of the install, changes nothing
```

group 턴이 401로 실패하면 `koto claude-login`을 실행하세요. 플래그는
`--status`, `--method oauth|api-key`, `--api-key-stdin`, `--no-verify`
입니다 (`claudeLoginUsage`, `daemon/claude_login.go`).

키 기반 provider의 경우 `koto venice-login`은 `creds/venice.key`를,
`koto minimax-login`은 `creds/minimax.key`를 씁니다 (둘 다 0600, state
dir 안; 키는 guest에 들어가지 않습니다). 키는 `--key-file F`,
`--api-key-stdin` 또는 숨김 터미널 프롬프트에서 받습니다. 쓰기 전에 인증이
필요한 요청 하나로 확인합니다: Venice `GET /api/v1/api_keys/rate_limits`
(Bearer), MiniMax `GET /anthropic/v1/models` (`x-api-key`). 2xx면 저장하고,
401/403이면 거부하고 1로 종료하며 기존 키 파일은 그대로 둡니다. provider에
연결할 수 없으면 (네트워크 오류, 15s 타임아웃, 5xx) 경고와 함께 저장합니다.
`--no-verify`는 요청을 건너뛰고, `--status`는 키 저장 여부만 알려줍니다.
오류 메시지에는 상태 코드나 전송 오류만 담기고 키는 절대 담기지 않습니다
(`providerKeyCheck`, `daemon/provider_keycheck.go`). setup 마법사의 `auth`
단계도 같은 확인을 사용합니다. daemon은 요청마다 키를 다시 읽으므로 재시작이
필요 없습니다.

`daemon/main.go`에서 디스패치되는 다른 최상위 하위 명령은 `daemon`,
`fcjail`, `ctl`, `setup`, `claude-login`, `openai-login`, `venice-login`, `minimax-login`, `uninstall`, `update`,
`install`, `tui`, `userns-check`, `pki`, `version`입니다. 알 수 없는 하위
명령은 ``unknown subcommand: <name>``을 출력하고 2로 종료합니다.
