// Package protocol holds the wire types shared between the daemon and any
// client (the TUI, ad-hoc socat probes, etc.). The daemon serializes these
// to line-delimited JSON over clawson.sock; most commands are one-shot
// (request, one response, close). `subscribe` keeps the connection open
// and pushes Event frames after the initial SubscribeResp ack.
//
// This package is a separate Go module (clawson-protocol) so it can be
// imported by both the root daemon module and the tui module without
// merging their dep trees — the TUI's blast-radius isolation still holds
// because this module is stdlib-only.
package protocol

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
	// the config.json `model` if set, else the provider default — kimi-k2.5
	// for venice (defaultVeniceModel), or "" for claudesdk (the claude CLI
	// picks its own default, which clawson doesn't set, so the TUI shows
	// "(default)" there).
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
}

type SendReq struct {
	Group string `json:"group"`
	Msg   string `json:"msg"`
}

// GroupReq covers stop / destroy / restart / clear — all take only `group`.
type GroupReq struct {
	Group string `json:"group"`
}

// HistoryReq is the paged variant for `cmd:"history"`. Limit=0 → server
// default (1000). Before=0 → tail page; otherwise return events with
// strict `ts < Before` so callers can walk older pages by repeatedly
// passing the smallest ts they've already seen.
type HistoryReq struct {
	Group  string  `json:"group"`
	Limit  int     `json:"limit,omitempty"`
	Before float64 `json:"before,omitempty"`
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
	Pip      json.RawMessage `json:"pip,omitempty"`
	Provider json.RawMessage `json:"provider,omitempty"`
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

type MetricsReq struct {
	Group string `json:"group,omitempty"`
}

type SubscribeReq struct {
	Group string `json:"group"`
}

// LogsReq subscribes to the daemon's internal log stream. No fields — the
// daemon log is global, not per-group. Like SubscribeReq, this transfers
// connection ownership to the daemon's log-subscriber registry.
type LogsReq struct{}

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

type HistoryResp struct {
	BaseResp
	Events []Event `json:"events"`
	// More signals that older events exist beyond what's returned. The TUI
	// uses this to decide whether scrolling near the top should trigger
	// another paged fetch.
	More bool `json:"more,omitempty"`
}

type ConfigResp struct {
	BaseResp
	Config map[string]any `json:"config"`
}

// MetricsResp keeps Metric and GlobalMetric without omitempty so the wire
// always carries explicit nulls when no metric has been recorded.
type MetricsResp struct {
	BaseResp
	Metric       map[string]any `json:"metric"`
	GlobalMetric map[string]any `json:"global_metric"`
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

type SubscribeResp struct {
	BaseResp
	Subscribed string `json:"subscribed,omitempty"`
}

// LogsResp is the ack for `cmd:"logs"`. After this frame the daemon may
// replay buffered log lines, then push fresh LogEvent frames as they happen.
type LogsResp struct {
	BaseResp
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
