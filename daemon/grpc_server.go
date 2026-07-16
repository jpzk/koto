package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"clawson-protocol/pb"

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

// clawsonServer implements pb.ClawsonServer. Each method is a thin wrapper over
// the existing daemon helpers (ensure/send/listGroups/configCmd/...): it
// converts the protobuf request to the helper's native types, calls the
// unchanged helper, and converts the result back to protobuf. Application-level
// failures are returned in-band via the response's ok/error fields with a nil
// gRPC error — matching the old {ok:false,error} JSON contract the clients read.
// gRPC status codes are reserved for transport/auth faults (see auth.go).
type clawsonServer struct {
	pb.UnimplementedClawsonServer
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
	}
}

func toPBGroupInfo(gi GroupInfo) *pb.GroupInfo {
	return &pb.GroupInfo{
		Port:     int32(gi.Port),
		Running:  gi.Running,
		Provider: gi.Provider,
		Model:    gi.Model,
		Effort:   gi.Effort,
		Stalled:  gi.Stalled,
		Queued:   int32(gi.Queued),
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

func (s *clawsonServer) Spawn(_ context.Context, r *pb.SpawnReq) (*pb.SpawnResp, error) {
	if !validGroupName(r.Group) {
		return &pb.SpawnResp{Error: "invalid group name (must match [A-Za-z0-9][A-Za-z0-9_-]{0,31})"}, nil
	}
	if r.Provider != "" && r.Provider != "claudesdk" && r.Provider != "venice" {
		return &pb.SpawnResp{Error: "provider must be claudesdk or venice"}, nil
	}
	if r.Size != "" {
		if _, ok := fcSizePresets[r.Size]; !ok {
			return &pb.SpawnResp{Error: "size must be small, medium, or large"}, nil
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

func (s *clawsonServer) Send(_ context.Context, r *pb.SendReq) (*pb.BaseResp, error) {
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
	msg := r.Msg
	if len(r.GetImage()) > 0 || len(r.GetAudio()) > 0 {
		m, err := processAttachments(r)
		if err != nil {
			return &pb.BaseResp{Error: err.Error()}, nil
		}
		msg = m
	}
	if _, err := enqueueSend(r.Group, msg); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *clawsonServer) List(_ context.Context, _ *pb.ListReq) (*pb.ListResp, error) {
	groups := map[string]*pb.GroupInfo{}
	for g, gi := range listGroups() {
		groups[g] = toPBGroupInfo(gi)
	}
	return &pb.ListResp{Ok: true, Groups: groups}, nil
}

func (s *clawsonServer) Stop(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	stopGroup(r.Group)
	return &pb.BaseResp{Ok: true}, nil
}

func (s *clawsonServer) Interrupt(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	if err := interruptAgent(r.Group); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *clawsonServer) Destroy(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	br := destroy(r.Group)
	return &pb.BaseResp{Ok: br.OK, Error: br.Error}, nil
}

func (s *clawsonServer) Restart(_ context.Context, r *pb.GroupReq) (*pb.SpawnResp, error) {
	if !validGroupName(r.Group) {
		return &pb.SpawnResp{Error: "invalid group name"}, nil
	}
	port, err := restart(r.Group)
	if err != nil {
		return &pb.SpawnResp{Error: err.Error()}, nil
	}
	return &pb.SpawnResp{Ok: true, Port: int32(port)}, nil
}

func (s *clawsonServer) History(_ context.Context, r *pb.HistoryReq) (*pb.HistoryResp, error) {
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

func (s *clawsonServer) Config(_ context.Context, r *pb.ConfigReq) (*pb.ConfigResp, error) {
	if !validGroupName(r.Group) {
		return &pb.ConfigResp{Error: "invalid group name"}, nil
	}
	resp := configCmd(fromPBConfigReq(r))
	return &pb.ConfigResp{Ok: resp.OK, Error: resp.Error, Config: toStruct(resp.Config)}, nil
}

func (s *clawsonServer) Metrics(_ context.Context, r *pb.MetricsReq) (*pb.MetricsResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.MetricsResp{Error: "invalid group name"}, nil
	}
	out := &pb.MetricsResp{Ok: true, GlobalMetric: toStruct(latestMetricAny())}
	if r.Group != "" {
		out.Metric = toStruct(latestMetric(r.Group))
	}
	return out, nil
}

func (s *clawsonServer) Clear(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	br := clearCmd(groupReq{Group: r.Group})
	return &pb.BaseResp{Ok: br.OK, Error: br.Error}, nil
}

func (s *clawsonServer) Skills(_ context.Context, r *pb.SkillListReq) (*pb.SkillsResp, error) {
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

func (s *clawsonServer) SkillNew(_ context.Context, r *pb.SkillNewReq) (*pb.SkillNewResp, error) {
	resp := skillNewCmd(skillNewReq{Name: r.Name})
	return &pb.SkillNewResp{Ok: resp.OK, Error: resp.Error, Path: resp.Path}, nil
}

func (s *clawsonServer) SkillRead(_ context.Context, r *pb.SkillReadReq) (*pb.SkillReadResp, error) {
	resp := skillReadCmd(skillReadReq{Name: r.Name})
	return &pb.SkillReadResp{Ok: resp.OK, Error: resp.Error, Name: resp.Name, Content: resp.Content}, nil
}

func (s *clawsonServer) SchedAdd(_ context.Context, r *pb.SchedAddReq) (*pb.SchedAddResp, error) {
	if !validGroupName(r.Group) {
		return &pb.SchedAddResp{Error: "invalid group name"}, nil
	}
	it, err := addSched(r.Group, r.Cron, r.Msg)
	if err != nil {
		return &pb.SchedAddResp{Error: err.Error()}, nil
	}
	return &pb.SchedAddResp{Ok: true, Item: toPBScheduleItem(it)}, nil
}

func (s *clawsonServer) SchedList(_ context.Context, r *pb.SchedListReq) (*pb.SchedListResp, error) {
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

func (s *clawsonServer) SchedDel(_ context.Context, r *pb.SchedIDReq) (*pb.BaseResp, error) {
	if err := delSched(r.Id); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *clawsonServer) SchedToggle(_ context.Context, r *pb.SchedToggleReq) (*pb.BaseResp, error) {
	if _, err := toggleSched(r.Id, r.Enabled); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *clawsonServer) SchedRun(_ context.Context, r *pb.SchedIDReq) (*pb.BaseResp, error) {
	if err := runSchedNow(r.Id); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

// ---- ACL management (acl_* verbs are hardcoded admin-only in acl.go) ------

func (s *clawsonServer) AclGet(_ context.Context, _ *pb.AclGetReq) (*pb.AclResp, error) {
	doc, err := readACLDoc()
	if err != nil {
		return &pb.AclResp{Error: err.Error()}, nil
	}
	return &pb.AclResp{Ok: true, Acl: toStruct(doc)}, nil
}

func (s *clawsonServer) AclSetRole(_ context.Context, r *pb.AclSetRoleReq) (*pb.AclResp, error) {
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

func (s *clawsonServer) AclDelRole(_ context.Context, r *pb.AclDelRoleReq) (*pb.AclResp, error) {
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
func (s *clawsonServer) RunScript(r *pb.RunScriptReq, stream pb.Clawson_RunScriptServer) error {
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
	emitLogf("exec", "info", "[%s] runscript start (%d-byte script)", r.Group, len(r.Script))
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
			emitLogf("exec", "warn", "[%s] runscript transport: %v", r.Group, err)
			return fail("guest connection lost: " + err.Error())
		}
		switch typ {
		case 'D':
			if serr := stream.Send(&pb.ScriptEvent{Event: "data", Chunk: payload}); serr != nil {
				return serr
			}
		case 'E':
			emitLogf("exec", "info", "[%s] runscript end", r.Group)
			return stream.Send(&pb.ScriptEvent{Event: "end"})
		case 'X':
			emitLogf("exec", "warn", "[%s] runscript error: %s", r.Group, string(payload))
			return fail(string(payload))
		}
	}
}

func (s *clawsonServer) SubscribeGroup(r *pb.SubscribeReq, stream pb.Clawson_SubscribeGroupServer) error {
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

func (s *clawsonServer) WatchState(_ *pb.WatchReq, stream pb.Clawson_WatchStateServer) error {
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

func (s *clawsonServer) SubscribeLogs(_ *pb.LogsReq, stream pb.Clawson_SubscribeLogsServer) error {
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
