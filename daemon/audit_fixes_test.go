package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// The 2026-09-04 audit's mechanical fixes, pinned. Each test names the
// finding it covers.

// H2: the unspecified address is control plane at L7 — literal, v6, mapped,
// and via a name that resolves to it — and the dial guard refuses it too.
func TestEgressUnspecifiedIsControlPlane(t *testing.T) {
	orig := egressLookupIP
	egressLookupIP = func(host string) ([]net.IP, error) {
		if host == "any.example" {
			return []net.IP{net.ParseIP("0.0.0.0")}, nil
		}
		return nil, fmt.Errorf("nxdomain")
	}
	defer func() { egressLookupIP = orig }()
	for _, hp := range []string{"0.0.0.0:8788", "[::]:8788", "[::ffff:0.0.0.0]:80", "any.example:8788"} {
		for _, pol := range []string{fcNetWAN, fcNetLAN, fcNetFull} {
			if ok, _ := egressTargetAllowed(hp, pol); ok {
				t.Errorf("egressTargetAllowed(%s, %s) = true; the unspecified address connects locally", hp, pol)
			}
		}
	}
	for _, hp := range []string{"0.0.0.0:1", "127.0.0.1:1", "[::1]:1", "[::]:1", "example.com:443", "garbage"} {
		if ip := egressDialIP(hp); ip != nil && fcClassifyDst(ip) != fcDstCtl {
			t.Errorf("dial guard would let %s through", hp)
		}
	}
	if ip := egressDialIP("1.1.1.1:443"); ip == nil || fcClassifyDst(ip) != fcDstWAN {
		t.Error("dial guard rejects a plain public ip:port")
	}
}

// M9b: model/effort are identifiers, not free text.
func TestApplyConfigValidatesModelAndEffort(t *testing.T) {
	cfg := map[string]any{}
	raw := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	for _, ok := range []string{"claude-sonnet-5", "qwen/qwen3-235b", "kimi-k2.5", "gpt-4.1:free", "high"} {
		applyConfig(cfg, "model", raw(ok))
		if cfg["model"] != ok {
			t.Errorf("model %q rejected", ok)
		}
	}
	for _, bad := range []string{"", " ", "\x1b]0;pwned\x07", "a b", "-leading", strings.Repeat("x", 129), "x\ny"} {
		delete(cfg, "effort")
		applyConfig(cfg, "effort", raw(bad))
		if _, set := cfg["effort"]; set {
			t.Errorf("effort %q accepted", bad)
		}
	}
}

// M5: schedules are bounded per group, and messages in length.
func TestAddSchedCaps(t *testing.T) {
	prev := SCHED_FILE
	SCHED_FILE = filepath.Join(t.TempDir(), "schedules.json")
	t.Cleanup(func() { SCHED_FILE = prev })
	schedLock.Lock()
	sched, parsed = nil, map[string]parsedCron{}
	schedLock.Unlock()
	if _, err := addSched("g", "* * * * *", strings.Repeat("m", schedMaxMsg+1)); err == nil {
		t.Fatal("oversized msg accepted")
	}
	for i := 0; i < schedMaxPerGroup; i++ {
		if _, err := addSched("g", "* * * * *", "m"); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := addSched("g", "* * * * *", "m"); err == nil {
		t.Fatal("cap not enforced")
	}
	if _, err := addSched("other", "* * * * *", "m"); err != nil {
		t.Fatalf("cap leaked across groups: %v", err)
	}
}

// M4: buffered job results per (group, session) are bounded.
func TestRecordJobDoneBounded(t *testing.T) {
	notifyMu.Lock()
	notifyPending = map[string][]jobResult{}
	notifyMu.Unlock()
	for i := 0; i < notifyMaxPending*3; i++ {
		recordJobDone("g", jobResult{ID: fmt.Sprint(i), RC: "0"})
	}
	notifyMu.Lock()
	n := len(notifyPending[notifyKey("g", "")])
	for _, tm := range notifyTimers {
		tm.Stop()
	}
	notifyMu.Unlock()
	if n > notifyMaxPending {
		t.Fatalf("pending grew to %d (cap %d)", n, notifyMaxPending)
	}
}

// L5: marker bodies are cut on a rune boundary with a visible mark.
func TestTruncateBytes(t *testing.T) {
	if got := truncateBytes("short", 10); got != "short" {
		t.Errorf("short string changed: %q", got)
	}
	got := truncateBytes(strings.Repeat("é", 100), 11) // 2-byte runes; 11 is mid-rune
	if !strings.HasSuffix(got, "…[truncated]") || strings.Count(strings.TrimSuffix(got, "…[truncated]"), "é") != 5 {
		t.Errorf("truncateBytes cut mid-rune or lost the mark: %q", got)
	}
}

// L9: a pre-existing state dir must be ours, a real directory, and private.
func TestStateDirTrusted(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o750)
	fi, _ := os.Lstat(dir)
	if err := stateDirTrusted(fi, me); err != nil {
		t.Fatalf("own private dir refused: %v", err)
	}
	os.Chmod(dir, 0o777)
	fi, _ = os.Lstat(dir)
	if err := stateDirTrusted(fi, me); err == nil {
		t.Fatal("world-writable dir accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(dir, link)
	fi, _ = os.Lstat(link)
	if err := stateDirTrusted(fi, me); err == nil {
		t.Fatal("symlink accepted")
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	fi, _ = os.Lstat(f)
	if err := stateDirTrusted(fi, me); err == nil {
		t.Fatal("regular file accepted")
	}
}

// L4: a turn stream may open only on a slot the daemon handed out.
func TestExpectedTurnSlots(t *testing.T) {
	if fcConsumeExpectedTurn("g", 3) {
		t.Fatal("slot with no outstanding turn accepted")
	}
	fcExpectTurn("g", 3)
	if !fcConsumeExpectedTurn("g", 3) {
		t.Fatal("handed-out slot refused")
	}
	if fcConsumeExpectedTurn("g", 3) {
		t.Fatal("slot accepted twice")
	}
}
