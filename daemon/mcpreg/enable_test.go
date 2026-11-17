package mcpreg

import (
	"errors"
	"strings"
	"testing"
)

// A disabled connection is exactly absent for Effective/ConnsFor, keeps its
// ceiling, auth, whitelist and grants, and re-enabling restores them.
func TestDisabledIsAbsent(t *testing.T) {
	r := effReg()
	before, err := r.Effective("gh", "alpha")
	if err != nil || len(before.Tools) == 0 {
		t.Fatalf("precondition: %v %v", err, before.Names())
	}
	if err := r.SetEnabled("gh", false); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"alpha", "beta", "gamma"} {
		e, err := r.Effective("gh", g)
		if !errors.Is(err, ErrNotWhitelisted) || len(e.Tools) != 0 {
			t.Fatalf("disabled %s: err=%v tools=%v", g, err, e.Names())
		}
		if e.Check("get_issue", nil) == nil {
			t.Fatalf("disabled %s: call allowed", g)
		}
		if got := r.ConnsFor(g); len(got) != 0 {
			t.Fatalf("disabled conn rendered for %s: %v", g, got)
		}
	}
	// same answer as a connection that does not exist
	_, errMissing := r.Effective("nope", "alpha")
	_, errDisabled := r.Effective("gh", "alpha")
	if errMissing != errDisabled {
		t.Fatalf("disabled differs from missing: %v vs %v", errDisabled, errMissing)
	}
	// grants, auth, ceiling preserved
	c := r.Conns["gh"]
	if c.Auth == nil || c.Auth.Value != secret || len(c.Tools) != 3 || len(c.Groups) != 3 {
		t.Fatalf("state lost on disable: %+v", c)
	}
	p, err := r.Preview("gh", "alpha")
	if err != nil || strings.Join(p.Names(), ",") != strings.Join(before.Names(), ",") {
		t.Fatalf("preview = %v %v", err, p.Names())
	}
	// survives a round trip through the file format
	b, _ := r.Marshal()
	if !strings.Contains(string(b), `"disabled": true`) {
		t.Fatalf("disabled not persisted:\n%s", b)
	}
	r2, err := Parse(b)
	if err != nil || !r2.Conns["gh"].Disabled {
		t.Fatalf("reload: %v", err)
	}
	// re-add keeps it disabled
	if err := r2.PutConn("gh", &Conn{URL: "https://mcp.example.com/v2", Class: ClassRead, Tools: c.Tools}, true); err != nil {
		t.Fatal(err)
	}
	if !r2.Conns["gh"].Disabled {
		t.Fatal("re-add silently re-enabled the connection")
	}
	if err := r2.SetEnabled("gh", true); err != nil {
		t.Fatal(err)
	}
	after, err := r2.Effective("gh", "alpha")
	if err != nil || strings.Join(after.Names(), ",") != strings.Join(before.Names(), ",") {
		t.Fatalf("re-enable did not restore: %v %v", err, after.Names())
	}
	if got := r2.ConnsFor("alpha"); len(got) != 1 {
		t.Fatalf("re-enabled ConnsFor = %v", got)
	}
	b, _ = r2.Marshal()
	if strings.Contains(string(b), "disabled") {
		t.Fatalf("enabled conn writes the flag:\n%s", b)
	}
	if err := r.SetEnabled("nope", true); err == nil {
		t.Fatal("enable on unknown connection accepted")
	}
}

// Files written before the flag existed load as enabled.
func TestOldFileLoadsEnabled(t *testing.T) {
	old := `{"version":1,"conns":{"gh":{"url":"https://mcp.example.com/mcp","class":"read",
		"tools":{"ping":null},"groups":{"alpha":{"tools":{"ping":null}}}}}}`
	r, err := Parse([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if r.Conns["gh"].Disabled {
		t.Fatal("old file loaded disabled")
	}
	if e, err := r.Effective("gh", "alpha"); err != nil || !e.Allowed("ping") {
		t.Fatalf("old file effective: %v %v", err, e.Names())
	}
	if _, err := Parse([]byte(strings.Replace(old, `"class":"read"`, `"class":"read","enabled":false`, 1))); err == nil {
		t.Fatal("unknown field enabled accepted (the on-disk key is disabled)")
	}
}
