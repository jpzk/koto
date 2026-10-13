package main

// Exporter credentials never reach a log line (security follow-up S3).
//
// OTEL_EXPORTER_OTLP_HEADERS (and the per-signal _TRACES_/_METRICS_/_LOGS_
// variants) usually carry a collector auth token, and an endpoint URL may
// carry userinfo. Every path that can render them goes through here:
//
//   - the OTel SDK's global logger. Its default (stdr on stderr) prints the
//     raw header VALUE when it fails to parse one ("escape header value",
//     "value", v) and the raw item for a missing '=', and the raw URL for an
//     unparsable endpoint. otelInstallLogging replaces it with a sink that
//     drops every key/value argument and redacts the message and error text.
//   - the SDK's global error handler (async export failures). Default is
//     log.Print to stderr; replaced with a rate-limited, redacted emitLogf.
//   - koto's own lines: the "export on" line prints otelRedactURL(endpoint),
//     init and shutdown errors pass through otelRedact.
//   - the env handed to daemon-run subprocesses (claudeBinEnviron) is
//     scrubbed of OTEL_EXPORTER_OTLP_* by otelScrubEnv.
//
// The headers are never put in a span/log attribute or a ctl reply: the
// only reader is the exporter itself.

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

const otelRedacted = "[redacted]"

// otelSecretMin: values shorter than this are not replaced by substring
// (a 1-2 byte "secret" would shred every line); the logr sink still drops
// all key/value arguments, which is where the SDK puts raw header bytes.
const otelSecretMin = 3

var otelHeaderVars = []string{
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	"OTEL_EXPORTER_OTLP_LOGS_HEADERS",
}

var otelEndpointVars = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"KOTO_OTEL_ENDPOINT",
}

// otelSecretsFrom lists every string that must never be logged: each raw
// header item, its raw and unescaped value, plus endpoint userinfo / query.
func otelSecretsFrom(getenv func(string) string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, v := range []string{s, strings.TrimSpace(s)} {
			if len(v) >= otelSecretMin && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	for _, k := range otelHeaderVars {
		raw := getenv(k)
		if raw == "" {
			continue
		}
		add(raw)
		for _, item := range strings.Split(raw, ",") {
			add(item)
			if _, v, ok := strings.Cut(item, "="); ok {
				add(v)
				if u, err := url.PathUnescape(v); err == nil {
					add(u)
				}
				// "Bearer <tok>": the token alone too.
				for _, f := range strings.Fields(v) {
					add(f)
				}
			}
		}
	}
	for _, k := range otelEndpointVars {
		raw := strings.TrimSpace(getenv(k))
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil {
			if u.User != nil {
				add(u.User.String())
				add(u.User.Username())
				if p, ok := u.User.Password(); ok {
					add(p)
				}
			}
			add(u.RawQuery)
		} else if at := strings.LastIndex(raw, "@"); at > 0 {
			// Unparsable, but may still hold scheme://user:pass@...
			ui := raw[:at]
			if i := strings.Index(ui, "://"); i >= 0 {
				ui = ui[i+3:]
			}
			add(ui)
			if _, p, ok := strings.Cut(ui, ":"); ok {
				add(p)
			}
		}
	}
	// Longest first, so a value is replaced before a substring of it.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

var otelSecrets atomic.Pointer[[]string]

// otelRedact replaces every known exporter secret in s.
func otelRedact(s string) string {
	p := otelSecrets.Load()
	if p == nil {
		return s
	}
	for _, sec := range *p {
		s = strings.ReplaceAll(s, sec, otelRedacted)
	}
	return s
}

func otelRedactErr(err error) string {
	if err == nil {
		return ""
	}
	return otelRedact(err.Error())
}

// otelRedactURL renders an endpoint without userinfo, query or fragment.
func otelRedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return otelRedacted
	}
	if u.User != nil {
		u.User = url.User("REDACTED")
	}
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.Fragment = ""
	return otelRedact(u.String())
}

