package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"koto-protocol/pb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// groupNameRE is the gRPC-boundary allowlist for group names. It's
// deliberately more permissive than ctl.go's ctlGroupRE (which is
// lowercase-only, for peer-spawned groups): existing groups spawned via this
// RPC predate any validation and use mixed case (CHARLIE, XYZ, Z00M, 9AZ,
// DELTA), so this only excludes what's actually dangerous — path
// separators, "..", and control characters — which is what let an
// unvalidated group name reach vol(g) (== filepath.Join(ROOT, g)) or any of
// the fc.go path helpers that concatenate g directly (fcPidPath, fcCfgPath,
// fcConsolePath, fcJailDir) and escape their intended directory. Every RPC
// that takes a bare group name must check this before the name reaches any
// of those helpers.
var groupNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

func validGroupName(g string) bool { return groupNameRE.MatchString(g) }

// kotoServer implements pb.KotoServer. Each method is a thin wrapper over
// the existing daemon helpers (ensure/send/listGroups/configCmd/...): it
// converts the protobuf request to the helper's native types, calls the
// unchanged helper, and converts the result back to protobuf. Application-level
// failures are returned in-band via the response's ok/error fields with a nil
// gRPC error — matching the old {ok:false,error} JSON contract the clients read.
// gRPC status codes are reserved for transport/auth faults (see auth.go).
type kotoServer struct {
	pb.UnimplementedKotoServer
}

// ---- converters (protocol/native types -> protobuf) -----------------------

func toPBEvent(ev Event) *pb.Event {
	return &pb.Event{
		Event:      ev.Event,
		Group:      ev.Group,
		Ts:         ev.Ts,
		Msg:        ev.Msg,
		Text:       ev.Text,
		Name:       ev.Name,
		Input:      ev.Input,
		Words:      int32(ev.Words),
		Body:       ev.Body,
		Historical: ev.Historical,
		Id:         ev.ID,
		Seq:        ev.Seq,
		Session:    ev.Session,
		Severity:   ev.Severity,
		Title:      ev.Title,
	}
}

func toPBGroupInfo(gi GroupInfo) *pb.GroupInfo {
	out := &pb.GroupInfo{
		Port:     int32(gi.Port),
		Running:  gi.Running,
		Provider: gi.Provider,
		Model:    gi.Model,
		Effort:   gi.Effort,
		Stalled:  gi.Stalled,
		Queued:   int32(gi.Queued),
		Sessions: gi.Sessions,
	}
	for _, j := range gi.Jobs {
		out.Jobs = append(out.Jobs, toPBJobInfo(j))
	}
	return out
}

func toPBJobInfo(j JobInfo) *pb.JobInfo {
	return &pb.JobInfo{
		Id: j.ID, Session: j.Session, Status: j.Status, Rc: j.RC,
		Cmd: j.Cmd, Started: j.Started, OutSize: j.OutSize, Group: j.Group,
	}
}

func toPBSkillItem(it skillItem) *pb.SkillItem {
	return &pb.SkillItem{Name: it.Name, Description: it.Description, Path: it.Path, Enabled: it.Enabled}
}

func toPBScheduleItem(it scheduleItem) *pb.ScheduleItem {
	return &pb.ScheduleItem{
		Id:          it.ID,
		Group:       it.Group,
		Cron:        it.Cron,
		Msg:         it.Msg,
		Enabled:     it.Enabled,
		CreatedAt:   it.CreatedAt,
		LastFiredAt: it.LastFiredAt,
		NextDueAt:   it.NextDueAt,
	}
}

