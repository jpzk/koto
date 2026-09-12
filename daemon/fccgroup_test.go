package main

// fccgroup_test.go — per-VM cgroup placement, in particular the two modes it
// has to pick between. No real cgroup tree: fcCgroupVMs is a var, so a temp
// dir stands in for the delegated vms/ parent and the files are just files.

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeCgroupVMs points the cgroup state at a temp dir for one test.
func fakeCgroupVMs(t *testing.T, clone3 bool) string {
	t.Helper()
	onWas, vmsWas, c3Was := fcCgroupOn, fcCgroupVMs, fcCgroupClone3
	t.Cleanup(func() { fcCgroupOn, fcCgroupVMs, fcCgroupClone3 = onWas, vmsWas, c3Was })
	fcCgroupOn, fcCgroupVMs, fcCgroupClone3 = true, t.TempDir(), clone3
	return fcCgroupVMs
}

// TestCgroupCreateWithoutClone3 pins the contract fcStart reads: no fd when
// clone-time placement is unavailable, but the leaf is still made and capped,
// so the post-fork path has somewhere to put the pid. Getting this wrong is
// how a seccomp-filtered host loses its per-VM caps silently.
func TestCgroupCreateWithoutClone3(t *testing.T) {
	vms := fakeCgroupVMs(t, false)
	fd, err := fcCgroupCreate("g", 4, 2048)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fd != -1 {
		syscall.Close(fd)
		t.Fatalf("fd = %d, want -1 when clone3 is unavailable", fd)
	}
	weight, err := os.ReadFile(filepath.Join(vms, "g", "cpu.weight"))
	if err != nil {
		t.Fatalf("leaf not configured: %v", err)
	}
	if got, want := string(weight), strconv.Itoa(4*fcCgroupWeightPerVCPU); got != want {
		t.Errorf("cpu.weight = %q, want %q", got, want)
	}
}

// TestCgroupPlaceWritesPid is the fallback itself: the pre-clone3 way of
// putting a process in a cgroup.
func TestCgroupPlaceWritesPid(t *testing.T) {
	vms := fakeCgroupVMs(t, false)
	if _, err := fcCgroupCreate("g", 2, 1024); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := fcCgroupPlace("g", 4242); err != nil {
		t.Fatalf("place: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(vms, "g", "cgroup.procs"))
	if err != nil {
		t.Fatalf("cgroup.procs: %v", err)
	}
	if string(b) != "4242" {
		t.Errorf("cgroup.procs = %q, want %q", b, "4242")
	}
}

// TestCgroupPlaceOffIsNoop — with cgroups off there is no tree to write to and
// a spawn must not be disturbed by saying so.
func TestCgroupPlaceOffIsNoop(t *testing.T) {
	onWas := fcCgroupOn
	t.Cleanup(func() { fcCgroupOn = onWas })
	fcCgroupOn = false
	if err := fcCgroupPlace("g", 1); err != nil {
		t.Fatalf("place with cgroups off: %v", err)
	}
}

// TestClone3ProbeIsSideEffectFree pins the property the probe rests on: a
// zero-sized args struct is rejected by the kernel's first argument check, so
// the probe cannot fork. A regression here would spawn a clone of the test
// binary on every daemon start.
func TestClone3ProbeIsSideEffectFree(t *testing.T) {
	r1, _, errno := syscall.RawSyscall(unix.SYS_CLONE3, 0, 0, 0)
	switch errno {
	case syscall.EINVAL:
		// The expected answer on any kernel that has clone3.
		if !fcClone3Available() {
			t.Error("fcClone3Available() = false though clone3 answered EINVAL")
		}
	case syscall.ENOSYS:
		// No clone3 (old kernel, or a seccomp filter — which is the case this
		// whole fallback exists for). Nothing forked either way.
		if fcClone3Available() {
			t.Error("fcClone3Available() = true though clone3 answered ENOSYS")
		}
	default:
		t.Fatalf("clone3(NULL, 0) returned %d, errno %v — want EINVAL or ENOSYS, "+
			"anything else may mean it cloned", r1, errno)
	}
}
