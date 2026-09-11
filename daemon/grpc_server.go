package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

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
	// Every string here is rendered by a client, and several are written by
	// guests or by main through the ctl plane — one chokepoint, as
	// sanitizeEvent already is for the event stream (audit M9b).
	out := &pb.GroupInfo{
		Port:      int32(gi.Port),
		Running:   gi.Running,
		Provider:  sanitize(gi.Provider),
		Model:     sanitize(gi.Model),
		Effort:    sanitize(gi.Effort),
		Stalled:   gi.Stalled,
		Queued:    int32(gi.Queued),
		Sessions:  sanitizeAll(gi.Sessions),
		TokPerSec: gi.TokPerSec,
		Network:   sanitize(gi.Network),
		Root:      gi.Root,
	}
	for _, j := range gi.Jobs {
		out.Jobs = append(out.Jobs, toPBJobInfo(j))
	}
	return out
}

// sanitizeAll sanitizes every string of a slice (session names, etc).
func sanitizeAll(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = sanitize(s)
	}
	return out
}

// sanitizeAny sanitizes every string leaf of a decoded-JSON value (the
// config map: model/effort are validated on write, but a config.json edited
// on disk is not).
func sanitizeAny(v any) any {
	switch x := v.(type) {
	case string:
		return sanitize(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[sanitize(k)] = sanitizeAny(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = sanitizeAny(e)
		}
		return out
	}
	return v
}

// sanitizeConfig is sanitizeAny for the config map's concrete type.
func sanitizeConfig(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out, _ := sanitizeAny(m).(map[string]any)
	return out
}

func toPBJobInfo(j JobInfo) *pb.JobInfo {
	return &pb.JobInfo{
		Id: sanitize(j.ID), Session: sanitize(j.Session), Status: sanitize(j.Status), Rc: sanitize(j.RC),
		Cmd: sanitize(j.Cmd), Started: j.Started, OutSize: j.OutSize, Group: j.Group,
	}
}

func toPBGoalItem(it goalItem) *pb.GoalItem {
	return &pb.GoalItem{
		Id:            it.ID,
		Group:         it.Group,
		Name:          sanitize(it.Name),
		Text:          sanitize(it.Text),
		Criteria:      sanitize(it.Criteria),
		Plan:          it.Plan,
		Status:        sanitize(it.Status),
		Iteration:     int32(it.Iteration),
		MaxIterations: int32(it.MaxIterations),
		LastFeedback:  sanitize(it.LastFeedback), // guest-written (goal_verdict)
		DoneNote:      sanitize(it.DoneNote),     // guest-written (goal_done)
		PausedReason:  sanitize(it.PausedReason),
		CreatedAt:     it.CreatedAt,
		UpdatedAt:     it.UpdatedAt,
		CompletedAt:   it.CompletedAt,
	}
}

func toPBScheduleItem(it scheduleItem) *pb.ScheduleItem {
	return &pb.ScheduleItem{
		Id:          it.ID,
		Group:       it.Group,
		Cron:        sanitize(it.Cron),
		Msg:         sanitize(it.Msg), // guest-written (sched_add)
		Enabled:     it.Enabled,
		CreatedAt:   it.CreatedAt,
		LastFiredAt: it.LastFiredAt,
		NextDueAt:   it.NextDueAt,
	}
}

// fromPBConfigReq re-synthesizes the json.RawMessage tri-state (absent / clear /
// set) the existing applyConfig/isClear logic expects, from the protobuf
// presence (optional scalars). Absent => nil; clear => an
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
	return out
}

