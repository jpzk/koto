module clawson-protocol

go 1.24.0

toolchain go1.24.13

// The shared wire contract is now protobuf/gRPC (see clawson.proto; generated
// code in ./pb). This module is no longer stdlib-only: the generated service
// stubs pull in grpc + protobuf, so importing clawson-protocol/pb widens the
// TUI's dep tree. Pins verified >=6 weeks old as of 2026-06-14:
//   google.golang.org/grpc     v1.80.0  — 2026-04-01
//   google.golang.org/protobuf v1.36.11 — 2025-12-12
require (
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260120221211-b8f7ae30c516 // indirect
)
