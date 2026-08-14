// Package wire holds the daemon's JSON wire types. The client-facing
// transport is gRPC (see protocol/koto.proto and the generated pb);
// these JSON types survive in two places: the per-group FIFO ctl plane
// (ctl.go serializes them as line-delimited JSON over ctl/ctl.out) and as
// the in-memory event/state shapes the daemon converts to and from the
// generated pb messages.
//
// This package is daemon-internal. It used to live in the shared
// koto-protocol module, but the cross-project contract is now the proto
// alone: the TUI and the Android app consume only koto.proto (or its
// committed generated code), never these types.
package wire

import "encoding/json"

// ---- Event ----------------------------------------------------------------

// Event is the streaming frame the daemon pushes to subscribers. All
// variant-specific fields use omitempty so each event carries only the
// keys relevant to its type. Ts is wire-emitted as float seconds
// (Python time.time() compatibility — clients that want int64 truncate
// at use sites).
type Event struct {
	Event      string  `json:"event"`
	Group      string  `json:"group"`
	Ts         float64 `json:"ts,omitempty"`
	Msg        string  `json:"msg,omitempty"`
	Text       string  `json:"text,omitempty"`
	Name       string  `json:"name,omitempty"`
	Input      string  `json:"input,omitempty"`
	Words      int     `json:"words,omitempty"`
	Body       string  `json:"body,omitempty"`
	Historical bool    `json:"historical,omitempty"`
	// ID identifies the schedule that produced a `sched_fired` event so the
	// TUI can correlate the firing back to its row in /sched list.
	ID string `json:"id,omitempty"`
	// Seq is the per-group monotonic sequence number the daemon assigns to
	// live streaming frames (see koto.proto Event.seq). Zero for events
	// re-parsed from the log by the History RPC.
	Seq uint64 `json:"seq,omitempty"`
	// Session within the group this frame belongs to ("" = the default
	// session; see koto.proto Event.session). Stamped by the log parser
	// from the [[session]] turn markers.
	Session string `json:"session,omitempty"`
	// Severity and Title carry the notification payload (event ==
	// "notification"): severity is "high" | "normal" (clamped by the ctl
	// verb; clients treat unknown as normal), title the short headline.
	// The message body rides Text.
	Severity string `json:"severity,omitempty"`
	Title    string `json:"title,omitempty"`
}

// GroupInfo is the value shape of the `groups` map in ListResp.
type GroupInfo struct {
	Port    int  `json:"port"`
	Running bool `json:"running"`
	// Provider is the per-group LLM backend, taken from config.json. Empty
	// or "claudesdk" mean the Claude-via-proxy path; "venice" routes to
	// api.venice.ai. Exposed in list so clients (TUI) can render per-provider
	// markers without a second config round-trip per group.
	Provider string `json:"provider,omitempty"`
	// Model is the EFFECTIVE model the group runs (daemon's groupModelName):
	// the config.json `model` if set, else the provider default —
	// claude-sonnet-5 for claudesdk (defaultClaudeModel) or kimi-k2.5 for
	// venice (defaultVeniceModel). Never empty in practice; the TUI's
	// "(default)" fallback survives only for old daemons.
	Model string `json:"model,omitempty"`
	// Effort is the configured reasoning-effort knob (config.json's `effort`
	// field). Only consumed by claudesdk; the venice path ignores it. Empty
	// when unset.
	Effort string `json:"effort,omitempty"`
	// Stalled is set when the daemon delivered a message but never observed
	// the turn's [[turn_end]] within its wait window — i.e. the sidecar's
	// FIFO loop is wedged (dead/hung), not merely running a slow turn (the
	// per-turn watchdog in entrypoint.sh bounds those). Cleared the moment any
	// turn_end is observed for the group. Lets clients flag a wedged group
	// instead of showing it as healthily "running".
	Stalled bool `json:"stalled,omitempty"`
	// Queued is the number of messages waiting in the group's send queue —
	// enqueued but not yet started (the in-flight turn is NOT counted). Lets
	// clients show backlog/backpressure per group. Zero (the common case) is
	// omitted from the wire.
	Queued int `json:"queued,omitempty"`
	// Sessions lists the group's named sessions (sorted; the default
	// session is implicit and never listed). See koto.proto.
	Sessions []string `json:"sessions,omitempty"`
	// Jobs is the daemon's mirror of the group's background jobs (cs-job),
	// each attributed to the chat session that launched it. See koto.proto
	// JobInfo and daemon/jobs.go.
	Jobs []JobInfo `json:"jobs,omitempty"`
	// TokPerSec is this group's output-token throughput over the daemon's
	// trailing window (daemon/tokrate.go). 0 = idle.
	TokPerSec float64 `json:"tok_per_sec,omitempty"`
	// Network is the effective egress profile (none|wan|lan|full) and Root
	// the effective sudo grant, both read from config.json (groupNetwork /
	// groupRoot — legacy `internet` already mapped). Exposed so clients can
	// render a fleet overview without a per-group Config round-trip.
	Network string `json:"network,omitempty"`
	Root    bool   `json:"root,omitempty"`
}