// otelScrubEnv drops the exporter credential/endpoint variables from a
// child environment: a daemon-run subprocess has no business with them.
func otelScrubEnv(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); strings.HasPrefix(k, "OTEL_EXPORTER_OTLP_") || k == "KOTO_OTEL_ENDPOINT" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// otelInstallLogging points the SDK's global logger and error handler at
// koto's (redacting) log funnel. Called by otelInit before any exporter is
// built, because the exporters parse the header env vars in New. Only
// called with an endpoint configured: telemetry off leaves the globals alone.
func otelInstallLogging(getenv func(string) string) {
	sec := otelSecretsFrom(getenv)
	otelSecrets.Store(&sec)
	otel.SetLogger(logr.New(otelLogSink{}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(otelHandleErr))
}

// Export errors repeat every batch while a receiver is down; per signal, log
// the first, then at most one per otelErrEvery with a suppressed count.
//
// One limiter per signal (F6): with a single shared limiter, a burst of
// metrics/logs failures used up the window and every trace-export failure
// ended up as "(N similar suppressed)" on another signal's line, so an
// operator could not tell that traces were failing at all.
var (
	otelErrMu    sync.Mutex
	otelErrState = map[string]*otelErrLimit{}
	otelErrEvery = 30 * time.Second
)

type otelErrLimit struct {
	last       time.Time
	suppressed int
}

// Limiter keys. otelErrOther collects errors that name no signal (config,
// TLS, instrument errors) so they cannot hide an export failure either.
const (
	otelErrTraces  = "traces"
	otelErrMetrics = "metrics"
	otelErrLogs    = "logs"
	otelErrOther   = "other"
)

// otelErrSignal maps an SDK error to its limiter key from the wrapping the
// exporters apply: "traces export: ..." (otlptrace, both transports),
// "failed to upload metrics: ..." (otlpmetric, both), "failed to send logs
// to ..." (otlploghttp) and "logs export: ..." (otelLogErrTag, which also
// covers otlploggrpc's bare status errors). An unwrapped HTTP error still
// names its /v1/<signal> path. Only used as a key, never logged.
func otelErrSignal(s string) string {
	switch {
	case strings.HasPrefix(s, "traces export:"):
		return otelErrTraces
	case strings.HasPrefix(s, "logs export:"), strings.HasPrefix(s, "failed to send logs"):
		return otelErrLogs
	case strings.HasPrefix(s, "failed to upload metrics"), strings.HasPrefix(s, "failed to upload incomplete metrics"),
		strings.HasPrefix(s, "failed to send metrics"):
		return otelErrMetrics
	case strings.Contains(s, "/v1/traces"):
		return otelErrTraces
	case strings.Contains(s, "/v1/metrics"):
		return otelErrMetrics
	case strings.Contains(s, "/v1/logs"):
		return otelErrLogs
	}
	return otelErrOther
}

func otelHandleErr(err error) {
	if err == nil {
		return
	}
	sig := otelErrSignal(err.Error())
	otelErrMu.Lock()
	st := otelErrState[sig]
	if st == nil {
		st = &otelErrLimit{}
		otelErrState[sig] = st
	}
	now := time.Now()
	if !st.last.IsZero() && now.Sub(st.last) < otelErrEvery {
		st.suppressed++
		otelErrMu.Unlock()
		return
	}
	n := st.suppressed
	st.last, st.suppressed = now, 0
	otelErrMu.Unlock()
	msg := otelRedactErr(err)
	if n > 0 {
		msg += fmt.Sprintf(" (%d similar %s errors suppressed)", n, sig)
	}
	// warn, not error: error lines become operator notifications, and a
	// down collector is not an emergency for an isolation box.
	emitLogfQuiet("otel", "warn", "export: %s", msg)
}

// otelErrReset clears every limiter (tests).
func otelErrReset() {
	otelErrMu.Lock()
	otelErrState = map[string]*otelErrLimit{}
	otelErrMu.Unlock()
}

// otelLogErrTag prefixes log-export errors with "logs export: ", as
// otlptrace does for traces: otlploggrpc returns bare gRPC status errors
// that would otherwise land in the "other" limiter.
type otelLogErrTag struct{ sdklog.Exporter }

func (e otelLogErrTag) Export(ctx context.Context, recs []sdklog.Record) error {
	if err := e.Exporter.Export(ctx, recs); err != nil {
		return fmt.Errorf("logs export: %w", err)
	}
	return nil
}

// otelLogSink is a logr.LogSink for the SDK's internal logger. It keeps the
// message and error text (redacted) and DROPS every key/value argument: the
// SDK passes raw header items/values and endpoint URLs as kv args.
type otelLogSink struct{ name string }

func (otelLogSink) Init(logr.RuntimeInfo) {}

// Enabled: errors always; Warn is V(1); Info V(4) / Debug V(8) are noise.
func (otelLogSink) Enabled(level int) bool { return level <= 1 }

func (s otelLogSink) Info(level int, msg string, kv ...any) {
	emitLogfQuiet("otel", "warn", "sdk: %s%s", otelRedact(msg), otelKeysOnly(kv))
}

func (s otelLogSink) Error(err error, msg string, kv ...any) {
	emitLogfQuiet("otel", "warn", "sdk: %s: %s%s", otelRedact(msg), otelRedactErr(err), otelKeysOnly(kv))
}

func (s otelLogSink) WithValues(...any) logr.LogSink { return s }
func (s otelLogSink) WithName(n string) logr.LogSink { s.name = n; return s }

// otelKeysOnly renders " (k=[redacted] ...)" for the kv args.
func otelKeysOnly(kv []any) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" (")
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(' ')
		}
		k, _ := kv[i].(string)
		b.WriteString(otelRedact(k))
		b.WriteString("=" + otelRedacted)
	}
	b.WriteByte(')')
	return b.String()
}
