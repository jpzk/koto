package main

// groupopts.go — the ONE table of new-group options.
//
// Every allowlisted value the TUI offers for a group knob — the new-group
// dialog's select fields, the /new command's provider and size checks — comes
// from groupOptTable below, so adding a value (a provider, a size preset) is a
// one-line change here and nowhere else. The values mirror what the daemon
// accepts (daemon/config.go applyConfig, daemon/grpc_server.go Spawn,
// daemon/fc.go fcSizePresets); the daemon stays the authority and re-validates
// everything, this table only keeps the client from offering what it would
// silently drop.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// groupOpt is one select-style knob: its config key, the allowlisted values in
// display order, the default (always values[0]), and the values worth a
// warning before submit (risky value → short note). Risky values are shown
// highlighted, never blocked.
type groupOpt struct {
	key    string
	values []string
	risky  map[string]string
	// spawn is true for the knobs the daemon's Spawn RPC itself takes
	// (provider, size); the rest go through config_set, which carries the
	// daemon's posture ACL (network/root are admin-only there).
	spawn bool
	// text marks a free-form knob (model, ports): no allowlist, the dialog
	// renders its own input for it. Such a row exists for the flags below and
	// for field order, never as a select.
	text bool
	// restart is true when a config_set of this key only takes effect after
	// the group's VM reboots (/restart). See the restart table above
	// groupOptTable for the daemon code each value is read from.
	restart bool
	// note is a short caveat on when a change applies, for keys where neither
	// "next turn" nor "on restart" is the whole story.
	note string
}

// effortDefault is the effort value meaning "leave it unset" — the daemon
// then runs the provider's own default.
const effortDefault = "default"

// When a config_set applies (the restart flag), read off the daemon:
//
//	provider  next request/turn. proxy.go groupProvider re-reads config.json
//	          on every proxied request; send.go hands config.json to the
//	          guest with every turn (fcSendMsg cfgB) and fcguest/turn.go
//	          readGroupConfig picks provider/model/effort from it per turn.
//	model     next turn, same path (fcguest/turn.go readGroupConfig).
//	effort    next turn, same path.
//	network   restart. daemon/config.go applyConfig: frame filter + guest env
//	          apply on the next spawn; only a LOWERING to none is enforced
//	          early, by the proxy's L7 gate (audit H1). Raising waits.
//	size      restart. fc.go fcSizePresets → vCPU/RAM in the VM config at
//	          boot (applyConfig "Applies on the next spawn").
//	root      restart. fc-agent applies it at boot (groupRoot → init "root").
//	kvm       restart. fcVMConfig masks VMX/SVM at boot (groupKVM).
//	autostart NO restart: read once at daemon start (groups.go groupAutostart,
//	          autostartGroups); a /restart of the group does nothing for it.
//	ports     restart. fc.go pushes the published-port list in the init
//	          request after boot and opens one host listener per port then.
//
// TestGroupOptRestartFlags pins this table.
//
// groupOptTable is the single source of truth. Order is the dialog's field
// order (text rows included).
var groupOptTable = []groupOpt{
	{key: "provider", values: []string{"claudesdk", "venice", "openai"}, spawn: true},
	{key: "model", text: true},
	{key: "effort", values: []string{effortDefault, "low", "medium", "high", "xhigh", "max"}},
	{key: "network", values: []string{"none", "wan", "lan", "full"}, restart: true, risky: map[string]string{
		"wan":  "public internet egress",
		"lan":  "host LAN reachable",
		"full": "internet + host LAN reachable",
	}},
	{key: "size", values: []string{"small", "medium", "large", "xlarge"}, spawn: true, restart: true},
	{key: "root", values: []string{"no", "yes"}, restart: true, risky: map[string]string{"yes": "passwordless sudo in guest"}},
	{key: "autostart", values: []string{"no", "yes"}, note: "applies at next daemon start"},
	{key: "kvm", values: []string{"no", "yes"}, restart: true, risky: map[string]string{"yes": "nested virtualization exposed"}},
	{key: "ports", text: true, restart: true},
}

// groupOptNeedsRestart reports whether changing key needs a VM restart to
// take effect (false for unknown keys).
func groupOptNeedsRestart(key string) bool {
	o, _ := groupOptByKey(key)
	return o.restart
}

// groupOptByKey returns the table row for key (zero value, false if absent).
func groupOptByKey(key string) (groupOpt, bool) {
	for _, o := range groupOptTable {
		if o.key == key {
			return o, true
		}
	}
	return groupOpt{}, false
}

// groupOptValues is the allowlist for key, in display order.
func groupOptValues(key string) []string {
	o, _ := groupOptByKey(key)
	return o.values
}

// groupOptDefault is key's default (the first value).
func groupOptDefault(key string) string {
	if v := groupOptValues(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// groupOptValid reports whether v is an allowlisted value for key.
func groupOptValid(key, v string) bool {
	for _, x := range groupOptValues(key) {
		if x == v {
			return true
		}
	}
	return false
}

// groupOptChoices renders key's allowlist as "a, b, or c" for error text.
func groupOptChoices(key string) string {
	v := groupOptValues(key)
	switch len(v) {
	case 0:
		return ""
	case 1:
		return v[0]
	case 2:
		return v[0] + " or " + v[1]
	}
	return strings.Join(v[:len(v)-1], ", ") + ", or " + v[len(v)-1]
}

// newGroupNameRE is the stricter of the daemon's two group-name rules (the
// ctl plane's lowercase one, daemon/ctl.go ctlGroupRE) so a name made here is
// valid on every plane.
var newGroupNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// validateNewGroupName returns "" for a usable new group name, else the
// inline error to show.
func validateNewGroupName(g string) string {
	switch {
	case g == "":
		return "name required"
	case len(g) > 32:
		return "name max 32 chars"
	case !newGroupNameRE.MatchString(g):
		return "name: a-z 0-9 _ - (start a-z/0-9)"
	case g == "main":
		return "name main is reserved"
	}
	return ""
}

// Port bounds, same as the daemon's ports knob (daemon/config.go: privileged
// ports refused, list capped at configMaxPorts).
const (
	portMin      = 1024
	portMax      = 65535
	portMaxCount = 32
)

// parsePortList parses "8080, 3000" / "8080 3000" into a canonical
// comma-joined list. Unlike the daemon, which silently drops bad entries, the
// dialog refuses the whole list with an inline error.
func parsePortList(s string) (string, error) {
	toks := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(toks) > portMaxCount {
		return "", fmt.Errorf("max %d ports", portMaxCount)
	}
	seen := map[int]bool{}
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		n, err := strconv.Atoi(t)
		if err != nil {
			return "", fmt.Errorf("port %q not a number", truncPort(t))
		}
		if n < portMin || n > portMax {
			return "", fmt.Errorf("port %d out of %d-%d", n, portMin, portMax)
		}
		if seen[n] {
			return "", fmt.Errorf("port %d repeated", n)
		}
		seen[n] = true
		out = append(out, strconv.Itoa(n))
	}
	return strings.Join(out, ","), nil
}

func truncPort(t string) string {
	if len(t) > 8 {
		return t[:8] + "…"
	}
	return t
}
