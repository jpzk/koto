package main

// posture.go — verify that the restrictions koto relies on are actually in
// effect, and say so LOUDLY when one is not.
//
// Every layer here used to be able to go missing quietly. Per-VM cgroup caps
// degraded to an info line; an unlimited fleet memory cap was an info line; a
// VMM whose nice failed said so only into its own console log; a daemon started
// outside its systemd unit ran with none of the unit's sandbox and said nothing
// at all; and nothing ever looked at a running VMM to confirm the jail it was
// supposed to be in. Each is a restriction the trust model describes as
// present, so its absence is reported the way a failure is: an error-level
// daemon-log line (journal, `koto ctl logs`, the TUI log pane) AND a
// high-severity operator notification titled "RESTRICTION INACTIVE: <what>"
// (banner, desktop popup, and the BEL window managers turn into an urgency
// hint).
//
// These are warnings, not refusals: the fleet keeps running. The point is that
// running with a layer missing is a decision the operator makes knowingly, not
// a state they discover in an audit.
//
// Two places check:
//   - postureStartup, once per daemon start, for the host-wide layers;
//   - postureVM, after every VM boot, reading the RUNNING VMM out of /proc —
//     what the kernel actually applied, not what the setup code intended.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// postureTitlePrefix heads every posture notification, so the banner reads the
// same way for every layer and a missed one is greppable in `koto ctl logs`.
const postureTitlePrefix = "RESTRICTION INACTIVE: "

// postureProc is the procfs root the checks read. A var so tests can stand up
// a fake process tree; production never changes it.
var postureProc = "/proc"

// postureFinding is one restriction that is not in effect.
type postureFinding struct {
	what   string // the restriction, as the notification title names it
	detail string // what was observed, and what that exposes
}

// postureAlert reports findings: one error-level line each (quiet, because
// the notification is raised here — forwarding the error line through
// logalert as well would banner every finding twice), and ONE notification
// for the lot, so a VM that came up with four layers missing is one banner,
// not four.
func postureAlert(g string, fs []postureFinding) {
	if len(fs) == 0 {
		return
	}
	where := ""
	if g != "" {
		where = "[" + g + "] "
	}
	var whats, details []string
	for _, f := range fs {
		emitLogfQuiet("posture", "error", "%s%s%s — %s", where, postureTitlePrefix, f.what, f.detail)
		whats = append(whats, f.what)
		details = append(details, f.what+": "+f.detail)
	}
	title := postureTitlePrefix + strings.Join(whats, ", ")
	if g != "" {
		title += " [" + g + "]"
	}
	if !notifyDeliver(ctlMainGroup, "high", "", title, strings.Join(details, " · ")) {
		emitLogfQuiet("posture", "error", "notification backlog full, posture alert not bannered: %s", title)
	}
}

// ---- startup -----------------------------------------------------------

// fcCgroupOffReason says why per-VM cgroup caps are unavailable; set by
// fcCgroupInit when it degrades. fcHostMemCapErr is set when the kernel-side
// fleet memory cap (memory.max/memory.high on the vms/ parent) could not be
// written, leaving only the admission check.
var (
	fcCgroupOffReason string
	fcHostMemCapErr   string
)

// postureStartup checks the host-wide layers once, after the cgroup and
// memory-cap probes have run and before any VM boots.
func postureStartup() { postureAlert("", postureStartupFindings()) }

func postureStartupFindings() []postureFinding {
	var fs []postureFinding

	if !fcCgroupOn {
		fs = append(fs, postureFinding{"per-VM cgroup caps",
			"not available (" + fcCgroupOffReason + "): no VM has a cpu.weight or memory.high of its own, " +
				"so one busy VM competes with the rest on equal terms. The installed unit's Delegate=yes provides them"})
	}
	switch {
	case fcHostMemUnknown:
		// Already refusing spawns at error (fchostmem.go); nothing to add.
	case fcHostMemCapMiB == 0:
		fs = append(fs, postureFinding{"fleet memory cap",
			"unlimited (KOTO_HOST_MEM_MIB=0): spawns are admitted whatever they commit, and the kernel " +
				"OOM killer, not koto, decides what dies when the host runs out"})
	case fcHostMemCapErr != "":
		fs = append(fs, postureFinding{"kernel fleet memory cap",
			"memory.max on the VM parent cgroup could not be set (" + fcHostMemCapErr + "): only koto's " +
				"admission check bounds the fleet, and a VM that grows past its preset is not stopped by the kernel"})
	}

	if mode, err := postureSeccompMode("self"); err == nil && mode != 2 {
		why := "the daemon has no seccomp filter"
		if os.Getenv("INVOCATION_ID") == "" {
			why = "the daemon is not running under koto.service"
		}
		fs = append(fs, postureFinding{"host sandbox",
			why + ", so none of the unit's hardening applies: no ProtectHome/ProtectSystem, no " +
				"DevicePolicy, no address-family or namespace restrictions, no syscall filter. " +
				"Expected for a dev daemon; never for an installed one"})
	}

	if bind := postureBindAddr(); !postureIsLoopback(bind) {
		fs = append(fs, postureFinding{"loopback-only gRPC",
			"KOTO_BIND=" + bind + " exposes the control plane beyond this host. mTLS, the client " +
				"allowlist and the bearer token still gate every call"})
	}
	return fs
}

