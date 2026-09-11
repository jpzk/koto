package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"koto-protocol/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The wire contract shared with the daemon is the proto alone (generated
// code in koto-protocol/pb); the view types it converts into live in
// wire.go. The daemon transport is gRPC over mTLS+token (see the daemon's
// auth.go); this file holds the client side. The historical `sock`
// parameter on every *Cmd is retained for call-site stability but is no
// longer the connection address — the endpoint + creds come from env.

// ---- connection (mTLS + bearer token, over a private overlay) -------------

// tokenCreds attaches the bearer token as `authorization` metadata on every
// RPC (unary and stream). RequireTransportSecurity is true — the token only
// ever travels inside the TLS channel.
type tokenCreds struct{ token string }

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}
func (tokenCreds) RequireTransportSecurity() bool { return true }

func clientTLS() (*tls.Config, error) {
	certPath := envOr("KOTO_CERT", "/koto-creds/client.crt")
	keyPath := envOr("KOTO_KEY", "/koto-creds/client.key")
	caPath := envOr("KOTO_CA", "/koto-creds/ca.crt")
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ca %s: no certificates parsed", caPath)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}
	// Override the verified server name when the dial target (an overlay IP)
	// doesn't itself appear in the server cert SAN.
	if sn := os.Getenv("KOTO_SERVER_NAME"); sn != "" {
		cfg.ServerName = sn
	}
	return cfg, nil
}

var (
	clientMu sync.Mutex
	client   pb.KotoClient
)

// getClient builds the shared gRPC client on first use. Failures are NOT
// cached (this was a sync.Once once): a cred file that is momentarily
// unreadable at startup would otherwise pin every future call — including
// the 5s reconnect loop's — to the same stale error until process restart.
// grpc.NewClient doesn't dial, so the success path still runs exactly once.
func getClient() (pb.KotoClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}
	tcfg, err := clientTLS()
	if err != nil {
		logErr("grpc", "client TLS setup failed: %v", err)
		return nil, err
	}
	ep := envOr("KOTO_ENDPOINT", "127.0.0.1:8443")
	cc, err := grpc.NewClient(ep,
		grpc.WithTransportCredentials(credentials.NewTLS(tcfg)),
		grpc.WithPerRPCCredentials(tokenCreds{os.Getenv("KOTO_TOKEN")}),
		// Transport keepalive replaces the daemon's old app-level `ping`
		// frames: HTTP/2 pings detect a dead link under the long-lived
		// Subscribe/Watch streams, which would otherwise block in Recv
		// forever. Time must stay >= the server's enforcement MinTime (10s).
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		logErr("grpc", "client construction for %s failed: %v", ep, err)
		return nil, err
	}
	logInfo("grpc", "client created: endpoint=%s server_name=%q", ep, os.Getenv("KOTO_SERVER_NAME"))
	client = pb.NewKotoClient(cc)
	return client, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- unary: keep the map[string]any contract the call sites consume --------

// daemonCall dispatches one unary RPC and returns its response as a
// map[string]any (via protojson with proto field names, so the existing
// snake_case map keys still resolve). Application-level failures surface as a
// non-nil error with the daemon's message; transport/auth failures surface as
// the raw gRPC error.
func daemonCall(_ string, cmd string, extra map[string]any) (map[string]any, error) {
	cl, err := getClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// One log line per unary RPC — the single choke point every command,
	// send, and poll goes through. Message bodies are logged as lengths, not
	// content (chat text doesn't belong in a debug log by default).
	group, _ := extra["group"].(string)
	msgLen := len(s2(extra["msg"]))
	t0 := time.Now()
	msg, err := callRPC(ctx, cl, cmd, extra)
	if err != nil {
		logWarn("rpc", "%s group=%q transport error after %s: %v", cmd, group, sinceMs(t0), err)
		return nil, err
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		logErr("rpc", "%s group=%q response marshal failed: %v", cmd, group, err)
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		logErr("rpc", "%s group=%q response unmarshal failed: %v", cmd, group, err)
		return nil, err
	}
	if ok, _ := out["ok"].(bool); !ok {
		es, _ := out["error"].(string)
		logWarn("rpc", "%s group=%q daemon error after %s: %s", cmd, group, sinceMs(t0), es)
		return out, fmt.Errorf("daemon: %s", es)
	}
	logDbg("rpc", "%s group=%q msg_len=%d ok in %s", cmd, group, msgLen, sinceMs(t0))
	return out, nil
}

