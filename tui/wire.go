package main

// TUI-local view types, populated from the generated pb messages (see
// pbToEvent / stateGroups / pbToLogEvent in daemon.go). These used to be
// aliases into the shared clawson-protocol module; they are now defined
// here so the only code shared with the daemon (and the Android app) is
// the proto contract itself — protocol/clawson.proto and its committed
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
}

// LogEvent is one frame of the daemon's own log stream (SubscribeLogs).
type LogEvent struct {
	Event string // always "log"
	Level string // info | warn | error | debug
	Msg   string
	Ts    float64
}
