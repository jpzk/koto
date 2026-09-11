package main

// 2026-09-11 M138: mountWorkspace ran `mkfs.ext4 -F` on ANY non-nil error from
// the first mount, so a dirty journal, a missing ext4 module, a wrong flag or
// an e2fsck-able inconsistency was answered by destroying the group's whole
// persistent workspace — transcripts, session ids, memory, jobs, uploads. The
// decision now turns on workspaceIsBlank, which says yes only for a device that
// has never been written, i.e. the one state in which formatting costs nothing.

import (
	"os"
	"path/filepath"
	"testing"
)

func writeDev(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWorkspaceIsBlankOnlyForAnUntouchedDevice(t *testing.T) {
	// A freshly truncated sparse image: zeros throughout. The only case the
	// mkfs fallback may fire in.
	if ok, err := workspaceIsBlank(writeDev(t, "fresh.img", make([]byte, wsBlankProbe+4096))); err != nil || !ok {
		t.Errorf("a zero-filled device: blank=%v err=%v, want blank", ok, err)
	}

	// An ext4 superblock lives at offset 1024 with its magic at 0x438. Anything
	// like this must never be reformatted.
	ext4 := make([]byte, wsBlankProbe+4096)
	ext4[0x438], ext4[0x439] = 0x53, 0xEF
	if ok, err := workspaceIsBlank(writeDev(t, "ext4.img", ext4)); err != nil || ok {
		t.Errorf("a device with an ext4 superblock: blank=%v err=%v, want NOT blank", ok, err)
	}

	// The case the old code could not tell from "no filesystem": a damaged
	// superblock over data that is still there. Backup superblocks make this
	// recoverable — reformatting makes it not.
	damaged := make([]byte, wsBlankProbe+4096)
	copy(damaged[64<<10:], []byte("the group's transcripts live here"))
	if ok, err := workspaceIsBlank(writeDev(t, "damaged.img", damaged)); err != nil || ok {
		t.Errorf("a device with no superblock but with data: blank=%v err=%v, want NOT blank", ok, err)
	}

	// A device too small to probe is not something to guess about: an error,
	// which mountWorkspace turns into a refusal to format rather than a format.
	if ok, err := workspaceIsBlank(writeDev(t, "tiny.img", make([]byte, 4096))); err == nil || ok {
		t.Errorf("a short device: blank=%v err=%v, want an error and NOT blank", ok, err)
	}
	if ok, err := workspaceIsBlank(filepath.Join(t.TempDir(), "absent.img")); err == nil || ok {
		t.Errorf("a missing device: blank=%v err=%v, want an error and NOT blank", ok, err)
	}
}
