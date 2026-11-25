package mcpreg

import (
	"errors"
	"testing"
)

func effReg() *Registry {
	r := sample()
	c := r.Conns["gh"]
	c.Groups["beta"] = &Grant{Tools: map[string]*ToolRule{
		// grant tries to widen get_issue with additional_args: ceiling still wins
		"get_issue": {AdditionalArgs: true, Args: map[string]*ArgRule{
			"repo": {Type: "string", Pattern: `koto/docs`},
		}},
	}}
	c.Groups["gamma"] = &Grant{} // whitelisted, no tools
	return r
}

func TestEffectiveDenyByDefault(t *testing.T) {
	r := effReg()
	for _, tc := range []struct{ conn, group string }{{"gh", "zeta"}, {"nope", "alpha"}, {"gh", ""}} {
		e, err := r.Effective(tc.conn, tc.group)
		if !errors.Is(err, ErrNotWhitelisted) || len(e.Tools) != 0 {
			t.Fatalf("%v: err=%v tools=%v", tc, err, e.Names())
		}
		if e.Check("get_issue", nil) == nil {
			t.Fatalf("%v: call allowed", tc)
		}
	}
	var nilReg *Registry
	if _, err := nilReg.Effective("gh", "alpha"); !errors.Is(err, ErrNotWhitelisted) {
		t.Fatal("nil registry must deny")
	}
	e, err := r.Effective("gh", "gamma")
	if err != nil || len(e.Tools) != 0 {
		t.Fatalf("whitelisted w/o grants: %v %v", err, e.Names())
	}
	if got := r.ConnsFor("zeta"); len(got) != 0 {
		t.Fatalf("ConnsFor zeta = %v", got)
	}
	if got := r.ConnsFor("alpha"); len(got) != 1 || got[0] != "gh" {
		t.Fatalf("ConnsFor alpha = %v", got)
	}
}

func TestEffectiveIntersection(t *testing.T) {
	r := effReg()
	a, _ := r.Effective("gh", "alpha")
	b, _ := r.Effective("gh", "beta")
	if n := a.Names(); len(n) != 1 || n[0] != "get_issue" {
		t.Fatalf("alpha tools %v", n)
	}
	if a.Allowed("search") || a.Allowed("ping") {
		t.Fatal("alpha sees ceiling tools it was not granted")
	}
	// nil grant rule = ceiling only
	if err := a.Check("get_issue", []byte(`{"repo":"koto/core","number":7}`)); err != nil {
		t.Fatal(err)
	}
	if err := a.Check("get_issue", []byte(`{"repo":"evil/x"}`)); err == nil {
		t.Fatal("ceiling pattern bypassed")
	}
	// beta: grant narrows repo
	if err := b.Check("get_issue", []byte(`{"repo":"koto/docs","number":7}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.Check("get_issue", []byte(`{"repo":"koto/core"}`)); err == nil {
		t.Fatal("grant narrowing ignored")
	}
	// beta's additional_args cannot widen: ceiling has no such arg
	if err := b.Check("get_issue", []byte(`{"repo":"koto/docs","extra":1}`)); err == nil {
		t.Fatal("grant widened the ceiling with additional_args")
	}
	// beta's number has no grant rule but ceiling limits still apply
	if err := b.Check("get_issue", []byte(`{"repo":"koto/docs","number":0}`)); err == nil {
		t.Fatal("ceiling minimum bypassed")
	}
}

func TestEffectiveCannotWiden(t *testing.T) {
	r := effReg()
	// via ops: refused
	if err := r.Grant("gh", "alpha", "delete_repo", nil); err == nil {
		t.Fatal("Grant outside ceiling accepted")
	}
	// via hand-edited file / ceiling shrink: dangling grant never effective
	r.Conns["gh"].Groups["alpha"].Tools["delete_repo"] = nil
	r.Conns["gh"].Groups["alpha"].Tools["search"] = nil
	delete(r.Conns["gh"].Tools, "search")
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	e, _ := r.Effective("gh", "alpha")
	if e.Allowed("delete_repo") || e.Allowed("search") {
		t.Fatalf("dangling grant effective: %v", e.Names())
	}
	// write-class conn exposes nothing in this version
	r.Conns["gh"].Class = ClassWrite
	e, err := r.Effective("gh", "alpha")
	if err != nil || len(e.Tools) != 0 {
		t.Fatalf("write conn: %v %v", err, e.Names())
	}
}

func TestEffectiveRevokeAndDeny(t *testing.T) {
	r := effReg()
	if err := r.Revoke("gh", "alpha", "get_issue"); err != nil {
		t.Fatal(err)
	}
	if e, _ := r.Effective("gh", "alpha"); e.Allowed("get_issue") {
		t.Fatal("revoke not effective")
	}
	if err := r.Deny("gh", "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Effective("gh", "beta"); !errors.Is(err, ErrNotWhitelisted) {
		t.Fatal("deny not effective")
	}
	if err := r.RmConn("gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Effective("gh", "gamma"); !errors.Is(err, ErrNotWhitelisted) {
		t.Fatal("rm conn not effective")
	}
}

func TestEffectiveRate(t *testing.T) {
	r := effReg()
	if err := r.SetRate("gh", "alpha", 30); err != nil {
		t.Fatal(err)
	}
	e, _ := r.Effective("gh", "alpha")
	if e.Rate == nil || e.Rate.PerMinute != 30 {
		t.Fatalf("rate %v", e.Rate)
	}
}
