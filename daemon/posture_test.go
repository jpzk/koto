package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeVMM is one process in a fake procfs, described by what the kernel would
// report for it.
type fakeVMM struct {
	uid        string // the whole Uid: line value
	nnp, secc  string
	root       string // /proc/<pid>/root target
	net        string // /proc/<pid>/ns/net target
	nice       int
	cgroupLine string
}

// postureFixture points postureProc, SOCK_DIR and the cgroup globals at a temp
// tree and restores them afterwards.
func postureFixture(t *testing.T) string {
	t.Helper()
	proc := t.TempDir()
	prevProc, prevSock := postureProc, SOCK_DIR
	prevOn, prevVMs, prevMount := fcCgroupOn, fcCgroupVMs, fcCgroupMount
	t.Cleanup(func() {
		postureProc, SOCK_DIR = prevProc, prevSock
		fcCgroupOn, fcCgroupVMs, fcCgroupMount = prevOn, prevVMs, prevMount
	})
	postureProc = proc
	SOCK_DIR = t.TempDir()
	fcCgroupMount = "/sys/fs/cgroup"
	fcCgroupVMs = "/sys/fs/cgroup/koto.service/vms"
	fcCgroupOn = true
	// The daemon's own net namespace.
	mustMkdir(t, filepath.Join(proc, "self", "ns"))
	mustSymlink(t, "net:[1000]", filepath.Join(proc, "self", "ns", "net"))
	return proc
}

func writeFakeVMM(t *testing.T, proc string, pid int, v fakeVMM) {
	t.Helper()
	d := filepath.Join(proc, fmt.Sprint(pid))
	mustMkdir(t, filepath.Join(d, "ns"))
	status := fmt.Sprintf("Name:\tfirecracker\nUid:\t%s\nNoNewPrivs:\t%s\nSeccomp:\t%s\n", v.uid, v.nnp, v.secc)
	mustWrite(t, filepath.Join(d, "status"), status)
	// comm deliberately contains a space and a paren, which is why the parser
	// anchors on the LAST ')'.
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[16] = fmt.Sprint(v.nice)
	mustWrite(t, filepath.Join(d, "stat"), fmt.Sprintf("%d (fire cra)cker) %s\n", pid, strings.Join(fields, " ")))
	mustWrite(t, filepath.Join(d, "cgroup"), v.cgroupLine+"\n")
	mustSymlink(t, v.root, filepath.Join(d, "root"))
	mustSymlink(t, v.net, filepath.Join(d, "ns", "net"))
}

func goodVMM(g string, uid int) fakeVMM {
	return fakeVMM{
		uid: fmt.Sprintf("%d\t%d\t%d\t%d", uid, uid, uid, uid), nnp: "1", secc: "2",
		root: fcJailDir(g), net: "net:[2000]", nice: fcVMNice,
		cgroupLine: "0::/koto.service/vms/" + g,
	}
}

func TestPostureVMAcceptsAFullyJailedVMM(t *testing.T) {
	proc := postureFixture(t)
	writeFakeVMM(t, proc, 4242, goodVMM("alpha", 30004))
	if fs := postureVMFindings("alpha", 4242, 30004); len(fs) != 0 {
		t.Fatalf("a fully jailed VMM produced findings: %+v", fs)
	}
}

// Every layer missing at once must be reported layer by layer, so the banner
// names exactly what is not in effect.
func TestPostureVMNamesEveryMissingLayer(t *testing.T) {
	proc := postureFixture(t)
	writeFakeVMM(t, proc, 4243, fakeVMM{
		uid: "1000\t1000\t1000\t1000", nnp: "0", secc: "0",
		root: "/", net: "net:[1000]", nice: 0,
		cgroupLine: "0::/user.slice/session-1.scope",
	})
	got := map[string]bool{}
	for _, f := range postureVMFindings("beta", 4243, 30005) {
		got[f.what] = true
	}
	for _, want := range []string{"VMM chroot", "VMM per-VM uid", "VMM no_new_privs",
		"Firecracker seccomp", "VMM network namespace", "VMM nice", "VMM cgroup placement"} {
		if !got[want] {
			t.Errorf("missing finding %q; got %v", want, got)
		}
	}
	if len(got) != 7 {
		t.Errorf("got %d findings, want exactly 7: %v", len(got), got)
	}
}

// A partially dropped uid (effective = jail, saved = daemon) is still a
// finding: the process could switch back.
func TestPostureVMRejectsAPartialUidDrop(t *testing.T) {
	proc := postureFixture(t)
	v := goodVMM("gamma", 30006)
	v.uid = "30006\t30006\t0\t30006"
	writeFakeVMM(t, proc, 4244, v)
	fs := postureVMFindings("gamma", 4244, 30006)
	if len(fs) != 1 || fs[0].what != "VMM per-VM uid" {
		t.Fatalf("findings = %+v, want exactly the uid finding", fs)
	}
}

