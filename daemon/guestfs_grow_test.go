package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workspace image is written by the guest, so every byte of its ext4
// metadata is guest-controlled. Growing it must not run a filesystem parser on
// the host: the host only extends the backing file and the guest grows the
// filesystem online after it mounts the disk (fcguest growWorkspace).
//
// The exec seam is PATH: shims for every e2fsprogs tool that could parse the
// image record each invocation, so any exec of them from the grow path shows
// up in the log regardless of how it is spelled.
func TestFcGrowWorkspaceImgOnlyTruncates(t *testing.T) {
	fcHarness(t)
	const g = "growg"

	shims := t.TempDir()
	execLog := filepath.Join(t.TempDir(), "exec.log")
	for _, tool := range []string{"e2fsck", "fsck.ext4", "fsck", "resize2fs", "debugfs", "dumpe2fs", "tune2fs", "e2image"} {
		script := "#!/bin/sh\necho \"" + tool + " $*\" >> '" + execLog + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(shims, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := fcHostDiskAdmit(g, "growing the workspace image"); err != nil {
		t.Skipf("test host filesystem refuses admission, grow path not reachable: %v", err)
	}

	dir := filepath.Join(ROOT, g)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "workspace.img")
	const cur, target = int64(16 << 20), int64(64 << 20)
	// Contents are irrelevant to the host now; mark a byte so we can check it
	// survived untouched.
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("guest-bytes"), 1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(cur); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := fcGrowWorkspaceImg(g, img, cur, target); err != nil {
		t.Fatalf("grow failed: %v", err)
	}
	fi, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != target {
		t.Errorf("image size after grow = %d, want %d (truncate)", fi.Size(), target)
	}
	buf := make([]byte, 11)
	if rf, err := os.Open(img); err == nil {
		_, _ = rf.ReadAt(buf, 1024)
		rf.Close()
	}
	if string(buf) != "guest-bytes" {
		t.Errorf("grow modified image contents: %q", buf)
	}
	if b, err := os.ReadFile(execLog); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		t.Errorf("grow path executed a filesystem tool on the guest-authored image:\n%s", b)
	}

	// Shrink / equal stays a no-op and execs nothing either.
	if err := fcGrowWorkspaceImg(g, img, target, cur); err != nil {
		t.Errorf("shrink request errored: %v", err)
	}
	if fi, _ := os.Stat(img); fi == nil || fi.Size() != target {
		t.Errorf("shrink request changed the image size")
	}
}

// The host preflight must not demand tools the daemon no longer runs.
func TestRuntimeToolsNoLongerRequireHostFsck(t *testing.T) {
	for _, c := range checkRuntimeTools() {
		if c.name == "e2fsck" || c.name == "resize2fs" {
			t.Errorf("runtime preflight still requires %q on the host", c.name)
		}
	}
}
