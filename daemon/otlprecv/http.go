package otlprecv

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// maxHTTPBody caps one OTLP/HTTP request body (after decompression).
const maxHTTPBody = 32 << 20

// Options picks the listeners. An empty address disables that transport;
// at least one must be set.
type Options struct {
	GRPCAddr string // OTLP/gRPC, e.g. "127.0.0.1:4317"
	HTTPAddr string // OTLP/HTTP protobuf, e.g. "127.0.0.1:4318"
}

// StartWith starts the requested listeners. Both transports feed the SAME
// Receiver, so a capture holds whatever arrived on either one in one format.
func StartWith(o Options, sink io.Writer) (*Receiver, error) {
	if o.GRPCAddr == "" && o.HTTPAddr == "" {
		return nil, errors.New("otlprecv: no listen address")
	}
	r := &Receiver{changed: make(chan struct{}, 1), sink: sink}
	if o.GRPCAddr != "" {
		ln, err := net.Listen("tcp", o.GRPCAddr)
		if err != nil {
			return nil, err
		}
		r.Addr, r.ln, r.srv = ln.Addr().String(), ln, grpc.NewServer()
		coltrace.RegisterTraceServiceServer(r.srv, r)
		colmetrics.RegisterMetricsServiceServer(r.srv, metricsSrv{r: r})
		collogs.RegisterLogsServiceServer(r.srv, logsSrv{r: r})
		go func() { _ = r.srv.Serve(ln) }()
	}
	if o.HTTPAddr != "" {
		hl, err := net.Listen("tcp", o.HTTPAddr)
		if err != nil {
			r.Stop()
			return nil, err
		}
		r.HTTPAddr, r.hln = hl.Addr().String(), hl
		r.hsrv = &http.Server{Handler: r.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = r.hsrv.Serve(hl) }()
	}
	return r, nil
}

// HTTPEndpoint is the base URL to hand an OTLP/HTTP exporter (the exporter
// appends /v1/<signal>). "" when the HTTP listener is off.
func (r *Receiver) HTTPEndpoint() string {
	if r.HTTPAddr == "" {
		return ""
	}
	return "http://" + r.HTTPAddr
}

// HTTPHandler serves the OTLP/HTTP protobuf binding: POST /v1/traces,
// /v1/metrics, /v1/logs with Content-Type application/x-protobuf (optionally
// gzip-encoded). Other content types get 415, other methods 405, unknown
// paths 404, undecodable bodies 400.
func (r *Receiver) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, q *http.Request) {
		req := &coltrace.ExportTraceServiceRequest{}
		if !readOTLP(w, q, req) {
			return
		}
		resp, _ := r.Export(q.Context(), req)
		writeOTLP(w, resp)
	})
	mux.HandleFunc("/v1/metrics", func(w http.ResponseWriter, q *http.Request) {
		req := &colmetrics.ExportMetricsServiceRequest{}
		if !readOTLP(w, q, req) {
			return
		}
		resp, _ := metricsSrv{r: r}.Export(q.Context(), req)
		writeOTLP(w, resp)
	})
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, q *http.Request) {
		req := &collogs.ExportLogsServiceRequest{}
		if !readOTLP(w, q, req) {
			return
		}
		resp, _ := logsSrv{r: r}.Export(q.Context(), req)
		writeOTLP(w, resp)
	})
	return mux
}

func readOTLP(w http.ResponseWriter, q *http.Request, msg proto.Message) bool {
	if q.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	mt, _, err := mime.ParseMediaType(q.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-protobuf" {
		http.Error(w, "unsupported content type, want application/x-protobuf", http.StatusUnsupportedMediaType)
		return false
	}
	var body io.Reader = http.MaxBytesReader(w, q.Body, maxHTTPBody)
	switch q.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return false
		}
		defer zr.Close()
		body = io.LimitReader(zr, maxHTTPBody+1)
	default:
		http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
		return false
	}
	b, err := io.ReadAll(body)
	if err != nil || len(b) > maxHTTPBody {
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return false
	}
	if err := proto.Unmarshal(b, msg); err != nil {
		http.Error(w, "bad protobuf body", http.StatusBadRequest)
		return false
	}
	return true
}

func writeOTLP(w http.ResponseWriter, resp proto.Message) {
	b, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (r *Receiver) stopHTTP() {
	if r.hsrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.hsrv.Shutdown(ctx)
}
