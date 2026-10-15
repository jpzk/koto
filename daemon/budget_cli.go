package main

// `koto budget` — the operator CLI over the admin-only Budget RPC.
//
//	koto budget [--json]                                     show every key
//	koto budget set <provider> [--auth X] <field> <value>    set a cap
//	koto budget unset <provider> [--auth X] <field|all>      clear a cap
//	koto budget price <provider> <model> in=<usd/Mtok> out=... [cache_read=] [cache_write=]
//	koto budget price <provider> <model> --delete
//
// Fields: usd_per_day, usd_per_month, tokens_per_day, max_window_used.<5h|1d|7d|30d>.
// Without --auth the daemon uses the provider's current auth. Day/month are
// UTC. A USD cap is refused until the provider has prices.

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"koto-protocol/pb"
)

const budgetUsage = `usage: koto budget [--json]
       koto budget set   <provider> [--auth <auth>] <field> <value>
       koto budget unset <provider> [--auth <auth>] <field|all>
       koto budget price <provider> <model> in=<usd/Mtok> out=<usd/Mtok> [cache_read=..] [cache_write=..]
       koto budget price <provider> <model> --delete

  provider: claudesdk (claude) | openai (codex) | venice | minimax
  auth:     subscription | apikey | upstream | env   (default: current)
  field:    usd_per_day | usd_per_month | tokens_per_day
            max_window_used.<5h|1d|7d|30d>   (0.9 or 90%)
  values:   5M, 2.5k, $20 accepted; day/month are UTC
  admin-only: the daemon refuses any other role.
`

// budgetCliParse turns argv into a request (pure; tested).
func budgetCliParse(args []string) (req *pb.BudgetReq, asJSON bool, err error) {
	var pos []string
	auth, del := "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			asJSON = true
		case a == "--delete":
			del = true
		case a == "--auth":
			if i+1 >= len(args) {
				return nil, false, errors.New("--auth needs a value")
			}
			i++
			auth = args[i]
		case strings.HasPrefix(a, "--auth="):
			auth = strings.TrimPrefix(a, "--auth=")
		case a == "-h" || a == "--help" || a == "help":
			return nil, false, errors.New("help")
		case strings.HasPrefix(a, "--"):
			return nil, false, fmt.Errorf("unknown flag %s", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 || pos[0] == "show" {
		if len(pos) > 1 || auth != "" || del {
			return nil, false, errors.New("show takes no arguments")
		}
		return &pb.BudgetReq{Op: "show"}, asJSON, nil
	}
	op, rest := pos[0], pos[1:]
	switch op {
	case "set":
		if len(rest) != 3 || del {
			return nil, false, errors.New("usage: koto budget set <provider> [--auth X] <field> <value>")
		}
		return &pb.BudgetReq{Op: op, Provider: rest[0], Auth: auth, Field: rest[1], Value: rest[2]}, asJSON, nil
	case "unset":
		if len(rest) != 2 || del {
			return nil, false, errors.New("usage: koto budget unset <provider> [--auth X] <field|all>")
		}
		return &pb.BudgetReq{Op: op, Provider: rest[0], Auth: auth, Field: rest[1]}, asJSON, nil
	case "price":
		if len(rest) < 2 || auth != "" {
			return nil, false, errors.New("usage: koto budget price <provider> <model> in=<usd/Mtok> out=... | --delete")
		}
		r := &pb.BudgetReq{Op: op, Provider: rest[0], Model: rest[1], Prices: map[string]string{}}
		if del {
			if len(rest) != 2 {
				return nil, false, errors.New("price --delete takes no prices")
			}
			return r, asJSON, nil
		}
		if len(rest) == 2 {
			return nil, false, errors.New("price needs at least one of in= out= cache_read= cache_write= (or --delete)")
		}
		for _, kv := range rest[2:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" || v == "" {
				return nil, false, fmt.Errorf("bad price %q (want type=usd_per_Mtok)", kv)
			}
			if _, dup := r.Prices[k]; dup {
				return nil, false, fmt.Errorf("price type %s given twice", k)
			}
			r.Prices[k] = v
		}
		return r, asJSON, nil
	}
	return nil, false, fmt.Errorf("unknown budget subcommand %q (show|set|unset|price)", op)
}

func budgetCliMain(args []string) {
	req, asJSON, err := budgetCliParse(args)
	if err != nil {
		fmt.Fprint(os.Stderr, budgetUsage)
		if err.Error() == "help" {
			os.Exit(0)
		}
		ctlFatal(2, "%v", err)
	}
	cl := ctlClient()
	ctx, cancel := ctlCtx()
	defer cancel()
	resp, err := cl.Budget(ctx, req)
	if asJSON {
		ctlPrint(resp, err)
		return
	}
	if err != nil {
		ctlFatal(1, "rpc: %v", err)
	}
	if !resp.Ok {
		ctlFatal(1, "daemon: %s", resp.Error)
	}
	var view map[string]any
	if resp.Budget != nil {
		view = resp.Budget.AsMap()
	}
	fmt.Print(budgetRender(view))
}