// JobInfo is one background job (sidecar/cs-job) in a group's guest. Mirrors
// koto.proto JobInfo.
type JobInfo struct {
	ID      string `json:"id"`
	Session string `json:"session,omitempty"` // "" = default session
	Status  string `json:"status"`            // running | done | orphaned | unknown
	RC      string `json:"rc,omitempty"`
	Cmd     string `json:"cmd,omitempty"`
	Started int64  `json:"started,omitempty"`
	OutSize int64  `json:"out_size,omitempty"`
	Group   string `json:"group,omitempty"` // set in JobsResp entries
}

// ---- Request envelopes ----------------------------------------------------

// CmdEnvelope is the dispatch peek — every request is unmarshalled into this
// first to read Cmd, then re-unmarshalled into the typed request below.
type CmdEnvelope struct {
	Cmd string `json:"cmd"`
}

type SpawnReq struct {
	Group    string `json:"group"`
	Main     bool   `json:"main,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Size     string `json:"size,omitempty"`
}

type SendReq struct {
	Group string `json:"group"`
	Msg   string `json:"msg"`
	// Session to deliver the turn into ("" = default; see koto.proto
	// SendReq.session).
	Session string `json:"session,omitempty"`
}

// GroupReq covers stop / destroy / restart / clear — all take `group`.
// Session is read by clear only: "" = whole group, "-"/"default" = the
// default session, a name = that session (see koto.proto GroupReq).
type GroupReq struct {
	Group   string `json:"group"`
	Session string `json:"session,omitempty"`
}

// ConfigReq uses RawMessage per field so the dispatcher can distinguish
// three states: absent (nil), clear (`""`, `null`, `[]`), and set
// (non-empty literal).
type ConfigReq struct {
	Group    string          `json:"group"`
	Model    json.RawMessage `json:"model,omitempty"`
	Effort   json.RawMessage `json:"effort,omitempty"`
	Ports    json.RawMessage `json:"ports,omitempty"`
	Provider json.RawMessage `json:"provider,omitempty"`
	// Network is the group's egress profile: "none" (default — the guest has
	// no NIC; the proxy reaches only the LLM upstream), "wan" (real L3
	// gateway, public internet only — LAN blocked), "lan" (LAN only), or
	// "full" (both). See fcnet.go. Applies on /restart.
	Network json.RawMessage `json:"network,omitempty"`
	// Internet is the DEPRECATED pre-rename profile key (none|full). Still
	// accepted from old clients; the daemon maps full→network=wan and
	// migrates the stored key (applyConfig).
	Internet json.RawMessage `json:"internet,omitempty"`
	// Size is the machine preset: "small" (default) | "medium" | "large" | "xlarge" —
	// sets vCPU, RAM, and workspace disk together. Applies on /restart.
	Size json.RawMessage `json:"size,omitempty"`
	// Root is passwordless-sudo-in-guest: "yes" | "no" (default). The microVM's
	// KVM boundary contains root-in-guest, so it doesn't widen the host blast
	// radius. Applies on /restart (fc-agent installs the sudoers grant at boot).
	Root json.RawMessage `json:"root,omitempty"`
	// Autostart is "yes" | "no" (default): boot this group's microVM as soon as
	// the daemon starts, instead of lazily on its first message. Read only at
	// daemon startup, so setting it takes effect on the next daemon restart.
	Autostart json.RawMessage `json:"autostart,omitempty"`
}

// LogEvent is the streaming frame the daemon pushes to log subscribers.
// Distinct from Event because daemon logs aren't keyed by group and carry
// a level. Ts matches Event's float-seconds convention.
type LogEvent struct {
	Event     string  `json:"event"` // always "log"
	Level     string  `json:"level"` // info | warn | error | debug
	Msg       string  `json:"msg"`
	Ts        float64 `json:"ts"`
	Subsystem string  `json:"subsystem,omitempty"` // acl | auth | fc | egress | sched | ...
}

// ---- Response envelopes ---------------------------------------------------

// BaseResp is embedded in every typed response. Error is omitempty so
// successful responses serialize as `{"ok":true,...}` without a trailing
// `,"error":""`. Failures use it directly: `{"ok":false,"error":"..."}`.
type BaseResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ErrResp is the canonical failure factory.
func ErrResp(msg string) BaseResp { return BaseResp{OK: false, Error: msg} }

type SpawnResp struct {
	BaseResp
	Port int `json:"port,omitempty"`
}

type ListResp struct {
	BaseResp
	Groups map[string]GroupInfo `json:"groups"`
}

type ConfigResp struct {
	BaseResp
	Config map[string]any `json:"config"`
}

// ResourcesResp is the ctl-plane view of the daemon's host-side resource
// accounting (see daemon/resources.go). Shape mirrors the gRPC ResourcesResp
// so the two planes stay legible together.
//
// Byte counts are raw so the agent can do its own arithmetic; the derived
// percentages are included because the useful judgements ("how full is this
// group", "how close is the host") are ratios, and making an agent recompute
// them from four fields every turn is a reliable way to get them wrong.
type ResourcesResp struct {
	BaseResp
	Groups []GroupResources `json:"groups"`
	Host   HostResources    `json:"host"`
}

type GroupResources struct {
	Group   string `json:"group"`
	Running bool   `json:"running,omitempty"`
	// AllocBytes is the workspace image's REAL host consumption; the images
	// are sparse, so this is far below DeclaredBytes.
	AllocBytes    int64 `json:"alloc_bytes"`
	DeclaredBytes int64 `json:"declared_bytes"`
	// AllocPct is AllocBytes as a percentage of DeclaredBytes — the group's
	// distance from its own ceiling.
	AllocPct float64 `json:"alloc_pct"`
	// GrowthBytesPerHour is measured over GrowthSpanSeconds of samples.
	// Allocation effectively never falls on its own (no discard in the
	// guest's virtio-blk), so a sustained positive rate is a countdown.
	GrowthBytesPerHour int64   `json:"growth_bytes_per_hour"`
	GrowthSpanSeconds  int64   `json:"growth_span_seconds"`
	RSSBytes           int64   `json:"rss_bytes,omitempty"`
	CPUPct             float64 `json:"cpu_pct,omitempty"`
	Vcpus              int32   `json:"vcpus,omitempty"`
	MemMiB             int32   `json:"mem_mib,omitempty"`
	// Guest-reported memory (guest /proc/meminfo, mirrored per sweep).
	// RSSBytes is a high-water mark of guest-touched pages — no balloon
	// device — so it reads ~100% of the preset on any VM that has done real
	// I/O; these are the truthful pressure figures. 0/absent = unknown.
	GuestDiskTotalBytes int64 `json:"guest_disk_total_bytes,omitempty"`
	GuestDiskAvailBytes int64 `json:"guest_disk_avail_bytes,omitempty"`
	GuestDiskUsedBytes  int64 `json:"guest_disk_used_bytes,omitempty"`
	// GuestDiskUsedPct is the guest filesystem's own fullness — the figure
	// that answers "is this group about to wedge". AllocPct answers a
	// different question (host cost against what was provisioned).
	GuestDiskUsedPct   float64 `json:"guest_disk_used_pct,omitempty"`
	GuestMemTotalBytes int64   `json:"guest_mem_total_bytes,omitempty"`
	GuestMemAvailBytes int64   `json:"guest_mem_avail_bytes,omitempty"`
	GuestMemUsedPct    float64 `json:"guest_mem_used_pct,omitempty"`
}

type HostResources struct {
	FSTotalBytes int64 `json:"fs_total_bytes"`
	FSFreeBytes  int64 `json:"fs_free_bytes"`
	// FSUsedPct is the host filesystem's fill level — the number that
	// actually predicts a fleet-wide outage, and the one a guest's own `df`
	// cannot see.
	FSUsedPct       float64 `json:"fs_used_pct"`
	AllocTotalBytes int64   `json:"alloc_total_bytes"`
	// ProvisionedBytes is the sum of every group's size preset. Exceeding
	// FSTotalBytes is normal (sparse overcommit by design).
	ProvisionedBytes int64 `json:"provisioned_bytes"`
	Groups           int32 `json:"groups"`
	RunningGroups    int32 `json:"running_groups"`
}

// ---- schedules ------------------------------------------------------------

// ScheduleItem is the persisted shape of one cron-style schedule. Cron is
// kept as the raw 5-field expression (or alias like `@daily`); the parsed
// bitmask is rebuilt on daemon startup, not serialized. CreatedAt /
// LastFiredAt / NextDueAt are unix seconds (matches Event.Ts).
type ScheduleItem struct {
	ID          string  `json:"id"`
	Group       string  `json:"group"`
	Cron        string  `json:"cron"`
	Msg         string  `json:"msg"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   float64 `json:"created_at"`
	LastFiredAt float64 `json:"last_fired_at,omitempty"`
	NextDueAt   float64 `json:"next_due_at,omitempty"`
}