// toStruct converts a map[string]any to a protobuf Struct. It JSON-normalizes
// first because the config map carries native Go slices (applyConfig writes
// []int for ports) that structpb.NewStruct rejects — the
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
	// Same cap the ctl plane's spawn verb enforces. It lived only there,
	// because the only untrusted spawner was a compromised main — but `spawn`
	// is an ordinary grantable verb, so a non-admin role holding it on "*" had
	// no bound at all. Re-spawning a group that already exists does not grow
	// the set and stays allowed.
	if existing := readGroups(); len(existing) >= ctlMaxSpawn {
		if _, already := existing[r.Group]; !already {
			return &pb.SpawnResp{Error: fmt.Sprintf("spawn cap reached (%d groups)", ctlMaxSpawn)}, nil
		}
	}
	if r.Provider != "" || r.Model != "" || r.Size != "" {
		if err := seedSpawnConfig(r.Group, r.Provider, r.Model, r.Size); err != nil {
			return &pb.SpawnResp{Error: err.Error()}, nil
		}
	}
	port, err := spawnEnsure(r.Group, r.Main)
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
	if isReservedSession(session) {
		return &pb.BaseResp{Error: "session " + session + " is reserved for the goal loop"}, nil
	}
	msg := r.Msg
	staged := false
	if len(r.GetImage()) > 0 || len(r.GetAudio()) > 0 {
		m, err := processAttachments(r)
		if err != nil {
			return &pb.BaseResp{Error: err.Error()}, nil
		}
		msg, staged = m, true
	}
	if _, err := enqueueSend(r.Group, session, msg); err != nil {
		// Roll the staging back. The file was written before the queue got a
		// say, so a refused send used to leave it behind — charged against the
		// group's pending-upload quota with no turn that will ever claim it
		// (audit M36).
		if staged {
			for _, f := range fcTurnUploads(r.Group, msg) {
				_ = os.Remove(filepath.Join(vol(r.Group), ".cs", "uploads", f))
			}
		}
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	// Register after a successful enqueue so the name shows up in
	// GroupInfo.sessions even before its first turn completes.
	registerSession(r.Group, session)
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) List(ctx context.Context, _ *pb.ListReq) (*pb.ListResp, error) {
	groups := map[string]*pb.GroupInfo{}
	for g, gi := range projectGroups(ctx, "list", listGroups()) {
		groups[g] = toPBGroupInfo(gi)
	}
	return &pb.ListResp{Ok: true, Groups: groups}, nil
}

// projectGroups narrows an aggregate snapshot to what the caller may see.
// `list` and `watch_state` are verb-only in the ACL, so the interceptor
// authorized the CALL and the handler then serialized the whole fleet — a role
// confined to one group by its other grants still enumerated every group, and
// with it every group's job records: command text, session, rc, timings,
// output size. The projection uses the caller's own grant for this verb
// (visibleTargets), which acl.json could always express and which every seeded
// role writes as "*", so a broad grant is unchanged.
//
// Jobs are narrowed a second time, by the `jobs` grant: seeing that a group
// exists and reading the command lines running inside it are different asks,
// and `jobs` is the verb that says the latter.
func projectGroups(ctx context.Context, verb string, gs map[string]GroupInfo) map[string]GroupInfo {
	id := identityOf(ctx)
	if id.Name == "" {
		return gs // no interceptor ran (in-process caller); nothing to project
	}
	acl := loadACL()
	vis := visibleTargets(acl, id.Roles, verb)
	jobsVis := visibleTargets(acl, id.Roles, "jobs")
	out := make(map[string]GroupInfo, len(gs))
	for g, gi := range gs {
		if !vis.covers(g) {
			continue
		}
		if len(gi.Jobs) > 0 && !jobsVis.covers(g) {
			gi.Jobs = nil
		}
		out[g] = gi
	}
	return out
}

func (s *kotoServer) Stop(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	stopGroup(r.Group)
	return &pb.BaseResp{Ok: true}, nil
}