// fromPBConfigReq re-synthesizes the json.RawMessage tri-state (absent / clear /
// set) the existing applyConfig/isClear logic expects, from the protobuf
// presence (optional scalars) + oneof (skills). Absent => nil; clear => an
// empty value isClear() recognizes; set => the JSON-encoded value.
func fromPBConfigReq(r *pb.ConfigReq) configReq {
	out := configReq{Group: r.GetGroup()}
	optRaw := func(p *string) json.RawMessage {
		if p == nil {
			return nil // absent
		}
		b, _ := json.Marshal(*p) // "" -> `""` (clear); value -> set
		return b
	}
	out.Model = optRaw(r.Model)
	out.Effort = optRaw(r.Effort)
	out.Ports = optRaw(r.Ports)
	out.Provider = optRaw(r.Provider)
	out.Internet = optRaw(r.Internet)
	out.Network = optRaw(r.Network)
	out.Size = optRaw(r.Size)
	out.Root = optRaw(r.Root)
	out.Autostart = optRaw(r.Autostart)
	switch r.GetSkillsAction().(type) {
	case *pb.ConfigReq_SkillsClear:
		out.Skills = json.RawMessage("[]") // isClear -> delete key
	case *pb.ConfigReq_SkillsSet:
		b, _ := json.Marshal(r.GetSkillsSet().GetItems())
		out.Skills = b
	}
	return out
}

// toStruct converts a map[string]any to a protobuf Struct. It JSON-normalizes
// first because the config map carries native Go slices (applyConfig writes
// []string for skills, []int for ports) that structpb.NewStruct rejects — the
// round-trip coerces them to the []any / float64 forms structpb accepts (and
// that the TUI already expects on the wire).
func toStruct(m map[string]any) *structpb.Struct {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var norm map[string]any
	if err := json.Unmarshal(b, &norm); err != nil {
		return nil
	}
	s, err := structpb.NewStruct(norm)
	if err != nil {
		return nil
	}
	return s
}

// ---- unary RPCs -----------------------------------------------------------

func (s *kotoServer) Spawn(_ context.Context, r *pb.SpawnReq) (*pb.SpawnResp, error) {
	if !validGroupName(r.Group) {
		return &pb.SpawnResp{Error: "invalid group name (must match [A-Za-z0-9][A-Za-z0-9_-]{0,31})"}, nil
	}
	if r.Provider != "" && r.Provider != "claudesdk" && r.Provider != "venice" {
		return &pb.SpawnResp{Error: "provider must be claudesdk or venice"}, nil
	}
	if r.Size != "" {
		if _, ok := fcSizePresets[r.Size]; !ok {
			return &pb.SpawnResp{Error: "size must be small, medium, large, or xlarge"}, nil
		}
	}
	if r.Provider != "" || r.Model != "" || r.Size != "" {
		if err := seedSpawnConfig(r.Group, r.Provider, r.Model, r.Size); err != nil {
			return &pb.SpawnResp{Error: err.Error()}, nil
		}
	}
	port, err := ensure(r.Group, r.Main)
	if err != nil {
		return &pb.SpawnResp{Error: err.Error()}, nil
	}
	return &pb.SpawnResp{Ok: true, Port: int32(port)}, nil
}

func (s *kotoServer) Send(_ context.Context, r *pb.SendReq) (*pb.BaseResp, error) {
	// Enqueue and return immediately — do NOT block on the turn. A turn runs for
	// up to turnWaitTimeout (25m); a unary RPC blocking that long blows any
	// client deadline (the TUI wraps every call in 30s, surfacing the long turn
	// as a spurious DeadlineExceeded) and pins an HTTP/2 stream the whole time.
	// Turn lifecycle (prompt/stream/done/turn_end) reaches clients over the
	// Subscribe stream; the only thing a caller needs synchronously is whether
	// the message made it into the bounded queue (overflow = backpressure),
	// which enqueueSend reports immediately. Matches the ctl/scheduler producers.
	//
	// Attachments are resolved HERE, before enqueueSend, so the queue still
	// carries a plain TEXT turn (queue.go / sendNow / the FIFO protocol stay
	// attachment-unaware): an image is saved to the workspace and referenced
	// inline. processAttachments runs synchronously (image save is a file write)
	// so a failure — oversize, or an audio attachment, which is no longer
	// supported since the whisper/DooD path was removed — surfaces in-band on
	// this RPC instead of silently dropping the turn. We only take the
	// attachment path when bytes are present, leaving the fast path untouched.
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	session, err := normalizeSession(r.Session)
	if err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	msg := r.Msg
	if len(r.GetImage()) > 0 || len(r.GetAudio()) > 0 {
		m, err := processAttachments(r)
		if err != nil {
			return &pb.BaseResp{Error: err.Error()}, nil
		}
		msg = m
	}
	if _, err := enqueueSend(r.Group, session, msg); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	// Register after a successful enqueue so the name shows up in
	// GroupInfo.sessions even before its first turn completes.
	registerSession(r.Group, session)
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) List(_ context.Context, _ *pb.ListReq) (*pb.ListResp, error) {
	groups := map[string]*pb.GroupInfo{}
	for g, gi := range listGroups() {
		groups[g] = toPBGroupInfo(gi)
	}
	return &pb.ListResp{Ok: true, Groups: groups}, nil
}

