package main

// Inbox attribution: a daemon-raised notification ABOUT a group (a group-
// attributed error log line, a posture finding, a per-group resource alert)
// is filed in the inbox under that group with source "daemon", while its
// banner still rides main's stream. A group's own cs-notify is filed under
// the group with source "guest".

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// refillLogAlerts undoes inboxRig's quietLogAlerts for one test, so
// forwardLogAlert reaches notifyDeliver.
func refillLogAlerts(t *testing.T) {
	t.Helper()
	prev := logAlertGlobal
	logAlertGlobal = &notifyBucket{tokens: logAlertGlobalBurst, last: time.Now()}
	logAlertMu.Lock()
	for k := range logAlertBuckets {
		delete(logAlertBuckets, k)
	}
	logAlertMu.Unlock()
	t.Cleanup(func() { logAlertGlobal = prev })
}

func noteByTitle(t *testing.T, title string) inboxNote {
	t.Helper()
	for _, n := range inboxNotesSnapshot() {
		if n.Title == title {
			return n
		}
	}
	t.Fatalf("no inbox note titled %q in %+v", title, inboxNotesSnapshot())
	return inboxNote{}
}

// queuedOn: a [[notify]] marker titled title is queued on g's stream.
func queuedOn(g, title string) bool {
	needle := title
	notifyQueueMu.Lock()
	defer notifyQueueMu.Unlock()
	for _, m := range notifyQueue[g] {
		if strings.Contains(m, " "+base64.StdEncoding.EncodeToString([]byte(needle))+" ") {
			return true
		}
	}
	return false
}

func TestInboxLogAlertAttributedToSubjectGroup(t *testing.T) {
	inboxRig(t)
	refillLogAlerts(t)
	forwardLogAlert("prompt", "tg", "error", "sonnet turn failed")
	forwardLogAlert("daemon", "", "error", "global failure")

	n := noteByTitle(t, "ERROR prompt [tg]")
	if n.Group != "tg" || n.Source != inboxSourceDaemon || n.Session != "" || n.Severity != "high" {
		t.Fatalf("group alert filed as %+v, want group=tg source=daemon high", n)
	}
	// The banner still goes to main's stream, not the subject group's.
	if !queuedOn("main", "ERROR prompt [tg]") || queuedOn("tg", "ERROR prompt [tg]") {
		t.Fatal("banner for a group alert must ride main's stream only")
	}
	if g := noteByTitle(t, "ERROR daemon"); g.Group != "main" || g.Source != inboxSourceDaemon {
		t.Fatalf("daemon-global alert filed as %+v, want main/daemon", g)
	}
}

func TestInboxPostureAndResourceAttributed(t *testing.T) {
	inboxRig(t)
	postureAlert("peer", []postureFinding{{what: "no seccomp", detail: "x"}})
	resNotifyOperator(1, "tg", "Group tg disk 82% full", "watch")
	resNotifyOperator(2, "", "Host disk 91% full", "reclaim")

	for title, want := range map[string]string{
		postureTitlePrefix + "no seccomp [peer]": "peer",
		"Group tg disk 82% full":                 "tg",
		"Host disk 91% full":                     "main",
	} {
		if n := noteByTitle(t, title); n.Group != want || n.Source != inboxSourceDaemon {
			t.Fatalf("%q filed as %+v, want group=%s source=daemon", title, n, want)
		}
	}
}

// An invalid subject falls back to the banner group; a session is never
// carried across to a different group.
func TestNotifyDeliverAboutFallbacks(t *testing.T) {
	inboxRig(t)
	notifyDeliverAbout("main", "../evil", inboxSourceDaemon, "high", "", "bad about", "")
	notifyDeliverAbout("main", "tg", inboxSourceDaemon, "normal", "s1", "cross", "")
	notifyDeliverAbout("main", "", "bogus", "normal", "", "bad source", "")
	if n := noteByTitle(t, "bad about"); n.Group != "main" {
		t.Fatalf("invalid about filed under %q", n.Group)
	}
	if n := noteByTitle(t, "cross"); n.Group != "tg" || n.Session != "" {
		t.Fatalf("cross-group note = %+v, want tg with no session", n)
	}
	if n := noteByTitle(t, "bad source"); n.Source != "" {
		t.Fatalf("unknown source stored as %q", n.Source)
	}
}

func TestInboxCtlNotifyIsGuestSourced(t *testing.T) {
	inboxRig(t)
	resp := ctlDispatch("tg", notifyLine(t, "normal", "built it", "ok"))
	if br, ok := resp.(baseResp); !ok || !br.OK {
		t.Fatalf("ctl notify: %+v", resp)
	}
	if n := noteByTitle(t, "built it"); n.Group != "tg" || n.Source != inboxSourceGuest {
		t.Fatalf("cs-notify filed as %+v, want tg/guest", n)
	}
	// ...and the wire item carries the source.
	for _, it := range inboxCall(t, context.Background()) {
		if nn := it.GetNotification(); nn != nil && nn.GetTitle() == "built it" {
			if nn.GetSource() != "guest" || nn.GetGroup() != "tg" {
				t.Fatalf("wire item = %v", nn)
			}
			return
		}
	}
	t.Fatal("ctl notification missing from Inbox")
}

// A watcher scoped to the subject group sees the daemon alert about it; a
// watcher scoped to main only no longer does.
func TestInboxDaemonAlertProjectedToSubjectWatcher(t *testing.T) {
	inboxRig(t)
	refillLogAlerts(t)
	inboxACL(t, `{"tgrole":{"inbox":["tg"]},"mainrole":{"inbox":["main"]}}`)
	forwardLogAlert("ensure", "tg", "error", "spawn failed")
	saw := func(ctx context.Context) bool {
		for _, it := range inboxCall(t, ctx) {
			if n := it.GetNotification(); n != nil && n.GetTitle() == "ERROR ensure [tg]" {
				return n.GetSource() == "daemon"
			}
		}
		return false
	}
	if !saw(inboxCtx("tgrole")) {
		t.Fatal("tg-scoped role must see the daemon alert about tg (source=daemon)")
	}
	if saw(inboxCtx("mainrole")) {
		t.Fatal("main-scoped role must not see an alert filed under tg")
	}
	if !saw(context.Background()) {
		t.Fatal("operator must see it")
	}
}

// source persists through inbox.json and is clamped on load.
func TestInboxSourcePersistsAndClamps(t *testing.T) {
	inboxRig(t)
	notifyDeliverAbout("main", "tg", inboxSourceDaemon, "high", "", "persist me", "")
	b, err := os.ReadFile(inboxFile())
	if err != nil {
		t.Fatal(err)
	}
	var in []inboxNote
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	in = append(in, inboxNote{ID: "n1-1", Group: "peer", Severity: "normal", Title: "forged", Ts: 1, Source: "root"})
	out, _ := json.Marshal(in)
	if err := os.WriteFile(inboxFile(), out, 0o600); err != nil {
		t.Fatal(err)
	}
	inboxMu.Lock()
	inboxLoaded, inboxNotes = "", nil
	inboxMu.Unlock()
	if n := noteByTitle(t, "persist me"); n.Group != "tg" || n.Source != inboxSourceDaemon {
		t.Fatalf("reloaded %+v", n)
	}
	if n := noteByTitle(t, "forged"); n.Source != "" {
		t.Fatalf("unknown source survived load: %q", n.Source)
	}
}
