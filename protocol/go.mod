module koto-protocol

go 1.26.0

toolchain go1.26.8

// This module is the entire cross-project contract: koto.proto plus its
// committed generated code in ./pb — nothing hand-written. The daemon and
// TUI import koto-protocol/pb; the Android app Wire-generates Kotlin
// from koto.proto at build time. The generated service stubs pull in
// grpc + protobuf. Pins verified >=6 weeks old as of 2026-09-05:
//   google.golang.org/grpc     v1.82.1  — 2026-07-15 (from v1.80.0: fixes
//     GO-2026-6061, reachable from the generated stubs — govulncheck)
//   google.golang.org/protobuf v1.36.11 — 2025-12-12
require (
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
)