func (s *kotoServer) Stop(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	stopGroup(r.Group)
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) Interrupt(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	if err := interruptAgent(r.Group); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) Destroy(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	br := destroy(r.Group)
	return &pb.BaseResp{Ok: br.OK, Error: br.Error}, nil
}

func (s *kotoServer) Restart(_ context.Context, r *pb.GroupReq) (*pb.SpawnResp, error) {
	if !validGroupName(r.Group) {
		return &pb.SpawnResp{Error: "invalid group name"}, nil
	}
	port, err := restart(r.Group)
	if err != nil {
		return &pb.SpawnResp{Error: err.Error()}, nil
	}
	return &pb.SpawnResp{Ok: true, Port: int32(port)}, nil
}

func (s *kotoServer) History(_ context.Context, r *pb.HistoryReq) (*pb.HistoryResp, error) {
	if !validGroupName(r.Group) {
		return &pb.HistoryResp{Error: "invalid group name"}, nil
	}
	evs, more := readHistory(r.Group, int(r.Limit), r.Before)
	out := make([]*pb.Event, len(evs))
	for i := range evs {
		out[i] = toPBEvent(sanitizeEvent(evs[i]))
	}
	return &pb.HistoryResp{Ok: true, Events: out, More: more}, nil
}

func (s *kotoServer) Config(_ context.Context, r *pb.ConfigReq) (*pb.ConfigResp, error) {
	if !validGroupName(r.Group) {
		return &pb.ConfigResp{Error: "invalid group name"}, nil
	}
	resp := configCmd(fromPBConfigReq(r))
	return &pb.ConfigResp{Ok: resp.OK, Error: resp.Error, Config: toStruct(resp.Config)}, nil
}

func (s *kotoServer) Metrics(_ context.Context, r *pb.MetricsReq) (*pb.MetricsResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.MetricsResp{Error: "invalid group name"}, nil
	}
	out := &pb.MetricsResp{Ok: true, GlobalMetric: toStruct(latestMetricAny())}
	if r.Group != "" {
		out.Metric = toStruct(latestMetric(r.Group))
	}
	return out, nil
}

// Jobs is the fresh "ls" of a group's background jobs — it re-reads the
// guest (bounded by fcExec's timeout) rather than serving the watch-loop
// mirror, so `koto ctl jobs` never shows stale state. Group "" sweeps every
// running group (ACL-wise that's the read-across-all form, like global
// metrics). Stopped groups simply contribute nothing: their job dirs are
// unreachable inside workspace.img, and observability must not boot VMs.
func (s *kotoServer) Jobs(_ context.Context, r *pb.JobsReq) (*pb.JobsResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.JobsResp{Error: "invalid group name"}, nil
	}
	groups := []string{r.Group}
	if r.Group == "" {
		groups = groups[:0]
		for g := range readGroups() {
			groups = append(groups, g)
		}
		sort.Strings(groups)
	}
	out := &pb.JobsResp{Ok: true}
	for _, g := range groups {
		for _, j := range refreshJobs(g) {
			j.Group = g
			out.Jobs = append(out.Jobs, toPBJobInfo(j))
		}
	}
	return out, nil
}

// JobLogs returns one job's metadata plus a tail of its combined output.
// Output is sanitized like chat events — it is attacker-influenceable bytes
// headed for a terminal.
func (s *kotoServer) JobLogs(_ context.Context, r *pb.JobLogsReq) (*pb.JobLogsResp, error) {
	if !validGroupName(r.Group) {
		return &pb.JobLogsResp{Error: "invalid group name"}, nil
	}
	if !fcRunning(r.Group) {
		return &pb.JobLogsResp{Error: "group not running"}, nil
	}
	res, err := fcJobLogs(r.Group, r.Id, r.Tail)
	if err != nil {
		return &pb.JobLogsResp{Error: err.Error()}, nil
	}
	j := res.Job
	j.Group = r.Group
	return &pb.JobLogsResp{
		Ok:        true,
		Job:       toPBJobInfo(j),
		Output:    sanitize(res.Output),
		Truncated: res.Truncated,
	}, nil
}

