module koto-fcagent

go 1.26.0

toolchain go1.26.9

// x/sys v0.48.0 (2026-08-31) is required by x/net v0.60.0, taken
// 2026-10-09 under the security-fix exception (see daemon/go.mod); it was
// v0.47.0 under the ordinary 6-week rule. v0.48.0 is the workspace-wide
// selection (`go work sync` — daemon and tui pin it too), keeping
// module-mode and workspace-mode builds identical.
//
// The indirect grpc v1.83.2 comes from koto-protocol's 2026-09-14
// security-fix bump; x/net v0.60.0 / x/text v0.42.0 from the 2026-10-09 one
// (see daemon/go.mod).
//
// koto-protocol is the in-repo contract module (protocol/guest.proto): the
// daemon↔guest vsock messages. Resolved by path so the rootfs build (which
// runs with GOWORK=off inside a container) sees the same source the daemon
// compiles against. protobuf v1.36.12 is the protocol module's own pin.
require (
	golang.org/x/sys v0.48.0
	google.golang.org/protobuf v1.36.12
	koto-protocol v0.0.0
)

require (
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.2 // indirect
)

replace koto-protocol => ../protocol
