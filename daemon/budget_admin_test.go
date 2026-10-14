package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"koto-protocol/pb"
)

func adminDo(t *testing.T, op, prov, auth, field, value, model string, prices map[string]string) (map[string]any, error) {
	t.Helper()
	return budgetAdmin(op, prov, auth, field, value, model, prices)
}

func mustAdmin(t *testing.T, op, prov, auth, field, value, model string, prices map[string]string) map[string]any {
	t.Helper()
	v, err := adminDo(t, op, prov, auth, field, value, model, prices)
	if err != nil {
		t.Fatalf("%s %s %s: %v", op, field, value, err)
	}
	return v
}

func loadCfg(t *testing.T) *budgetConfig {
	t.Helper()
	c, err := loadBudgetConfig()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBudgetAdminSetUnsetEachKind(t *testing.T) {
	capsTestEnv(t)
	mustAdmin(t, "set", "claude", "subscription", "max_window_used.7d", "90%", "", nil)
	mustAdmin(t, "set", "claudesdk", "sub", "max_window_used.5h", "0.5", "", nil)
	mustAdmin(t, "set", "codex", "apikey", "tokens_per_day", "5M", "", nil)
	c := loadCfg(t)
	got := c.caps(kSub)
	if got.MaxWindowUsed["7d"] != 0.9 || got.MaxWindowUsed["5h"] != 0.5 {
		t.Fatalf("window caps = %+v", got)
	}
	if c.caps(budgetKey{providerOpenAI, budgetAuthAPIKey}).TokensPerDay != 5_000_000 {
		t.Fatalf("tokens cap = %+v", c.Caps)
	}
	st, err := os.Stat(budgetConfigPath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("budget.json mode = %v, %v", st.Mode(), err)
	}
	// the proxy now enforces what was set (same file, read per request)
	observe7d(kSub, 0.95, capsTestEnvFuture())
	if r := budgetCheckCaps("g", kSub); r == nil || r.Kind != budgetCapWindow {
		t.Fatalf("set cap not enforced: %+v", r)
	}
	mustAdmin(t, "unset", "claudesdk", "subscription", "max_window_used.7d", "", "", nil)
	if r := budgetCheckCaps("g", kSub); r != nil {
		t.Fatalf("unset cap still enforced: %+v", r)
	}
	mustAdmin(t, "unset", "claudesdk", "subscription", "all", "", "", nil)
	if _, ok := loadCfg(t).Caps[kSub.String()]; ok {
		t.Fatal("unset all left the key")
	}
}

func capsTestEnvFuture() int64 { return budgetCapNow().Unix() + 3600 }

func TestBudgetAdminUSDCapNeedsPrices(t *testing.T) {
	capsTestEnv(t)
	_, err := adminDo(t, "set", "claudesdk", "apikey", "usd_per_day", "20", "", nil)
	if err == nil || !strings.Contains(err.Error(), "needs prices") {
		t.Fatalf("usd cap without prices: %v", err)
	}
	if _, statErr := os.Stat(budgetConfigPath()); !os.IsNotExist(statErr) {
		t.Fatal("rejected set still wrote budget.json")
	}
	// priced for another provider only: still rejected
	mustAdmin(t, "price", "openai", "", "", "", "gpt-5.5", map[string]string{"in": "1.25", "out": "10"})
	if _, err := adminDo(t, "set", "claudesdk", "apikey", "usd_per_month", "300", "", nil); err == nil {
		t.Fatal("usd cap accepted with prices only for another provider")
	}
	mustAdmin(t, "price", "claude", "", "", "", "claude-sonnet-5", map[string]string{"in": "3", "out": "15"})
	// a used, unpriced model blocks the USD cap
	budgetRecordSpend(kAPI, "claude-opus-5", usageOf(10, 10, 0, 0), loadCfg(t))
	_, err = adminDo(t, "set", "claudesdk", "apikey", "usd_per_day", "20", "", nil)
	if err == nil || !strings.Contains(err.Error(), "claude-opus-5") {
		t.Fatalf("unpriced used model not named: %v", err)
	}
	mustAdmin(t, "price", "claude", "", "", "", "claude-opus-5", map[string]string{"in": "15", "out": "75", "cache_read": "1.5"})
	mustAdmin(t, "set", "claudesdk", "apikey", "usd_per_day", "$20", "", nil)
	mustAdmin(t, "set", "claudesdk", "apikey", "usd_per_month", "300", "", nil)
	c := loadCfg(t).caps(kAPI)
	if c.USDPerDay != 20 || c.USDPerMonth != 300 {
		t.Fatalf("usd caps = %+v", c)
	}
	// deleting a price that would orphan the USD cap is refused
	mustAdmin(t, "price", "claude", "", "", "", "claude-sonnet-5", nil)
	if _, err := adminDo(t, "price", "claude", "", "", "", "claude-opus-5", nil); err == nil {
		t.Fatal("deleting the last needed price accepted under a USD cap")
	}
	p, ok := loadCfg(t).price(providerClaude, "claude-opus-5")
	if !ok || *p.CacheRead != 1.5 {
		t.Fatalf("price lost after refused delete: %+v %v", p, ok)
	}
}

func TestBudgetAdminUnknownBucketsDoNotBlockUSDCap(t *testing.T) {
	capsTestEnv(t)
	mustAdmin(t, "price", "venice", "", "", "", "llama-4", map[string]string{"in": "0.5", "out": "1"})
	budgetRecordSpend(budgetKey{providerVenice, budgetAuthAPIKey}, "", usageOf(5, 5, 0, 0), loadCfg(t))
	mustAdmin(t, "set", "venice", "", "usd_per_day", "5", "", nil)
}

func TestBudgetAdminRejectsBadInput(t *testing.T) {
	capsTestEnv(t)
	bad := []struct{ op, prov, auth, field, value, model string }{
		{"set", "nope", "apikey", "tokens_per_day", "1", ""},
		{"set", "claudesdk", "root", "tokens_per_day", "1", ""},
		{"set", "claudesdk", "apikey", "tokens_per_day", "-1", ""},
		{"set", "claudesdk", "apikey", "tokens_per_day", "1.5", ""},
		{"set", "claudesdk", "apikey", "tokens_per_day", "NaN", ""},
		{"set", "claudesdk", "apikey", "max_window_used.2d", "0.5", ""},
		{"set", "claudesdk", "apikey", "max_window_used.7d", "0", ""},
		{"set", "claudesdk", "apikey", "max_window_used.7d", "150%", ""},
		{"set", "claudesdk", "apikey", "bogus", "1", ""},
		{"price", "claudesdk", "", "", "", "Bad Model!"},
		{"price", "claudesdk", "", "", "", "other"},
		{"frobnicate", "", "", "", "", ""},
	}
	for _, b := range bad {
		var prices map[string]string
		if b.op == "price" {
			prices = map[string]string{"in": "1"}
		}
		if _, err := adminDo(t, b.op, b.prov, b.auth, b.field, b.value, b.model, prices); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
	if _, err := adminDo(t, "price", "claudesdk", "", "", "", "m", map[string]string{"bogus": "1"}); err == nil {
		t.Error("unknown price type accepted")
	}
	if _, err := os.Stat(budgetConfigPath()); !os.IsNotExist(err) {
		t.Fatal("rejected input wrote budget.json")
	}
}

func TestBudgetAdminBrokenConfigRefusesEdits(t *testing.T) {
	capsTestEnv(t)
	if err := os.WriteFile(budgetConfigPath(), []byte(`{"caps":{"x":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adminDo(t, "set", "claudesdk", "apikey", "tokens_per_day", "1", "", nil); err == nil {
		t.Fatal("edit on a broken budget.json accepted (would overwrite it)")
	}
	v := budgetView(false)
	if s, _ := v["config_error"].(string); s == "" || strings.Contains(s, budgetConfigPath()) {
		t.Fatalf("guest view config_error = %q", s)
	}
	if s, _ := budgetView(true)["config_error"].(string); s == "" {
		t.Fatal("operator view lost the config error")
	}
}

func TestBudgetViewShowsWindowsSpendCapsRefused(t *testing.T) {
	capsTestEnv(t)
	observe7d(kSub, 0.42, capsTestEnvFuture())
	mustAdmin(t, "price", "claude", "", "", "", "claude-sonnet-5", map[string]string{"in": "3", "out": "15"})
	budgetRecordSpend(kAPI, "claude-sonnet-5", usageOf(1000, 1000, 0, 0), loadCfg(t))
	mustAdmin(t, "set", "claudesdk", "apikey", "tokens_per_day", "1500", "", nil)
	if r := budgetCheckCaps("g", kAPI); r == nil {
		t.Fatal("expected refusal")
	}
	b, _ := json.Marshal(toStruct(budgetView(true)).AsMap())
	s := string(b)
	for _, want := range []string{`"auth":"subscription"`, `"used":0.42`, `"day_tokens":2000`, `"tokens_per_day":1500`, `"refused":{"tokens_per_day":1}`, `"claude-sonnet-5"`, `"day_usd":0.018`} {
		if !strings.Contains(s, want) {
			t.Errorf("view missing %s:\n%s", want, s)
		}
	}
	out := budgetRender(toStruct(budgetView(true)).AsMap())
	for _, want := range []string{"claudesdk/subscription", "7d=42%", "claudesdk/apikey", "today 2.0k tok", "1.5k tok/day", "refused: tokens_per_day=1", "usd: today $0.02", "claudesdk claude-sonnet-5: in=3 out=15"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestBudgetCliParse(t *testing.T) {
	ok := []struct {
		args []string
		want *pb.BudgetReq
	}{
		{nil, &pb.BudgetReq{Op: "show"}},
		{[]string{"--json"}, &pb.BudgetReq{Op: "show"}},
		{[]string{"set", "openai", "--auth", "subscription", "max_window_used.7d", "0.9"}, &pb.BudgetReq{Op: "set", Provider: "openai", Auth: "subscription", Field: "max_window_used.7d", Value: "0.9"}},
		{[]string{"set", "venice", "tokens_per_day", "5M"}, &pb.BudgetReq{Op: "set", Provider: "venice", Field: "tokens_per_day", Value: "5M"}},
		{[]string{"unset", "claude", "--auth=apikey", "all"}, &pb.BudgetReq{Op: "unset", Provider: "claude", Auth: "apikey", Field: "all"}},
		{[]string{"price", "openai", "gpt-5.5", "in=1.25", "out=10"}, &pb.BudgetReq{Op: "price", Provider: "openai", Model: "gpt-5.5", Prices: map[string]string{"in": "1.25", "out": "10"}}},
		{[]string{"price", "openai", "gpt-5.5", "--delete"}, &pb.BudgetReq{Op: "price", Provider: "openai", Model: "gpt-5.5", Prices: map[string]string{}}},
	}
	for _, c := range ok {
		got, _, err := budgetCliParse(c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(c.want)
		if string(g) != string(w) {
			t.Errorf("%v: got %s want %s", c.args, g, w)
		}
	}
	for _, args := range [][]string{
		{"set", "openai", "usd_per_day"},
		{"unset", "openai"},
		{"price", "openai", "gpt-5.5"},
		{"price", "openai", "gpt-5.5", "in"},
		{"price", "openai", "gpt-5.5", "in=1", "in=2"},
		{"price", "openai", "gpt-5.5", "--delete", "in=1"},
		{"set", "openai", "--auth"},
		{"set", "openai", "--bogus", "x", "y"},
		{"show", "extra"},
		{"nuke"},
	} {
		if _, _, err := budgetCliParse(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

// ACL: same class as config_posture — admin only, never grantable.
func TestBudgetVerbAdminOnly(t *testing.T) {
	acl := aclTable{"ops": {"*": targetSet{any: true}}, "bud": {"budget": targetSet{any: true}}}
	if !rolesAllowed(acl, []string{defaultRole}, "budget", "", false) {
		t.Fatal("admin denied budget")
	}
	for _, r := range []string{"ops", "bud", "agent"} {
		if rolesAllowed(acl, []string{r}, "budget", "", false) {
			t.Errorf("role %s granted budget via acl.json", r)
		}
	}
	if verbFromMethod(pb.Koto_Budget_FullMethodName) != "budget" {
		t.Fatalf("verb = %q", verbFromMethod(pb.Koto_Budget_FullMethodName))
	}
}

// In-guest ctl plane: peers refused; main read-only; no host path leaks.
func TestBudgetCtlPlane(t *testing.T) {
	capsTestEnv(t)
	mustAdmin(t, "set", "claudesdk", "apikey", "tokens_per_day", "100", "", nil)
	if r, ok := ctlDispatch("peer1", []byte(`{"cmd":"budget"}`)).(baseResp); !ok || r.OK {
		t.Fatalf("peer got budget: %#v", r)
	}
	for _, op := range []string{"set", "unset", "price"} {
		line := []byte(`{"cmd":"budget","op":"` + op + `","provider":"claudesdk","field":"tokens_per_day","value":"999999"}`)
		if r, ok := ctlDispatch(ctlMainGroup, line).(baseResp); !ok || r.OK || !strings.Contains(r.Error, "operator-only") {
			t.Fatalf("main %s: %#v", op, r)
		}
	}
	if loadCfg(t).caps(kAPI).TokensPerDay != 100 {
		t.Fatal("ctl plane changed a cap")
	}
	r, ok := ctlDispatch(ctlMainGroup, []byte(`{"cmd":"budget"}`)).(ctlBudgetResp)
	if !ok || !r.OK {
		t.Fatalf("main show: %#v", r)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"tokens_per_day":100`) || strings.Contains(string(b), HERE) {
		t.Fatalf("main view: %s", b)
	}
}
