package main

// TUI-local view types, populated from the generated pb messages (see
// pbToEvent / stateGroups / pbToLogEvent in daemon.go). These used to be
// aliases into the shared koto-protocol module; they are now defined
// here so the only code shared with the daemon (and the Android app) is
// the proto contract itself — protocol/koto.proto and its committed
// generated pb. Field semantics are documented on the proto messages.

// Event is one streaming frame from SubscribeGroup / History.
type Event struct {
	Event      string
	Group      string
	Ts         float64
	Msg        string
	Text       string
	Name       string
	Input      string
	Words      int
	Body       string
	Historical bool
	// ID identifies the schedule that produced a `sched_fired` event so the
	// TUI can correlate the firing back to its row in /sched list.
	ID string
	// Seq is the per-group monotonic sequence number of live streaming
	// frames; zero for events replayed by the History RPC.
	Seq uint64
	// Session within the group this frame belongs to ("" = the default
	// session). Stamped by the daemon's log parser; pre-session daemons
	// leave it empty, which reads as the default session.
	Session string
	// Severity ("high" | "normal"; unknown reads as normal) and Title carry
	// the notification payload (event == "notification"); the message body
	// rides Text.
	Severity string
	Title    string
}

// GroupInfo is one group's row in the WatchState / List snapshot.
type GroupInfo struct {
	Port     int
	Running  bool
	Provider string
	Model    string
	Effort   string
	Stalled  bool
	Queued   int
	// Sessions lists the group's named sessions (the default session is
	// implicit and never listed). Drives the /session picker.
	Sessions []string
	// Jobs is the daemon's mirror of the group's background jobs (cs-job),
	// attributed to the chat session that launched each. Drives the tree's
	// job rows and the hover peek pane.
	Jobs []JobInfo
	// TokPerSec is the group's output-token throughput over the daemon's
	// trailing window, measured daemon-side (daemon/tokrate.go). 0 = idle.
	TokPerSec float64
}

// JobInfo is one background job in a group's guest (koto.proto JobInfo).
type JobInfo struct {
	ID      string
	Session string // "" = default session
	Status  string // running | done | orphaned | unknown
	RC      string
	Cmd     string
	Started int64
	OutSize int64
}

// GroupRes is one group's host-side resource snapshot (koto.proto
// GroupResources), trimmed to what the metrics bar renders: CPU as a
// fraction of the VM's vcpus, RSS against the mem preset, allocated image
// bytes against the size preset's disk ceiling.
type GroupRes struct {
	Running       bool
	CPUPct        float64 // percent of ONE core (4-vCPU VM may exceed 100)
	Vcpus         int32
	MemMiB        int32
	RSSBytes      int64 // VMM RSS: high-water mark of guest-touched pages, not usage
	AllocBytes    int64
	DeclaredBytes int64
	// Guest-reported memory (guest /proc/meminfo mirrored by the daemon);
	// 0 = unknown (stopped VM or agent unreachable). The truthful pressure
	// figure — RSSBytes ratchets to ~100% of the preset and stays there.
	GuestMemTotal int64
	GuestMemAvail int64
}

// LogEvent is one frame of the daemon's own log stream (SubscribeLogs).
type LogEvent struct {
	Event     string // always "log"
	Level     string // info | warn | error | debug
	Msg       string
	Ts        float64
	Subsystem string // emitting subsystem (acl, fc, egress, ...); may be empty from older daemons
	Group     string // owning group for group-scoped lines; "" = daemon-wide
}
