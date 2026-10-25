package main

// providers_test.go — the provider select offers only providers with a
// credential on the daemon host (providers.go): filtering in the ctrl+n
// dialog, the ctrl+e dialog keeping an unconfigured current provider, the
// empty-state hint with Create refusing, and the RPC-error fallback to the
// full list. Every state renders within 40 columns.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func (f *fakeKoto) Providers(ctx context.Context, r *pb.ProvidersReq) (*pb.ProvidersResp, error) {
	f.mu.Lock()
	resp := f.providers
	f.mu.Unlock()
	if resp == nil {
		return f.UnimplementedKotoServer.Providers(ctx, r)
	}
	return resp, nil
}

// provResp builds a Providers answer: every allowlisted provider, the named
// ones configured.
func provResp(configured ...string) *pb.ProvidersResp {
	on := map[string]bool{}
	for _, p := range configured {
		on[p] = true
	}
	r := &pb.ProvidersResp{Ok: true}
	for _, p := range groupOptValues("provider") {
		st := &pb.ProviderStatus{Name: p, Configured: on[p]}
		if on[p] {
			st.Kind = "api-key"
		}
		r.Providers = append(r.Providers, st)
	}
	return r
}

// openNew presses ctrl+n and runs everything it triggers (models +
// providers).
func openNew(t *testing.T, m Model) Model {
	t.Helper()
	m, cmd := pressCmd(t, m, tea.KeyCtrlN)
	if !m.newGroup.open {
		t.Fatal("ctrl+n did not open the dialog")
	}
	return runCmds(t, m, cmd, 6)
}

// mutating drops the config reads from a call log.
func mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "read ") {
			out = append(out, c)
		}
	}
	return out
}

// fits40 renders m at 40 columns and fails on any wider line; returns the
// ANSI-stripped frame.
func fits40(t *testing.T, label string, m Model) string {
	t.Helper()
	m.width, m.height = 40, 50
	out := m.View()
	for i, ln := range strings.Split(out, "\n") {
		if w := lipgloss.Width(ln); w > 40 {
			t.Errorf("%s: line %d is %d cols > 40: %q", label, i, w, stripANSI(ln))
		}
	}
	return stripANSI(out)
}

