package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"clawson-protocol/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// The wire contract shared with the daemon is the proto alone (generated
// code in clawson-protocol/pb); the view types it converts into live in
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
	certPath := envOr("CLAWSON_CERT", "/clawson-creds/client.crt")
	keyPath := envOr("CLAWSON_KEY", "/clawson-creds/client.key")
	caPath := envOr("CLAWSON_CA", "/clawson-creds/ca.crt")
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
	if sn := os.Getenv("CLAWSON_SERVER_NAME"); sn != "" {
		cfg.ServerName = sn
	}
	return cfg, nil
}

var (
	clientOnce sync.Once
	client     pb.ClawsonClient
	clientErr  error
)

func getClient() (pb.ClawsonClient, error) {
	clientOnce.Do(func() {
		tcfg, err := clientTLS()
		if err != nil {
			clientErr = err
			return
		}
		ep := envOr("CLAWSON_ENDPOINT", "127.0.0.1:8443")
		cc, err := grpc.NewClient(ep,
			grpc.WithTransportCredentials(credentials.NewTLS(tcfg)),
			grpc.WithPerRPCCredentials(tokenCreds{os.Getenv("CLAWSON_TOKEN")}),
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
			clientErr = err
			return
		}
		client = pb.NewClawsonClient(cc)
	})
	return client, clientErr
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

	msg, err := callRPC(ctx, cl, cmd, extra)
	if err != nil {
		return nil, err
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if ok, _ := out["ok"].(bool); !ok {
		es, _ := out["error"].(string)
		return out, fmt.Errorf("daemon: %s", es)
	}
	return out, nil
}

func callRPC(ctx context.Context, cl pb.ClawsonClient, cmd string, extra map[string]any) (proto.Message, error) {
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
		return cl.Send(ctx, &pb.SendReq{Group: s("group"), Msg: s("msg")})
	case "stop":
		return cl.Stop(ctx, &pb.GroupReq{Group: s("group")})
	case "interrupt":
		return cl.Interrupt(ctx, &pb.GroupReq{Group: s("group")})
	case "destroy":
		return cl.Destroy(ctx, &pb.GroupReq{Group: s("group")})
	case "restart":
		return cl.Restart(ctx, &pb.GroupReq{Group: s("group")})
	case "clear":
		return cl.Clear(ctx, &pb.GroupReq{Group: s("group")})
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
	case "config":
		return cl.Config(ctx, buildConfigReq(extra))
	case "skills":
		return cl.Skills(ctx, &pb.SkillListReq{Group: s("group")})
	case "skill_new":
		return cl.SkillNew(ctx, &pb.SkillNewReq{Name: s("name")})
	case "skill_read":
		return cl.SkillRead(ctx, &pb.SkillReadReq{Name: s("name")})
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
	}
	return nil, fmt.Errorf("unknown cmd: %s", cmd)
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
// skills arrives as a []string (set) or "" (clear) from skillToggleCmd; a bare
// non-empty string for skills is a no-op (matches the daemon's reject-non-list
// behavior).
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
	setOpt("size", &r.Size)
	setOpt("root", &r.Root)
	if sk, ok := extra["skills"]; ok {
		switch v := sk.(type) {
		case []string:
			r.SkillsAction = &pb.ConfigReq_SkillsSet{SkillsSet: &pb.SkillList{Items: v}}
		case []any:
			items := make([]string, 0, len(v))
			for _, e := range v {
				if str, ok := e.(string); ok {
					items = append(items, str)
				}
			}
			r.SkillsAction = &pb.ConfigReq_SkillsSet{SkillsSet: &pb.SkillList{Items: items}}
		case string:
			if v == "" {
				r.SkillsAction = &pb.ConfigReq_SkillsClear{SkillsClear: &emptypb.Empty{}}
			}
		}
	}
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
			Port:     int(gi.GetPort()),
			Running:  gi.GetRunning(),
			Provider: gi.GetProvider(),
			Model:    gi.GetModel(),
			Effort:   gi.GetEffort(),
			Stalled:  gi.GetStalled(),
			Queued:   int(gi.GetQueued()),
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

func pbToEvent(p *pb.Event) Event {
	return Event{
		Event: p.Event, Group: p.Group, Ts: p.Ts, Msg: p.Msg, Text: p.Text,
		Name: p.Name, Input: p.Input, Words: int(p.Words), Body: p.Body,
		Historical: p.Historical, ID: p.Id, Seq: p.Seq,
	}
}

func pbToLogEvent(p *pb.LogEvent) LogEvent {
	return LogEvent{Event: p.Event, Level: p.Level, Msg: p.Msg, Ts: p.Ts}
}
