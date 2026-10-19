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
// OTLP/HTTP protobuf exporters (otel.go), 2026-10-13, same release train,
// cutoff 2026-08-31 (verified via proxy.golang.org/<mod>/@v/<ver>.info):
//   /exporters/otlp/otlptrace/otlptracehttp v1.46.0 — 2026-08-25
//   /exporters/otlp/otlpmetric/otlpmetrichttp v1.46.0 — 2026-08-25
//   /exporters/otlp/otlplog/otlploghttp v0.22.0 — 2026-08-25
//   They add NO new module to the graph: every requirement (otlp proto
//   v1.11.0, backoff v5.0.3, logr v1.4.4, grpc v1.83.1 <= v1.83.2, protobuf
//   v1.36.12) is already pinned above. Transport is net/http from stdlib.
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
// Host-side packet parsers (fcnet.go), 2026-10-18, CVE-audit upgrade under
// the ordinary 6-week rule, cutoff 2026-09-06 (verified via
// proxy.golang.org/<mod>/@v/<ver>.info). The netstack parses every guest
// frame inside the daemon, so these are the highest-exposure deps:
//   github.com/containers/gvisor-tap-vsock v0.9.0 — tag 2026-10-07
//     (6dbb5d4fa003). NOT age-gated: operator one-time override 2026-10-19
//     (chore/deps-nogate). Replaces the main@2026-09-02 d3d4f055 snapshot.
//     d3d4f055..v0.9.0 (46 commits): DNS forwards SOA/PTR/AAAA and returns
//     NOERROR/NODATA for zone-owned names (we set no zones -> forward path),
//     unix-socket listener cleanup, gateway API bearer-token auth + audit log
//     (gvproxy HTTP API — koto does not expose it), net.ErrClosed handling.
//     No change to types.Configuration / virtualnetwork.New used by fcnet.go.
//     Requires the same gvisor 20260413; MVS also lifts test-only deps
//     ginkgo v2.33.0, gomega v1.44.0, go-mockdns v1.3.0 (go.sum only).
//     v0.8.9 (2026-05-11) still pinned gvisor 2024-09; the udp
//     ForwarderHandler -> bool API change and ICMP echo forwarder (covered by
//     fcEgressConn's dst-IP filter for every protocol) came in after it.
//   gvisor.dev/gvisor v0.0.0-20260413194555-9680d69bf798 — 2026-04-13
//   github.com/insomniacslk/dhcp v0.0.0-20260719225207-c76316d4aa82 —
//     2026-07-19: GO-2026-6237 (the @latest c6bb46ad is 2026-10-14: too new)
//   Taken with what they require (MVS): inetaf/tcpproxy 20260515 — 2026-05-15,
//   miekg/dns v1.1.73 — 2026-08-19, sirupsen/logrus v1.10.2 — 2026-08-25,
//   mdlayher/socket v0.6.0 — 2026-03-31, mdlayher/vsock v1.3.0 — 2026-05-26,
//   x/time v0.12.0 — 2025-06-04, linuxkit/virtsock 20220523, pkg/errors
//   v0.9.1 — 2020-01-14, x/exp 20231110.

require (
	github.com/containers/gvisor-tap-vsock v0.9.0
	github.com/go-logr/logr v1.4.4
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc v0.22.0
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.22.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.46.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.46.0
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
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/inetaf/tcpproxy v0.0.0-20260515195445-c159a6051109 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20260719225207-c76316d4aa82 // indirect
	github.com/linuxkit/virtsock v0.0.0-20220523201153-1a23e78aa7a2 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/mdlayher/vsock v1.3.0 // indirect
	github.com/miekg/dns v1.1.73 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20231110203233-9a3e6036ecaa // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260819154853-08b0e4226688 // indirect
	gvisor.dev/gvisor v0.0.0-20260413194555-9680d69bf798 // indirect
)

replace koto-protocol => ../protocol
