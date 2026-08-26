module koto-fcagent

go 1.24.0

// x/sys pinned per the 6-week dependency-lag rule: v0.40.0 released
// 2025-12-19 (verified via proxy.golang.org/golang.org/x/sys/@v/v0.40.0.info).
// v0.40.0 is the workspace-wide selection (`go work sync` — daemon and tui
// already pin it), keeping module-mode and workspace-mode builds identical.
//
// koto-protocol is the in-repo contract module (protocol/guest.proto): the
// daemon↔guest vsock messages. Resolved by path so the rootfs build (which
// runs with GOWORK=off inside a container) sees the same source the daemon
// compiles against. protobuf v1.36.11 is the protocol module's own pin.
require (
	golang.org/x/sys v0.40.0
	google.golang.org/protobuf v1.36.11
	koto-protocol v0.0.0
)

require (
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260120221211-b8f7ae30c516 // indirect
	google.golang.org/grpc v1.80.0 // indirect
)

replace koto-protocol => ../protocol
