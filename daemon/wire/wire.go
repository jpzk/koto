// Package wire holds the daemon's JSON wire types. The client-facing
// transport is gRPC (see protocol/clawson.proto and the generated pb);
// these JSON types survive in two places: the per-group FIFO ctl plane
// (ctl.go serializes them as line-delimited JSON over ctl/ctl.out) and as
// the in-memory event/state shapes the daemon converts to and from the
// generated pb messages.
//
// This package is daemon-internal. It used to live in the shared
// clawson-protocol module, but the cross-project contract is now the proto
// alone: the TUI and the Android app consume only clawson.proto (or its
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
	// live streaming frames (see clawson.proto Event.seq). Zero for events
	// re-parsed from the log by the History RPC.
	Seq uint64 `json:"seq,omitempty"`
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
}

// SkillItem describes one skill in SkillsResp.
type SkillItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Enabled     bool   `json:"enabled,omitempty"`
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
}

// GroupReq covers stop / destroy / restart / clear — all take only `group`.
type GroupReq struct {
	Group string `json:"group"`
}

// ConfigReq uses RawMessage per field so the dispatcher can distinguish
// three states: absent (nil), clear (`""`, `null`, `[]`), and set
// (non-empty literal).
type ConfigReq struct {
	Group    string          `json:"group"`
	Model    json.RawMessage `json:"model,omitempty"`
	Effort   json.RawMessage `json:"effort,omitempty"`
	Skills   json.RawMessage `json:"skills,omitempty"`
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
	// Size is the machine preset: "small" (default) | "medium" | "large" —
	// sets vCPU, RAM, and workspace disk together. Applies on /restart.
	Size json.RawMessage `json:"size,omitempty"`
	// Root is passwordless-sudo-in-guest: "yes" | "no" (default). The microVM's
	// KVM boundary contains root-in-guest, so it doesn't widen the host blast
	// radius. Applies on /restart (fc-agent installs the sudoers grant at boot).
	Root json.RawMessage `json:"root,omitempty"`
}

type SkillListReq struct {
	Group string `json:"group,omitempty"`
}

type SkillNewReq struct {
	Name string `json:"name"`
}

type SkillReadReq struct {
	Name string `json:"name"`
}

// LogEvent is the streaming frame the daemon pushes to log subscribers.
// Distinct from Event because daemon logs aren't keyed by group and carry
// a level. Ts matches Event's float-seconds convention.
type LogEvent struct {
	Event string  `json:"event"` // always "log"
	Level string  `json:"level"` // info | warn | error | debug
	Msg   string  `json:"msg"`
	Ts    float64 `json:"ts"`
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

type SkillsResp struct {
	BaseResp
	Skills []SkillItem `json:"skills"`
}

type SkillNewResp struct {
	BaseResp
	Path string `json:"path,omitempty"`
}

type SkillReadResp struct {
	BaseResp
	Name    string `json:"name,omitempty"`
	Content string `json:"content,omitempty"`
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