// An unreadable VMM cannot be confirmed jailed, and that is itself loud.
func TestPostureVMCannotVerifyIsAFinding(t *testing.T) {
	postureFixture(t)
	fs := postureVMFindings("delta", 99999, 30007)
	if len(fs) != 1 || fs[0].what != "VMM verification" {
		t.Fatalf("findings = %+v, want one VMM verification finding", fs)
	}
}

// With cgroups off the per-VM placement check is skipped: postureStartup
// reports the missing caps once, not once per VM.
func TestPostureVMSkipsPlacementWhenCgroupsAreOff(t *testing.T) {
	proc := postureFixture(t)
	fcCgroupOn = false
	v := goodVMM("eps", 30008)
	v.cgroupLine = "0::/somewhere/else"
	writeFakeVMM(t, proc, 4245, v)
	if fs := postureVMFindings("eps", 4245, 30008); len(fs) != 0 {
		t.Fatalf("findings = %+v, want none", fs)
	}
}

func postureStartupFixture(t *testing.T, seccomp string) {
	t.Helper()
	proc := postureFixture(t)
	mustWrite(t, filepath.Join(proc, "self", "status"), "Name:\tkoto\nSeccomp:\t"+seccomp+"\n")
	prevCap, prevUnknown, prevErr, prevReason := fcHostMemCapMiB, fcHostMemUnknown, fcHostMemCapErr, fcCgroupOffReason
	t.Cleanup(func() {
		fcHostMemCapMiB, fcHostMemUnknown, fcHostMemCapErr, fcCgroupOffReason = prevCap, prevUnknown, prevErr, prevReason
	})
}

func TestPostureStartupSilentWhenEverythingIsActive(t *testing.T) {
	postureStartupFixture(t, "2")
	fcCgroupOn, fcHostMemCapMiB, fcHostMemUnknown, fcHostMemCapErr = true, 20000, false, ""
	t.Setenv("KOTO_BIND", "127.0.0.1")
	t.Setenv("INVOCATION_ID", "abc")
	if fs := postureStartupFindings(); len(fs) != 0 {
		t.Fatalf("an intact host produced findings: %+v", fs)
	}
}

func TestPostureStartupNamesEveryMissingLayer(t *testing.T) {
	postureStartupFixture(t, "0")
	fcCgroupOn, fcCgroupOffReason = false, "mkdir: permission denied"
	fcHostMemCapMiB, fcHostMemUnknown = 0, false
	t.Setenv("KOTO_BIND", "0.0.0.0")
	t.Setenv("INVOCATION_ID", "")
	got := map[string]string{}
	for _, f := range postureStartupFindings() {
		got[f.what] = f.detail
	}
	for _, want := range []string{"per-VM cgroup caps", "fleet memory cap", "host sandbox", "loopback-only gRPC"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing finding %q; got %v", want, got)
		}
	}
	if !strings.Contains(got["per-VM cgroup caps"], "permission denied") {
		t.Errorf("cgroup finding does not carry the reason: %q", got["per-VM cgroup caps"])
	}
	if !strings.Contains(got["host sandbox"], "not running under koto.service") {
		t.Errorf("sandbox finding does not say why: %q", got["host sandbox"])
	}
}

// The kernel-side cap failing is its own finding, distinct from "unlimited".
func TestPostureStartupReportsAFailedKernelMemoryCap(t *testing.T) {
	postureStartupFixture(t, "2")
	fcCgroupOn, fcHostMemCapMiB, fcHostMemUnknown = true, 20000, false
	fcHostMemCapErr = "memory.max: permission denied"
	t.Setenv("KOTO_BIND", "::1")
	fs := postureStartupFindings()
	if len(fs) != 1 || fs[0].what != "kernel fleet memory cap" {
		t.Fatalf("findings = %+v, want exactly the kernel cap finding", fs)
	}
}

// Findings reach the operator as ONE high notification with the posture title,
// plus an error line each in the daemon log.
func TestPostureAlertIsOneHighNotification(t *testing.T) {
	setupNotifyRoot(t, ctlMainGroup)
	postureAlert("zeta", []postureFinding{{"VMM chroot", "root is /"}, {"VMM nice", "nice is 0"}})
	deliverNotify(ctlMainGroup)

	var notes []Event
	for _, e := range readGroupLog(t, ctlMainGroup) {
		if e.Event == "notification" {
			notes = append(notes, e)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notifications, want 1: %+v", len(notes), notes)
	}
	n := notes[0]
	if n.Severity != "high" || !strings.HasPrefix(n.Title, postureTitlePrefix) ||
		!strings.Contains(n.Title, "VMM chroot") || !strings.Contains(n.Title, "VMM nice") || !strings.Contains(n.Title, "[zeta]") {
		t.Errorf("notification = %+v", n)
	}

	logSubsLock.Lock()
	errs := 0
	for _, le := range logRing {
		if le.Subsystem == "posture" && le.Level == "error" && strings.Contains(le.Msg, "[zeta]") {
			errs++
		}
	}
	logSubsLock.Unlock()
	if errs != 2 {
		t.Errorf("got %d posture error lines for zeta, want 2", errs)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}