// JobTail streams one job's combined output live: `tail -c <window> -f` in
// the guest over the agent's exec_stream (client cancel closes the vsock
// conn, which makes the guest agent kill the tail — same lifecycle as
// RunScript). Output is line-buffered and sanitized per line before it goes
// to the client: job output is attacker-influenceable bytes headed for a
// terminal, unlike RunScript's deliberately-raw operator channel. The
// stream keeps following until the client cancels — a finished job simply
// stops producing frames after the initial window.
func (s *kotoServer) JobTail(r *pb.JobTailReq, stream pb.Koto_JobTailServer) error {
	fail := func(msg string) error {
		return stream.Send(&pb.ScriptEvent{Event: "error", Error: msg})
	}
	if !validGroupName(r.Group) {
		return fail("invalid group name")
	}
	if !jobIDRE.MatchString(r.Id) {
		return fail("invalid job id")
	}
	if !fcRunning(r.Group) {
		return fail("group not running")
	}
	window := r.Tail
	if window <= 0 {
		window = jobLogsMaxTail
	}
	if window > jobLogsMaxTail {
		window = jobLogsMaxTail
	}
	path := "/workspace/.cs/jobs/" + r.Id + "/out"
	rc, err := fcExecStream(r.Group, fmt.Sprintf("exec tail -c %d -f %s", window, path))
	if err != nil {
		return fail(err.Error())
	}
	defer rc.Close()
	ctx := stream.Context()
	go func() { // client gone → close the vsock conn → guest kills the tail
		<-ctx.Done()
		rc.Close()
	}()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	// Parsed mode runs each line through the same logParser grammar the group
	// log tailer uses (a cs-subagent job's out carries that framing), sending
	// typed `event` frames the client renders exactly like chat. Raw mode
	// keeps the original sanitized-line chunks. The tail window can open
	// mid-block; the grammar degrades gracefully there (stray close markers
	// are swallowed, body-before-window is simply absent).
	lp := logParser{}
	for sc.Scan() {
		if r.Parsed {
			for _, ev := range lp.feedLine(sc.Text()) {
				if serr := stream.Send(&pb.ScriptEvent{Event: "event", Parsed: toPBEvent(sanitizeEvent(ev))}); serr != nil {
					return serr
				}
			}
			continue
		}
		line := sanitize(sc.Text())
		if serr := stream.Send(&pb.ScriptEvent{Event: "data", Chunk: []byte(line + "\n")}); serr != nil {
			return serr
		}
	}
	if ctx.Err() != nil {
		return ctx.Err() // client cancelled — the normal end
	}
	// Guest side went away (VM stopped/restarted mid-tail).
	return stream.Send(&pb.ScriptEvent{Event: "end"})
}

func (s *kotoServer) Clear(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	br := clearCmd(groupReq{Group: r.Group, Session: r.Session})
	return &pb.BaseResp{Ok: br.OK, Error: br.Error}, nil
}

func (s *kotoServer) Skills(_ context.Context, r *pb.SkillListReq) (*pb.SkillsResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.SkillsResp{Error: "invalid group name"}, nil
	}
	resp := skillListCmd(skillListReq{Group: r.Group})
	out := make([]*pb.SkillItem, len(resp.Skills))
	for i := range resp.Skills {
		out[i] = toPBSkillItem(resp.Skills[i])
	}
	return &pb.SkillsResp{Ok: resp.OK, Error: resp.Error, Skills: out}, nil
}

func (s *kotoServer) SkillNew(_ context.Context, r *pb.SkillNewReq) (*pb.SkillNewResp, error) {
	resp := skillNewCmd(skillNewReq{Name: r.Name})
	return &pb.SkillNewResp{Ok: resp.OK, Error: resp.Error, Path: resp.Path}, nil
}

func (s *kotoServer) SkillRead(_ context.Context, r *pb.SkillReadReq) (*pb.SkillReadResp, error) {
	resp := skillReadCmd(skillReadReq{Name: r.Name})
	return &pb.SkillReadResp{Ok: resp.OK, Error: resp.Error, Name: resp.Name, Content: resp.Content}, nil
}