func s2(v any) string { s, _ := v.(string); return s }

func sinceMs(t0 time.Time) time.Duration { return time.Since(t0).Round(time.Millisecond) }

func callRPC(ctx context.Context, cl pb.KotoClient, cmd string, extra map[string]any) (proto.Message, error) {
	s := func(k string) string { v, _ := extra[k].(string); return v }
	switch cmd {
	case "list":
		return cl.List(ctx, &pb.ListReq{})
	case "spawn":
		r := &pb.SpawnReq{Group: s("group"), Provider: s("provider"), Model: s("model"), Size: s("size")}
		if b, ok := extra["main"].(bool); ok {
			r.Main = b
		}
		return cl.Spawn(ctx, r)
	case "send":
		return cl.Send(ctx, &pb.SendReq{Group: s("group"), Msg: s("msg"), Session: s("session")})
	case "stop":
		return cl.Stop(ctx, &pb.GroupReq{Group: s("group")})
	case "interrupt":
		// Session-scoped: a group runs several turns at once, so esc must
		// abort the conversation on screen, not whichever one the daemon
		// would have picked.
		return cl.Interrupt(ctx, &pb.GroupReq{Group: s("group"), Session: s("session")})
	case "destroy":
		return cl.Destroy(ctx, &pb.GroupReq{Group: s("group")})
	case "restart":
		return cl.Restart(ctx, &pb.GroupReq{Group: s("group")})
	case "clear":
		return cl.Clear(ctx, &pb.GroupReq{Group: s("group"), Session: s("session")})
	case "history":
		r := &pb.HistoryReq{Group: s("group")}
		if v, ok := asFloat(extra["before"]); ok {
			r.Before = v
		}
		if v, ok := asFloat(extra["limit"]); ok {
			r.Limit = int32(v)
		}
		return cl.History(ctx, r)
	case "metrics":
		return cl.Metrics(ctx, &pb.MetricsReq{Group: s("group")})
	case "job_logs":
		r := &pb.JobLogsReq{Group: s("group"), Id: s("id")}
		if v, ok := asFloat(extra["tail"]); ok {
			r.Tail = int64(v)
		}
		return cl.JobLogs(ctx, r)
	case "config":
		return cl.Config(ctx, buildConfigReq(extra))
	case "sched_add":
		return cl.SchedAdd(ctx, &pb.SchedAddReq{Group: s("group"), Cron: s("cron"), Msg: s("msg")})
	case "sched_list":
		return cl.SchedList(ctx, &pb.SchedListReq{Group: s("group")})
	case "sched_del":
		return cl.SchedDel(ctx, &pb.SchedIDReq{Id: s("id")})
	case "sched_toggle":
		r := &pb.SchedToggleReq{Id: s("id")}
		if b, ok := extra["enabled"].(bool); ok {
			r.Enabled = b
		}
		return cl.SchedToggle(ctx, r)
	case "sched_run":
		return cl.SchedRun(ctx, &pb.SchedIDReq{Id: s("id")})
	case "goal_set":
		r := &pb.GoalSetReq{Group: s("group"), Name: s("name"), Text: s("text"), Criteria: s("criteria")}
		if v, ok := asFloat(extra["max_iterations"]); ok {
			r.MaxIterations = int32(v)
		}
		if b, ok := extra["plan"].(bool); ok {
			r.Plan = &b
		}
		return cl.GoalSet(ctx, r)
	case "goal_list":
		return cl.GoalList(ctx, &pb.GoalListReq{Group: s("group")})
	// The NAME matters on every one of these. goalOpCmd puts the selected run
	// in extra["name"] and these five dropped it, so the daemon fell back to
	// implicit selection — with several goals in a group, /goals cancel on the
	// row you picked could resolve to a different eligible run, or fail as
	// ambiguous (audit M96). A group runs several goals at once by design, so
	// this is the ordinary case, not a corner.
	case "goal_approve":
		return cl.GoalApprove(ctx, &pb.GoalGroupReq{Group: s("group"), Name: s("name")})
	case "goal_pause":
		return cl.GoalPause(ctx, &pb.GoalGroupReq{Group: s("group"), Name: s("name")})
	case "goal_interrupt":
		return cl.GoalInterrupt(ctx, &pb.GoalGroupReq{Group: s("group"), Name: s("name")})
	case "goal_resume":
		return cl.GoalResume(ctx, &pb.GoalGroupReq{Group: s("group"), Name: s("name")})
	case "goal_cancel":
		return cl.GoalCancel(ctx, &pb.GoalGroupReq{Group: s("group"), Name: s("name")})
	}
	return nil, fmt.Errorf("unknown cmd: %s", cmd)
}

