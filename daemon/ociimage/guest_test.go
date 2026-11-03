package ociimage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeSuper(t *testing.T, p, label string, size int) {
	t.Helper()
	b := make([]byte, size)
	b[1024+0x38], b[1024+0x39] = 0x53, 0xEF
	copy(b[1024+0x78:1024+0x88], label)
	if err := os.WriteFile(p, b, 0o444); err != nil {
		t.Fatal(err)
	}
}

func TestShippedGuestNoAgentIsTheAsset(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "guest.img")
	writeSuper(t, p, GuestLabel, 2048)
	got, err := ShippedGuest{Path: p}.Ensure(filepath.Join(d, "cache"), nil)
	if err != nil || got != p {
		t.Fatalf("Ensure = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(d, "cache")); err == nil {
		t.Fatal("no-agent Ensure wrote a cache copy")
	}
}

func TestShippedGuestRejects(t *testing.T) {
	d := t.TempDir()
	if _, err := (ShippedGuest{Path: filepath.Join(d, "nope")}).Ensure(d, nil); err == nil {
		t.Fatal("missing guest.img accepted")
	}
	p := filepath.Join(d, "guest.img")
	writeSuper(t, p, "somethingelse", 2048)
	if _, err := (ShippedGuest{Path: p}).Ensure(d, nil); err == nil || !strings.Contains(err.Error(), GuestLabel) {
		t.Fatalf("wrong label: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "raw"), make([]byte, 2048), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := CheckGuest(filepath.Join(d, "raw")); err == nil {
		t.Fatal("non-ext4 accepted")
	}
}

func TestShippedGuestKeyTracksAgent(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "guest.img")
	writeSuper(t, p, GuestLabel, 2048)
	a := filepath.Join(d, "agent")
	if err := os.WriteFile(a, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	k0, err := ShippedGuest{Path: p}.Key(d)
	if err != nil {
		t.Fatal(err)
	}
	k1, err := ShippedGuest{Path: p, Agent: a}.Key(d)
	if err != nil || k1 == k0 || !strings.HasPrefix(k1, k0+"-a") {
		t.Fatalf("keys %q %q %v", k0, k1, err)
	}
}

// Real ext4 round trip: the agent override replaces /bin/fc-agent in a
// cached, read-only copy and leaves the shipped file untouched.
func TestShippedGuestAgentOverride(t *testing.T) {
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	d := t.TempDir()
	tree := filepath.Join(d, "tree", "bin")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "fc-agent"), []byte("old-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "guest.img")
	if b, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-L", GuestLabel, "-d", filepath.Join(d, "tree"), p, "8M").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs: %v %s", err, b)
	}
	_ = os.Chmod(p, 0o444)
	before, _ := fileSha(p, filepath.Join(d, "memo"))
	a := filepath.Join(d, "agent")
	if err := os.WriteFile(a, []byte("new-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := ShippedGuest{Path: p, Agent: a}.Ensure(filepath.Join(d, "cache"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out == p || CheckGuest(out) != nil {
		t.Fatalf("derived %q", out)
	}
	got := filepath.Join(d, "dumped")
	if err := debugfsDump(out, "/bin/fc-agent", got); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); string(b) != "new-agent" {
		t.Fatalf("agent in derived image: %q", b)
	}
	_ = os.Remove(filepath.Join(d, "memo"))
	if after, _ := fileSha(p, filepath.Join(d, "memo")); after != before {
		t.Fatal("shipped guest.img modified")
	}
	again, err := ShippedGuest{Path: p, Agent: a}.Ensure(filepath.Join(d, "cache"), nil)
	if err != nil || again != out {
		t.Fatalf("second Ensure %q %v", again, err)
	}
}