// Interrupt aborts one CONVERSATION's turn — by default the caller's session.
// It deliberately cannot signal the whole group: up to groupSlots turns run at
// once, and esc in the TUI means "stop answering me", not "abort everything
// running in this VM". Goal sessions are excluded outright; abandoning an
// iteration is /goals interrupt, which pauses the goal as well as signaling it.
func (s *kotoServer) Interrupt(_ context.Context, r *pb.GroupReq) (*pb.BaseResp, error) {
	if !validGroupName(r.Group) {
		return &pb.BaseResp{Error: "invalid group name"}, nil
	}
	session, err := normalizeSession(r.Session)
	if err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	if isReservedSession(session) {
		return &pb.BaseResp{Error: "goal sessions are follow-only — use /goals interrupt"}, nil
	}
	if !sessionBusy(r.Group, session) {
		return &pb.BaseResp{Error: "no turn in flight in session '" + sessionMarkerName(session) + "'"}, nil
	}
	// Arm the discard FIRST: from this point the turn is dead even if its
	// worker hasn't spawned yet (VM booting, delivery in flight) — sendNow
	// aborts a pre-delivery turn and keeps re-signaling a delivered one until
	// it dies (send.go abort loop), then the queue advances to the next
	// prompt. The immediate SIGINT below is just the fast path for the common
	// case; "no agent process yet" is no longer a failed interrupt.
	if !requestTurnCancel(r.Group, session) {
		// The turn retired between the busy check and here.
		return &pb.BaseResp{Error: "no turn in flight in session '" + sessionMarkerName(session) + "'"}, nil
	}
	if err := interruptAgent(r.Group, session); err != nil {
		emitLogfG("send", r.Group, "info", "interrupt group=%s session=%s: %v (cancel armed; abort loop takes over)",
			r.Group, sessionMarkerName(session), err)
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
	return &pb.ConfigResp{Ok: resp.OK, Error: resp.Error, Config: toStruct(sanitizeConfig(resp.Config))}, nil
}

func (s *kotoServer) Metrics(ctx context.Context, r *pb.MetricsReq) (*pb.MetricsResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.MetricsResp{Error: "invalid group name"}, nil
	}
	out := &pb.MetricsResp{Ok: true}
	// GlobalMetric is the newest record from ANY group — path, status, request
	// id, token counts, rate-limit and provider-org metadata — and it used to
	// ride along with every scoped answer. The ACL had done its job: it
	// checked `metrics` against r.Group. This field simply ignored the target,
	// so asking about a group you may see returned another group's newest
	// request (audit M30). It is a global read, so it needs the "*" grant the
	// untargeted form of this verb already needs.
	if id := identityOf(ctx); id.Name == "" || visibleTargets(loadACL(), id.Roles, "metrics").any {
		out.GlobalMetric = toStruct(latestMetricAny())
	}
	if r.Group != "" {
		out.Metric = toStruct(latestMetric(r.Group))
	}
	return out, nil
}

// Resources reports host-side fleet resource consumption. Unlike Jobs it
// serves the collector's cached samples and never touches a guest, so it is
// cheap, cannot block on a wedged VM, and stays truthful for groups that are
// stopped or read-only. See daemon/resources.go.
func (s *kotoServer) Resources(_ context.Context, _ *pb.ResourcesReq) (*pb.ResourcesResp, error) {
	groups, host := resourcesSnapshot()
	out := &pb.ResourcesResp{
		Ok: true,
		Host: &pb.HostResources{
			FsTotalBytes:     host.FSTotalBytes,
			FsFreeBytes:      host.FSFreeBytes,
			AllocTotalBytes:  host.AllocTotalBytes,
			ProvisionedBytes: host.ProvisionedBytes,
			Groups:           host.Groups,
			RunningGroups:    host.RunningGroups,
			MemCapMib:        host.MemCapMiB,
			MemCommittedMib:  host.MemCommittedMiB,
			MemHostTotalMib:  host.MemHostTotalMiB,
		},
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, &pb.GroupResources{
			Group:               g.Group,
			Running:             g.Running,
			AllocBytes:          g.AllocBytes,
			DeclaredBytes:       g.DeclaredBytes,
			GrowthBytesPerHour:  g.GrowthPerHour,
			GrowthSpanSeconds:   g.GrowthSpanSecs,
			RssBytes:            g.RSSBytes,
			CpuPct:              g.CPUPct,
			Vcpus:               g.Vcpus,
			MemMib:              g.MemMiB,
			GuestMemTotalBytes:  g.GuestMemTotal,
			GuestMemAvailBytes:  g.GuestMemAvail,
			GuestDiskTotalBytes: g.GuestDiskTotal,
			GuestDiskAvailBytes: g.GuestDiskAvail,
			GuestDiskUsedBytes:  g.GuestDiskUsed,
			MemCommittedMib:     g.MemCommittedMiB,
		})
	}
	return out, nil
}

