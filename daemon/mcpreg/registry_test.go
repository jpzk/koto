package mcpreg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "Bearer sk-test-SECRET-0123456789"

func sample() *Registry {
	r := New()
	r.Conns["gh"] = &Conn{
		URL:   "https://mcp.example.com/mcp",
		Auth:  &AuthHeader{Name: "Authorization", Value: secret},
		Class: ClassRead,
		Tools: map[string]*ToolRule{
			"get_issue": {Args: map[string]*ArgRule{
				"repo":   {Type: "string", Pattern: `koto/[a-z-]+`},
				"number": {Type: "integer", Minimum: f(1), Maximum: f(100000)},
			}},
			"search": {AdditionalArgs: true},
			"ping":   nil,
		},
		Groups: map[string]*Grant{
			"alpha": {Tools: map[string]*ToolRule{"get_issue": nil}},
		},
	}
	return r
}

func f(x float64) *float64 { return &x }
func ip(x int) *int        { return &x }

func TestParseRoundTrip(t *testing.T) {
	b, err := sample().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := r.Marshal()
	if string(b) != string(b2) {
		t.Fatalf("round trip differs:\n%s\n%s", b, b2)
	}
	if r.Conns["gh"].Tools["get_issue"].Args["repo"].re == nil {
		t.Fatal("pattern not compiled on parse")
	}
}

func TestParseRejects(t *testing.T) {
	good, _ := sample().Marshal()
	cases := map[string]string{
		"unknown field":  strings.Replace(string(good), `"class"`, `"klass": "read", "class"`, 1),
		"dup key":        strings.Replace(string(good), `"class": "read"`, `"class": "read", "class": "write"`, 1),
		"bad version":    strings.Replace(string(good), `"version": 1`, `"version": 9`, 1),
		"bad class":      strings.Replace(string(good), `"class": "read"`, `"class": "money"`, 1),
		"bad scheme":     strings.Replace(string(good), `https://`, `file://`, 1),
		"userinfo":       strings.Replace(string(good), `https://`, `https://u:p@`, 1),
		"bad pattern":    strings.Replace(string(good), `koto/[a-z-]+`, `koto/[`, 1),
		"unsupported kw": strings.Replace(string(good), `"type": "integer"`, `"type": "integer", "format": "x"`, 1),
		"bad type":       strings.Replace(string(good), `"type": "integer"`, `"type": "object"`, 1),
		"bad group":      strings.Replace(string(good), `"alpha"`, `"al pha"`, 1),
		"trailing":       string(good) + "{}",
		"crlf in secret": strings.Replace(string(good), "SECRET", `SE\r\nX-Evil: 1`, 1),
		"min>max":        strings.Replace(string(good), `"minimum": 1`, `"minimum": 999999`, 1),
	}
	for name, doc := range cases {
		if doc == string(good) && name != "trailing" {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: error leaks secret: %v", name, err)
		}
	}
}

func TestBadConnName(t *testing.T) {
	r := sample()
	r.Conns["Bad Name"] = r.Conns["gh"]
	if err := r.Validate(); err == nil {
		t.Fatal("accepted bad conn name")
	}
}

func TestSecretRedaction(t *testing.T) {
	r := sample()
	for _, s := range []string{
		fmt.Sprintf("%v", r.Conns["gh"].Auth),
		fmt.Sprintf("%+v", *r.Conns["gh"].Auth),
		fmt.Sprintf("%#v", *r.Conns["gh"].Auth),
		fmt.Sprintf("%v", *r.Conns["gh"]),
	} {
		if strings.Contains(s, "SECRET") {
			t.Fatalf("fmt leaks secret: %s", s)
		}
	}
	b, _ := json.Marshal(r.Redacted())
	if strings.Contains(string(b), "SECRET") {
		t.Fatal("Redacted leaks secret")
	}
	if r.Conns["gh"].Auth.Value != secret {
		t.Fatal("Redacted mutated original")
	}
}