// budgetRender is the human view (pure; tested). Narrow on purpose: one
// short line per fact so it reads on a phone-width terminal.
func budgetRender(view map[string]any) string {
	var b strings.Builder
	if e, _ := view["config_error"].(string); e != "" {
		fmt.Fprintf(&b, "!! %s (every LLM request is refused until fixed)\n", e)
	}
	keys, _ := view["keys"].([]any)
	if len(keys) == 0 {
		b.WriteString("no budget data yet (no LLM call, spend or cap recorded)\n")
	}
	for _, ki := range keys {
		k, _ := ki.(map[string]any)
		fmt.Fprintf(&b, "%v/%v\n", k["provider"], k["auth"])
		if ws, ok := k["windows"].(map[string]any); ok {
			var parts []string
			for _, l := range budgetLabelOrder {
				w, ok := ws[l].(map[string]any)
				if !ok {
					continue
				}
				s := l + "=?"
				if u, ok := w["used"].(float64); ok {
					s = l + "=" + budgetPct(u)
				}
				if st, _ := w["stale"].(bool); st {
					s += " (stale)"
				}
				parts = append(parts, s)
			}
			fmt.Fprintf(&b, "  windows: %s\n", strings.Join(parts, " | "))
		}
		if o, _ := k["overage"].(string); o != "" {
			fmt.Fprintf(&b, "  overage: %s\n", o)
		}
		if c, ok := k["credits"].(map[string]any); ok {
			s := "none"
			if u, _ := c["unlimited"].(bool); u {
				s = "unlimited"
			} else if bal, _ := c["balance"].(string); bal != "" {
				s = bal
			} else if h, _ := c["has_credits"].(bool); h {
				s = "yes"
			}
			fmt.Fprintf(&b, "  credits: %s\n", s)
		}
		if s, ok := k["spend"].(map[string]any); ok {
			line := fmt.Sprintf("  spend: today %s tok, month %s tok",
				budgetHumanTok(s["day_tokens"]), budgetHumanTok(s["month_tokens"]))
			if d, ok := s["day_usd"].(float64); ok {
				m, _ := s["month_usd"].(float64)
				lb := ""
				if x, _ := s["usd_lower_bound"].(bool); x {
					lb = " (at least)"
				}
				line += fmt.Sprintf("\n  usd: today $%.2f, month $%.2f%s", d, m, lb)
			}
			b.WriteString(line + "\n")
		}
		if c, ok := k["caps"].(map[string]any); ok {
			var parts []string
			if w, ok := c[budgetCapWindow].(map[string]any); ok {
				ls := make([]string, 0, len(w))
				for l := range w {
					ls = append(ls, l)
				}
				sort.Strings(ls)
				for _, l := range ls {
					if f, ok := w[l].(float64); ok {
						parts = append(parts, l+"<"+budgetPct(f))
					}
				}
			}
			if f, ok := c[budgetCapTokensDay].(float64); ok {
				parts = append(parts, budgetHumanTok(f)+" tok/day")
			}
			if f, ok := c[budgetCapUSDDay].(float64); ok {
				parts = append(parts, fmt.Sprintf("$%.2f/day", f))
			}
			if f, ok := c[budgetCapUSDMonth].(float64); ok {
				parts = append(parts, fmt.Sprintf("$%.2f/month", f))
			}
			fmt.Fprintf(&b, "  caps: %s\n", strings.Join(parts, ", "))
		}
		if r, ok := k["refused"].(map[string]any); ok {
			ks := make([]string, 0, len(r))
			for kind := range r {
				ks = append(ks, kind)
			}
			sort.Strings(ks)
			var parts []string
			for _, kind := range ks {
				parts = append(parts, fmt.Sprintf("%s=%v", kind, r[kind]))
			}
			fmt.Fprintf(&b, "  refused: %s\n", strings.Join(parts, " "))
		}
	}
	if p, ok := view["prices"].(map[string]any); ok {
		b.WriteString("prices (USD/Mtok)\n")
		provs := make([]string, 0, len(p))
		for k := range p {
			provs = append(provs, k)
		}
		sort.Strings(provs)
		for _, prov := range provs {
			ms, _ := p[prov].(map[string]any)
			models := make([]string, 0, len(ms))
			for m := range ms {
				models = append(models, m)
			}
			sort.Strings(models)
			for _, m := range models {
				pr, _ := ms[m].(map[string]any)
				var parts []string
				for _, t := range []string{budgetTokIn, budgetTokOut, budgetTokCacheRead, budgetTokCacheWrite} {
					if f, ok := pr[t].(float64); ok {
						parts = append(parts, t+"="+strconv.FormatFloat(f, 'f', -1, 64))
					}
				}
				fmt.Fprintf(&b, "  %s %s: %s\n", prov, m, strings.Join(parts, " "))
			}
		}
	}
	return b.String()
}

// budgetHumanTok renders a token count as 1.2M / 340k / 512.
func budgetHumanTok(v any) string {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int64:
		f = float64(x)
	default:
		return "?"
	}
	switch {
	case f >= 1e6:
		return strconv.FormatFloat(f/1e6, 'f', 1, 64) + "M"
	case f >= 1e3:
		return strconv.FormatFloat(f/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatFloat(f, 'f', 0, 64)
}
