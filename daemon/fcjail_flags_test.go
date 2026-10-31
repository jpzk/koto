package main

import (
	"syscall"
	"testing"
)

// A state dir on a nosuid,nodev,relatime mount (measured: /workspace in the
// e2e VM) made the read-only bind remount EPERM inside the jail's user
// namespace, because it dropped the locked flags. The remount must carry them.
func TestFcRemountROFlagsKeepsLockedFlags(t *testing.T) {
	base := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY)
	if got := fcRemountROFlags(0); got != base {
		t.Fatalf("plain mount: %#x, want %#x", got, base)
	}
	got := fcRemountROFlags(stNoSuid | stNoDev | stRelAtime)
	want := base | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_RELATIME
	if got != want {
		t.Fatalf("nosuid,nodev,relatime: %#x, want %#x", got, want)
	}
	got = fcRemountROFlags(stNoExec | stNoAtime | stNoDirAtime | 0x1 /* ST_RDONLY */)
	want = base | syscall.MS_NOEXEC | syscall.MS_NOATIME | syscall.MS_NODIRATIME
	if got != want {
		t.Fatalf("noexec,noatime,nodiratime: %#x, want %#x", got, want)
	}
}
