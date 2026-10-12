module koto

go 1.26.0

toolchain go1.26.9

// Pins taken under the security-fix exception to the 6-week lag (CLAUDE.md,
// Conventions), 2026-09-05 — each closes a REACHABLE govulncheck finding:
//   golang.org/x/crypto v0.56.0 — 2026-09-02: GO-2026-6354/6355, DoS in
//     x/crypto/ssh channels, reached from gvisor-tap-vsock's virtualnetwork.New
//     (fcnet.go). Requires go >= 1.26, which is why the toolchain is 1.26.x.
//   golang.org/x/text v0.41.0 — 2026-08-11: required by that x/crypto.
// And 2026-09-14, taken BEFORE govulncheck's DB carries the advisory (it
// reported nothing): the daemon is a gRPC server, so it is the module the
// fix actually matters to:
//   google.golang.org/grpc v1.83.2 — 2026-08-25: GHSA-vp52-pcj8-j9qc
//     (CVE-2026-84304, fixed in 1.83.1), heap exhaustion from a stream split
//     into many tiny HTTP/2 DATA frames, in the server transport :8443 uses.
//     Exposure is narrow — the client-cert allowlist is enforced at the TLS
//     handshake (auth.go), before any HTTP/2 frame, and the bind is loopback
//     — so the worst case is an allowlisted client OOMing the daemon. The
//     other two advisories in this range (GHSA-2v4p-qf9q-27wj,
//     GHSA-qc2q-p7wx-3px3) are xDS-only and do not apply.
//   golang.org/x/net v0.58.0 — 2026-08-12, genproto/googleapis/rpc
//     v0.0.0-20260526163538 — 2026-05-26: required by that grpc.
// And 2026-10-09, same exception (GOWORK=off govulncheck, all four modules):
//   golang.org/x/net v0.60.0 — 2026-10-08: GO-2026-6603/6611/6612/6617
//     (HTTP/2 server memory exhaustion via trailers, window-update CPU burn,
//     double flow-control refund, HPACK encoder race crash), reachable through
//     grpc's http2 transport — the :8443 server here. The stdlib half of the
//     same advisories is closed by toolchain go1.26.9. Taken with exactly
//     what it requires: x/crypto v0.57.0, x/sys v0.48.0, x/term v0.46.0,
//     x/text v0.42.0 (and x/text's x/mod v0.41.0, x/sync v0.23.0,
//     x/tools v0.49.0). Moved in all four modules together.
// Everything else follows the ordinary rule (verified via
// proxy.golang.org/<mod>/@v/<ver>.info; cutoff 2026-07-25; protobuf v1.36.12 —
// 2026-08-10, re-checked 2026-10-09 against cutoff 2026-08-28).
// OpenTelemetry export (otel.go), 2026-10-12, ordinary rule, cutoff
// 2026-08-31 (verified via proxy.golang.org/<mod>/@v/<ver>.info). Opt-in
// only: nothing below dials anything unless an OTLP endpoint is configured.
//   go.opentelemetry.io/otel, /metric, /trace, /sdk, /sdk/metric,
//     /exporters/otlp/otlptrace, /otlptrace/otlptracegrpc,
//     /otlpmetric/otlpmetricgrpc v1.46.0 — 2026-08-25. (v1.47.0 is
//     2026-10-02: too new.)
//   go.opentelemetry.io/otel/log, /sdk/log, /exporters/otlp/otlplog/
//     otlploggrpc v0.22.0 — 2026-08-25 (the log signal is still pre-1.0).
//   The exporters require grpc v1.83.1, so the v1.83.2 pin above holds.
// Taken with exactly what they require (MVS, not latest):
//   go.opentelemetry.io/proto/otlp v1.11.0 — 2026-07-22
//   google.golang.org/genproto/googleapis/rpc + /api
//     v0.0.0-20260819154853 — 2026-08-19 (rpc moved up from 20260526;
//     `go get` first resolved 2026-09/10 pseudo-versions, pinned back down)
//   github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 — 2026-08-05
//   github.com/go-logr/logr v1.4.4 — 2026-07-20, go-logr/stdr v1.2.2 — 2021-12-14
//   go.opentelemetry.io/auto/sdk v1.2.1 — 2025-09-15
//   github.com/cenkalti/backoff/v5 v5.0.3 — 2025-07-23
//   github.com/cespare/xxhash/v2 v2.3.0 — 2024-04-04
//   github.com/google/uuid v1.6.0 — 2024-01-23

require (
	github.com/containers/gvisor-tap-vsock v0.8.9
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc v0.22.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.46.0
	go.opentelemetry.io/otel/log v0.22.0
	go.opentelemetry.io/otel/metric v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/sdk/log v0.22.0
	go.opentelemetry.io/otel/sdk/metric v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
	go.opentelemetry.io/proto/otlp v1.11.0
	golang.org/x/sys v0.48.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
	koto-protocol v0.0.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/apparentlymart/go-cidr v1.1.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/inetaf/tcpproxy v0.0.0-20250222171855-c4b9df066048 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20240710054256-ddd8a41251c9 // indirect
	github.com/miekg/dns v1.1.72 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.5.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260819154853-08b0e4226688 // indirect
	gvisor.dev/gvisor v0.0.0-20240916094835-a174eb65023f // indirect
)

replace koto-protocol => ../protocol
