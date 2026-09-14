module koto-protocol

go 1.26.0

toolchain go1.26.8

// This module is the entire cross-project contract: koto.proto plus its
// committed generated code in ./pb — nothing hand-written. The daemon and
// TUI import koto-protocol/pb; the Android app Wire-generates Kotlin
// from koto.proto at build time. The generated service stubs pull in
// grpc + protobuf. Pins:
//   google.golang.org/grpc     v1.83.2  — 2026-08-25, taken 2026-09-14 under
//     the security-fix exception to the 6-week lag (from v1.82.1: fixes
//     GHSA-vp52-pcj8-j9qc, HTTP/2 server-transport heap exhaustion — see
//     daemon/go.mod for the exposure analysis). Moved in all four modules
//     together so the workspace selects one grpc.
//   google.golang.org/protobuf v1.36.11 — 2025-12-12 (verified >=6 weeks old)
require (
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)
