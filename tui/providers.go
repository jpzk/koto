package main

// providers.go — which providers the new-group (ctrl+n) and edit-group
// (ctrl+e) dialogs offer.
//
// The daemon's Providers RPC says, per provider, whether a credential is set
// up on its host (no secret, just name/configured/kind). The dialog's
// provider select then offers only the configured ones:
//
//   - edit mode keeps the group's CURRENT provider even when it has no login
//     (marked), so Save never silently switches provider;
//   - none configured: an inline hint naming the login commands, and Create
//     refuses (plan error on the provider field);
//   - RPC unavailable (an older daemon without Providers, or any error): the
//     full allowlist, as before this feature, plus a dim note.
//
// While the answer is in flight the select shows the full allowlist (the
// pre-feature behaviour); on arrival the current choice is kept when it is
// still offered.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	provLoading  = iota // Providers RPC in flight: full list
	provLoaded          // filtered to configured (+ edit's current)
	provFallback        // RPC failed: full list + note
)

// provChoice is the dialog's view of the Providers answer.
type provChoice struct {
	state      int
	configured map[string]string // provider -> kind, configured ones only
	err        string            // provFallback: why (shown dim, cleaned)
}

// providersLoadedMsg carries one Providers answer.
type providersLoadedMsg struct {
	configured map[string]string
	err        error
}

// providersCmd asks the daemon which providers have a credential. Names the
// TUI does not know are dropped; kinds are cleaned like any daemon string.
func providersCmd(sock string) tea.Cmd {
	return func() tea.Msg {
		out, err := daemonCall(sock, "providers", nil)
		if err != nil {
			return providersLoadedMsg{err: err}
		}
		conf := map[string]string{}
		raw, _ := out["providers"].([]any)
		for _, r := range raw {
			p, _ := r.(map[string]any)
			name := s2(p["name"])
			if ok, _ := p["configured"].(bool); ok && groupOptValid("provider", name) {
				conf[name] = provKind(s2(p["kind"]))
			}
		}
		return providersLoadedMsg{configured: conf}
	}
}

// providerValues is the provider select's options, in allowlist order.
func (f *newGroupForm) providerValues() []string {
	all := groupOptValues("provider")
	if f.prov.state != provLoaded {
		return all
	}
	cur, curKnown := "", false
	if f.edit.on && !f.edit.loading {
		cur, curKnown = f.edit.orig["provider"]
	}
	out := make([]string, 0, len(all))
	for _, p := range all {
		if _, ok := f.prov.configured[p]; ok || (curKnown && p == cur) {
			out = append(out, p)
		}
	}
	return out
}

// values is a select key's options: the table's, except provider.
func (f *newGroupForm) values(key string) []string {
	if key == "provider" {
		return f.providerValues()
	}
	return groupOptValues(key)
}

// selectValue points key's select at v when v is offered, else at the first
// option; reports whether v was found.
func (f *newGroupForm) selectValue(key, v string) bool {
	for i, x := range f.values(key) {
		if x == v {
			f.sel[key] = i
			return true
		}
	}
	f.sel[key] = 0
	return false
}

// providerNoLogin reports whether provider p is known to have no credential
// (only after a successful Providers answer).
func (f *newGroupForm) providerNoLogin(p string) bool {
	if f.prov.state != provLoaded || p == "" {
		return false
	}
	_, ok := f.prov.configured[p]
	return !ok
}

// noProviderLogin: the daemon answered and nothing is configured.
func (f *newGroupForm) noProviderLogin() bool {
	return f.prov.state == provLoaded && len(f.prov.configured) == 0
}

// handleProvidersLoaded applies an answer to the open dialog, keeping the
// current provider when still offered, and reloads the model list when the
// provider changed under it.
func (m *Model) handleProvidersLoaded(msg providersLoadedMsg) tea.Cmd {
	f := &m.newGroup
	if !f.open || f.prov.state != provLoading {
		return nil
	}
	before := f.value("provider")
	if msg.err != nil {
		f.prov = provChoice{state: provFallback, err: provErrText(msg.err)}
	} else {
		f.prov = provChoice{state: provLoaded, configured: msg.configured}
	}
	f.selectValue("provider", before)
	if f.edit.on && f.edit.loading {
		return nil // handleEditRead selects the group's provider and loads models
	}
	if f.value("provider") != before {
		return m.reloadModels()
	}
	return nil
}

// providerHintLines are the lines under the provider row: the no-login
// mark, the empty-state login hint, the fallback note. Plain text; the
// caller styles and wraps them.
func (f *newGroupForm) providerHintLines() (warn, dim []string) {
	switch f.prov.state {
	case provFallback:
		dim = append(dim, "login check unavailable ("+f.prov.err+"); showing all providers")
	case provLoaded:
		if v := f.value("provider"); f.providerNoLogin(v) {
			warn = append(warn, "! "+v+": no login on this host")
		}
		if f.noProviderLogin() {
			warn = append(warn, "no provider has a login on this host. run: koto claude-login or koto openai-login")
		}
	}
	return warn, dim
}

// provKind passes the daemon's coarse credential kind through only when it
// is one of the documented values.
func provKind(k string) string {
	switch k {
	case "subscription", "api-key", "env", "upstream":
		return k
	}
	return ""
}

// provErrText is the fallback note's reason: "older daemon" for a daemon
// without the Providers RPC, else the cleaned error, kept short.
func provErrText(err error) string {
	if status.Code(err) == codes.Unimplemented {
		return "older daemon"
	}
	s := strings.TrimPrefix(err.Error(), "daemon: ")
	return cleanModelText(s, 40)
}