func (s *kotoServer) SchedAdd(_ context.Context, r *pb.SchedAddReq) (*pb.SchedAddResp, error) {
	if !validGroupName(r.Group) {
		return &pb.SchedAddResp{Error: "invalid group name"}, nil
	}
	it, err := addSched(r.Group, r.Cron, r.Msg)
	if err != nil {
		return &pb.SchedAddResp{Error: err.Error()}, nil
	}
	return &pb.SchedAddResp{Ok: true, Item: toPBScheduleItem(it)}, nil
}

func (s *kotoServer) SchedList(_ context.Context, r *pb.SchedListReq) (*pb.SchedListResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.SchedListResp{Error: "invalid group name"}, nil
	}
	items := listSched(r.Group)
	out := make([]*pb.ScheduleItem, len(items))
	for i := range items {
		out[i] = toPBScheduleItem(items[i])
	}
	return &pb.SchedListResp{Ok: true, Schedules: out}, nil
}

func (s *kotoServer) SchedDel(_ context.Context, r *pb.SchedIDReq) (*pb.BaseResp, error) {
	if err := delSched(r.Id); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) SchedToggle(_ context.Context, r *pb.SchedToggleReq) (*pb.BaseResp, error) {
	if _, err := toggleSched(r.Id, r.Enabled); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) SchedRun(_ context.Context, r *pb.SchedIDReq) (*pb.BaseResp, error) {
	if err := runSchedNow(r.Id); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

// ---- ACL management (acl_* verbs are hardcoded admin-only in acl.go) ------

func (s *kotoServer) AclGet(_ context.Context, _ *pb.AclGetReq) (*pb.AclResp, error) {
	doc, err := readACLDoc()
	if err != nil {
		return &pb.AclResp{Error: err.Error()}, nil
	}
	return &pb.AclResp{Ok: true, Acl: toStruct(doc)}, nil
}

func (s *kotoServer) AclSetRole(_ context.Context, r *pb.AclSetRoleReq) (*pb.AclResp, error) {
	var grants map[string]any
	if r.Grants != nil {
		grants = r.Grants.AsMap()
	}
	doc, err := aclSetRoleCmd(r.Role, grants)
	if err != nil {
		return &pb.AclResp{Error: err.Error()}, nil
	}
	emitLogf("acl", "info", "role %s updated", r.Role)
	return &pb.AclResp{Ok: true, Acl: toStruct(doc)}, nil
}

func (s *kotoServer) AclDelRole(_ context.Context, r *pb.AclDelRoleReq) (*pb.AclResp, error) {
	doc, err := aclDelRoleCmd(r.Role)
	if err != nil {
		return &pb.AclResp{Error: err.Error()}, nil
	}
	emitLogf("acl", "info", "role %s deleted", r.Role)
	return &pb.AclResp{Ok: true, Acl: toStruct(doc)}, nil
}

// ---- server-streaming RPCs ------------------------------------------------

// RunScript runs a POSIX sh script in the group's microVM as the guest
// worker user (node, uid 1000) and streams combined stdout+stderr back live.
// Hardcoded admin-only (adminOnlyVerbs, acl.go). ensure() first so it works
// against a stopped group, same as Send. Failures are in-band `error` frames
// (gRPC status stays reserved for transport/auth, matching the unary verbs);
// client cancel closes the vsock conn, which makes the guest agent SIGKILL
// the script's process group. Output is raw — the caller asked for this
// script's bytes, so no sanitizer (unlike agent streams rendered in the TUI).
func (s *kotoServer) RunScript(r *pb.RunScriptReq, stream pb.Koto_RunScriptServer) error {
	fail := func(msg string) error {
		return stream.Send(&pb.ScriptEvent{Event: "error", Error: msg})
	}
	if !validGroupName(r.Group) {
		return fail("invalid group name")
	}
	if strings.TrimSpace(r.Script) == "" {
		return fail("empty script")
	}
	if _, err := ensure(r.Group, r.Group == "main"); err != nil {
		return fail(err.Error())
	}
	emitLogfG("exec", r.Group, "info", "[%s] runscript start (%d-byte script)", r.Group, len(r.Script))
	c, err := fcRunScriptDial(r.Group, r.Script)
	if err != nil {
		return fail(err.Error())
	}
	defer c.Close()
	ctx := stream.Context()
	go func() { // client gone → close the vsock conn → guest kills the script
		<-ctx.Done()
		c.Close()
	}()
	for {
		typ, payload, err := fcReadScriptFrame(c)
		if err != nil {
			// Frame read failed: either the client cancelled (ctx.Done closed
			// c out from under us) or the VM died mid-run. The former is not
			// an error to report; the latter is, but the stream is already
			// going away — a best-effort error frame covers both.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			emitLogfG("exec", r.Group, "warn", "[%s] runscript transport: %v", r.Group, err)
			return fail("guest connection lost: " + err.Error())
		}
		switch typ {
		case 'D':
			if serr := stream.Send(&pb.ScriptEvent{Event: "data", Chunk: payload}); serr != nil {
				return serr
			}
		case 'E':
			emitLogfG("exec", r.Group, "info", "[%s] runscript end", r.Group)
			return stream.Send(&pb.ScriptEvent{Event: "end"})
		case 'X':
			emitLogfG("exec", r.Group, "warn", "[%s] runscript error: %s", r.Group, string(payload))
			return fail(string(payload))
		}
	}
}

// AttachShell is the first bidi-streaming RPC in this codebase: the client
// opens with one ShellInput{open:...}, then sends any number of
// data/resize/close messages, while receiving a stream of ShellFrame (raw
// pty output). An ordinary per-role grantable verb (attach_shell) — NOT
// hardcoded admin-only like RunScript, since this is meant to be
// agent-cooperative (the group's own agent can attach the same tmux session
// via its Bash tool), not an operator bypass. Output is raw, same as
// RunScript's ScriptEvent.Chunk — no sanitizer — safe here only because the
// TUI renders it through its own terminal-emulator library rather than
// writing it straight to a real terminal (see the shared-shell plan doc).
//
// Every ShellInput carries `group`, and daemon/auth.go's aclStream.RecvMsg
// re-authorizes each one independently — so a mid-session ACL revocation
// takes effect on the very next frame, not just at connect time. This
// handler additionally pins `group` to whatever the `open` message named:
// later messages naming a different group are a protocol error (the vsock
// conn was dialed once, to one group), even though ACL would independently
// authorize that other group on its own.
func (s *kotoServer) AttachShell(stream pb.Koto_AttachShellServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first message must be `open`")
	}
	group := first.Group
	if !validGroupName(group) {
		return status.Error(codes.InvalidArgument, "invalid group name")
	}
	session := open.Session
	if session == "" {
		session = "koto-shell"
	}
	if _, err := ensure(group, group == "main"); err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	emitLogfG("shell", group, "info", "[%s] attach session=%s cols=%d rows=%d", group, session, open.Cols, open.Rows)

	c, err := fcShellDial(group, session, uint16(open.Cols), uint16(open.Rows))
	if err != nil {
		return stream.Send(&pb.ShellFrame{Event: "error", Error: err.Error()})
	}
	defer c.Close()

	ctx := stream.Context()
	go func() { // client gone → close the vsock conn → guest detaches this client (SIGHUP, not the tmux session)
		<-ctx.Done()
		c.Close()
	}()

	// guest -> client: pty output / end / error frames.
	go func() {
		for {
			typ, payload, ferr := fcReadShellFrame(c)
			if ferr != nil {
				return
			}
			switch typ {
			case 'D':
				if stream.Send(&pb.ShellFrame{Event: "data", Chunk: payload}) != nil {
					return
				}
			case 'E':
				emitLogfG("shell", group, "info", "[%s] session=%s detached", group, session)
				_ = stream.Send(&pb.ShellFrame{Event: "end"})
				return
			case 'X':
				emitLogfG("shell", group, "warn", "[%s] session=%s error: %s", group, session, string(payload))
				_ = stream.Send(&pb.ShellFrame{Event: "error", Error: string(payload)})
				return
			}
		}
	}()

	// client -> guest: keystrokes / pastes / resizes. Every Recv() here has
	// already passed aclCheck (auth.go's aclStream) against this message's
	// own Group before this loop ever sees it.
	for {
		in, rerr := stream.Recv()
		if rerr != nil {
			return nil // client closed/cancelled — normal end
		}
		if in.Group != group {
			return status.Error(codes.InvalidArgument, "group changed mid-stream")
		}
		switch v := in.Input.(type) {
		case *pb.ShellInput_Data:
			if fcWriteShellFrame(c, 'I', v.Data) != nil {
				return nil
			}
		case *pb.ShellInput_Resize:
			payload := make([]byte, 4)
			binary.BigEndian.PutUint16(payload[0:2], uint16(v.Resize.Cols))
			binary.BigEndian.PutUint16(payload[2:4], uint16(v.Resize.Rows))
			if fcWriteShellFrame(c, 'R', payload) != nil {
				return nil
			}
		case *pb.ShellInput_Close:
			return nil
		}
	}
}