// postureBindAddr mirrors daemonMain's KOTO_BIND default.
func postureBindAddr() string {
	if b := os.Getenv("KOTO_BIND"); b != "" {
		return b
	}
	return "127.0.0.1"
}

func postureIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// ---- per VM ------------------------------------------------------------

// postureVM verifies the running VMM for group g against every layer of its
// jail, reading what the kernel applied rather than what fcjailMain intended.
// Called once the guest agent answers, i.e. after the shim has exec'd
// Firecracker, so the process is in its final state.
func postureVM(g string, pid, jailUID int) { postureAlert(g, postureVMFindings(g, pid, jailUID)) }

func postureVMFindings(g string, pid, jailUID int) []postureFinding {
	var fs []postureFinding
	p := filepath.Join(postureProc, strconv.Itoa(pid))
	add := func(what, detail string) { fs = append(fs, postureFinding{what, detail}) }

	status, err := os.ReadFile(filepath.Join(p, "status"))
	if err != nil {
		// Not a finding about a layer — the process is gone or unreadable,
		// and the jail cannot be confirmed either way. Say so, loudly: an
		// unverifiable jail is not a verified one.
		add("VMM verification", fmt.Sprintf("pid %d unreadable (%v), so its jail could not be confirmed", pid, err))
		return fs
	}
	field := func(name string) string {
		for _, line := range strings.Split(string(status), "\n") {
			if rest, ok := strings.CutPrefix(line, name+":"); ok {
				return strings.TrimSpace(rest)
			}
		}
		return ""
	}

	// chroot: the VMM's root is its own jail dir.
	root, _ := os.Readlink(filepath.Join(p, "root"))
	want := fcJailDir(g)
	if w, err := filepath.EvalSymlinks(want); err == nil {
		want = w
	}
	if root != want && root != fcJailDir(g) {
		add("VMM chroot", fmt.Sprintf("root is %q, not the jail %q: the VMM sees the host filesystem", root, fcJailDir(g)))
	}

	// uid: the per-VM id, never the daemon's own. Uid: lists real, effective,
	// saved, fs — all four must be the jail uid.
	if uids := strings.Fields(field("Uid")); len(uids) != 4 || uids[0] != strconv.Itoa(jailUID) ||
		uids[1] != uids[0] || uids[2] != uids[0] || uids[3] != uids[0] {
		add("VMM per-VM uid", fmt.Sprintf("uids are %q, want %d throughout: the VMM does not run as its own unprivileged id", field("Uid"), jailUID))
	}

	if v := field("NoNewPrivs"); v != "1" {
		add("VMM no_new_privs", "NoNewPrivs is "+strconv.Quote(v)+": an exec from the VMM could gain privileges")
	}
	if v := field("Seccomp"); v != "2" {
		add("Firecracker seccomp", "Seccomp is "+strconv.Quote(v)+", not 2 (filter): the VMM's own syscall filter is not installed")
	}

	// network namespace: the VMM's must differ from the daemon's.
	vmNet, err1 := os.Readlink(filepath.Join(p, "ns", "net"))
	selfNet, err2 := os.Readlink(filepath.Join(postureProc, "self", "ns", "net"))
	if err1 != nil || err2 != nil || vmNet == selfNet {
		add("VMM network namespace", fmt.Sprintf("VMM net ns %q, daemon %q: the VMM shares the host network", vmNet, selfNet))
	}

	// nice: /proc/<pid>/stat field 19. The comm field (2) can contain spaces
	// and parens, so parse from the LAST ')'.
	if b, err := os.ReadFile(filepath.Join(p, "stat")); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			// After ") " the fields are state(3), ppid(4) … nice(19): index 16.
			if f := strings.Fields(s[i+1:]); len(f) > 16 {
				if n, err := strconv.Atoi(f[16]); err == nil && n < fcVMNice {
					add("VMM nice", fmt.Sprintf("nice is %d, want %d: a VM spinning its vCPUs competes with the daemon and proxy at equal priority", n, fcVMNice))
				}
			}
		}
	}

	// cgroup: when per-VM caps are on at all, the VMM must be in its leaf.
	// (When they are off, postureStartup has already said so once.)
	if fcCgroupOn {
		leaf := strings.TrimPrefix(filepath.Join(fcCgroupVMs, g), fcCgroupMount)
		b, _ := os.ReadFile(filepath.Join(p, "cgroup"))
		in := false
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if path, ok := strings.CutPrefix(line, "0::"); ok && path == leaf {
				in = true
			}
		}
		if !in {
			add("VMM cgroup placement", fmt.Sprintf("the VMM is not in %s (%q): its cpu.weight and memory.high do not apply", leaf, strings.TrimSpace(string(b))))
		}
	}
	return fs
}

// postureSeccompMode reads the Seccomp: field of /proc/<pid>/status (0 none,
// 1 strict, 2 filter).
func postureSeccompMode(pid string) (int, error) {
	b, err := os.ReadFile(filepath.Join(postureProc, pid, "status"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Seccomp:"); ok {
			return strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	return 0, fmt.Errorf("no Seccomp field")
}
