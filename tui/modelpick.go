package main

// modelpick.go — the new-group dialog's model select.
//
// The model field is no longer free text: it is a select whose options are
// "provider default", the models the daemon's Models RPC lists for the
// dialog's provider (fetched host-side with the host credential, cached and
// sanitized there), and a final "custom…" that reveals an id input checked
// against the same id rule the daemon applies. Changing the provider reloads
// the list asynchronously; the dialog never waits on it.
//
// Everything that arrives here is still treated as untrusted display text:
// the daemon already sanitizes, but a stale or foreign daemon must not be able
// to paint escape sequences into the frame, so ids are re-checked against the
// regex and display names are stripped of control/format runes again.

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// modelIDRE is the brief's id rule, identical to the daemon's.
var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)

const (
	modelsMaxEntries = 100
	modelNameMax     = 64 // runes
	modelDropRows    = 8  // visible rows of the open list
)

// Load states of the dialog's model list.
const (
	modelsIdle = iota
	modelsLoading
	modelsLive
	modelsFallback
	modelsFailed
)

type modelOpt struct{ id, name string }

// modelsLoadedMsg carries one Models RPC result back to the dialog. seq ties
// it to the request that is still current; anything older is dropped.
type modelsLoadedMsg struct {
	provider string
	seq      int
	source   string
	reason   string
	models   []modelOpt
	err      error
	at       time.Time // when the result arrived (cache age)
}

// modelsTUICacheTTL bounds how long the TUI reuses a live list across
// provider switches and dialog opens; the daemon keeps its own 10 min cache.
const modelsTUICacheTTL = 5 * time.Minute

// cacheable: only live lists are reused, so an offline or failed answer is
// retried on the next provider switch.
func (r modelsLoadedMsg) cacheable() bool { return r.err == nil && r.source == "live" }

// cleanModelText removes every control and format rune (so no ESC, CSI, OSC,
// bidi override survives), collapses whitespace and caps the rune count.
func cleanModelText(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = n > 0
			continue
		}
		if space {
			if n+1 >= maxRunes {
				break
			}
			b.WriteRune(' ')
			n++
			space = false
		}
		if n >= maxRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// sanitizeModelOpts converts the RPC's model list (protojson-decoded) into
// options: bad ids dropped, duplicates dropped, names cleaned (falling back to
// the id), capped at modelsMaxEntries. Order is the daemon's (sorted there).
func sanitizeModelOpts(raw []any) []modelOpt {
	out := []modelOpt{}
	seen := map[string]bool{}
	for _, e := range raw {
		m, _ := e.(map[string]any)
		id := s2(m["id"])
		if !modelIDRE.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		name := cleanModelText(s2(m["display_name"]), modelNameMax)
		if name == "" {
			name = id
		}
		out = append(out, modelOpt{id: id, name: name})
		if len(out) == modelsMaxEntries {
			break
		}
	}
	return out
}

// modelsCmd asks the daemon for provider's model list.
func modelsCmd(sock, provider string, seq int) tea.Cmd {
	return func() tea.Msg {
		msg := modelsLoadedMsg{provider: provider, seq: seq}
		out, err := daemonCall(sock, "models", map[string]any{"provider": provider})
		msg.at = time.Now()
		if err != nil {
			msg.err = err
			return msg
		}
		msg.source = s2(out["source"])
		msg.reason = cleanModelText(s2(out["reason"]), 80)
		raw, _ := out["models"].([]any)
		msg.models = sanitizeModelOpts(raw)
		return msg
	}
}

// ---- form state -------------------------------------------------------------

// modelPick is the dialog's model select. Index 0 is "provider default",
// 1..len(opts) the fetched models, len(opts)+1 "custom…".
type modelPick struct {
	provider string // provider the opts belong to
	opts     []modelOpt
	state    int
	reason   string
	sel      int
	want     string // id to preselect when a list (re)loads
	seq      int
	cache    map[string]modelsLoadedMsg
	open     bool // the drop-down list is showing
	cur      int  // drop-down cursor
}

func (p *modelPick) count() int     { return len(p.opts) + 2 }
func (p *modelPick) customIdx() int { return len(p.opts) + 1 }
func (p *modelPick) isCustom() bool { return p.sel == p.customIdx() }

// label is option i as shown in the collapsed field.
func (p *modelPick) label(i int) string {
	switch {
	case i <= 0:
		return "provider default"
	case i <= len(p.opts):
		return p.opts[i-1].name
	}
	return gl("custom…", "custom...")
}

// listID is the model id of a list choice ("" for default/custom).
func (p *modelPick) listID() string {
	if p.sel >= 1 && p.sel <= len(p.opts) {
		return p.opts[p.sel-1].id
	}
	return ""
}

// indexOf returns the option index of id in the list, or -1.
func (p *modelPick) indexOf(id string) int {
	for i, o := range p.opts {
		if o.id == id {
			return i + 1
		}
	}
	return -1
}

// setSel moves the selection and records the chosen id as the value to
// preselect after the next reload. custom remembers itself by keeping want
// empty and the custom flag via sel.
func (p *modelPick) setSel(i int) {
	n := p.count()
	p.sel = ((i % n) + n) % n
	p.want = p.listID()
}