type SchedAddReq struct {
	Group string `json:"group"`
	Cron  string `json:"cron"`
	Msg   string `json:"msg"`
}

type SchedAddResp struct {
	BaseResp
	Item ScheduleItem `json:"item"`
}

type SchedListReq struct {
	Group string `json:"group,omitempty"`
}

type SchedListResp struct {
	BaseResp
	Schedules []ScheduleItem `json:"schedules"`
}

type SchedIDReq struct {
	ID string `json:"id"`
}

type SchedToggleReq struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// ---- goals ------------------------------------------------------------------

// GoalItem is the persisted shape of one goal (goals.json; see daemon/goals.go).
// At most one goal exists per group. Iteration counts execution iterations
// only — the plan turn and judge turns are not charged against
// MaxIterations. Timestamps are unix seconds (matches ScheduleItem).
type GoalItem struct {
	ID    string `json:"id"`
	Group string `json:"group"`
	// Name is the run's short human-readable handle AND its session name in
	// the group ("goal-<name>"), so it is what the tree row, /session, the
	// jump picker and the ctl plane all show. Slugged from Text when the
	// caller supplies none. Empty on records written before names existed —
	// those fall back to the ID (goals.go goalSessionSlug).
	Name     string `json:"name,omitempty"`
	Text     string `json:"text"`
	Criteria string `json:"criteria"`
	// Plan records whether the goal was set plan-first (one planning turn,
	// then awaiting_approval until a human approves).
	Plan          bool   `json:"plan"`
	Status        string `json:"status"` // planning | awaiting_approval | running | paused | met | cancelled
	Iteration     int    `json:"iteration"`
	MaxIterations int    `json:"max_iterations"`
	// LastFeedback is the judge's per-criterion failure report from the most
	// recently rejected done-claim; embedded into later iteration prompts.
	LastFeedback string `json:"last_feedback,omitempty"`
	// DoneNote is the worker's evidence summary from the accepted goal_done.
	DoneNote     string  `json:"done_note,omitempty"`
	PausedReason string  `json:"paused_reason,omitempty"` // cap | stalled | judge | operator | stopped
	CreatedAt    float64 `json:"created_at"`
	UpdatedAt    float64 `json:"updated_at,omitempty"`
	CompletedAt  float64 `json:"completed_at,omitempty"`
}

type GoalSetReq struct {
	Group         string `json:"group"`
	Text          string `json:"text"`
	Criteria      string `json:"criteria"`
	MaxIterations int    `json:"max_iterations,omitempty"`
	// Plan is tri-state on the ctl plane (absent = default true), matching
	// the proto's optional bool.
	Plan *bool `json:"plan,omitempty"`
	// Name is the run's short handle; empty => slugged from Text.
	Name string `json:"name,omitempty"`
}

// GoalGroupReq covers approve / pause / interrupt / resume / cancel. Name
// addresses one of the group's goals (run name or id); empty resolves to the
// sole candidate and errors when several goals are active (goals run
// concurrently, so guessing would steer the wrong run).
type GoalGroupReq struct {
	Group string `json:"group"`
	Name  string `json:"name,omitempty"`
}

type GoalListReq struct {
	Group string `json:"group,omitempty"`
}

type GoalResp struct {
	BaseResp
	Item GoalItem `json:"item"`
}

type GoalListResp struct {
	BaseResp
	Goals []GoalItem `json:"goals"`
}
