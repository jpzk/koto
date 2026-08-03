package main

// ctl_cli.go — `koto ctl`: host-side CLI client for the daemon's gRPC API.
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
//   KOTO_ADDR        dial target            (default 127.0.0.1:8443)
//   KOTO_CREDS_DIR   PKI directory          (default ./creds)
//   KOTO_CLIENT      identity name          (default agent) — resolves
//                       client-<name>.{crt,key} + token-<name> in CREDS_DIR
//   KOTO_CERT/KEY/CA/TOKEN  individual overrides (TOKEN is the value)
//   KOTO_SERVER_NAME TLS server-name override for off-SAN dial targets
//
// Output contract: unary verbs print the full response as one protojson line
// (snake_case fields, same shapes the TUI consumes); `tail` prints one event
// per line forever; `ask` prints the response text plainly (or events as
// JSON with -json). Exit 0 = ok, 1 = daemon said ok:false or transport
// error, 2 = usage.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"koto-protocol/pb"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const ctlUsage = `usage: koto ctl <verb> [flags] [args]

group lifecycle
  list                                     all groups + state
  spawn [-provider P] [-model M] <group>   create/start a group
  stop | interrupt | destroy | restart | clear  <group>

conversation
  send <group> <msg...>        enqueue a message, return immediately ("-" = stdin)
  ask  [-timeout D] [-json] <group> <msg...>
                               send + stream the reply, exit at turn end
  history [-limit N] [-before TS] <group>

config
  config <group>                           print effective config (no flags = read)
  config <group> [-model M] [-provider P] [-network none|wan|lan|full]
         [-size small|medium|large|xlarge] [-root yes|no] [-effort E] [-ports P]
         [-autostart yes|no] [-skills a,b,c | -skills-clear]
                                           set keys ("" clears a key)

skills
  skills [group]                           catalog + a group's enabled set
  skill-new <name>                         scaffold skills/<name>/SKILL.md
  skill-read <name>                        print a skill's SKILL.md

streams
  metrics [group]
  resources                                host-side disk/mem/cpu per group
                                           + fleet rollup (admin)
  tail   [-since N] <group>                group event stream
  logs                                     daemon's own log stream
  watch                                    group-state snapshots on change
  jobs [group]                             ls background jobs (cs-job), fresh
                                           from the guest; no group = all
  job-logs [-tail N] <group> <id>          one job's metadata + output tail
  job-tail [-tail N] <group> <id>          follow a job's output live (^C stops)
  sched list [group] | add <group> <cron...> <msg...> | del|on|off|run <id>

shared shell
  shell <group> [session]                  attach an interactive terminal to
                                           the group's microVM (default tmux
                                           session "koto-shell" = the default
                                           chat session's shell; a named chat
                                           session's is "koto-shell-<name>" —
                                           the same one that session's agent
                                           joins via its Bash tool); ctrl-]
                                           detaches without ending the session

admin role only
  runscript <group> <script...>            run a POSIX script in the group's
                                           microVM as node ("-" = stdin);
                                           output streams live to stdout
  acl get                                  print the ACL document
  acl set <role> <verb>[:<groups>] ...     replace a role's grants
                                           groups = comma list or "*" (default "*")
                                           e.g. acl set ops stop:ghost restart:ghost list
  acl del <role>                           remove a role

env: KOTO_ADDR (127.0.0.1:8443), KOTO_CREDS_DIR (./creds),
     KOTO_CLIENT (agent), KOTO_CERT/KEY/CA/TOKEN, KOTO_SERVER_NAME
