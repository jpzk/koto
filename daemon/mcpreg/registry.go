// Package mcpreg is the daemon's MCP connection registry: operator-registered
// remote MCP servers ("connections"), each with its own tool ceiling, shared by
// a whitelist of groups that the operator can narrow further per group.
//
// The file holds host-only upstream credentials, so the loader is strict: it
// refuses symlinks (O_NOFOLLOW), non-regular files and any group/world
// permission bits, rejects unknown fields and duplicate JSON keys, and keeps
// the LAST GOOD registry when a reload fails instead of failing open.
package mcpreg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Version is the on-disk schema version.
const Version = 1

// Class is the coarse effect class of a connection. Only "read" is served by
// the gateway in this release; "write" is stored so policy can be authored
// ahead of approval support, but calls on it are refused.
type Class string

const (
	ClassRead  Class = "read"
	ClassWrite Class = "write"
)

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	groupRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	toolRE    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	headerRE  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	argNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

// ValidName reports whether s is a valid connection name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidGroup reports whether s is a valid koto group name.
func ValidGroup(s string) bool { return groupRE.MatchString(s) }

// ValidTool reports whether s is a valid tool name.
func ValidTool(s string) bool { return toolRE.MatchString(s) }

// AuthHeader is a static upstream auth header. Value is a secret: it only
// ever travels daemon -> upstream, never into a guest, a log or a CLI view.
type AuthHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// String redacts the value so a stray %v never leaks it.
func (a AuthHeader) String() string { return a.Name + ": [redacted]" }

// GoString redacts too (%#v).
func (a AuthHeader) GoString() string { return a.String() }

// ArgRule is the JSON-schema SUBSET allowed for one argument. Any keyword
// outside it is a load error (unknown field), never silently ignored.
type ArgRule struct {
	Type      string            `json:"type,omitempty"` // string|number|integer|boolean
	Const     json.RawMessage   `json:"const,omitempty"`
	Enum      []json.RawMessage `json:"enum,omitempty"`
	Pattern   string            `json:"pattern,omitempty"` // RE2, anchored ^(?:...)$
	MaxLength *int              `json:"maxLength,omitempty"`
	Minimum   *float64          `json:"minimum,omitempty"`
	Maximum   *float64          `json:"maximum,omitempty"`

	re *regexp.Regexp
}

// ToolRule constrains one tool. A nil/empty Args with AdditionalArgs=false
// means the tool may only be called with no arguments.
type ToolRule struct {
	Args           map[string]*ArgRule `json:"args,omitempty"`
	AdditionalArgs bool                `json:"additional_args,omitempty"`
}

// Rate is a per-group call budget on one connection.
type Rate struct {
	PerMinute int `json:"per_minute,omitempty"`
}

// Grant is one whitelisted group's narrowing of a connection. Presence in
// Conn.Groups is the whitelist entry; Tools is the subset it may use.
type Grant struct {
	Tools map[string]*ToolRule `json:"tools,omitempty"`
	Rate  *Rate                `json:"rate,omitempty"`
}

// Conn is one registered upstream MCP connection (streamable HTTP).
type Conn struct {
	URL    string               `json:"url"`
	Auth   *AuthHeader          `json:"auth,omitempty"`
	Class  Class                `json:"class"`
	Tools  map[string]*ToolRule `json:"tools,omitempty"` // the connection ceiling
	Groups map[string]*Grant    `json:"groups,omitempty"`
	// Disabled deactivates the connection without forgetting it: the
	// ceiling, whitelist, grants and auth stay stored, but the gateway and
	// the harness config treat it exactly as absent. omitempty: files
	// written before the flag existed load as enabled.
	Disabled bool `json:"disabled,omitempty"`
}

// Registry is the whole file.
type Registry struct {
	Version int              `json:"version"`
	Conns   map[string]*Conn `json:"conns"`
}

// New returns an empty, valid registry.
func New() *Registry { return &Registry{Version: Version, Conns: map[string]*Conn{}} }

