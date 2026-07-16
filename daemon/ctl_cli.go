package main

// ctl_cli.go — `clawson ctl`: host-side CLI client for the daemon's gRPC API.
//
// This is the integration surface for coding agents (claude code, or any
// framework with a shell tool): every verb is one process invocation that
// prints JSON to stdout and exits. Auth is the same mTLS + bearer token the
// TUI uses; mint a dedicated identity with
//
//   make pki-client NAME=agent ROLE=agent
//
// The identity's role(s) are enforced server-side by the ACL (acl.go): each
// verb-on-group call is checked against the role's grants, so a leaked agent
// token can do only what creds/acl.json lets its role do — nothing here
// widens that. A verb the role lacks comes back as a PermissionDenied rpc
// error (exit 1), the same as any other client.
//
// Endpoint + creds resolve from env, defaulting to a `creds/` dir under the
// current working directory (agents run from the project root):
//
//   CLAWSON_ADDR        dial target            (default 127.0.0.1:8443)
//   CLAWSON_CREDS_DIR   PKI directory          (default ./creds)
//   CLAWSON_CLIENT      identity name          (default agent) — resolves
//                       client-<name>.{crt,key} + token-<name> in CREDS_DIR
//   CLAWSON_CERT/KEY/CA/TOKEN  individual overrides (TOKEN is the value)
//   CLAWSON_SERVER_NAME TLS server-name override for off-SAN dial targets
//
// Output contract: unary verbs print the full response as one protojson line
// (snake_case fields, same shapes the TUI consumes); `tail` prints one event
// per line forever; `ask` prints the response text plainly (or events as
// JSON with -json). Exit 0 = ok, 1 = daemon said ok:false or transport
// error, 2 = usage.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clawson-protocol/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const ctlUsage = `usage: clawson ctl <verb> [flags] [args]

group lifecycle
  list                                     all groups + state
  spawn [-provider P] [-model M] <group>   create/start a group
  stop | interrupt | destroy | restart | clear  <group>

conversation
  send <group> <msg...>        enqueue a message, return immediately ("-" = stdin)
  ask  [-timeout D] [-json] <group> <msg...>
                               send + stream the reply, exit at turn end
  tail [-since N] <group>      raw event stream, one JSON line per event
  history [-limit N] [-before TS] <group>

other
  metrics [group]
  sched list [group] | add <group> <cron...> <msg...> | del|on|off|run <id>

acl (admin role only)
  acl get                                  print the ACL document
  acl set <role> <verb>[:<groups>] ...     replace a role's grants
                                           groups = comma list or "*" (default "*")
                                           e.g. acl set ops stop:ghost restart:ghost list
  acl del <role>                           remove a role

env: CLAWSON_ADDR (127.0.0.1:8443), CLAWSON_CREDS_DIR (./creds),
     CLAWSON_CLIENT (agent), CLAWSON_CERT/KEY/CA/TOKEN, CLAWSON_SERVER_NAME