`

func ctlEnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ctlFatal(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "koto ctl: "+format+"\n", a...)
	os.Exit(code)
}

// ctlClient dials the daemon with the same mTLS+token scheme as the TUI
// (tui/daemon.go); kept separate because the TUI is its own module.
func ctlClient() pb.KotoClient {
	credsDir := ctlEnvOr("KOTO_CREDS_DIR", "creds")
	name := ctlEnvOr("KOTO_CLIENT", "agent")
	certPath := ctlEnvOr("KOTO_CERT", filepath.Join(credsDir, "client-"+name+".crt"))
	keyPath := ctlEnvOr("KOTO_KEY", filepath.Join(credsDir, "client-"+name+".key"))
	caPath := ctlEnvOr("KOTO_CA", filepath.Join(credsDir, "ca.crt"))

	token := os.Getenv("KOTO_TOKEN")
	if token == "" {
		b, err := os.ReadFile(filepath.Join(credsDir, "token-"+name))
		if err != nil {
			ctlFatal(1, "no token: set KOTO_TOKEN or provide %s (mint with `make pki-client NAME=%s ROLE=agent`)",
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
	if sn := os.Getenv("KOTO_SERVER_NAME"); sn != "" {
		tcfg.ServerName = sn
	}

	cc, err := grpc.NewClient(ctlEnvOr("KOTO_ADDR", "127.0.0.1:8443"),
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
	return pb.NewKotoClient(cc)
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
	groupVerb := func(call func(context.Context, pb.KotoClient, string) (proto.Message, error)) {
		if len(rest) != 1 {
			ctlFatal(2, "usage: koto ctl %s <group>", verb)
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
			ctlFatal(2, "usage: koto ctl spawn [-provider P] [-model M] <group>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Spawn(ctx, &pb.SpawnReq{Group: fs.Arg(0), Provider: *provider, Model: *model})
		ctlPrint(resp, err)

	case "send":
		fs := flag.NewFlagSet("send", flag.ExitOnError)
		session := fs.String("session", "", "chat session within the group (\"\" = default)")
		fs.Parse(rest)
		if fs.NArg() < 2 {
			ctlFatal(2, "usage: koto ctl send [-session S] <group> <msg...>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Send(ctx, &pb.SendReq{Group: fs.Arg(0), Msg: ctlMsgArg(fs.Args()[1:]), Session: *session})
		ctlPrint(resp, err)

	case "ask":
		ctlAsk(rest)

	case "tail":
		fs := flag.NewFlagSet("tail", flag.ExitOnError)
		since := fs.Uint64("since", 0, "replay ring frames with seq > N before going live")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			ctlFatal(2, "usage: koto ctl tail [-since N] <group>")
		}
		cl := ctlClient()
		stream, err := cl.SubscribeGroup(context.Background(), &pb.SubscribeReq{Group: fs.Arg(0), SinceSeq: *since})
		if err != nil {
			ctlFatal(1, "subscribe: %v", err)
		}
		ctlStreamJSON(func() (proto.Message, error) { return stream.Recv() })

	case "history":
		fs := flag.NewFlagSet("history", flag.ExitOnError)
		limit := fs.Int("limit", 0, "max events")
		before := fs.Float64("before", 0, "ts cursor (exclusive)")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			ctlFatal(2, "usage: koto ctl history [-limit N] [-before TS] <group>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.History(ctx, &pb.HistoryReq{Group: fs.Arg(0), Limit: int32(*limit), Before: *before})
		ctlPrint(resp, err)

	case "stop":
		groupVerb(func(ctx context.Context, cl pb.KotoClient, g string) (proto.Message, error) {
			return cl.Stop(ctx, &pb.GroupReq{Group: g})
		})
	case "interrupt":
		groupVerb(func(ctx context.Context, cl pb.KotoClient, g string) (proto.Message, error) {
			return cl.Interrupt(ctx, &pb.GroupReq{Group: g})
		})
	case "destroy":
		groupVerb(func(ctx context.Context, cl pb.KotoClient, g string) (proto.Message, error) {
			return cl.Destroy(ctx, &pb.GroupReq{Group: g})
		})
	case "restart":
		groupVerb(func(ctx context.Context, cl pb.KotoClient, g string) (proto.Message, error) {
			return cl.Restart(ctx, &pb.GroupReq{Group: g})
		})
	case "clear":
		fs := flag.NewFlagSet("clear", flag.ExitOnError)
		session := fs.String("session", "", "clear only this chat session (\"-\" or \"default\" = the default session); omit for the whole group")
		fs.Parse(rest)
		if fs.NArg() != 1 {
			ctlFatal(2, "usage: koto ctl clear [-session S] <group>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Clear(ctx, &pb.GroupReq{Group: fs.Arg(0), Session: *session})
		ctlPrint(resp, err)

	case "jobs":
		g := ""
		if len(rest) == 1 {
			g = rest[0]
		} else if len(rest) > 1 {
			ctlFatal(2, "usage: koto ctl jobs [group]")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Jobs(ctx, &pb.JobsReq{Group: g})
		ctlPrint(resp, err)

	case "job-logs":
		fs := flag.NewFlagSet("job-logs", flag.ExitOnError)
		tail := fs.Int64("tail", 0, "output bytes from the end (default 4096, max 65536)")
		fs.Parse(rest)
		if fs.NArg() != 2 {
			ctlFatal(2, "usage: koto ctl job-logs [-tail N] <group> <id>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.JobLogs(ctx, &pb.JobLogsReq{Group: fs.Arg(0), Id: fs.Arg(1), Tail: *tail})
		ctlPrint(resp, err)

	case "job-tail":
		fs := flag.NewFlagSet("job-tail", flag.ExitOnError)
		tail := fs.Int64("tail", 0, "initial window bytes (default 65536)")
		fs.Parse(rest)
		if fs.NArg() != 2 {
			ctlFatal(2, "usage: koto ctl job-tail [-tail N] <group> <id>")
		}
		cl := ctlClient()
		stream, err := cl.JobTail(context.Background(), &pb.JobTailReq{Group: fs.Arg(0), Id: fs.Arg(1), Tail: *tail})
		if err != nil {
			ctlFatal(1, "job-tail: %v", err)
		}
		for {
			ev, rerr := stream.Recv()
			if rerr != nil {
				ctlFatal(1, "stream: %v", rerr)
			}
			switch ev.Event {
			case "data":
				os.Stdout.Write(ev.Chunk)
			case "end":
				return
			case "error":
				ctlFatal(1, "%s", ev.Error)
			}
		}

	case "metrics":
		g := ""
		if len(rest) == 1 {
			g = rest[0]
		} else if len(rest) > 1 {
			ctlFatal(2, "usage: koto ctl metrics [group]")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Metrics(ctx, &pb.MetricsReq{Group: g})
		ctlPrint(resp, err)

	case "resources":
		if len(rest) != 0 {
			ctlFatal(2, "usage: koto ctl resources")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Resources(ctx, &pb.ResourcesReq{})
		ctlPrint(resp, err)

	case "config":
		ctlConfig(rest)

	case "skills":
		g := ""
		if len(rest) == 1 {
			g = rest[0]
		} else if len(rest) > 1 {
			ctlFatal(2, "usage: koto ctl skills [group]")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.Skills(ctx, &pb.SkillListReq{Group: g})
		ctlPrint(resp, err)

	case "skill-new":
		if len(rest) != 1 {
			ctlFatal(2, "usage: koto ctl skill-new <name>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.SkillNew(ctx, &pb.SkillNewReq{Name: rest[0]})
		ctlPrint(resp, err)

	case "skill-read":
		if len(rest) != 1 {
			ctlFatal(2, "usage: koto ctl skill-read <name>")
		}
		cl := ctlClient()
		ctx, cancel := ctlCtx()
		defer cancel()
		resp, err := cl.SkillRead(ctx, &pb.SkillReadReq{Name: rest[0]})
		ctlPrint(resp, err)

	case "logs":
		if len(rest) != 0 {
			ctlFatal(2, "usage: koto ctl logs")
		}
		stream, err := ctlClient().SubscribeLogs(context.Background(), &pb.LogsReq{})
		if err != nil {
			ctlFatal(1, "logs: %v", err)
		}
		ctlStreamJSON(func() (proto.Message, error) { return stream.Recv() })

	case "watch":
		if len(rest) != 0 {
			ctlFatal(2, "usage: koto ctl watch")
		}
		stream, err := ctlClient().WatchState(context.Background(), &pb.WatchReq{})
		if err != nil {
			ctlFatal(1, "watch: %v", err)
		}
		ctlStreamJSON(func() (proto.Message, error) { return stream.Recv() })

	case "sched":
		ctlSched(rest)

	case "runscript":
		// Raw output on purpose — this verb is "run my script, show me its
		// bytes", not an event feed, so no protojson framing like tail/logs.
		if len(rest) < 2 {
			ctlFatal(2, "usage: koto ctl runscript <group> <script...>")
		}
		cl := ctlClient()
		stream, err := cl.RunScript(context.Background(),
			&pb.RunScriptReq{Group: rest[0], Script: ctlMsgArg(rest[1:])})
		if err != nil {
			ctlFatal(1, "runscript: %v", err)
		}
		for {
			ev, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				ctlFatal(1, "stream: %v", err)
			}
			switch ev.Event {
			case "data":
				_, _ = os.Stdout.Write(ev.Chunk)
			case "end":
				return
			case "error":
				ctlFatal(1, "%s", ev.Error)
			}
		}

	case "shell":
		if len(rest) < 1 || len(rest) > 2 {
			ctlFatal(2, "usage: koto ctl shell <group> [session]")
		}
		session := ""
		if len(rest) == 2 {
			session = rest[1]
		}
		ctlShell(rest[0], session)

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
		ctlFatal(2, "usage: koto ctl acl get|set|del ...")
	}
	sub, rest := args[0], args[1:]
	cl := ctlClient()
	ctx, cancel := ctlCtx()
	defer cancel()

	switch sub {
	case "get":
		if len(rest) != 0 {
			ctlFatal(2, "usage: koto ctl acl get")
		}
		resp, err := cl.AclGet(ctx, &pb.AclGetReq{})
		ctlPrint(resp, err)

	case "set":
		if len(rest) < 1 {
			ctlFatal(2, "usage: koto ctl acl set <role> <verb>[:<groups>] ...")
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
			ctlFatal(2, "usage: koto ctl acl del <role>")
		}
		resp, err := cl.AclDelRole(ctx, &pb.AclDelRoleReq{Role: rest[0]})
		ctlPrint(resp, err)

	default:
		ctlFatal(2, "unknown acl subcommand: %s (get|set|del)", sub)
	}
}

// ctlStreamJSON drains a server stream to stdout, one protojson line per
// frame, until the stream ends or errors. Shared by tail/logs/watch — every
// streaming verb prints the same one-frame-per-line shape agents can pipe
// into jq. A clean EOF (io.EOF) exits 0; any other error exits 1.
func ctlStreamJSON(recv func() (proto.Message, error)) {
	for {
		msg, err := recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			ctlFatal(1, "stream: %v", err)
		}
		b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
		fmt.Println(string(b))
	}
}

// ctlShell drives the AttachShell bidi RPC as an interactive terminal — the
// fastest way to exercise the shared-shell feature without the TUI. Puts
// stdin into raw mode (cfmakeraw-equivalent via direct termios ioctls —
// golang.org/x/sys/unix is already this module's dependency, no need for a
// separate terminal library) so keystrokes reach the guest pty unmodified,
// including control characters (ctrl-C etc. go to the remote shell, not to
// this process — ISIG is disabled). ctrl-] (0x1d) is the local detach key,
// mirroring telnet's escape convention, since raw mode swallows the usual
// ctrl-C/ctrl-D interrupt path.
func ctlShell(group, session string) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		ctlFatal(1, "shell: stdin is not a terminal: %v", err)
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		ctlFatal(1, "shell: enter raw mode: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
		}
	}
	defer restore()
	// ctlFatal calls os.Exit directly, which skips defers — every fatal path
	// below must restore the terminal itself first.
	fail := func(format string, a ...any) {
		restore()
		ctlFatal(1, format, a...)
	}

	cols, rows := ctlWinsize()
	cl := ctlClient()
	stream, err := cl.AttachShell(context.Background())
	if err != nil {
		fail("shell: %v", err)
	}
	if err := stream.Send(&pb.ShellInput{
		Group: group,
		Input: &pb.ShellInput_Open{Open: &pb.ShellOpen{Session: session, Cols: uint32(cols), Rows: uint32(rows)}},
	}); err != nil {
		fail("shell: open: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[attached to %s — ctrl-] detaches]\r\n", group)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			c, r := ctlWinsize()
			_ = stream.Send(&pb.ShellInput{
				Group: group,
				Input: &pb.ShellInput_Resize{Resize: &pb.ShellResize{Cols: uint32(c), Rows: uint32(r)}},
			})
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if bytes.IndexByte(buf[:n], 0x1d) >= 0 {
					restore()
					fmt.Fprintf(os.Stderr, "\r\n[detached — session left running]\r\n")
					os.Exit(0)
				}
				chunk := append([]byte(nil), buf[:n]...)
				if serr := stream.Send(&pb.ShellInput{Group: group, Input: &pb.ShellInput_Data{Data: chunk}}); serr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	for {
		frame, rerr := stream.Recv()
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			fail("shell: %v", rerr)
		}
		switch frame.Event {
		case "data":
			_, _ = os.Stdout.Write(frame.Chunk)
		case "end":
			restore()
			fmt.Fprintf(os.Stderr, "\r\n[connection ended — session may still be running]\r\n")
			return
		case "error":
			fail("%s", frame.Error)
		}
	}
}

func ctlWinsize() (cols, rows int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}

// ctlConfig reads or sets a group's config.json. With no -flags it's a pure
// read (the daemon returns the effective config, all keys filled). A flag is
// only sent when explicitly passed (fs.Visit), so an absent flag leaves its
// key unchanged while an explicit empty value ("") clears it — matching the
// optional-field semantics the TUI uses. Most keys apply on the next
// /restart (network/size/root/ports); model/provider/effort take effect on
// the next message; autostart is read only at daemon start.
func ctlConfig(args []string) {
	// Group is the first positional, flags follow (config <group> [-flags]).
	// Go's flag package stops at the first non-flag token, so the group must
	// lead — parse everything after it as flags.
	if len(args) < 1 {
		ctlFatal(2, "usage: koto ctl config <group> [-model M] [-network none|wan|lan|full] [-skills a,b|-skills-clear] ...")
	}
	group, rest := args[0], args[1:]
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	model := fs.String("model", "", `model (""=clear)`)
	effort := fs.String("effort", "", "reasoning effort")
	ports := fs.String("ports", "", "published ports (comma list)")
	provider := fs.String("provider", "", "claudesdk|venice")
	network := fs.String("network", "", "none|wan|lan|full")
	size := fs.String("size", "", "small|medium|large|xlarge")
	root := fs.String("root", "", "yes|no")
	autostart := fs.String("autostart", "", "yes|no — boot this group with the daemon")
	skills := fs.String("skills", "", "enabled skills (comma list)")
	skillsClear := fs.Bool("skills-clear", false, "clear the enabled-skills list")
	fs.Parse(rest)
	if fs.NArg() != 0 {
		ctlFatal(2, "config: unexpected args after group: %v (flags follow the group)", fs.Args())
	}
	req := &pb.ConfigReq{Group: group}

	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["model"] {
		req.Model = model
	}
	if seen["effort"] {
		req.Effort = effort
	}
	if seen["ports"] {
		req.Ports = ports
	}
	if seen["provider"] {
		req.Provider = provider
	}
	if seen["network"] {
		req.Network = network
	}
	if seen["size"] {
		req.Size = size
	}
	if seen["root"] {
		req.Root = root
	}
	if seen["autostart"] {
		req.Autostart = autostart
	}
	if seen["skills-clear"] && *skillsClear {
		req.SkillsAction = &pb.ConfigReq_SkillsClear{SkillsClear: &emptypb.Empty{}}
	} else if seen["skills"] {
		var items []string
		for _, s := range strings.Split(*skills, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		req.SkillsAction = &pb.ConfigReq_SkillsSet{SkillsSet: &pb.SkillList{Items: items}}
	}

	cl := ctlClient()
	ctx, cancel := ctlCtx()
	defer cancel()
	resp, err := cl.Config(ctx, req)
	ctlPrint(resp, err)
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
	session := fs.String("session", "", "chat session within the group (\"\" = default)")
	fs.Parse(args)
	if fs.NArg() < 2 {
		ctlFatal(2, "usage: koto ctl ask [-timeout D] [-json] [-session S] <group> <msg...>")
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
	resp, err := cl.Send(sendCtx, &pb.SendReq{Group: group, Msg: msg, Session: *session})
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
		ctlFatal(2, "usage: koto ctl sched list|add|del|on|off|run ...")
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
			ctlFatal(2, "usage: koto ctl sched list [group]")
		}
		resp, err := cl.SchedList(ctx, &pb.SchedListReq{Group: g})
		ctlPrint(resp, err)

	case "add":
		if len(rest) < 3 {
			ctlFatal(2, "usage: koto ctl sched add <group> <cron...> <msg...>")
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
			ctlFatal(2, "usage: koto ctl sched %s <id>", sub)
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