// Parse decodes and validates a registry, rejecting unknown fields and
// duplicate object keys anywhere in the document.
func Parse(b []byte) (*Registry, error) {
	if err := CheckNoDupKeys(b); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	r := &Registry{}
	if err := dec.Decode(r); err != nil {
		return nil, fmt.Errorf("mcp registry: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("mcp registry: trailing data after document")
	}
	if r.Conns == nil {
		r.Conns = map[string]*Conn{}
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// Marshal encodes the registry deterministically (map keys are sorted by
// encoding/json).
func (r *Registry) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Clone deep-copies via the wire form, recompiling patterns.
func (r *Registry) Clone() *Registry {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	c := &Registry{}
	if err := json.Unmarshal(b, c); err != nil {
		panic(err)
	}
	if c.Conns == nil {
		c.Conns = map[string]*Conn{}
	}
	_ = c.Validate() // compiles patterns; r was valid
	return c
}

// Validate checks the whole registry and compiles argument patterns.
// Grants naming tools outside the ceiling are NOT an error here (the ceiling
// may shrink after a grant); they are simply never effective. Grant() refuses
// to create them.
func (r *Registry) Validate() error {
	if r.Version != Version {
		return fmt.Errorf("mcp registry: unsupported version %d (want %d)", r.Version, Version)
	}
	for _, n := range sortedKeys(r.Conns) {
		if err := validateConn(n, r.Conns[n]); err != nil {
			return err
		}
	}
	return nil
}

func validateConn(name string, c *Conn) error {
	if !ValidName(name) {
		return fmt.Errorf("mcp conn %q: invalid name (want %s)", name, nameRE)
	}
	if c == nil {
		return fmt.Errorf("mcp conn %q: null", name)
	}
	if err := ValidateURL(c.URL); err != nil {
		return fmt.Errorf("mcp conn %q: %w", name, err)
	}
	if c.Auth != nil {
		if !headerRE.MatchString(c.Auth.Name) {
			return fmt.Errorf("mcp conn %q: invalid auth header name", name)
		}
		if c.Auth.Value == "" || strings.ContainsAny(c.Auth.Value, "\r\n\x00") {
			// never echo the value
			return fmt.Errorf("mcp conn %q: auth header value empty or contains control characters", name)
		}
	}
	switch c.Class {
	case ClassRead, ClassWrite:
	default:
		return fmt.Errorf("mcp conn %q: class must be read or write", name)
	}
	for _, t := range sortedKeys(c.Tools) {
		if err := validateTool(name, "ceiling", t, c.Tools[t]); err != nil {
			return err
		}
	}
	for _, g := range sortedKeys(c.Groups) {
		if !ValidGroup(g) {
			return fmt.Errorf("mcp conn %q: invalid group name %q", name, g)
		}
		gr := c.Groups[g]
		if gr == nil {
			return fmt.Errorf("mcp conn %q group %q: null grant", name, g)
		}
		if gr.Rate != nil && gr.Rate.PerMinute < 0 {
			return fmt.Errorf("mcp conn %q group %q: negative rate", name, g)
		}
		for _, t := range sortedKeys(gr.Tools) {
			if err := validateTool(name, "group "+g, t, gr.Tools[t]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateURL accepts http(s) URLs with a host and no userinfo/fragment
// (credentials belong in the auth header, where they are redacted).
func ValidateURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("invalid url")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("url scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("url has no host")
	}
	if u.User != nil {
		return errors.New("url must not carry userinfo; use the auth header")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return errors.New("url must not carry a fragment")
	}
	return nil
}

func validateTool(conn, where, name string, t *ToolRule) error {
	if !ValidTool(name) {
		return fmt.Errorf("mcp conn %q %s: invalid tool name %q", conn, where, name)
	}
	if t == nil {
		return nil // bare entry: no args allowed
	}
	for _, a := range sortedKeys(t.Args) {
		if !argNameRE.MatchString(a) {
			return fmt.Errorf("mcp conn %q %s tool %q: invalid arg name %q", conn, where, name, a)
		}
		if err := t.Args[a].compile(); err != nil {
			return fmt.Errorf("mcp conn %q %s tool %q arg %q: %w", conn, where, name, a, err)
		}
	}
	return nil
}

func (a *ArgRule) compile() error {
	if a == nil {
		return errors.New("null rule")
	}
	switch a.Type {
	case "", "string", "number", "integer", "boolean":
	default:
		return fmt.Errorf("unsupported type %q", a.Type)
	}
	if a.Const != nil && !json.Valid(a.Const) {
		return errors.New("const is not valid JSON")
	}
	if a.MaxLength != nil && *a.MaxLength < 0 {
		return errors.New("maxLength < 0")
	}
	if a.Minimum != nil && a.Maximum != nil && *a.Minimum > *a.Maximum {
		return errors.New("minimum > maximum")
	}
	a.re = nil
	if a.Pattern != "" {
		re, err := regexp.Compile(`^(?:` + a.Pattern + `)$`)
		if err != nil {
			return fmt.Errorf("pattern: %v", err)
		}
		a.re = re
	}
	return nil
}

// CheckArgs validates call arguments (a JSON object, or empty/null for none)
// against the rule. Duplicate keys are rejected so the gateway and the
// upstream parser cannot disagree about what was sent.
func (t *ToolRule) CheckArgs(raw json.RawMessage) error {
	args, err := DecodeArgs(raw)
	if err != nil {
		return err
	}
	var rules map[string]*ArgRule
	allowExtra := false
	if t != nil {
		rules, allowExtra = t.Args, t.AdditionalArgs
	}
	for _, k := range sortedKeys(args) {
		r, ok := rules[k]
		if !ok {
			if allowExtra {
				continue
			}
			return fmt.Errorf("argument %q not permitted", k)
		}
		if err := r.check(args[k]); err != nil {
			return fmt.Errorf("argument %q: %w", k, err)
		}
	}
	return nil
}

// DecodeArgs decodes a tool-call arguments object with duplicate-key
// rejection. Empty or null means no arguments.
func DecodeArgs(raw json.RawMessage) (map[string]json.RawMessage, error) {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 || bytes.Equal(s, []byte("null")) {
		return map[string]json.RawMessage{}, nil
	}
	if s[0] != '{' {
		return nil, errors.New("arguments must be a JSON object")
	}
	if err := CheckNoDupKeys(s); err != nil {
		return nil, err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(s, &m); err != nil {
		return nil, fmt.Errorf("arguments: %v", err)
	}
	return m, nil
}

func (a *ArgRule) check(v json.RawMessage) error {
	if a == nil {
		return errors.New("no rule")
	}
	if a.Pattern != "" && a.re == nil {
		if err := a.compile(); err != nil {
			return err
		}
	}
	var x any
	d := json.NewDecoder(bytes.NewReader(v))
	d.UseNumber()
	if err := d.Decode(&x); err != nil {
		return errors.New("invalid JSON value")
	}
	switch a.Type {
	case "string":
		if _, ok := x.(string); !ok {
			return errors.New("must be a string")
		}
	case "boolean":
		if _, ok := x.(bool); !ok {
			return errors.New("must be a boolean")
		}
	case "number":
		if _, ok := x.(json.Number); !ok {
			return errors.New("must be a number")
		}
	case "integer":
		n, ok := x.(json.Number)
		if !ok {
			return errors.New("must be an integer")
		}
		if _, err := n.Int64(); err != nil {
			return errors.New("must be an integer")
		}
	}
	if a.Const != nil && !jsonEqual(a.Const, v) {
		return errors.New("value not allowed (const)")
	}
	if len(a.Enum) > 0 {
		hit := false
		for _, e := range a.Enum {
			if jsonEqual(e, v) {
				hit = true
				break
			}
		}
		if !hit {
			return errors.New("value not allowed (enum)")
		}
	}
	if a.re != nil || a.MaxLength != nil {
		s, ok := x.(string)
		if !ok {
			return errors.New("must be a string")
		}
		if a.MaxLength != nil && utf8.RuneCountInString(s) > *a.MaxLength {
			return errors.New("too long")
		}
		if a.re != nil && !a.re.MatchString(s) {
			return errors.New("value does not match pattern")
		}
	}
	if a.Minimum != nil || a.Maximum != nil {
		n, ok := x.(json.Number)
		if !ok {
			return errors.New("must be a number")
		}
		f, err := n.Float64()
		if err != nil {
			return errors.New("must be a number")
		}
		if a.Minimum != nil && f < *a.Minimum {
			return errors.New("below minimum")
		}
		if a.Maximum != nil && f > *a.Maximum {
			return errors.New("above maximum")
		}
	}
	return nil
}

// jsonEqual compares two JSON values semantically (numbers by value).
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	return canon(x) == canon(y)
}

func canon(v any) string {
	switch t := v.(type) {
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return fmt.Sprintf("n:%v", f)
		}
		return "n:" + t.String()
	case map[string]any:
		var sb strings.Builder
		sb.WriteString("{")
		for _, k := range sortedKeys(t) {
			b, _ := json.Marshal(k)
			sb.Write(b)
			sb.WriteString(":")
			sb.WriteString(canon(t[k]))
			sb.WriteString(",")
		}
		sb.WriteString("}")
		return sb.String()
	case []any:
		var sb strings.Builder
		sb.WriteString("[")
		for _, e := range t {
			sb.WriteString(canon(e))
			sb.WriteString(",")
		}
		sb.WriteString("]")
		return sb.String()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// CheckNoDupKeys walks a JSON document and fails on any object with a
// repeated key, at any depth.
func CheckNoDupKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := walkDup(d); err != nil {
		return err
	}
	return nil
}

func walkDup(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("json: %v", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return fmt.Errorf("json: %v", err)
			}
			k, _ := kt.(string)
			if seen[k] {
				return fmt.Errorf("duplicate JSON key %q", k)
			}
			seen[k] = true
			if err := walkDup(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := walkDup(d); err != nil {
				return err
			}
		}
	}
	if _, err := d.Token(); err != nil { // closing delim
		return fmt.Errorf("json: %v", err)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Redacted returns a deep copy with every auth value replaced, for CLI views
// and logs.
func (r *Registry) Redacted() *Registry {
	c := r.Clone()
	for _, cn := range c.Conns {
		if cn.Auth != nil {
			cn.Auth.Value = "[redacted]"
		}
	}
	return c
}