`

func ctlEnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ctlFatal(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "clawson ctl: "+format+"\n", a...)
	os.Exit(code)
}

// ctlClient dials the daemon with the same mTLS+token scheme as the TUI
// (tui/daemon.go); kept separate because the TUI is its own module.
func ctlClient() pb.ClawsonClient {
	credsDir := ctlEnvOr("CLAWSON_CREDS_DIR", "creds")
	name := ctlEnvOr("CLAWSON_CLIENT", "agent")
	certPath := ctlEnvOr("CLAWSON_CERT", filepath.Join(credsDir, "client-"+name+".crt"))
	keyPath := ctlEnvOr("CLAWSON_KEY", filepath.Join(credsDir, "client-"+name+".key"))
	caPath := ctlEnvOr("CLAWSON_CA", filepath.Join(credsDir, "ca.crt"))

	token := os.Getenv("CLAWSON_TOKEN")
	if token == "" {
		b, err := os.ReadFile(filepath.Join(credsDir, "token-"+name))
		if err != nil {
			ctlFatal(1, "no token: set CLAWSON_TOKEN or provide %s (mint with `make pki-client NAME=%s ROLE=agent`)",
				filepath.Join(credsDir, "token-"+name), name)
		}
		token = strings.TrimSpace(string(b))
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		ctlFatal(1, "client cert: %v", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		ctlFatal(1, "ca: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		ctlFatal(1, "ca %s: no certificates parsed", caPath)
	}
	tcfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}
	if sn := os.Getenv("CLAWSON_SERVER_NAME"); sn != "" {
		tcfg.ServerName = sn
	}

	cc, err := grpc.NewClient(ctlEnvOr("CLAWSON_ADDR", "127.0.0.1:8443"),
		grpc.WithTransportCredentials(credentials.NewTLS(tcfg)),
		grpc.WithPerRPCCredentials(tokenCreds{token}),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		ctlFatal(1, "dial: %v", err)
	}
	return pb.NewClawsonClient(cc)
}

// tokenCreds attaches the bearer token to every RPC (same shape as the TUI's).
type tokenCreds struct{ token string }

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}
func (tokenCreds) RequireTransportSecurity() bool { return true }

// ctlPrint marshals a unary response to one protojson line and exits 1 when
// the daemon reported ok:false (the in-band application-error contract).
func ctlPrint(msg proto.Message, err error) {
	if err != nil {
		ctlFatal(1, "rpc: %v", err)
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		ctlFatal(1, "marshal: %v", err)
	}
	fmt.Println(string(b))
	var probe struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &probe) == nil && !probe.OK {
		ctlFatal(1, "daemon: %s", probe.Error)
	}
}

func ctlCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// ctlMsgArg joins trailing args into the message; a lone "-" reads stdin so
// agents can pipe long prompts without shell-quoting pain.
func ctlMsgArg(args []string) string {
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			ctlFatal(1, "stdin: %v", err)
		}
		return strings.TrimRight(string(b), "\n")
	}
	return strings.Join(args, " ")
}

func ctlCliMain(args []string) {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, ctlUsage)
		os.Exit(2)
	}
	verb, rest := args[0], args[1:]

	// Group-only verbs share one shape.
	groupVerb := func(call func(context.Context, pb.ClawsonClient, string) (proto.Message, error)) {
		if len(rest) != 1 {
			ctlFatal(2, "usage: clawson ctl %s <group>", verb)
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		msg, err := call(ctx, cl, rest[0])
		ctlPrint(msg, err)
	}

	switch verb {
	case "list":
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.List(ctx, &pb.ListReq{})
		ctlPrint(resp, err)

	case "spawn":
		fs := flag.NewFlagSet("spawn", flag.ExitOnError)
		provider := fs.String("provider", "", "claudesdk|venice")
		model := fs.String("model", "", "model override")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			ctlFatal(2, "usage: clawson ctl spawn [-provider P] [-model M] <group>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Spawn(ctx, &pb.SpawnReq{Group: fs.Arg(0), Provider: *provider, Model: *model})
		ctlPrint(resp, err)

	case "send":
		if len(rest) < 2 {
			ctlFatal(2, "usage: clawson ctl send <group> <msg...>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Send(ctx, &pb.SendReq{Group: rest[0], Msg: ctlMsgArg(rest[1:])})
		ctlPrint(resp, err)

	case "ask":
		ctlAsk(rest)

	case "tail":
		fs := flag.NewFlagSet("tail", flag.ExitOnError)
		since := fs.Uint64("since", 0, "replay ring frames with seq > N before going live")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			ctlFatal(2, "usage: clawson ctl tail [-since N] <group>")
		}
		cl := ctlClient()
		stream, err := cl.SubscribeGroup(context.Background(), &pb.SubscribeReq{Group: fs.Arg(0), SinceSeq: *since})
		if err != nil {
			ctlFatal(1, "subscribe: %v", err)
		}
		for {
			ev, err := stream.Recv()
			if err != nil {
				ctlFatal(1, "stream: %v", err)
			}
			b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(ev)
			fmt.Println(string(b))
		}

	case "history":
		fs := flag.NewFlagSet("history", flag.ExitOnError)
		limit := fs.Int("limit", 0, "max events")
		before := fs.Float64("before", 0, "ts cursor (exclusive)")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			ctlFatal(2, "usage: clawson ctl history [-limit N] [-before TS] <group>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.History(ctx, &pb.HistoryReq{Group: fs.Arg(0), Limit: int32(*limit), Before: *before})
		ctlPrint(resp, err)

	case "stop":
		groupVerb(func(ctx context.Context, cl pb.ClawsonClient, g string) (proto.Message, error) {
			return cl.Stop(ctx, &pb.GroupReq{Group: g})
		})
	case "interrupt":
		groupVerb(func(ctx context.Context, cl pb.ClawsonClient, g string) (proto.Message, error) {
			return cl.Interrupt(ctx, &pb.GroupReq{Group: g})
		})
	case "destroy":
		groupVerb(func(ctx context.Context, cl pb.ClawsonClient, g string) (proto.Message, error) {
			return cl.Destroy(ctx, &pb.GroupReq{Group: g})
		})
	case "restart":
		groupVerb(func(ctx context.Context, cl pb.ClawsonClient, g string) (proto.Message, error) {
			return cl.Restart(ctx, &pb.GroupReq{Group: g})
		})
	case "clear":
		groupVerb(func(ctx context.Context, cl pb.ClawsonClient, g string) (proto.Message, error) {
			return cl.Clear(ctx, &pb.GroupReq{Group: g})
		})

	case "metrics":
		g := ""
		if len(rest) == 1 {
			g = rest[0]
		} else if len(rest) > 1 {
			ctlFatal(2, "usage: clawson ctl metrics [group]")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Metrics(ctx, &pb.MetricsReq{Group: g})
		ctlPrint(resp, err)

	case "sched":
		ctlSched(rest)

	case "acl":
		ctlAcl(rest)

	default:
		fmt.Fprint(os.Stderr, ctlUsage)
		ctlFatal(2, "unknown verb: %s", verb)
	}
}

// ctlAcl drives the admin-only ACL management verbs. `set` builds the grants
// object from shell-friendly "verb:group,group" / "verb" (= "verb:*") tokens
// so a role can be defined without hand-writing JSON:
//
//	acl get
//	acl set ops stop:ghost restart:ghost list metrics:*
//	acl del ops
//
// A non-admin token gets PermissionDenied from the daemon (exit 1) — the
// acl_* verbs are hardcoded admin-only server-side.
func ctlAcl(args []string) {
	if len(args) < 1 {
		ctlFatal(2, "usage: clawson ctl acl get|set|del ...")
	}
	sub, rest := args[0], args[1:]
	cl := ctlClient()
	ctx, cancel := ctlCtx()
	defer cancel()

	switch sub {
	case "get":
		if len(rest) != 0 {
			ctlFatal(2, "usage: clawson ctl acl get")
		}
		resp, err := cl.AclGet(ctx, &pb.AclGetReq{})
		ctlPrint(resp, err)

	case "set":
		if len(rest) < 1 {
			ctlFatal(2, "usage: clawson ctl acl set <role> <verb>[:<groups>] ...")
		}
		role, grantArgs := rest[0], rest[1:]
		grants := map[string]any{}
		for _, tok := range grantArgs {
			verb, targets, _ := strings.Cut(tok, ":")
			if verb == "" {
				ctlFatal(2, "acl set: empty verb in %q", tok)
			}
			if targets == "" || targets == "*" {
				grants[verb] = "*"
				continue
			}
			var list []any
			for _, g := range strings.Split(targets, ",") {
				if g = strings.TrimSpace(g); g != "" {
					list = append(list, g)
				}
			}
			grants[verb] = list
		}
		gs, err := structpb.NewStruct(grants)
		if err != nil {
			ctlFatal(2, "acl set: bad grants: %v", err)
		}
		resp, err := cl.AclSetRole(ctx, &pb.AclSetRoleReq{Role: role, Grants: gs})
		ctlPrint(resp, err)

	case "del":
		if len(rest) != 1 {
			ctlFatal(2, "usage: clawson ctl acl del <role>")
		}
		resp, err := cl.AclDelRole(ctx, &pb.AclDelRoleReq{Role: rest[0]})
		ctlPrint(resp, err)

	default:
		ctlFatal(2, "unknown acl subcommand: %s (get|set|del)", sub)
	}
}

// ctlAsk is the synchronous primitive agents actually want: send a message,
// stream the reply, exit when the turn ends. It subscribes BEFORE sending so
// no frame can slip between send and subscribe, then starts capturing at the
// prompt event echoing our message (sends are queued — frames from an
// earlier in-flight turn may arrive first) and exits 0 at the next turn_end.
func ctlAsk(args []string) {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	timeout := fs.Duration("timeout", 30*time.Minute, "give up after this long")
	asJSON := fs.Bool("json", false, "print captured events as JSON lines instead of plain text")
	fs.Parse(args)
	if fs.NArg() < 2 {
		ctlFatal(2, "usage: clawson ctl ask [-timeout D] [-json] <group> <msg...>")
	}
	group, msg := fs.Arg(0), ctlMsgArg(fs.Args()[1:])

	cl := ctlClient()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	stream, err := cl.SubscribeGroup(ctx, &pb.SubscribeReq{Group: group, SinceSeq: 0})
	if err != nil {
		ctlFatal(1, "subscribe: %v", err)
	}
	sendCtx, sendCancel := ctlCtx()
	resp, err := cl.Send(sendCtx, &pb.SendReq{Group: group, Msg: msg})
	sendCancel()
	if err != nil {
		ctlFatal(1, "send: %v", err)
	}
	if !resp.GetOk() {
		ctlFatal(1, "daemon: %s", resp.GetError())
	}

	capturing := false
	for {
		ev, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				ctlFatal(1, "timed out after %s waiting for turn end", *timeout)
			}
			ctlFatal(1, "stream: %v", err)
		}
		if !capturing {
			if ev.GetEvent() == "prompt" && ev.GetMsg() == msg {
				capturing = true
			}
			continue
		}
		if *asJSON {
			b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(ev)
			fmt.Println(string(b))
		}
		switch ev.GetEvent() {
		case "done":
			if !*asJSON {
				fmt.Println(ev.GetText())
			}
		case "err":
			if !*asJSON {
				fmt.Fprintln(os.Stderr, ev.GetText())
			}
		case "turn_end":
			return
		}
	}
}

// ctlSched parses the shell-friendly schedule forms:
//
//	sched list [group]
//	sched add <group> <cron...> <msg...>   cron = 5 fields, or one @alias
//	sched del|on|off|run <id>
func ctlSched(args []string) {
	if len(args) < 1 {
		ctlFatal(2, "usage: clawson ctl sched list|add|del|on|off|run ...")
	}
	sub, rest := args[0], args[1:]
	cl := ctlClient()
	ctx, cancel := ctlCtx()
	defer cancel()

	switch sub {
	case "list":
		g := ""
		if len(rest) == 1 {
			g = rest[0]
		} else if len(rest) > 1 {
			ctlFatal(2, "usage: clawson ctl sched list [group]")
		}
		resp, err := cl.SchedList(ctx, &pb.SchedListReq{Group: g})
		ctlPrint(resp, err)

	case "add":
		if len(rest) < 3 {
			ctlFatal(2, "usage: clawson ctl sched add <group> <cron...> <msg...>")
		}
		group := rest[0]
		var cron, msg string
		if strings.HasPrefix(rest[1], "@") {
			cron, msg = rest[1], strings.Join(rest[2:], " ")
		} else {
			if len(rest) < 7 {
				ctlFatal(2, "sched add: need 5 cron fields (or one @alias) + msg")
			}
			cron, msg = strings.Join(rest[1:6], " "), strings.Join(rest[6:], " ")
		}
		if msg == "" {
			ctlFatal(2, "sched add: empty message")
		}
		resp, err := cl.SchedAdd(ctx, &pb.SchedAddReq{Group: group, Cron: cron, Msg: msg})
		ctlPrint(resp, err)

	case "del", "run", "on", "off":
		if len(rest) != 1 {
			ctlFatal(2, "usage: clawson ctl sched %s <id>", sub)
		}
		switch sub {
		case "del":
			resp, err := cl.SchedDel(ctx, &pb.SchedIDReq{Id: rest[0]})
			ctlPrint(resp, err)
		case "run":
			resp, err := cl.SchedRun(ctx, &pb.SchedIDReq{Id: rest[0]})
			ctlPrint(resp, err)
		default:
			resp, err := cl.SchedToggle(ctx, &pb.SchedToggleReq{Id: rest[0], Enabled: sub == "on"})
			ctlPrint(resp, err)
		}

	default:
		ctlFatal(2, "unknown sched subcommand: %s", sub)
	}
}