func TestCheckArgs(t *testing.T) {
	tr := sample().Conns["gh"].Tools["get_issue"]
	ok := []string{`{"repo":"koto/koto","number":7}`, `{}`, ``, `null`, `{"number":1}`}
	bad := []string{
		`{"repo":"evil-koto/koto"}`,         // anchored pattern
		`{"repo":"koto/koto\nx"}`,           // anchored, no multiline
		`{"repo":7}`,                        // type
		`{"number":0}`,                      // minimum
		`{"number":100001}`,                 // maximum
		`{"number":1.5}`,                    // integer
		`{"number":"1"}`,                    // type
		`{"owner":"x"}`,                     // extra arg
		`{"repo":"koto/a","repo":"evil/b"}`, // duplicate key
		`{"repo":{"x":1,"x":2}}`,            // nested duplicate
		`[1]`,                               // not an object
		`{`,                                 // malformed
	}
	for _, a := range ok {
		if err := tr.CheckArgs(json.RawMessage(a)); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range bad {
		if err := tr.CheckArgs(json.RawMessage(a)); err == nil {
			t.Errorf("%s: accepted", a)
		}
	}
	// bare rule (nil) => no args at all
	var none *ToolRule
	if err := none.CheckArgs(json.RawMessage(`{"a":1}`)); err == nil {
		t.Error("nil rule accepted an argument")
	}
	extra := sample().Conns["gh"].Tools["search"]
	if err := extra.CheckArgs(json.RawMessage(`{"q":"x","y":2}`)); err != nil {
		t.Errorf("additional_args: %v", err)
	}
}

func TestArgRuleKeywords(t *testing.T) {
	r := &ToolRule{Args: map[string]*ArgRule{
		"c": {Const: json.RawMessage(`"fixed"`)},
		"e": {Enum: []json.RawMessage{json.RawMessage(`"a"`), json.RawMessage(`2`)}},
		"l": {Type: "string", MaxLength: ip(3)},
		"b": {Type: "boolean"},
		"n": {Type: "number", Maximum: f(1.5)},
	}}
	for _, a := range r.Args {
		if err := a.compile(); err != nil {
			t.Fatal(err)
		}
	}
	good := []string{`{"c":"fixed"}`, `{"e":"a"}`, `{"e":2.0}`, `{"l":"äöü"}`, `{"b":false}`, `{"n":1.5}`}
	bad := []string{`{"c":"other"}`, `{"e":"b"}`, `{"l":"abcd"}`, `{"b":"true"}`, `{"n":1.6}`, `{"n":true}`}
	for _, a := range good {
		if err := r.CheckArgs(json.RawMessage(a)); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range bad {
		if err := r.CheckArgs(json.RawMessage(a)); err == nil {
			t.Errorf("%s: accepted", a)
		}
	}
}

func TestOps(t *testing.T) {
	r := sample()
	if err := r.Grant("gh", "beta", "ping", nil); err == nil {
		t.Fatal("grant to un-whitelisted group accepted")
	}
	if err := r.Allow("gh", "beta"); err != nil {
		t.Fatal(err)
	}
	if err := r.Grant("gh", "beta", "delete_repo", nil); err == nil {
		t.Fatal("grant outside ceiling accepted (group widened the connection)")
	}
	if err := r.Grant("gh", "beta", "ping", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Revoke("gh", "beta", "ping"); err != nil {
		t.Fatal(err)
	}
	if err := r.Revoke("gh", "beta", "ping"); err == nil {
		t.Fatal("double revoke accepted")
	}
	if err := r.SetRate("gh", "beta", 5); err != nil || r.Conns["gh"].Groups["beta"].Rate.PerMinute != 5 {
		t.Fatal("rate", err)
	}
	if err := r.Deny("gh", "beta"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Conns["gh"].Groups["beta"]; ok {
		t.Fatal("deny kept group")
	}
	// PutConn keeps whitelist and (optionally) auth
	if err := r.PutConn("gh", &Conn{URL: "https://other.example/mcp", Tools: map[string]*ToolRule{"ping": nil}}, true); err != nil {
		t.Fatal(err)
	}
	c := r.Conns["gh"]
	if c.Auth == nil || c.Auth.Value != secret || c.Groups["alpha"] == nil || c.Class != ClassRead {
		t.Fatalf("PutConn lost state: %+v", c)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("dangling grant (ceiling shrank) must still validate: %v", err)
	}
	if err := r.RmConn("gh"); err != nil || len(r.Conns) != 0 {
		t.Fatal("rm", err)
	}
}

func writeRaw(t *testing.T, p string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestStorePersist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds", "mcp")
	p := filepath.Join(dir, "registry.json")
	s, err := Open(p)
	if err != nil {
		t.Fatalf("missing file must open empty: %v", err)
	}
	if len(s.Current().Conns) != 0 {
		t.Fatal("not empty")
	}
	if err := s.Update(func(r *Registry) error {
		*r = *sample()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi.Mode(), err)
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
	// reopen from disk
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Current().Conns["gh"].Auth.Value != secret {
		t.Fatal("not persisted")
	}
	// a failed update changes nothing
	if err := s2.Update(func(r *Registry) error { return r.Grant("gh", "alpha", "nope", nil) }); err == nil {
		t.Fatal("bad update accepted")
	}
	if err := s2.Update(func(r *Registry) error { r.Conns["gh"].Class = "money"; return nil }); err == nil {
		t.Fatal("invalid registry persisted")
	}
	r3, err := Open(p)
	if err != nil || r3.Current().Conns["gh"].Class != ClassRead {
		t.Fatal("failed update touched disk")
	}
}

func TestStoreLastGoodAndReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "registry.json")
	b, _ := sample().Marshal()
	writeRaw(t, p, b, 0o600)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	// external edit (revoke) is seen on the next Current()
	r := sample()
	delete(r.Conns["gh"].Groups, "alpha")
	b2, _ := r.Marshal()
	writeRaw(t, p+".new", b2, 0o600)
	os.Rename(p+".new", p)
	if _, ok := s.Current().Conns["gh"].Groups["alpha"]; ok {
		t.Fatal("external revoke not picked up")
	}
	// corrupt file: last good kept, error surfaced, update refused
	writeRaw(t, p+".bad", []byte(`{"version":1,"conns":{`), 0o600)
	os.Rename(p+".bad", p)
	if s.Current().Conns["gh"] == nil {
		t.Fatal("parse error dropped last good")
	}
	if s.LastError() == nil {
		t.Fatal("parse error not surfaced")
	}
	if err := s.Update(func(*Registry) error { return nil }); err == nil {
		t.Fatal("update over a corrupt file accepted")
	}
	// fixed again
	writeRaw(t, p+".ok", b, 0o600)
	os.Rename(p+".ok", p)
	if s.Current().Conns["gh"].Groups["alpha"] == nil || s.LastError() != nil {
		t.Fatal("recovery not picked up")
	}
	// removed file = deny all
	os.Remove(p)
	if len(s.Current().Conns) != 0 {
		t.Fatal("removed file kept conns")
	}
}

func TestStoreRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	b, _ := sample().Marshal()

	p := filepath.Join(dir, "world.json")
	writeRaw(t, p, b, 0o644)
	if _, err := Open(p); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("world-readable accepted: %v", err)
	}
	p = filepath.Join(dir, "group.json")
	writeRaw(t, p, b, 0o640)
	if _, err := Open(p); err == nil {
		t.Fatal("group-readable accepted")
	}

	real := filepath.Join(dir, "real.json")
	writeRaw(t, real, b, 0o600)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink accepted: %v", err)
	}
	// writing through a symlinked path is refused too
	s := &Store{path: link, cur: New()}
	if err := s.Update(func(r *Registry) error { *r = *sample(); return nil }); err == nil {
		t.Fatal("update replaced a symlink")
	}

	d := filepath.Join(dir, "adir")
	os.Mkdir(d, 0o700)
	if _, err := Open(d); err == nil {
		t.Fatal("directory accepted")
	}

	// symlinked parent dir refused on write
	os.Mkdir(filepath.Join(dir, "target"), 0o700)
	os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "ldir"))
	s2, _ := Open(filepath.Join(dir, "ldir", "r.json"))
	if err := s2.Update(func(r *Registry) error { return nil }); err == nil {
		t.Fatal("write through symlinked dir accepted")
	}
}

func TestStoreUmaskProof(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.json")
	s, _ := Open(p)
	if err := s.Update(func(r *Registry) error { *r = *sample(); return nil }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), secret) {
		t.Fatal("secret must persist host-side")
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %v", fi.Mode())
	}
}
