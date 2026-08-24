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
	// Network is the group's effective egress profile (none|wan|lan|full)
	// and Root its sudo grant, read daemon-side from config.json. Rendered
	// by the fleet (top) view; empty Network means an older daemon.
	Network string
	Root    bool
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

// GroupRes is one group's resource snapshot (koto.proto GroupResources),
// trimmed to what the metrics bar renders: CPU as a fraction of the VM's
// vcpus, guest memory fullness (RSS against the mem preset as the fallback),
// guest disk fullness (image allocation against the size ceiling as the
// fallback).
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
	// GuestDiskTotal/GuestDiskAvail are the guest's own /workspace filesystem
	// — how full the disk actually is. AllocBytes is the host's cost for this
	// group and is a high-water mark of every block ever touched, so the two
	// diverge without limit under churn. 0 = unknown (VM stopped or
	// unreachable).
	GuestDiskTotal int64
	GuestDiskAvail int64
	// GuestDiskUsed is read from the guest, not derived: ext4 reserves ~5%
	// for root, which is neither used nor available, so total-minus-avail
	// charges that reserve to the group and puts an empty workspace at 5%.
	GuestDiskUsed int64
}

// HostRes is the fleet-wide rollup of the Resources RPC (koto.proto
// HostResources): the daemon-side filesystem holding the workspace images,
// total image allocation, and the provisioned (overcommit) sum of every
// group's size preset. Rendered as the top view's summary line.
type HostRes struct {
	FsTotalBytes     int64
	FsFreeBytes      int64
	AllocTotalBytes  int64
	ProvisionedBytes int64
	Groups           int32
	RunningGroups    int32
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

// guestDiskUsage returns the guest filesystem's used and total bytes, and
// whether they are known. This is the honest "how full is this disk" pair:
// GroupRes.AllocBytes is the host's cost for the group and ratchets upward
// with every block ever touched, so it answers a different question. Unknown
// (a stopped or unreachable guest) is reported rather than approximated, so
// callers choose their own fallback instead of silently mixing the two.
// The reported fraction is df's — used/(used+avail), NOT used/size — so the
// chip agrees with what `df` prints inside the guest. `total` is returned
// separately because the fleet table shows the filesystem's real size beside
// the fraction, exactly as `df -h` does.
func guestDiskUsage(r GroupRes) (used, total int64, frac float64, ok bool) {
	if r.GuestDiskTotal <= 0 {
		return 0, 0, 0, false
	}
	den := r.GuestDiskUsed + r.GuestDiskAvail
	if den <= 0 {
		return 0, 0, 0, false
	}
	return r.GuestDiskUsed, r.GuestDiskTotal, float64(r.GuestDiskUsed) / float64(den), true
}

// guestMemUsage is guestDiskUsage's memory twin: the guest's own used and
// total bytes (used = MemTotal - MemAvailable, the kernel's estimate that
// counts reclaimable cache as free), and whether they are known. This is the
// honest "how full is this VM" pair: GroupRes.RSSBytes is the VMM's resident
// set, a high-water mark of every guest page ever touched (no balloon
// device), which any I/O-heavy turn parks near 100% forever. Unknown (a
// stopped or unreachable guest — the daemon zeroes both fields) is reported
// rather than approximated, so callers choose their own fallback instead of
// silently mixing the two figures.
func guestMemUsage(r GroupRes) (used, total int64, frac float64, ok bool) {
	if r.GuestMemTotal <= 0 || r.GuestMemAvail <= 0 || r.GuestMemAvail > r.GuestMemTotal {
		return 0, 0, 0, false
	}
	used = r.GuestMemTotal - r.GuestMemAvail
	return used, r.GuestMemTotal, float64(used) / float64(r.GuestMemTotal), true
}
