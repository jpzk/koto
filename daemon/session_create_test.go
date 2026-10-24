package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"koto-protocol/pb"
)

// TestSessionCreateRegistersBeforeSend covers the SessionCreate RPC: a named
// session lands in the group's registry (and so in GroupInfo.sessions) with
// no turn sent, idempotently, while the default session, the goal-reserved
// namespace, malformed names and unregistered groups are refused.
func TestSessionCreateRegistersBeforeSend(t *testing.T) {
	const g = "sess-rpc"
	dir := t.TempDir()
	oldHere, oldGroups, oldRoot := HERE, GROUPS_FILE, ROOT
	HERE, GROUPS_FILE, ROOT = dir, filepath.Join(dir, "groups.json"), filepath.Join(dir, "groups")
	t.Cleanup(func() { HERE, GROUPS_FILE, ROOT = oldHere, oldGroups, oldRoot })
	writeGroups(map[string]int{g: 8788})
	s := &kotoServer{}
	ctx := context.Background()

	for i := 0; i < 2; i++ { // second call: idempotent, still ok
		r, err := s.SessionCreate(ctx, &pb.GroupReq{Group: g, Session: "review"})
		if err != nil || !r.Ok {
			t.Fatalf("create #%d: ok=%v err=%v resp-err=%q", i, r.GetOk(), err, r.GetError())
		}
	}
	if got := listSessions(g); !slices.Equal(got, []string{"review"}) {
		t.Fatalf("registry = %v, want [review]", got)
	}

	refused := []struct {
		name, group, sess, want string
	}{
		{"default alias", g, "default", "required"},
		{"empty", g, "", "required"},
		{"dash alias", g, "-", "required"},
		{"bad name", g, "has space", "invalid session name"},
		{"goal namespace", g, "goal-x", "reserved"},
		{"unknown group", "nope", "review", "no such group"},
		{"bad group", "../x", "review", "invalid group name"},
	}
	for _, c := range refused {
		r, err := s.SessionCreate(ctx, &pb.GroupReq{Group: c.group, Session: c.sess})
		if err != nil {
			t.Fatalf("%s: transport err %v", c.name, err)
		}
		if r.Ok || !strings.Contains(r.Error, c.want) {
			t.Errorf("%s: ok=%v err=%q, want refusal containing %q", c.name, r.Ok, r.Error, c.want)
		}
	}
	if got := listSessions(g); !slices.Equal(got, []string{"review"}) {
		t.Fatalf("refusals touched the registry: %v", got)
	}
	if verbFromMethod("/koto.Koto/SessionCreate") != "session_create" {
		t.Fatalf("acl verb = %q", verbFromMethod("/koto.Koto/SessionCreate"))
	}
}