// beginLoad switches the list to provider. A cached result applies at once
// (no fetch, no flicker); otherwise the list empties to default/custom with a
// loading state and the returned seq identifies the request to make.
func (p *modelPick) beginLoad(provider string) (fetch bool) {
	custom := p.isCustom()
	p.provider = provider
	p.open = false
	if p.cache == nil {
		p.cache = map[string]modelsLoadedMsg{}
	}
	p.seq++
	if c, ok := p.cache[provider]; ok && time.Since(c.at) < modelsTUICacheTTL {
		p.applyResult(c, custom)
		return false
	}
	p.opts, p.state, p.reason = nil, modelsLoading, ""
	if custom {
		p.sel = p.customIdx()
	} else {
		p.sel = 0
	}
	return true
}

// applyResult installs a list and preselects: the wanted id if the list has
// it, else custom if custom was chosen, else provider default.
func (p *modelPick) applyResult(r modelsLoadedMsg, custom bool) {
	want := p.want
	p.opts, p.reason = r.models, r.reason
	switch {
	case r.err != nil:
		p.state = modelsFailed
		p.reason = cleanModelText(r.err.Error(), 80)
		p.opts = nil
	case r.source == "fallback":
		p.state = modelsFallback
	default:
		p.state = modelsLive
	}
	switch i := p.indexOf(want); {
	case want != "" && i > 0:
		p.sel = i
	case custom:
		p.sel = p.customIdx()
	default:
		p.sel = 0
	}
	p.want = want
}

// loaded handles a Models result for the dialog: stale (other provider or an
// older request) results are cached but do not touch the selection.
func (p *modelPick) loaded(r modelsLoadedMsg) {
	if p.cache == nil {
		p.cache = map[string]modelsLoadedMsg{}
	}
	if r.cacheable() {
		p.cache[r.provider] = r
	}
	if r.provider != p.provider || r.seq != p.seq {
		return
	}
	p.applyResult(r, p.isCustom())
}

// hint is the status line under the model field ("" for none).
func (p *modelPick) hint(focused bool) string {
	switch p.state {
	case modelsLoading:
		return gl("loading…", "loading...")
	case modelsFallback:
		h := "offline list"
		if p.reason != "" {
			h += ": " + p.reason
		}
		return h
	case modelsFailed:
		return "no list: " + p.reason
	}
	if p.open {
		// The cursor row's full id: list rows truncate it.
		if p.cur >= 1 && p.cur <= len(p.opts) {
			return p.opts[p.cur-1].id
		}
		return ""
	}
	if id := p.listID(); id != "" && id != p.opts[p.sel-1].name {
		return id
	}
	if focused && p.state == modelsLive {
		return "enter: browse " + strconv.Itoa(len(p.opts)) + " models"
	}
	return ""
}

// truncCells cuts s to at most w terminal cells, ending in … when cut.
func truncCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if cellWidth(s) <= w {
		return s
	}
	ell := gl("…", ".")
	budget := w - cellWidth(ell)
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := cellWidth(string(r))
		if used+rw > budget {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + ell
}

// dropRow formats option i of the open list into w cells as (name, id):
// the id is rendered dimmed by the caller; both are truncated so the pair
// always fits, the id keeping at least a third of the row.
func (p *modelPick) dropRow(i, w int) (name, id string) {
	name = p.label(i)
	if i >= 1 && i <= len(p.opts) {
		id = p.opts[i-1].id
	}
	if id == "" || id == name {
		return truncCells(name, w), ""
	}
	nw, iw := cellWidth(name), cellWidth(id)
	if nw+1+iw <= w {
		return name, id
	}
	idB := min(iw, max(8, w/3))
	nameB := w - 1 - idB
	if nw < nameB {
		idB += nameB - nw
		nameB = nw
	}
	return truncCells(name, nameB), truncCells(id, idB)
}

// window returns the [start,end) slice of options visible in the open list,
// keeping the cursor in view.
func (p *modelPick) window() (int, int) {
	n := p.count()
	if n <= modelDropRows {
		return 0, n
	}
	start := min(max(0, p.cur-modelDropRows/2), n-modelDropRows)
	return start, start + modelDropRows
}

// jump moves the cursor to the next option after it whose name or id starts
// with r (case-insensitive) — type-ahead for long lists.
func (p *modelPick) jump(r rune) {
	n := p.count()
	lr := unicode.ToLower(r)
	for k := 1; k <= n; k++ {
		i := (p.cur + k) % n
		cands := []string{p.label(i)}
		if i >= 1 && i <= len(p.opts) {
			cands = append(cands, p.opts[i-1].id)
		}
		for _, c := range cands {
			for _, cr := range c {
				if unicode.ToLower(cr) == lr {
					p.cur = i
					return
				}
				break
			}
		}
	}
}

// validateModelID returns "" for a usable custom id, else the inline error.
func validateModelID(id string) string {
	switch {
	case id == "":
		return "model id required"
	case len(id) > 64:
		return "model id max 64 chars"
	case !modelIDRE.MatchString(id):
		return "model id: A-Z a-z 0-9 . _ : / -"
	}
	return ""
}