// unwrap joins a wrapped frame's text so a phrase split across lines (and
// box borders) still matches.
func unwrap(s string) string {
	s = strings.NewReplacer("│", " ", "|", " ", "\n", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// TestNewGroupOffersOnlyConfiguredProviders: claudesdk has no login, so the
// select offers venice/openai only, defaults to venice, cycles within them,
// the model list follows venice, and Create spawns venice.
func TestNewGroupOffersOnlyConfiguredProviders(t *testing.T) {
	fk := startFakeKoto(t)
	fk.providers = provResp("venice", "openai")
	m := openNew(t, focusModel(t))
	f := m.newGroup
	if f.prov.state != provLoaded {
		t.Fatalf("prov state = %d, want loaded", f.prov.state)
	}
	if got, want := f.values("provider"), []string{"venice", "openai"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider options = %v, want %v", got, want)
	}
	if f.value("provider") != "venice" {
		t.Fatalf("default provider = %q, want venice", f.value("provider"))
	}
	if f.mp.provider != "venice" || fk.modelCalls("venice") == 0 {
		t.Fatalf("model list not loaded for venice: mp.provider=%q calls=%d", f.mp.provider, fk.modelCalls("venice"))
	}
	m = gotoField(t, m, "provider")
	var seen []string
	for i := 0; i < 4; i++ {
		seen = append(seen, m.newGroup.value("provider"))
		m, _ = pressCmd(t, m, tea.KeyRight)
	}
	if want := []string{"venice", "openai", "venice", "openai"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("cycled %v, want %v", seen, want)
	}
	out := unwrap(fits40(t, "filtered", m))
	if strings.Contains(out, "no login") || strings.Contains(out, "unavailable") {
		t.Errorf("configured providers carry a warning:\n%s", out)
	}
	m = gotoField(t, m, ngName)
	m = typeText(t, m, "research")
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	runCmds(t, m, cmd, 4)
	if got, want := mutating(fk.got()), []string{"spawn group=research provider=venice size=small"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon calls\n got %v\nwant %v", got, want)
	}
}

// TestEditGroupKeepsUnconfiguredCurrentProvider: the group runs venice, which
// has no login; it stays selected and offered (marked), Save stays a no-op,
// and the configured provider is still reachable. Both arrival orders of the
// config read and the Providers answer give the same form.
func TestEditGroupKeepsUnconfiguredCurrentProvider(t *testing.T) {
	check := func(t *testing.T, m Model, fk *fakeKoto) {
		t.Helper()
		f := m.newGroup
		if got, want := f.values("provider"), []string{"claudesdk", "venice"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("provider options = %v, want %v", got, want)
		}
		if f.value("provider") != "venice" {
			t.Fatalf("provider = %q, want the group's venice", f.value("provider"))
		}
		if p, _, e := f.editPlan(); e != "" || len(p.config) != 0 {
			t.Fatalf("untouched edit plans %+v (%s)", p, e)
		}
		if _, enabled := f.editButton(); enabled {
			t.Fatal("Save enabled with nothing changed")
		}
		out := unwrap(fits40(t, "edit", m))
		if !strings.Contains(out, "venice: no login") {
			t.Errorf("unconfigured current provider not marked:\n%s", out)
		}
		m = gotoField(t, m, "provider")
		m, _ = pressCmd(t, m, tea.KeyLeft)
		if m.newGroup.value("provider") != "claudesdk" {
			t.Fatalf("left -> %q, want claudesdk", m.newGroup.value("provider"))
		}
		m, _ = pressCmd(t, m, tea.KeyRight)
		if m.newGroup.value("provider") != "venice" {
			t.Fatalf("right -> %q, want venice back", m.newGroup.value("provider"))
		}
		if p, _, _ := m.newGroup.editPlan(); p.config["provider"] != nil {
			t.Fatalf("provider round trip sends %v", p.config["provider"])
		}
		m = saveEdit(t, m)
		for _, c := range mutating(fk.got()) {
			if strings.Contains(c, "provider=") {
				t.Fatalf("save switched provider: %v", fk.got())
			}
		}
	}
	t.Run("read first", func(t *testing.T) {
		fk := startFakeKoto(t)
		fk.providers = provResp("claudesdk")
		check(t, openEdit(t, fk, editCfg()), fk)
	})
	t.Run("providers first", func(t *testing.T) {
		fk := startFakeKoto(t)
		fk.providers = provResp("claudesdk")
		fk.cfg = editCfg()
		m, cmd := pressCmd(t, focusModel(t), tea.KeyCtrlE)
		b, ok := cmd().(tea.BatchMsg)
		if !ok || len(b) != 2 {
			t.Fatalf("ctrl+e cmd = %T, want a batch of read + providers", cmd())
		}
		read, prov := b[0](), b[1]()
		if _, ok := prov.(providersLoadedMsg); !ok {
			read, prov = prov, read
		}
		nm, next := m.Update(prov)
		m = runCmds(t, nm.(Model), next, 4)
		if !m.newGroup.edit.loading {
			t.Fatal("read landed early")
		}
		nm, next = m.Update(read)
		m = runCmds(t, nm.(Model), next, 4)
		check(t, m, fk)
	})
}

// TestNewGroupNoProviderLogin: nothing configured -> no provider offered,
// the login hint inline, Create refuses (no spawn) and says why.
func TestNewGroupNoProviderLogin(t *testing.T) {
	fk := startFakeKoto(t)
	fk.providers = provResp()
	m := openNew(t, focusModel(t))
	f := m.newGroup
	if len(f.values("provider")) != 0 || f.value("provider") != "" {
		t.Fatalf("offered %v / %q with no login", f.values("provider"), f.value("provider"))
	}
	for p, n := range map[string]int{"claudesdk": fk.modelCalls("claudesdk"), "venice": fk.modelCalls("venice"), "openai": fk.modelCalls("openai")} {
		if p != "claudesdk" && n != 0 {
			t.Errorf("models fetched for unoffered %s", p)
		}
	}
	m = typeText(t, m, "research")
	out := unwrap(fits40(t, "empty", m))
	for _, w := range []string{"koto claude-login", "koto openai-login", "no provider has a login", "cannot create", "provider none"} {
		if !strings.Contains(out, w) {
			t.Errorf("empty-state frame missing %q:\n%s", w, out)
		}
	}
	m = gotoField(t, m, ngCreate)
	m, cmd := pressCmd(t, m, tea.KeyEnter)
	m = runCmds(t, m, cmd, 4)
	if got := mutating(fk.got()); len(got) != 0 {
		t.Fatalf("Create with no provider called %v", got)
	}
	if m.newGroup.busy || m.newGroup.errAt != "provider" || m.newGroup.field() != "provider" {
		t.Fatalf("errAt=%q field=%q busy=%v, want the provider error focused", m.newGroup.errAt, m.newGroup.field(), m.newGroup.busy)
	}
	// left/right on an empty select is a no-op, not a panic.
	m, _ = pressCmd(t, m, tea.KeyRight)
	if m.newGroup.value("provider") != "" {
		t.Fatal("empty select produced a value")
	}
}

// TestProvidersRPCErrorFallsBackToFullList: an older daemon (Unimplemented)
// or an error answer -> the full allowlist plus a dim note; Create works.
func TestProvidersRPCErrorFallsBackToFullList(t *testing.T) {
	for name, resp := range map[string]*pb.ProvidersResp{
		"unimplemented": nil,
		"daemon error":  {Ok: false, Error: "providers: verb not allowed"},
	} {
		t.Run(name, func(t *testing.T) {
			fk := startFakeKoto(t)
			fk.providers = resp
			m := openNew(t, focusModel(t))
			f := m.newGroup
			if f.prov.state != provFallback {
				t.Fatalf("prov state = %d, want fallback", f.prov.state)
			}
			if got, want := f.values("provider"), groupOptValues("provider"); !reflect.DeepEqual(got, want) {
				t.Fatalf("options = %v, want the full list %v", got, want)
			}
			out := unwrap(fits40(t, name, m))
			if !strings.Contains(out, "login check unavailable") || strings.Contains(out, "no login") {
				t.Errorf("fallback note wrong:\n%s", out)
			}
			if why := map[string]string{"unimplemented": "older daemon", "daemon error": "verb not allowed"}[name]; !strings.Contains(out, why) {
				t.Errorf("fallback note lacks %q:\n%s", why, out)
			}
			m = typeText(t, m, "research")
			m = gotoField(t, m, ngCreate)
			m, cmd := pressCmd(t, m, tea.KeyEnter)
			runCmds(t, m, cmd, 4)
			if got, want := mutating(fk.got()), []string{"spawn group=research provider=claudesdk size=small"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("daemon calls %v, want %v", got, want)
			}
		})
	}
}

// TestProvidersCmdDropsUnknownAndUnconfigured: only allowlisted, configured
// names make it into the answer; a kind off the documented set is dropped.
func TestProvidersCmdDropsUnknownAndUnconfigured(t *testing.T) {
	fk := startFakeKoto(t)
	fk.providers = &pb.ProvidersResp{Ok: true, Providers: []*pb.ProviderStatus{
		{Name: "claudesdk", Configured: true, Kind: "subscription\x1b[31m"},
		{Name: "venice"},
		{Name: "evil\x1b[2J", Configured: true, Kind: "api-key"},
	}}
	msg := providersCmd("")().(providersLoadedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if want := map[string]string{"claudesdk": ""}; !reflect.DeepEqual(msg.configured, want) {
		t.Fatalf("configured = %q, want %q", msg.configured, want)
	}
}
