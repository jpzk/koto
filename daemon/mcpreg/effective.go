package mcpreg

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Effective is what one group may do on one connection right now:
// intersection(connection ceiling, group grant). It is recomputed from the
// current registry on every gateway request, so a revoke bites on the next
// call.
type Effective struct {
	Conn  string
	Group string
	Class Class
	// Tools holds only names present in BOTH the ceiling and the grant.
	Tools map[string]EffTool
	Rate  *Rate
}

// EffTool pairs the two rules for one tool. A call must satisfy both, so a
// permissive grant rule (e.g. additional_args) can never admit an argument
// the ceiling refuses: narrowing composes, widening is impossible by
// construction.
type EffTool struct {
	Ceiling *ToolRule // nil: no arguments allowed
	Grant   *ToolRule // nil: no further narrowing beyond the ceiling
}

// ErrNotWhitelisted is returned for a group absent from a connection's
// whitelist (or an unknown connection): deny by default.
var ErrNotWhitelisted = errors.New("not whitelisted")

// Effective computes the group's permissions on conn. Unknown connection or
// group not on the whitelist => ErrNotWhitelisted and an empty set. Grants
// naming a tool that is no longer in the ceiling are dropped. Connections of
// class "write" expose nothing in this version (no approval flow yet).
func (r *Registry) Effective(conn, group string) (*Effective, error) {
	e := &Effective{Conn: conn, Group: group, Tools: map[string]EffTool{}}
	if r == nil {
		return e, ErrNotWhitelisted
	}
	c, ok := r.Conns[conn]
	if !ok || c == nil || c.Disabled {
		return e, ErrNotWhitelisted
	}
	return r.effective(e, c, group)
}

// Preview computes what Effective would return if conn were enabled. It is
// for operator views only (the TUI group editor and `mcp effective` show
// the grants a disabled connection keeps); the gateway must use Effective.
func (r *Registry) Preview(conn, group string) (*Effective, error) {
	e := &Effective{Conn: conn, Group: group, Tools: map[string]EffTool{}}
	if r == nil {
		return e, ErrNotWhitelisted
	}
	c, ok := r.Conns[conn]
	if !ok || c == nil {
		return e, ErrNotWhitelisted
	}
	return r.effective(e, c, group)
}

func (r *Registry) effective(e *Effective, c *Conn, group string) (*Effective, error) {
	g, ok := c.Groups[group]
	if !ok || g == nil {
		return e, ErrNotWhitelisted
	}
	e.Class, e.Rate = c.Class, g.Rate
	if c.Class != ClassRead {
		return e, nil
	}
	for name, gr := range g.Tools {
		cr, ok := c.Tools[name]
		if !ok {
			continue // dangling grant: ceiling shrank, never effective
		}
		e.Tools[name] = EffTool{Ceiling: cr, Grant: gr}
	}
	return e, nil
}

// ConnsFor lists the ENABLED connections group is whitelisted on (sorted).
// It feeds the harness config, so a disabled connection is left out.
func (r *Registry) ConnsFor(group string) []string {
	var out []string
	if r == nil {
		return nil
	}
	for _, n := range sortedKeys(r.Conns) {
		c := r.Conns[n]
		if c == nil || c.Disabled {
			continue
		}
		if _, ok := c.Groups[group]; ok {
			out = append(out, n)
		}
	}
	return out
}

// Allowed reports whether tool is in the effective set.
func (e *Effective) Allowed(tool string) bool {
	_, ok := e.Tools[tool]
	return ok
}

// Check authorises one tools/call: the tool must be effective and the
// arguments must pass the ceiling rule AND the grant rule.
func (e *Effective) Check(tool string, args json.RawMessage) error {
	t, ok := e.Tools[tool]
	if !ok {
		return fmt.Errorf("tool %q not permitted for group %q on %q", tool, e.Group, e.Conn)
	}
	if err := t.Ceiling.CheckArgs(args); err != nil {
		return fmt.Errorf("tool %q: %w (connection ceiling)", tool, err)
	}
	if t.Grant != nil {
		if err := t.Grant.CheckArgs(args); err != nil {
			return fmt.Errorf("tool %q: %w (group grant)", tool, err)
		}
	}
	return nil
}

// Names returns the effective tool names, sorted.
func (e *Effective) Names() []string { return sortedKeys(e.Tools) }
