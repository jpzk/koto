package ociimage

// ShippedGuest: guest.img as its own release asset (fcassets/guest.img,
// guest.img_<ver>_x86_64.zst in SHA256SUMS). It replaces RootfsTools, which
// derived the 1.8.0 tools.img from rootfs.img; RootfsTools stays only as the
// fallback for a host that has rootfs.img but no guest.img yet (one release).

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// guestAgentPath is where fc-agent lives inside guest.img.
const guestAgentPath = "/bin/fc-agent"

// ShippedGuest serves the shipped guest.img. Agent, when set, replaces
// /bin/fc-agent in a cached copy (dev builds / e2e, KOTO_IMAGE_AGENT).
type ShippedGuest struct {
	Path  string
	Agent string
}

// GuestPath is where a derived guest image with key k lives.
func GuestPath(cache, key string) string { return filepath.Join(cache, "guest", key+".img") }

// CheckGuest verifies p is an ext4 image carrying the guest label.
func CheckGuest(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("guest.img: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("guest.img: %s is not a regular file", p)
	}
	lbl, _, err := ReadSuper(p)
	if err != nil {
		return fmt.Errorf("guest.img: %w", err)
	}
	if lbl != GuestLabel {
		return fmt.Errorf("guest.img: %s has label %q, want %q", p, lbl, GuestLabel)
	}
	return nil
}

// Key identifies a derived image: guest sha + agent sha.
func (g ShippedGuest) Key(cache string) (string, error) {
	gs, err := fileSha(g.Path, filepath.Join(cache, "guest", ".guest.sha"))
	if err != nil {
		return "", fmt.Errorf("guest.img: %w", err)
	}
	k := "g-" + gs[:16]
	if g.Agent != "" {
		as, err := fileSha(g.Agent, filepath.Join(cache, "guest", ".agent.sha"))
		if err != nil {
			return "", fmt.Errorf("guest.img agent override: %w", err)
		}
		k += "-a" + as[:12]
	}
	return k, nil
}

// Ensure returns the image to attach as vda. Without an agent override that
// is the shipped file itself (attached read-only, never written).
func (g ShippedGuest) Ensure(cache string, logf func(string, ...any)) (string, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := CheckGuest(g.Path); err != nil {
		return "", err
	}
	if g.Agent == "" {
		return g.Path, nil
	}
	toolsMu.Lock()
	defer toolsMu.Unlock()
	key, err := g.Key(cache)
	if err != nil {
		return "", err
	}
	out := GuestPath(cache, key)
	if fi, err := os.Stat(out); err == nil && fi.Mode().Perm()&0o222 == 0 && CheckGuest(out) == nil {
		return out, nil
	}
	for _, tool := range []string{"debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			return "", fmt.Errorf("guest.img agent override needs e2fsprogs on the host (%s not found in PATH)", tool)
		}
	}
	t0 := time.Now()
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	img := out + ".part"
	_ = os.Remove(img)
	if err := copyFile(g.Path, img); err != nil {
		return "", fmt.Errorf("guest.img copy: %w", err)
	}
	sp := img + ".debugfs"
	defer os.Remove(sp)
	sf, err := os.Create(sp)
	if err != nil {
		os.Remove(img)
		return "", err
	}
	s := &script{w: bufio.NewWriter(sf), nodes: map[string]*node{"/": {typ: 'd'}, "/bin": {typ: 'd'}}, links: map[string]int{}}
	qa, err := q(g.Agent)
	if err == nil {
		s.cd("/bin")
		s.emit("rm fc-agent")
		s.mk(guestAgentPath, 'f', "write %[2]s %[1]s", qa)
		s.meta(guestAgentPath, 0, 0, 0o100755, time.Unix(1700000000, 0))
		if s.err == nil {
			s.err = s.w.Flush()
		}
		err = s.err
	}
	sf.Close()
	if err != nil {
		os.Remove(img)
		return "", err
	}
	env := append(os.Environ(), "E2FSPROGS_FAKE_TIME="+fakeTime)
	if err := runDebugfs(sp, img, env); err != nil {
		os.Remove(img)
		return "", err
	}
	if b, err := exec.Command("e2fsck", "-fn", img).CombinedOutput(); err != nil {
		os.Remove(img)
		return "", fmt.Errorf("e2fsck: %v\n%s", err, b)
	}
	if err := os.Chmod(img, 0o444); err != nil {
		os.Remove(img)
		return "", err
	}
	if err := os.Rename(img, out); err != nil {
		os.Remove(img)
		return "", err
	}
	logf("guest.img %s derived with agent override in %s", key, time.Since(t0).Round(time.Millisecond))
	return out, nil
}
