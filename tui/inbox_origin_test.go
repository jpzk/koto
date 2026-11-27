package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Origin column: "group · session", "—" when the session is empty,
// padded to one width so kind/subject line up across rows.

func TestInboxOriginCell(t *testing.T) {
	for _, c := range []struct {
		e    inboxEntry
		want string
	}{
		{inboxEntry{group: "alpha", session: "goal-x"}, "alpha · goal-x"},
		{inboxEntry{group: "main"}, "main · —"},
		{inboxEntry{group: "beta", session: "a\nb"}, "beta · a b"},
	} {
		if got := inboxOrigin(c.e); got != c.want {
			t.Fatalf("origin %+v = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestInboxOriginColumnAligned(t *testing.T) {
	m := ibMixed(t)
	v := ansi.Strip(m.View())
	col := -1
	n := 0
	for _, l := range strings.Split(v, "\n") {
		var k int
		switch {
		case strings.Contains(l, "main · —"):
			k = ansi.StringWidth(l[:strings.Index(l, "main · —")])
		case strings.Contains(l, "alpha · goal-x"):
			k = ansi.StringWidth(l[:strings.Index(l, "alpha · goal-x")])
		case strings.Contains(l, "beta · —"):
			k = ansi.StringWidth(l[:strings.Index(l, "beta · —")])
		default:
			continue
		}
		n++
		if col >= 0 && k != col {
			t.Fatalf("origin column not aligned (%d vs %d):\n%s", k, col, v)
		}
		col = k
	}
	if n != 5 {
		t.Fatalf("want 5 rows with an origin cell, got %d:\n%s", n, v)
	}
	// NOTE/DECIDE sits right after the time; the origin column starts at the same cell on every row
	ow := inboxOriginWidth(m.inboxVisible(), m.width-3)
	if ow != ansi.StringWidth("alpha · goal-x") {
		t.Fatalf("origin width %d", ow)
	}
	for _, e := range m.inboxVisible() {
		r := inboxRowText(e, 200, ow)
		if got := ansi.StringWidth(r[:strings.Index(r, e.kindTag())]); got != 5+2 {
			t.Fatalf("type tag at cell %d in %q, want %d", got, r, 5+2)
		}
		if got := ansi.StringWidth(r[:strings.Index(r, e.subKind()+e.subject)]); got != 5+2+6+2+ow+2 {
			t.Fatalf("subject at cell %d in %q, want %d", got, r, 5+2+6+2+ow+2)
		}
	}
}

// A very long origin is truncated to a third of the row so the subject
// keeps room.
func TestInboxOriginColumnCapped(t *testing.T) {
	long := inboxEntry{group: strings.Repeat("g", 60), session: "s", subject: "the subject"}
	vis := []inboxEntry{long}
	ow := inboxOriginWidth(vis, 60)
	if ow != 20 {
		t.Fatalf("cap %d, want 20", ow)
	}
	r := inboxRowText(long, 60, ow)
	if !strings.Contains(r, "…") || !strings.Contains(r, "the subject") {
		t.Fatalf("row %q: want truncated origin and visible subject", r)
	}
}

func TestInboxTypeTag(t *testing.T) {
	note := inboxEntry{kind: inboxKindNote, severity: "high", subject: "disk full"}
	plan := inboxEntry{kind: inboxKindGoalPlan, subject: "p"}
	if note.kindTag() != "NOTE  " || plan.kindTag() != "DECIDE" {
		t.Fatalf("tags %q %q", note.kindTag(), plan.kindTag())
	}
	if len(note.kindTag()) != len(plan.kindTag()) {
		t.Fatal("type tags must be fixed width")
	}
	if note.subKind() != "" || plan.subKind() != "plan: " {
		t.Fatalf("subkind %q %q", note.subKind(), plan.subKind())
	}
}