// Jobs is the fresh "ls" of a group's background jobs — it re-reads the
// guest (bounded by fcExec's timeout) rather than serving the watch-loop
// mirror, so `koto ctl jobs` never shows stale state. Group "" sweeps every
// running group (ACL-wise that's the read-across-all form, like global
// metrics). Stopped groups simply contribute nothing: their job dirs are
// unreachable inside workspace.img, and observability must not boot VMs.
// jobQueryMaxGlobal bounds concurrent job-observability guest calls. Jobs and
// JobLogs each cost a host→guest connection, a guest shell and a response
// buffer, and the per-call timeout bounds one of them but not how many (audit
// M80). refreshJobs additionally shares the background refresher's in-flight
// marker now, so N concurrent callers asking about the SAME group cost one
// exec; this bounds N spread across DIFFERENT groups.
const jobQueryMaxGlobal = 16

var jobQuerySem = make(chan struct{}, jobQueryMaxGlobal)

func jobQueryAdmit() bool {
	select {
	case jobQuerySem <- struct{}{}:
		return true
	default:
		return false
	}
}

func jobQueryRelease() { <-jobQuerySem }

func (s *kotoServer) Jobs(_ context.Context, r *pb.JobsReq) (*pb.JobsResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.JobsResp{Error: "invalid group name"}, nil
	}
	if !jobQueryAdmit() {
		return &pb.JobsResp{Error: "too many concurrent job queries; retry shortly"}, nil
	}
	defer jobQueryRelease()
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
	if !jobQueryAdmit() {
		return &pb.JobLogsResp{Error: "too many concurrent job queries; retry shortly"}, nil
	}
	defer jobQueryRelease()
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
// Live tails are bounded per group and fleet-wide (audit M78). Each one costs
// a daemon goroutine, an HTTP/2 stream, a vsock connection and a `tail -f`
// PROCESS in the guest, and cleanup is tied to the RPC context ending — so an
// authorized reader reopening streams for a known job accumulated all four.
// The guest side is additionally capped by fcMaxConnsPerGroup, but that cap is
// shared with the group's ctl and turn channels, and having job tails be what
// exhausts it would take the group's control plane down with them.
//
// The numbers are an exhaustion backstop, not a scheduler: the TUI opens ONE
// tail per hovered row, so even several attached operators stay far below.
const (
	jobTailMaxPerGroup = 8
	jobTailMaxGlobal   = 64
)

var (
	jobTailMu    sync.Mutex
	jobTailCount = map[string]int{}
	jobTailTotal int
)

func jobTailAdmit(g string) bool {
	jobTailMu.Lock()
	defer jobTailMu.Unlock()
	if jobTailTotal >= jobTailMaxGlobal || jobTailCount[g] >= jobTailMaxPerGroup {
		return false
	}
	jobTailCount[g]++
	jobTailTotal++
	return true
}

func jobTailRelease(g string) {
	jobTailMu.Lock()
	defer jobTailMu.Unlock()
	if n := jobTailCount[g] - 1; n > 0 {
		jobTailCount[g] = n
	} else {
		delete(jobTailCount, g)
	}
	if jobTailTotal > 0 {
		jobTailTotal--
	}
}

