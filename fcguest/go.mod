module koto-fcagent

go 1.25.0

toolchain go1.25.14

// x/sys pinned per the 6-week dependency-lag rule: v0.47.0 released
// 2026-06-30 (verified via proxy.golang.org/golang.org/x/sys/@v/v0.47.0.info;
// was v0.40.0 until the 2026-09-05 govulncheck-driven bump of grpc/x/net/
// x/text, which pulled x/sys along). v0.47.0 is the workspace-wide selection
// (`go work sync` — daemon and tui pin it too), keeping module-mode and
// workspace-mode builds identical.
//
// koto-protocol is the in-repo contract module (protocol/guest.proto): the
// daemon↔guest vsock messages. Resolved by path so the rootfs build (which
// runs with GOWORK=off inside a container) sees the same source the daemon
// compiles against. protobuf v1.36.11 is the protocol module's own pin.
require (
	golang.org/x/sys v0.47.0
	google.golang.org/protobuf v1.36.11
	koto-protocol v0.0.0
)

require (
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/grpc v1.82.1 // indirect
)

replace koto-protocol => ../protocol
