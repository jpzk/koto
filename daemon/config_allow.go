package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"koto/netallow"
)

// ---- egress allowlist (config key "allow") ----------------------------------
//
// The allowlist narrows the network profile: effective = profile ∩ allow, and
// an empty/absent list is exactly the pre-allowlist behaviour. It is stored in
// config.json as a JSON array of CANONICAL entries (netallow's spelling), so
// what the operator reads back is what the gateway enforces.
//
// Unlike most config keys a bad value is REFUSED with an error rather than
// silently dropped: a typo in an egress allowlist must never quietly leave the
// previous (possibly wider) list in force without the operator noticing.

const cfgAllow = "allow"

// errAllowCorrupt marks a stored list that no longer parses (hand edit,
// downgrade). Enforcement treats it as deny-all — fail closed.
var errAllowCorrupt = errors.New("stored allowlist is invalid")

// allowFromRaw validates a ConfigReq.allow value BEFORE the config write.
// Accepted shapes: a comma-separated string ("a, b,c") or a JSON array of
// strings. "", null and [] clear the list. ok reports whether the request
// touches the key at all.
func allowFromRaw(raw json.RawMessage) (l *netallow.List, ok bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if isClear(raw) {
		return nil, true, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		l, err = netallow.Parse(s)
		return l, true, err
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		l, err = netallow.ParseEntries(arr)
		return l, true, err
	}
	return nil, true, fmt.Errorf("allow must be a comma-separated string or a list of strings")
}

// setAllowConfig writes a validated list; an empty list deletes the key so
// the on-disk form of "no allowlist" is unique.
func setAllowConfig(cfg map[string]any, l *netallow.List) {
	if l.Empty() {
		delete(cfg, cfgAllow)
		return
	}
	cfg[cfgAllow] = l.Entries()
}

// allow returns the group's parsed allowlist. nil, nil = no allowlist (the
// profile alone decides). A stored value that no longer parses returns
// errAllowCorrupt; callers enforcing egress must treat that as deny-all.
func (c groupConfig) allow() (*netallow.List, error) {
	v, present := c[cfgAllow]
	if !present || v == nil {
		return nil, nil
	}
	var entries []string
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, errAllowCorrupt
			}
			entries = append(entries, s)
		}
	case []string:
		entries = t
	case string: // hand-edited comma form
		entries = strings.Split(t, ",")
	default:
		return nil, errAllowCorrupt
	}
	l, err := netallow.ParseEntries(entries)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAllowCorrupt, err)
	}
	return l, nil
}

// allowDisplay is the list as clients render it: the canonical entries, or a
// single marker when the stored list is corrupt (enforced as deny-all).
func (c groupConfig) allowDisplay() []string {
	l, err := c.allow()
	if err != nil {
		return []string{allowCorruptMarker}
	}
	return l.Entries()
}

const allowCorruptMarker = "!invalid (deny-all)"

// groupAllow is the per-group accessor for runtime callers.
func groupAllow(g string) (*netallow.List, error) { return loadGroupConfig(g).allow() }

// ---- live allow state --------------------------------------------------------
//
// The L3 gateway filter and the L7 proxy gate run host-side, so a changed
// allowlist does not need a VM restart: both read the group's current policy
// from here on every new flow. configCmd swaps it atomically after the
// config.json commit; spawn seeds it from config.json.

// allowPolicy is one immutable snapshot. denyAll is set when the stored list
// is corrupt (fail closed).
type allowPolicy struct {
	list    *netallow.List
	denyAll bool
}

// active reports whether the policy narrows the profile at all.
func (p *allowPolicy) active() bool { return p != nil && (p.denyAll || !p.list.Empty()) }

var liveAllow sync.Map // group -> *atomic.Pointer[allowPolicy]

func allowSlot(g string) *atomic.Pointer[allowPolicy] {
	v, _ := liveAllow.LoadOrStore(g, &atomic.Pointer[allowPolicy]{})
	return v.(*atomic.Pointer[allowPolicy])
}

// currentAllow is what enforcement consults. If nothing was ever stored for
// the group (e.g. a test or a path that skipped seeding) it falls back to
// config.json so the answer is never "no policy" by accident.
func currentAllow(g string) *allowPolicy {
	if p := allowSlot(g).Load(); p != nil {
		return p
	}
	return seedAllow(g)
}

// seedAllow (re)loads the policy from config.json — called at spawn.
func seedAllow(g string) *allowPolicy {
	l, err := groupAllow(g)
	p := &allowPolicy{list: l, denyAll: err != nil}
	allowSlot(g).Store(p)
	return p
}

// allowApplyLive publishes a freshly committed list. The return value is
// shown to the operator: the swap is always immediate for new flows; flows
// already open to a now-disallowed destination are dropped on their next
// packet (the gateway re-checks established flows against the generation).
func allowApplyLive(g string, l *netallow.List) string {
	allowSlot(g).Store(&allowPolicy{list: l})
	if fcRunning(g) {
		return "live (applied to the running VM, no restart needed)"
	}
	return "stored (applies when the VM next starts)"
}