func (s *kotoServer) JobTail(r *pb.JobTailReq, stream pb.Koto_JobTailServer) error {
	fail := func(msg string) error {
		return stream.Send(&pb.ScriptEvent{Event: "error", Error: msg})
	}
	if !validGroupName(r.Group) {
		return fail("invalid group name")
	}
	if !jobTailAdmit(r.Group) {
		return fail(fmt.Sprintf("too many live job tails (per-group %d, global %d) — close one and retry",
			jobTailMaxPerGroup, jobTailMaxGlobal))
	}
	defer jobTailRelease(r.Group)
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
	// A targeted clear of a goal session would race the driver's own
	// fresh-context clears; the whole-group form ("" session) stays allowed —
	// the next iteration simply rebuilds from the workspace files.
	if isReservedSession(r.Session) {
		return &pb.BaseResp{Error: "session " + r.Session + " is reserved for the goal loop"}, nil
	}
	br := clearCmd(groupReq{Group: r.Group, Session: r.Session})
	return &pb.BaseResp{Ok: br.OK, Error: br.Error}, nil
}

// registeredGroup reports whether g is a live group. Both deferred-work verbs
// check it: a schedule or a goal is a promise to send LATER, and ensure() no
// longer provisions on a send (M17), so an unregistered target used to persist
// a record that could only ever fail at fire time. Refusing at creation turns
// that into an error the caller can read, and keeps `spawn` the one verb that
// brings a group into existence (audit M29).
func registeredGroup(g string) bool {
	_, ok := readGroups()[g]
	return ok
}

