../README.md의 번역본입니다. 영어 원문이 기준입니다.

# koto 문서

koto 사용자(group과 대화하는 사람)와 운영자(daemon을 설치하고 운영하는
사람)를 위한 레퍼런스 문서입니다. 이 문서의 모든 사실 주장에는 소스
참조(파일 + 함수/상수 이름)가 붙어 있어 이 저장소에서 grep으로 확인할 수
있습니다. 코드가 모호한 경우에는 추측하는 대신 "unverified(미검증)"라고
명시했습니다. 문서와 코드가 다르면 코드가 우선합니다 — 문서를 고쳐 주세요.

## 시작하기

- [getting-started.md](getting-started.md) — 빌드 및 런타임 요구 사항,
  `make` 단계(fetch / build / install / wizard), `install.sh` 경로,
  `koto install`의 사전 점검(preflight)과 `koto setup` 마법사의 각 단계.
- [architecture.md](architecture.md) — 계층 구조(호스트 CLI, daemon,
  proxy, gVisor 네트워크 게이트웨이, fcjail, Firecracker guest), daemon
  기동 순서, VM 크기 프리셋과 vsock 포트 맵.

## 레퍼런스

- [configuration.md](configuration.md) — 코드가 읽는 모든
  `/etc/koto/koto.env` 키와 `KOTO_*` 환경 변수(기본값 포함), 그리고
  group별 설정 키(model, provider, network, size, root, effort, ports,
  autostart, …)와 허용 값.
- [ctl-reference.md](ctl-reference.md) — 호스트의 모든 `koto ctl` 동사와
  플래그, 그리고 guest 내부 ctl 플레인(`/workspace/.cs/ctl`)의 동사들.
- [acl-and-auth.md](acl-and-auth.md) — PKI, 클라이언트 토큰, 역할(role),
  동사 × 대상 ACL과 관리자 전용 동사.
- [protocol.md](protocol.md) — `koto.proto`의 gRPC 서비스(모든 RPC와
  요청 필드)와 채널별 guest vsock 프레이밍.

## group 사용하기

- [groups-goals-schedules.md](groups-goals-schedules.md) — 채팅 세션과
  큐 제한, `cs-job` 백그라운드 작업, goal 자동 조종(autopilot), cron
  스케줄, 그리고 report/reply 위임 창구.

## 운영하기

- [operations.md](operations.md) — daemon 로그와 알림, 리소스 샘플러와
  경보, `koto update`로 업그레이드, `koto uninstall`로 제거, 그리고
  정확한 오류 메시지별 문제 해결.

## 설계 노트 (기존 문서)

- [firecracker-vsock.md](../firecracker-vsock.md) — Firecracker microVM
  group과 vsock IPC에 대한 설계 노트.
- [kernel-amzn-vs-vanilla.md](../kernel-amzn-vs-vanilla.md) — guest
  커널을 kernel.org 바닐라 대신 Amazon Linux 소스로 빌드하는 이유.

## 이력 (보관된 보고서, 유지 관리하지 않음)

특정 시점의 분석 자료입니다. 작성 당시의 코드를 기술하므로 최신 상태가
아닐 수 있습니다.

- [history/SECURITY_19_MAY.md](../history/SECURITY_19_MAY.md) — 백엔드
  보안 분석, 2026-05-19.
- [history/ANALYSIS_31_MAY.md](../history/ANALYSIS_31_MAY.md) — 문제
  분석, 2026-05-31.
- [history/SECURITY_11_SEP.md](../history/SECURITY_11_SEP.md) — 보안
  평가 분류, 2026-09-11.
- [history/FUZZ_09_OCT.md](../history/FUZZ_09_OCT.md) — 퍼징(fuzzing)
  캠페인, 2026-10-09.
