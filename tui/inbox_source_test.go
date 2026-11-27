package main

import (
	"strings"
	"testing"

	"koto-protocol/pb"

	"github.com/charmbracelet/x/ansi"
)

// source: the daemon files its alerts under the group they are about and
// tags them source=daemon; the TUI carries the field and leads the selected
// notification's detail with it.

func TestInboxSourceCarriedAndShown(t *testing.T) {
	alert := &pb.InboxItem{Item: &pb.InboxItem_Notification{Notification: &pb.InboxNotification{
		Id: "n9", Group: "alpha", Severity: "high", Title: "ERROR prompt [alpha]",
		Body: "turn failed", Ts: 900, Source: "daemon"}}}
	guest := &pb.InboxItem{Item: &pb.InboxItem_Notification{Notification: &pb.InboxNotification{
		Id: "n8", Group: "alpha", Session: "goal-x", Severity: "normal", Title: "built",
		Body: "ok", Ts: 800, Source: "guest\x1b[31m"}}}
	legacy := ibNote("n7", 700, "old")

	es := pbToInbox(&pb.InboxResp{Items: []*pb.InboxItem{alert, guest, legacy}})
	if len(es) != 3 || es[0].source != "daemon" || es[0].group != "alpha" || es[2].source != "" {
		t.Fatalf("entries = %+v", es)
	}
	if strings.ContainsRune(es[1].source, 0x1b) {
		t.Fatalf("source not scrubbed: %q", es[1].source)
	}
	if got := inboxDetailText(es[0]); got != "from daemon · turn failed" {
		t.Fatalf("detail = %q", got)
	}
	if got := inboxDetailText(es[2]); got != "details here" {
		t.Fatalf("legacy detail = %q", got)
	}

	m := openList(t, ibModel(t, alert, legacy))
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "alpha · —") || !strings.Contains(v, "from daemon · turn failed") {
		t.Fatalf("view missing attributed alert/source:\n%s", v)
	}
}