func (s *kotoServer) SubscribeGroup(r *pb.SubscribeReq, stream pb.Koto_SubscribeGroupServer) error {
	g := r.GetGroup()
	if !validGroupName(g) {
		return status.Error(codes.InvalidArgument, "invalid group name")
	}
	ensureTail(g)
	sub := &groupSub{ch: make(chan *pb.Event, 256), done: make(chan struct{})}
	// Snapshot the resume replay and register under ONE lock acquisition:
	// anything emitted after the snapshot lands in sub.ch, so the
	// replay/live boundary has neither a gap nor a duplicate.
	subsLock.Lock()
	var replay []*pb.Event
	if since := r.GetSinceSeq(); since > 0 {
		replay = replayFrom(g, since)
	}
	subscribers[g] = append(subscribers[g], sub)
	subsLock.Unlock()
	defer func() {
		subsLock.Lock()
		kept := subscribers[g][:0]
		for _, x := range subscribers[g] {
			if x != sub {
				kept = append(kept, x)
			}
		}
		subscribers[g] = kept
		subsLock.Unlock()
	}()
	for _, ev := range replay {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	ctx := stream.Context()
	for {
		select {
		case ev := <-sub.ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		case <-sub.done: // group destroyed, or this subscriber overflowed (resume via since_seq)
			return nil
		case <-ctx.Done(): // client disconnected
			return nil
		}
	}
}

func (s *kotoServer) WatchState(_ *pb.WatchReq, stream pb.Koto_WatchStateServer) error {
	// Compute the initial frame BEFORE registering: the watcher must not wait
	// up to a full tick for its first snapshot. A state change racing between
	// this compute and the registration is not lost — lastSent still holds
	// the pre-change hash, so the next tick pushes the newer frame.
	gs := listGroups()
	sub := &stateSub{ch: make(chan *pb.StateFrame, 4), lastSent: stateHash(gs)}
	stateSubsLock.Lock()
	stateSubs = append(stateSubs, sub)
	stateSubsLock.Unlock()
	defer func() {
		stateSubsLock.Lock()
		kept := stateSubs[:0]
		for _, x := range stateSubs {
			if x != sub {
				kept = append(kept, x)
			}
		}
		stateSubs = kept
		stateSubsLock.Unlock()
	}()
	if err := stream.Send(toStateFrame(gs)); err != nil {
		return err
	}
	ctx := stream.Context()
	for {
		select {
		case f := <-sub.ch:
			if err := stream.Send(f); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *kotoServer) SubscribeLogs(_ *pb.LogsReq, stream pb.Koto_SubscribeLogsServer) error {
	sub := &logSub{ch: make(chan *pb.LogEvent, 256)}
	// Snapshot the ring and register under one lock so no frame is dropped or
	// duplicated across the replay/live boundary.
	logSubsLock.Lock()
	ring := append([]*pb.LogEvent(nil), logRing...)
	logSubs = append(logSubs, sub)
	logSubsLock.Unlock()
	defer func() {
		logSubsLock.Lock()
		kept := logSubs[:0]
		for _, x := range logSubs {
			if x != sub {
				kept = append(kept, x)
			}
		}
		logSubs = kept
		logSubsLock.Unlock()
	}()
	for _, ev := range ring {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	ctx := stream.Context()
	for {
		select {
		case ev := <-sub.ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}