func (s *kotoServer) SchedAdd(_ context.Context, r *pb.SchedAddReq) (*pb.SchedAddResp, error) {
	if !validGroupName(r.Group) {
		return &pb.SchedAddResp{Error: "invalid group name"}, nil
	}
	if !registeredGroup(r.Group) {
		return &pb.SchedAddResp{Error: "no such group " + r.Group + " — spawn it first"}, nil
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

// schedTargetCheck re-runs the ACL target check for a schedule addressed by
// id: SchedIDReq/SchedToggleReq carry no group, so the interceptor could only
// check the verb, and a role scoped to one group could fire, disable or delete
// any group's schedules by id (audit L1). The ctl plane has ownsSched; this
// is its gRPC twin. An unknown id passes so the handler's own "no schedule"
// error is what the caller sees.
func schedTargetCheck(ctx context.Context, verb, id string) error {
	for _, it := range listSched("") {
		if it.ID == id {
			ident, err := authFromCtx(ctx)
			if err != nil {
				return err
			}
			return aclCheck(ctx, ident, verb, &pb.SchedAddReq{Group: it.Group})
		}
	}
	return nil
}

func (s *kotoServer) SchedDel(ctx context.Context, r *pb.SchedIDReq) (*pb.BaseResp, error) {
	if err := schedTargetCheck(ctx, "sched_del", r.Id); err != nil {
		return nil, err
	}
	if err := delSched(r.Id); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) SchedToggle(ctx context.Context, r *pb.SchedToggleReq) (*pb.BaseResp, error) {
	if err := schedTargetCheck(ctx, "sched_toggle", r.Id); err != nil {
		return nil, err
	}
	if _, err := toggleSched(r.Id, r.Enabled); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

func (s *kotoServer) SchedRun(ctx context.Context, r *pb.SchedIDReq) (*pb.BaseResp, error) {
	if err := schedTargetCheck(ctx, "sched_run", r.Id); err != nil {
		return nil, err
	}
	if err := runSchedNow(r.Id); err != nil {
		return &pb.BaseResp{Error: err.Error()}, nil
	}
	return &pb.BaseResp{Ok: true}, nil
}

// ---- goals ----------------------------------------------------------------

func (s *kotoServer) GoalSet(_ context.Context, r *pb.GoalSetReq) (*pb.GoalResp, error) {
	if !validGroupName(r.Group) {
		return &pb.GoalResp{Error: "invalid group name"}, nil
	}
	if !registeredGroup(r.Group) {
		return &pb.GoalResp{Error: "no such group " + r.Group + " — spawn it first"}, nil
	}
	plan := r.Plan == nil || r.GetPlan() // absent = plan-first default
	// "" creator: the operator set this one. It is the human the plan gate
	// defers to, and it approves over GoalApprove — the guest ctl plane must
	// not be able to (audit M49).
	it, err := goalSetBy("", r.Group, r.Text, r.Criteria, r.Name, int(r.MaxIterations), plan)
	if err != nil {
		return &pb.GoalResp{Error: err.Error()}, nil
	}
	return &pb.GoalResp{Ok: true, Item: toPBGoalItem(it)}, nil
}

func (s *kotoServer) GoalList(_ context.Context, r *pb.GoalListReq) (*pb.GoalListResp, error) {
	if r.Group != "" && !validGroupName(r.Group) {
		return &pb.GoalListResp{Error: "invalid group name"}, nil
	}
	items := goalList(r.Group)
	out := make([]*pb.GoalItem, len(items))
	for i := range items {
		out[i] = toPBGoalItem(items[i])
	}
	return &pb.GoalListResp{Ok: true, Goals: out}, nil
}

// goalGroupRPC wraps the goal transitions (approve/pause/interrupt/resume/
// cancel) — identical shape, different transition. `name` picks the goal
// when the group runs several concurrently.
func goalGroupRPC(group, name string, fn func(string, string) (goalItem, error)) (*pb.GoalResp, error) {
	if !validGroupName(group) {
		return &pb.GoalResp{Error: "invalid group name"}, nil
	}
	it, err := fn(group, name)
	if err != nil {
		return &pb.GoalResp{Error: err.Error()}, nil
	}
	return &pb.GoalResp{Ok: true, Item: toPBGoalItem(it)}, nil
}

func (s *kotoServer) GoalApprove(_ context.Context, r *pb.GoalGroupReq) (*pb.GoalResp, error) {
	return goalGroupRPC(r.Group, r.Name, goalApprove)
}

func (s *kotoServer) GoalPause(_ context.Context, r *pb.GoalGroupReq) (*pb.GoalResp, error) {
	return goalGroupRPC(r.Group, r.Name, goalPause)
}

func (s *kotoServer) GoalInterrupt(_ context.Context, r *pb.GoalGroupReq) (*pb.GoalResp, error) {
	return goalGroupRPC(r.Group, r.Name, goalInterrupt)
}

func (s *kotoServer) GoalResume(_ context.Context, r *pb.GoalGroupReq) (*pb.GoalResp, error) {
	return goalGroupRPC(r.Group, r.Name, goalResume)
}

func (s *kotoServer) GoalCancel(_ context.Context, r *pb.GoalGroupReq) (*pb.GoalResp, error) {
	return goalGroupRPC(r.Group, r.Name, goalCancel)
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
	emitLogfG("exec", r.Group, "info", "[%s] runscript start (%d-byte script, raw=%v)", r.Group, len(r.Script), r.Raw)
	// Sanitized by default (audit M9a): the operator chooses the script, but
	// the GUEST authors the output — with root=yes it can replace /bin/sh or
	// cat through the overlay — and this stream used to reach the operator's
	// tty untouched, so any script run against a hostile group could write
	// the clipboard (OSC 52), retitle the window or leave mouse mode on.
	// JobTail already sanitized the identical case; this is the same policy
	// (SGR passes, every other escape and control is dropped), line-buffered
	// so an escape split across two frames is judged whole. raw is the
	// explicit opt-in for binary output going to a file.
	scrub := newChunkSanitizer(!r.Raw)
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
		f, err := fcReadAgentFrame(c)
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
		switch k := f.Kind.(type) {
		case *pb.AgentFrame_Data:
			if out := scrub.write(k.Data); len(out) > 0 {
				if serr := stream.Send(&pb.ScriptEvent{Event: "data", Chunk: out}); serr != nil {
					return serr
				}
			}
		case *pb.AgentFrame_End:
			if out := scrub.flush(); len(out) > 0 {
				if serr := stream.Send(&pb.ScriptEvent{Event: "data", Chunk: out}); serr != nil {
					return serr
				}
			}
			emitLogfG("exec", r.Group, "info", "[%s] runscript end", r.Group)
			return stream.Send(&pb.ScriptEvent{Event: "end"})
		case *pb.AgentFrame_Error:
			// Guest-authored, so sanitized like every other byte the guest
			// puts on this stream. The DATA frames went through
			// newChunkSanitizer and this one did not (audit M74), which is
			// backwards: an error is the frame a guest can produce on demand,
			// by making the operation fail. It reaches the operator's terminal
			// and the TUI's debug log, where nothing downstream strips control
			// sequences.
			msg := sanitize(k.Error)
			emitLogfG("exec", r.Group, "warn", "[%s] runscript error: %s", r.Group, msg)
			return fail(msg)
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
// shellSessionRE is the tmux session namespace the daemon mints: "koto-shell"
// plus an optional "-<chat session>", whose charset is sessionNameRE's.
var shellSessionRE = regexp.MustCompile(`^koto-shell(-[A-Za-z0-9][A-Za-z0-9_-]{0,31})?$`)

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
	// The value is a raw tmux selector: the guest runs `tmux new-session -A
	// -s <session>`, so an unconstrained one creates an unmanaged session
	// outside the group's namespace, and a newline forges daemon log records
	// on the line below. Pin it to the two shapes the daemon itself mints —
	// "koto-shell" for the default chat session, "koto-shell-<name>" for a
	// named one (shellSessionName in the TUI, turn.go in the guest). This is
	// hygiene, not a boundary: attach_shell on a group already yields a shell
	// as the guest worker, from which `tmux attach` reaches any session in it.
	if !shellSessionRE.MatchString(session) {
		return status.Error(codes.InvalidArgument, "invalid shell session name")
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
			f, ferr := fcReadAgentFrame(c)
			if ferr != nil {
				return
			}
			switch k := f.Kind.(type) {
			case *pb.AgentFrame_Data:
				if stream.Send(&pb.ShellFrame{Event: "data", Chunk: k.Data}) != nil {
					return
				}
			case *pb.AgentFrame_End:
				emitLogfG("shell", group, "info", "[%s] session=%s detached", group, session)
				_ = stream.Send(&pb.ShellFrame{Event: "end"})
				return
			case *pb.AgentFrame_Error:
				// Same reasoning as RunScript's error frame (M74). The shell
				// pane's DATA is deliberately raw — the TUI renders it through
				// a terminal emulator — but this string is not pty output, it
				// is a daemon error the client prints directly.
				msg := sanitize(k.Error)
				emitLogfG("shell", group, "warn", "[%s] session=%s error: %s", group, session, msg)
				_ = stream.Send(&pb.ShellFrame{Event: "error", Error: msg})
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
			if fcWriteFrame(c, &pb.AgentFrame{Kind: &pb.AgentFrame_Input{Input: v.Data}}) != nil {
				return nil
			}
		case *pb.ShellInput_Resize:
			if fcWriteFrame(c, &pb.AgentFrame{Kind: &pb.AgentFrame_Resize{Resize: v.Resize}}) != nil {
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
	// A client attaching mid-turn (fresh launch, or a resume whose window the
	// ring couldn't cover) has no way to learn the current phase — activity
	// frames are transitions, and the next one may be minutes away. Seed it
	// with the live phase as a synthetic seq-0 frame, same convention as `gap`
	// so it never disturbs the resume cursor.
	if a := activitySnapshot(g); a != nil {
		if err := stream.Send(toPBEvent(sanitizeEvent(*a))); err != nil {
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
	// Same projection List applies, per watcher: the broadcaster shares one
	// frame across watchers, so a narrowed one must carry its own filter (and
	// its own hash — its view changes on a different schedule).
	ctxID := stream.Context()
	var project func(map[string]GroupInfo) map[string]GroupInfo
	if id := identityOf(ctxID); id.Name != "" {
		acl := loadACL()
		if vis := visibleTargets(acl, id.Roles, "watch_state"); !vis.any ||
			!visibleTargets(acl, id.Roles, "jobs").any {
			project = func(in map[string]GroupInfo) map[string]GroupInfo {
				return projectGroups(ctxID, "watch_state", in)
			}
		}
	}
	if project != nil {
		gs = project(gs)
	}
	sub := &stateSub{ch: make(chan *pb.StateFrame, 4), lastSent: stateHash(gs), project: project}
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