// fetchResources pulls the fleet resource snapshot (Resources RPC) and keys
// it by group. Typed rather than routed through daemonCall's protojson map:
// the int64 byte fields arrive as JSON strings there, and the metrics bar
// wants numbers, not another asInt64 dance.
func fetchResources() (map[string]GroupRes, HostRes, error) {
	cl, err := getClient()
	if err != nil {
		return nil, HostRes{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := cl.Resources(ctx, &pb.ResourcesReq{})
	if err != nil {
		logWarn("rpc", "resources transport error: %v", err)
		return nil, HostRes{}, err
	}
	if !resp.GetOk() {
		logWarn("rpc", "resources daemon error: %s", resp.GetError())
		return nil, HostRes{}, fmt.Errorf("daemon: %s", resp.GetError())
	}
	out := make(map[string]GroupRes, len(resp.GetGroups()))
	for _, g := range resp.GetGroups() {
		out[g.GetGroup()] = GroupRes{
			Running:         g.GetRunning(),
			CPUPct:          g.GetCpuPct(),
			Vcpus:           g.GetVcpus(),
			MemMiB:          g.GetMemMib(),
			RSSBytes:        g.GetRssBytes(),
			AllocBytes:      g.GetAllocBytes(),
			DeclaredBytes:   g.GetDeclaredBytes(),
			GuestMemTotal:   g.GetGuestMemTotalBytes(),
			GuestMemAvail:   g.GetGuestMemAvailBytes(),
			GuestDiskTotal:  g.GetGuestDiskTotalBytes(),
			GuestDiskAvail:  g.GetGuestDiskAvailBytes(),
			GuestDiskUsed:   g.GetGuestDiskUsedBytes(),
			MemCommittedMiB: g.GetMemCommittedMib(),
		}
	}
	h := resp.GetHost()
	host := HostRes{
		FsTotalBytes:     h.GetFsTotalBytes(),
		FsFreeBytes:      h.GetFsFreeBytes(),
		AllocTotalBytes:  h.GetAllocTotalBytes(),
		ProvisionedBytes: h.GetProvisionedBytes(),
		Groups:           h.GetGroups(),
		RunningGroups:    h.GetRunningGroups(),
		MemCapMiB:        h.GetMemCapMib(),
		MemCommittedMiB:  h.GetMemCommittedMib(),
		MemHostTotalMiB:  h.GetMemHostTotalMib(),
	}
	return out, host, nil
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// buildConfigReq encodes the absent/clear/set tri-state. Scalar config values
// arrive as strings (present => set, "" => clear, absent key => leave unset).
func buildConfigReq(extra map[string]any) *pb.ConfigReq {
	r := &pb.ConfigReq{}
	if g, ok := extra["group"].(string); ok {
		r.Group = g
	}
	setOpt := func(key string, dst **string) {
		if v, ok := extra[key]; ok {
			if str, ok := v.(string); ok {
				sv := str
				*dst = &sv
			}
		}
	}
	setOpt("model", &r.Model)
	setOpt("effort", &r.Effort)
	setOpt("ports", &r.Ports)
	setOpt("provider", &r.Provider)
	setOpt("internet", &r.Internet)
	setOpt("network", &r.Network)
	setOpt("size", &r.Size)
	setOpt("root", &r.Root)
	setOpt("autostart", &r.Autostart)
	return r
}

// ---- streaming -------------------------------------------------------------

// openGroupStream subscribes to a group's event stream. since > 0 asks the
// daemon to replay every frame with seq > since from its ring before going
// live (gapless resume after a broken stream); since = 0 is live-only, used
// on first attach where the History RPC seeds the view instead.
func openGroupStream(group string, since uint64) (grpc.ServerStreamingClient[pb.Event], context.CancelFunc, error) {
	cl, err := getClient()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := cl.SubscribeGroup(ctx, &pb.SubscribeReq{Group: group, SinceSeq: since})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return stream, cancel, nil
}

// openStateStream subscribes to daemon-pushed group-state snapshots
// (WatchState), replacing the old 1s List polling loop.
func openStateStream() (grpc.ServerStreamingClient[pb.StateFrame], context.CancelFunc, error) {
	cl, err := getClient()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := cl.WatchState(ctx, &pb.WatchReq{})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return stream, cancel, nil
}

// stateGroups converts a WatchState frame into the map the listMsg handler
// already consumes.
func stateGroups(f *pb.StateFrame) map[string]GroupInfo {
	out := map[string]GroupInfo{}
	for g, gi := range f.GetGroups() {
		out[g] = GroupInfo{
			Running:   gi.GetRunning(),
			Provider:  gi.GetProvider(),
			Model:     gi.GetModel(),
			Effort:    gi.GetEffort(),
			Stalled:   gi.GetStalled(),
			Queued:    int(gi.GetQueued()),
			Sessions:  gi.GetSessions(),
			Jobs:      pbToJobs(gi.GetJobs()),
			TokPerSec: gi.GetTokPerSec(),
			Network:   gi.GetNetwork(),
			Root:      gi.GetRoot(),
		}
	}
	return out
}

func openLogStream() (grpc.ServerStreamingClient[pb.LogEvent], context.CancelFunc, error) {
	cl, err := getClient()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := cl.SubscribeLogs(ctx, &pb.LogsReq{})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return stream, cancel, nil
}

// startRunScript opens a RunScript stream (admin-only) and pumps its combined
// stdout+stderr into the model as scriptLogMsg lines. Output chunks are raw
// bytes on no particular line boundary, so we buffer and split on '\n',
// emitting one message per complete line (the trailing partial flushes when
// the stream ends). Mirrors startSubscribe's goroutine+prog.Send shape.
func startRunScript(group, name, script string) {
	if prog == nil {
		return // no program to push output into (tests)
	}
	go func() {
		cl, err := getClient()
		if err != nil {
			prog.Send(scriptLogMsg{group: group, kind: "err", text: "runscript: " + err.Error()})
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream, err := cl.RunScript(ctx, &pb.RunScriptReq{Group: group, Script: script})
		if err != nil {
			logWarn("script", "runscript %s on %s failed to open: %v", name, group, err)
			prog.Send(scriptLogMsg{group: group, kind: "err", text: "runscript: " + err.Error()})
			return
		}
		logInfo("script", "runscript %s on %s: stream open (%d bytes)", name, group, len(script))
		prog.Send(scriptLogMsg{group: group, kind: "sys", text: "runscript ▶ " + name})
		// The partial-line buffer is BOUNDED (audit 2026-09-11 L94). Bytes
		// left it only at a newline, so a stream with no newline in it grew it
		// for the life of the stream — and the daemon's sanitizer threshold
		// and the transport's frame limit are both per-fragment, so neither is
		// a budget for the stream. A run of output with no line break is not
		// lines; it is a blob, and the transcript renders it as one entry that
		// maxLines counts as one.
		const scriptLineMax = 64 << 10
		var buf []byte
		var clipped bool
		emit := func(text string) {
			prog.Send(scriptLogMsg{group: group, kind: "script", text: text})
		}
		flush := func(final bool) {
			for {
				i := bytesIndexByte(buf, '\n')
				if i < 0 {
					break
				}
				emit(string(buf[:i]))
				buf = buf[i+1:]
				clipped = false
			}
			if len(buf) > scriptLineMax {
				// Emit the head as its own line and say so, then discard the
				// rest of this logical line until a newline arrives. The
				// alternative — keep buffering — is the unbounded case.
				if !clipped {
					emit(string(buf[:scriptLineMax]) + "…[line too long; the rest of it is not shown]")
					clipped = true
				}
				buf = buf[:0]
			}
			if final && len(buf) > 0 {
				emit(string(buf))
				buf = nil
			}
		}
		for {
			ev, err := stream.Recv()
			if err != nil {
				flush(true)
				logWarn("script", "runscript %s on %s: stream broke: %v", name, group, err)
				prog.Send(scriptLogMsg{group: group, kind: "err", text: "runscript: " + err.Error()})
				return
			}
			switch ev.Event {
			case "data":
				buf = append(buf, ev.Chunk...)
				flush(false)
			case "error":
				flush(true)
				logWarn("script", "runscript %s on %s: daemon error: %s", name, group, ev.Error)
				prog.Send(scriptLogMsg{group: group, kind: "err", text: "runscript: " + ev.Error})
				return
			case "end":
				flush(true)
				logInfo("script", "runscript %s on %s: done", name, group)
				prog.Send(scriptLogMsg{group: group, kind: "sys", text: "runscript ✓ " + name})
				return
			}
		}
	}()
}

// startJobTail opens a live JobTail stream for the hovered job row and pumps
// its frames into the Update loop as jobTailMsg — the same push shape as
// startSubscribe/startRunScript. Returns the cancel func the model stores;
// cancelling tears the stream (and the guest-side tail) down. sid stamps
// every frame so the model can drop leftovers from a superseded hover.
func startJobTail(sid int, group, id string) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	if prog == nil {
		return cancel // no program to push frames into (tests)
	}
	go func() {
		// Release the stream's resources as soon as the tail is over — the
		// model's peekCancel (stored from the return value) may not fire
		// until the next hover change; double-cancel is harmless.
		defer cancel()
		cl, err := getClient()
		if err != nil {
			prog.Send(jobTailMsg{sid: sid, errText: err.Error()})
			return
		}
		stream, err := cl.JobTail(ctx, &pb.JobTailReq{Group: group, Id: id, Tail: jobPeekTailBytes, Parsed: true})
		if err != nil {
			logWarn("jobtail", "open group=%s id=%s failed: %v", group, id, err)
			prog.Send(jobTailMsg{sid: sid, errText: err.Error()})
			return
		}
		logDbg("jobtail", "open group=%s id=%s sid=%d", group, id, sid)
		prog.Send(jobTailMsg{sid: sid, opened: true})
		for {
			ev, rerr := stream.Recv()
			if rerr != nil {
				switch {
				case ctx.Err() != nil:
					// Hover moved on; cancellation is not an error.
					logDbg("jobtail", "group=%s id=%s cancelled (hover moved)", group, id)
				case errors.Is(rerr, io.EOF):
					prog.Send(jobTailMsg{sid: sid, end: true})
				default:
					// A transport failure mid-tail is not a clean end —
					// surface it so the peek pane says so.
					logWarn("jobtail", "group=%s id=%s stream broke: %v", group, id, rerr)
					prog.Send(jobTailMsg{sid: sid, errText: rerr.Error()})
				}
				return
			}
			switch ev.Event {
			case "data":
				// Raw-line frame: a daemon predating JobTailReq.parsed
				// ignores the flag and streams these; keep rendering them.
				// Scrubbed client-side. The current daemon sanitizes every
				// guest-authored byte it relays, but this branch exists
				// precisely for daemons that DON'T — one predating
				// JobTailReq.parsed streams raw frames, and the raw peek
				// renderer hands them to ANSI-aware wrapping and lipgloss,
				// which preserve control sequences rather than removing them.
				// A job whose output contains OSC 52, a title set, or cursor
				// control would then have it interpreted by the operator's
				// terminal the moment they hovered the row (audit M62). A
				// client cannot rely on a peer it is explicitly compatible
				// with; scrubVT is the same policy daemon/sanitize.go applies.
				line := scrubVTStrict(string(ev.Chunk))
				for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
					line = line[:len(line)-1]
				}
				prog.Send(jobTailMsg{sid: sid, line: line})
			case "event":
				if ev.Parsed != nil {
					pe := pbToEvent(ev.Parsed)
					prog.Send(jobTailMsg{sid: sid, ev: &pe})
				}
			case "end":
				prog.Send(jobTailMsg{sid: sid, end: true})
				return
			case "error":
				prog.Send(jobTailMsg{sid: sid, errText: ev.Error})
				return
			}
		}
	}()
	return cancel
}

// bytesIndexByte avoids importing bytes just for one call.
func bytesIndexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func pbToJobs(js []*pb.JobInfo) []JobInfo {
	if len(js) == 0 {
		return nil
	}
	out := make([]JobInfo, 0, len(js))
	for _, j := range js {
		out = append(out, JobInfo{
			ID: j.GetId(), Session: j.GetSession(), Status: j.GetStatus(),
			RC: j.GetRc(), Cmd: j.GetCmd(), Started: j.GetStarted(), OutSize: j.GetOutSize(),
		})
	}
	return out
}

func pbToEvent(p *pb.Event) Event {
	return Event{
		Event: p.Event, Group: p.Group, Ts: p.Ts, Msg: p.Msg, Text: p.Text,
		Name: p.Name, Input: p.Input, Words: int(p.Words), Body: p.Body,
		Historical: p.Historical, ID: p.Id, Seq: p.Seq, Session: p.Session,
		Severity: p.Severity, Title: p.Title,
	}
}

func pbToLogEvent(p *pb.LogEvent) LogEvent {
	return LogEvent{Event: p.Event, Level: p.Level, Msg: p.Msg, Ts: p.Ts, Subsystem: p.Subsystem, Group: p.Group}
}
