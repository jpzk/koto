package mcpreg

import (
	"errors"
	"fmt"
)

// The operator-plane mutations. Each is meant to run inside Store.Update.

// PutConn adds or replaces a connection's upstream settings and ceiling,
// keeping its existing whitelist/grants. A nil auth keeps the stored one
// when keepAuth is true (so `conn add` re-runs need not re-enter the secret).
func (r *Registry) PutConn(name string, c *Conn, keepAuth bool) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid connection name %q", name)
	}
	if c == nil {
		return errors.New("nil connection")
	}
	nc := *c
	if old, ok := r.Conns[name]; ok {
		nc.Groups = old.Groups
		nc.Disabled = old.Disabled // a re-add never silently re-enables
		if nc.Auth == nil && keepAuth {
			nc.Auth = old.Auth
		}
	} else {
		nc.Groups = nil
	}
	if nc.Class == "" {
		nc.Class = ClassRead
	}
	r.Conns[name] = &nc
	return nil
}

// RmConn deletes a connection (and with it every group's access).
func (r *Registry) RmConn(name string) error {
	if _, ok := r.Conns[name]; !ok {
		return fmt.Errorf("no connection %q", name)
	}
	delete(r.Conns, name)
	return nil
}

func (r *Registry) conn(name string) (*Conn, error) {
	c, ok := r.Conns[name]
	if !ok {
		return nil, fmt.Errorf("no connection %q", name)
	}
	return c, nil
}

// Allow whitelists group on conn with no tools yet (idempotent: an existing
// grant is kept).
func (r *Registry) Allow(conn, group string) error {
	c, err := r.conn(conn)
	if err != nil {
		return err
	}
	if !ValidGroup(group) {
		return fmt.Errorf("invalid group name %q", group)
	}
	if c.Groups == nil {
		c.Groups = map[string]*Grant{}
	}
	if _, ok := c.Groups[group]; !ok {
		c.Groups[group] = &Grant{}
	}
	return nil
}

// Deny removes group from conn's whitelist, dropping its grants.
func (r *Registry) Deny(conn, group string) error {
	c, err := r.conn(conn)
	if err != nil {
		return err
	}
	if _, ok := c.Groups[group]; !ok {
		return fmt.Errorf("group %q is not whitelisted on %q", group, conn)
	}
	delete(c.Groups, group)
	return nil
}

// Grant gives a whitelisted group one tool, optionally narrowed by rule. The
// tool must be in the connection ceiling: a grant can never widen it.
func (r *Registry) Grant(conn, group, tool string, rule *ToolRule) error {
	c, err := r.conn(conn)
	if err != nil {
		return err
	}
	g, ok := c.Groups[group]
	if !ok {
		return fmt.Errorf("group %q is not whitelisted on %q (mcp allow first)", group, conn)
	}
	if _, ok := c.Tools[tool]; !ok {
		return fmt.Errorf("tool %q is not in connection %q's ceiling", tool, conn)
	}
	if g.Tools == nil {
		g.Tools = map[string]*ToolRule{}
	}
	g.Tools[tool] = rule
	return nil
}

// Revoke removes one tool from a group's grant.
func (r *Registry) Revoke(conn, group, tool string) error {
	c, err := r.conn(conn)
	if err != nil {
		return err
	}
	g, ok := c.Groups[group]
	if !ok {
		return fmt.Errorf("group %q is not whitelisted on %q", group, conn)
	}
	if _, ok := g.Tools[tool]; !ok {
		return fmt.Errorf("group %q has no grant for tool %q on %q", group, tool, conn)
	}
	delete(g.Tools, tool)
	return nil
}

// SetRate sets a group's per-minute cap on conn (0 = connection default).
func (r *Registry) SetRate(conn, group string, perMinute int) error {
	c, err := r.conn(conn)
	if err != nil {
		return err
	}
	g, ok := c.Groups[group]
	if !ok {
		return fmt.Errorf("group %q is not whitelisted on %q", group, conn)
	}
	if perMinute < 0 {
		return errors.New("rate must be >= 0")
	}
	if perMinute == 0 {
		g.Rate = nil
	} else {
		g.Rate = &Rate{PerMinute: perMinute}
	}
	return nil
}

// SetEnabled activates or deactivates a connection. Nothing else changes:
// ceiling, auth, whitelist and grants are kept, so re-enabling restores the
// exact previous access.
func (r *Registry) SetEnabled(name string, enabled bool) error {
	c, err := r.conn(name)
	if err != nil {
		return err
	}
	c.Disabled = !enabled
	return nil
}
