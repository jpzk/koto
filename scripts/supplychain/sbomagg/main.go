// sbomagg — merge koto's per-binary Go SBOMs and the pinned non-Go inputs into
// ONE CycloneDX 1.6 JSON document describing a koto release:
//
//	koto (root)
//	├─ koto, koto-tui                    Go, from cyclonedx-gomod (-std)
//	├─ firecracker                       Rust, crates from Cargo.lock @ FC_COMMIT
//	├─ vmlinux                           amazonlinux/linux @ KERNEL_COMMIT
//	└─ guest.img
//	   ├─ fc-agent                       Go, from cyclonedx-gomod (-std)
//	   ├─ node, npm, claude-code, codex  ARG pins in fcguest/Dockerfile.guest
//	   ├─ pi                             fcguest/pi/install/package-lock.json
//	   ├─ tmux, libevent, ncurses        static tmux built in the guest image
//	   └─ fedora base (pkg:oci, digest)  + build toolchain from rpms.lock
//	                                       (scope "excluded": not shipped)
//
// Standard library only, no go.mod of its own (the repo root has none, and a
// fifth module would join every per-module gate): sbom.sh runs it file-mode,
// `go run scripts/supplychain/sbomagg/main.go`. No third-party code.
//
// Deterministic: components and dependencies are sorted by bom-ref, the
// serialNumber is a UUID derived from (version, arch, commit) and the
// timestamp is the commit time (-epoch), so the same commit gives the same
// bytes (given the same cyclonedx-gomod output).
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type obj = map[string]any

var (
	root     = flag.String("root", ".", "koto repo root")
	parts    = flag.String("parts", "", "dir with koto.cdx.json, koto-tui.cdx.json, fc-agent.cdx.json")
	out      = flag.String("out", "", "output file")
	version  = flag.String("version", "", "koto version (no leading v)")
	arch     = flag.String("arch", "x86_64", "arch")
	commit   = flag.String("commit", "", "40-char git commit of the source tree")
	epoch    = flag.Int64("epoch", 0, "commit time, unix seconds")
	cargoArg = flag.String("cargo-lock", "", "Firecracker Cargo.lock at FC_COMMIT (fetched by commit if empty)")
)

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, "sbomagg: "+f+"\n", a...); os.Exit(1) }

// --- document -----------------------------------------------------------

type doc struct {
	comps map[string]obj            // bom-ref -> component (flat)
	deps  map[string]map[string]bool // bom-ref -> dependsOn set
}

func (d *doc) add(c obj) string {
	ref, _ := c["bom-ref"].(string)
	if ref == "" {
		die("component without bom-ref: %v", c["name"])
	}
	if _, ok := d.comps[ref]; !ok {
		d.comps[ref] = c
	}
	if d.deps[ref] == nil {
		d.deps[ref] = map[string]bool{}
	}
	return ref
}

func (d *doc) dep(from string, to ...string) {
	if d.deps[from] == nil {
		d.deps[from] = map[string]bool{}
	}
	for _, t := range to {
		if t != from {
			d.deps[from][t] = true
		}
	}
}

func prop(name, val string) obj { return obj{"name": name, "value": val} }

func hash(alg, hexv string) []any { return []any{obj{"alg": alg, "content": strings.ToLower(hexv)}} }

func comp(typ, ref, name, ver, purl string) obj {
	c := obj{"type": typ, "bom-ref": ref, "name": name, "version": ver}
	if purl != "" {
		c["purl"] = purl
	}
	return c
}

// --- Go: per-binary cyclonedx-gomod SBOMs --------------------------------

func flatten(c obj, d *doc) {
	nested, _ := c["components"].([]any)
	delete(c, "components")
	d.add(c)
	for _, n := range nested {
		if m, ok := n.(obj); ok {
			flatten(m, d)
		}
	}
}

// goPart merges one cyclonedx-gomod document and returns its main component's
// bom-ref (the binary).
func goPart(path, binName string, d *doc) string {
	b, err := os.ReadFile(path)
	if err != nil {
		die("%v", err)
	}
	var in obj
	if err := json.Unmarshal(b, &in); err != nil {
		die("%s: %v", path, err)
	}
	if in["bomFormat"] != "CycloneDX" {
		die("%s: not CycloneDX", path)
	}
	md, _ := in["metadata"].(obj)
	mc, _ := md["component"].(obj)
	if mc == nil {
		die("%s: no metadata.component", path)
	}
	ref, _ := mc["bom-ref"].(string)
	props, _ := mc["properties"].([]any)
	mc["properties"] = append(props, prop("koto:binary", binName))
	flatten(mc, d)
	for _, c := range asList(in["components"]) {
		flatten(c, d)
	}
	for _, x := range asList(in["dependencies"]) {
		from, _ := x["ref"].(string)
		var to []string
		on, _ := x["dependsOn"].([]any)
		for _, t := range on {
			if s, ok := t.(string); ok {
				to = append(to, s)
			}
		}
		d.dep(from, to...)
	}
	return ref
}

func asList(v any) []obj {
	l, _ := v.([]any)
	var r []obj
	for _, x := range l {
		if m, ok := x.(obj); ok {
			r = append(r, m)
		}
	}
	return r
}

// --- pins out of the build scripts / Dockerfile --------------------------

var (
	argRe  = regexp.MustCompile(`^ARG ([A-Z0-9_]+)=(\S+)\s*$`)
	shPin  = regexp.MustCompile(`^([A-Z_]+)="\$\{[A-Z_]+:-([^}]+)\}"`)
	hex40  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex128 = regexp.MustCompile(`^[0-9a-f]{128}$`)
)

func readPins(path string, re *regexp.Regexp) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	m := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		if g := re.FindStringSubmatch(strings.TrimSpace(s.Text())); g != nil {
			if _, dup := m[g[1]]; !dup {
				m[g[1]] = g[2]
			}
		}
	}
	return m
}

func need(m map[string]string, file string, keys ...string) {
	for _, k := range keys {
		if m[k] == "" {
			die("%s: pin %s not found", file, k)
		}
	}
}

func must(re *regexp.Regexp, v, what string) string {
	if !re.MatchString(v) {
		die("%s: %q is not %s", what, v, re)
	}
	return v
}

// npmPurl: pkg:npm/%40scope/name@ver (purl spec: the @ of a scope is encoded).
func npmPurl(name, ver string) string {
	if strings.HasPrefix(name, "@") {
		return "pkg:npm/%40" + name[1:] + "@" + ver
	}
	return "pkg:npm/" + name + "@" + ver
}

// --- Firecracker: Cargo.lock ---------------------------------------------

type crate struct {
	name, ver, source, checksum string
	deps                        []string
}

func parseCargoLock(b []byte) []crate {
	var cs []crate
	var cur *crate
	inDeps := false
	unq := func(s string) string { return strings.Trim(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ",")), `"`) }
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case t == "[[package]]":
			cs = append(cs, crate{})
			cur = &cs[len(cs)-1]
			inDeps = false
		case cur == nil:
		case inDeps:
			if t == "]" {
				inDeps = false
			} else if t != "" {
				cur.deps = append(cur.deps, unq(t))
			}
		case strings.HasPrefix(t, "dependencies = ["):
			rest := strings.TrimPrefix(t, "dependencies = [")
			if strings.HasSuffix(rest, "]") { // one-line form
				for _, x := range strings.Split(strings.TrimSuffix(rest, "]"), ",") {
					if x = unq(x); x != "" {
						cur.deps = append(cur.deps, x)
					}
				}
			} else {
				inDeps = true
			}
		case strings.HasPrefix(t, "name = "):
			cur.name = unq(t[7:])
		case strings.HasPrefix(t, "version = "):
			cur.ver = unq(t[10:])
		case strings.HasPrefix(t, "source = "):
			cur.source = unq(t[9:])
		case strings.HasPrefix(t, "checksum = "):
			cur.checksum = unq(t[11:])
		}
	}
	return cs
}

func crateRef(c crate) string {
	switch {
	case strings.HasPrefix(c.source, "registry+https://github.com/rust-lang/crates.io-index"):
		return "pkg:cargo/" + c.name + "@" + c.ver
	case strings.HasPrefix(c.source, "git+"):
		u, err := url.Parse(strings.TrimPrefix(c.source, "git+"))
		if err != nil || u.Fragment == "" {
			die("cargo: unsupported git source %q", c.source)
		}
		return "pkg:cargo/" + c.name + "@" + c.ver + "?vcs_url=" + url.QueryEscape("git+"+u.Scheme+"://"+u.Host+u.Path+"@"+u.Fragment)
	case c.source == "":
		return "" // Firecracker's own workspace crate
	}
	die("cargo: unsupported source %q", c.source)
	return ""
}

func firecracker(d *doc, lock []byte, fcVer, fcCommit string) string {
	fc := comp("application", "pkg:github/firecracker-microvm/firecracker@"+fcVer,
		"firecracker", fcVer, "pkg:github/firecracker-microvm/firecracker@"+fcVer)
	fc["properties"] = []any{prop("koto:asset", "fcassets/firecracker"), prop("koto:source-commit", fcCommit),
		prop("koto:pin", "fcguest/build-firecracker.sh FC_VERSION/FC_COMMIT")}
	fcRef := d.add(fc)
	cs := parseCargoLock(lock)
	if len(cs) < 50 {
		die("cargo: only %d packages in Cargo.lock", len(cs))
	}
	byName := map[string][]crate{}
	for _, c := range cs {
		byName[c.name] = append(byName[c.name], c)
	}
	resolve := func(spec string) (crate, bool) {
		f := strings.Fields(spec)
		for _, c := range byName[f[0]] {
			if len(f) == 1 || c.ver == f[1] {
				return c, true
			}
		}
		return crate{}, false
	}
	refOf := func(c crate) string {
		if r := crateRef(c); r != "" {
			return r
		}
		return fcRef // local crates fold into firecracker itself
	}
	locals := 0
	for _, c := range cs {
		r := crateRef(c)
		if r == "" {
			locals++
		} else {
			x := comp("library", r, c.name, c.ver, r)
			if c.checksum != "" {
				x["hashes"] = hash("SHA-256", must(hex64, c.checksum, "cargo checksum"))
			}
			x["properties"] = []any{prop("koto:shipped-in", "firecracker")}
			d.add(x)
		}
		from := refOf(c)
		for _, s := range c.deps {
			t, ok := resolve(s)
			if !ok {
				die("cargo: %s %s: unresolved dependency %q", c.name, c.ver, s)
			}
			d.dep(from, refOf(t))
		}
	}
	fmt.Fprintf(os.Stderr, "sbomagg: firecracker %s: %d crates (%d workspace-local folded)\n", fcVer, len(cs)-locals, locals)
	return fcRef
}

func fetch(u string) []byte {
	cl := &http.Client{Timeout: 60 * time.Second}
	var last error
	for i := 0; i < 3; i++ {
		resp, err := cl.Get(u)
		if err == nil {
			b, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && rerr == nil {
				return b
			}
			err = errors.Join(rerr, fmt.Errorf("HTTP %d", resp.StatusCode))
		}
		last = err
		time.Sleep(time.Duration(i+1) * 2 * time.Second)
	}
	die("fetch %s: %v", u, last)
	return nil
}

// --- guest.img -------------------------------------------------------------

func sriHex(integrity string) (alg, hx string) {
	p := strings.SplitN(integrity, "-", 2)
	if len(p) != 2 {
		die("npm: bad integrity %q", integrity)
	}
	raw, err := base64.StdEncoding.DecodeString(p[1])
	if err != nil {
		die("npm: bad integrity %q: %v", integrity, err)
	}
	switch p[0] {
	case "sha512":
		return "SHA-512", hex.EncodeToString(raw)
	case "sha384":
		return "SHA-384", hex.EncodeToString(raw)
	case "sha256":
		return "SHA-256", hex.EncodeToString(raw)
	case "sha1":
		return "SHA-1", hex.EncodeToString(raw)
	}
	die("npm: unsupported integrity alg %q", p[0])
	return "", ""
}

func piLock(d *doc, lockPath, piVer, lockSHA string) string {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		die("%v", err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != lockSHA {
		die("pi: %s sha256 %s != PI_LOCK_SHA256 %s (Dockerfile.guest)", lockPath, got, lockSHA)
	}
	var lk struct {
		Packages map[string]struct {
			Name      string            `json:"name"`
			Version   string            `json:"version"`
			Integrity string            `json:"integrity"`
			Dev       bool              `json:"dev"`
			Optional  bool              `json:"optional"`
			Link      bool              `json:"link"`
			Deps      map[string]string `json:"dependencies"`
			OptDeps   map[string]string `json:"optionalDependencies"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &lk); err != nil {
		die("pi: %v", err)
	}
	inst := comp("application", "koto:guest/pi-install@"+piVer, "pi-install", piVer, "")
	inst["description"] = "npm ci of fcguest/pi/install/package-lock.json into /opt/pi in guest.img"
	inst["hashes"] = hash("SHA-256", lockSHA)
	instRef := d.add(inst)
	keys := make([]string, 0, len(lk.Packages))
	for k := range lk.Packages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	refs := map[string]string{"": instRef} // lock key -> bom-ref
	nameOf := func(k string) string {
		i := strings.LastIndex(k, "node_modules/")
		return k[i+len("node_modules/"):]
	}
	n := 0
	for _, k := range keys {
		p := lk.Packages[k]
		if k == "" || p.Dev || p.Link {
			continue
		}
		name := nameOf(k)
		r := npmPurl(name, p.Version)
		refs[k] = r
		x := comp("library", r, name, p.Version, r)
		if p.Integrity == "" {
			die("pi: %s has no integrity", k)
		}
		alg, hx := sriHex(p.Integrity)
		x["hashes"] = hash(alg, hx)
		pr := []any{prop("koto:shipped-in", "guest.img:/opt/pi")}
		if p.Optional {
			pr = append(pr, prop("koto:npm-optional", "true"))
		}
		x["properties"] = pr
		d.add(x)
		n++
	}
	// npm resolution: a dependency of package at key K resolves to the
	// nearest K/node_modules/<dep>, walking up to the top-level node_modules.
	lookup := func(from, dep string) string {
		base := from
		for {
			cand := "node_modules/" + dep
			if base != "" {
				cand = base + "/node_modules/" + dep
			}
			if r, ok := refs[cand]; ok {
				return r
			}
			if base == "" {
				return ""
			}
			if i := strings.LastIndex(base, "/node_modules/"); i >= 0 {
				base = base[:i]
			} else {
				base = ""
			}
		}
	}
	for _, k := range keys {
		p := lk.Packages[k]
		from, ok := refs[k]
		if !ok {
			continue
		}
		dn := make([]string, 0, len(p.Deps)+len(p.OptDeps))
		for x := range p.Deps {
			dn = append(dn, x)
		}
		for x := range p.OptDeps {
			dn = append(dn, x)
		}
		for _, x := range dn {
			if r := lookup(k, x); r != "" {
				d.dep(from, r)
			} else if _, opt := p.OptDeps[x]; !opt {
				die("pi: %s: dependency %s not in lock", k, x)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "sbomagg: pi %s: %d npm packages\n", piVer, n)
	return instRef
}

type rpm struct{ sha, src, ver, rel, name, arch string }

func rpmsLock(path string) []rpm {
	b, err := os.ReadFile(path)
	if err != nil {
		die("%v", err)
	}
	var r []rpm
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) != 6 {
			die("rpms.lock: bad line %q", ln)
		}
		r = append(r, rpm{must(hex64, f[0], "rpm sha256"), f[1], f[2], f[3], f[4], f[5]})
	}
	return r
}

var fcRel = regexp.MustCompile(`\.fc([0-9]+)$`)

func guest(d *doc, rootDir, fcAgentRef string) string {
	df := filepath.Join(rootDir, "fcguest/Dockerfile.guest")
	a := readPins(df, argRe)
	need(a, df, "FEDORA", "NODE_VERSION", "NODE_SHA256", "NPM_VERSION", "NPM_SHA512",
		"CLAUDE_CODE_VERSION", "CLAUDE_CODE_SHA512", "CLAUDE_CODE_LINUX_X64_SHA512",
		"CODEX_VERSION", "CODEX_SHA256", "CODEX_CODE_MODE_HOST_SHA256", "PI_VERSION", "PI_LOCK_SHA256",
		"TMUX_VERSION", "TMUX_SHA256", "LIBEVENT_VERSION", "LIBEVENT_SHA256", "NCURSES_VERSION", "NCURSES_SHA256")
	g := comp("file", "koto:asset/guest.img@"+*version, "guest.img", *version, "")
	g["description"] = "guest root filesystem (fcguest/Dockerfile.guest)"
	g["properties"] = []any{prop("koto:asset", "fcassets/guest.img")}
	gRef := d.add(g)
	d.dep(gRef, fcAgentRef)
	in := func(c obj, where string) string {
		pr, _ := c["properties"].([]any)
		c["properties"] = append(pr, prop("koto:shipped-in", where))
		r := d.add(c)
		d.dep(gRef, r)
		return r
	}

	// base image, by digest
	img := a["FEDORA"]
	i := strings.Index(img, "@sha256:")
	if i < 0 {
		die("Dockerfile.guest: FEDORA %q is not pinned by digest", img)
	}
	repo, dg := img[:i], must(hex64, img[i+8:], "FEDORA digest")
	oci := comp("container", "pkg:oci/fedora@sha256%3A"+dg+"?repository_url="+url.QueryEscape(repo), "fedora", "sha256:"+dg, "")
	oci["purl"] = oci["bom-ref"]
	oci["hashes"] = hash("SHA-256", dg)
	oci["description"] = "build base of guest.img; only /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem is copied into the image"
	ociRef := in(oci, "guest.img (build base)")

	// node / npm
	nv := a["NODE_VERSION"]
	node := comp("application", "pkg:github/nodejs/node@v"+nv, "node", nv, "pkg:github/nodejs/node@v"+nv)
	node["hashes"] = hash("SHA-256", must(hex64, a["NODE_SHA256"], "NODE_SHA256"))
	node["externalReferences"] = []any{obj{"type": "distribution", "url": "https://nodejs.org/dist/v" + nv + "/node-v" + nv + "-linux-x64.tar.xz"}}
	in(node, "guest.img:/bin/node")
	npm := comp("application", npmPurl("npm", a["NPM_VERSION"]), "npm", a["NPM_VERSION"], npmPurl("npm", a["NPM_VERSION"]))
	npm["hashes"] = hash("SHA-512", must(hex128, a["NPM_SHA512"], "NPM_SHA512"))
	in(npm, "guest.img:/lib/node_modules/npm")

	// claude code
	cv := a["CLAUDE_CODE_VERSION"]
	cl := comp("application", npmPurl("@anthropic-ai/claude-code", cv), "@anthropic-ai/claude-code", cv, npmPurl("@anthropic-ai/claude-code", cv))
	cl["hashes"] = hash("SHA-512", must(hex128, a["CLAUDE_CODE_SHA512"], "CLAUDE_CODE_SHA512"))
	clRef := in(cl, "guest.img:/lib/node_modules/@anthropic-ai/claude-code")
	cn := comp("application", npmPurl("@anthropic-ai/claude-code-linux-x64", cv), "@anthropic-ai/claude-code-linux-x64", cv, npmPurl("@anthropic-ai/claude-code-linux-x64", cv))
	cn["hashes"] = hash("SHA-512", must(hex128, a["CLAUDE_CODE_LINUX_X64_SHA512"], "CLAUDE_CODE_LINUX_X64_SHA512"))
	d.dep(clRef, in(cn, "guest.img:/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe"))

	// codex: GitHub release assets
	xv := a["CODEX_VERSION"]
	for _, x := range []struct{ name, asset, sha string }{
		{"codex", "codex-x86_64-unknown-linux-musl", a["CODEX_SHA256"]},
		{"codex-code-mode-host", "codex-code-mode-host-x86_64-unknown-linux-musl", a["CODEX_CODE_MODE_HOST_SHA256"]},
	} {
		p := "pkg:github/openai/codex@rust-v" + xv + "#" + x.asset + ".tar.gz"
		c := comp("application", p, x.name, xv, p)
		c["hashes"] = hash("SHA-256", must(hex64, x.sha, x.name+" sha256"))
		c["externalReferences"] = []any{obj{"type": "distribution", "url": "https://github.com/openai/codex/releases/download/rust-v" + xv + "/" + x.asset + ".tar.gz"}}
		in(c, "guest.img:/bin/"+x.name)
	}

	// pi
	piRef := piLock(d, filepath.Join(rootDir, "fcguest/pi/install/package-lock.json"), a["PI_VERSION"], must(hex64, a["PI_LOCK_SHA256"], "PI_LOCK_SHA256"))
	d.dep(gRef, piRef)

	// static tmux and what it links
	tm := comp("application", "pkg:github/tmux/tmux@"+a["TMUX_VERSION"], "tmux", a["TMUX_VERSION"], "pkg:github/tmux/tmux@"+a["TMUX_VERSION"])
	tm["hashes"] = hash("SHA-256", must(hex64, a["TMUX_SHA256"], "TMUX_SHA256"))
	tmRef := in(tm, "guest.img:/bin/tmux")
	le := comp("library", "pkg:github/libevent/libevent@release-"+a["LIBEVENT_VERSION"], "libevent", a["LIBEVENT_VERSION"], "pkg:github/libevent/libevent@release-"+a["LIBEVENT_VERSION"])
	le["hashes"] = hash("SHA-256", must(hex64, a["LIBEVENT_SHA256"], "LIBEVENT_SHA256"))
	nc := comp("library", "pkg:generic/ncurses@"+a["NCURSES_VERSION"], "ncurses", a["NCURSES_VERSION"],
		"pkg:generic/ncurses@"+a["NCURSES_VERSION"]+"?download_url="+url.QueryEscape("https://invisible-island.net/archives/ncurses/ncurses-"+a["NCURSES_VERSION"]+".tar.gz"))
	nc["hashes"] = hash("SHA-256", must(hex64, a["NCURSES_SHA256"], "NCURSES_SHA256"))
	d.dep(tmRef, d.add(le), d.add(nc))
	for _, r := range []string{le["bom-ref"].(string), nc["bom-ref"].(string)} {
		d.comps[r]["properties"] = []any{prop("koto:shipped-in", "guest.img:/bin/tmux (static)")}
	}

	// build toolchain (not shipped) from rpms.lock
	rp := rpmsLock(filepath.Join(rootDir, "fcguest/guest/rpms.lock"))
	for _, r := range rp {
		distro := ""
		if m := fcRel.FindStringSubmatch(r.rel); m != nil {
			distro = "&distro=fedora-" + m[1]
		}
		p := "pkg:rpm/fedora/" + r.name + "@" + r.ver + "-" + r.rel + "?arch=" + r.arch + distro
		c := comp("library", p, r.name, r.ver+"-"+r.rel, p)
		c["hashes"] = hash("SHA-256", r.sha)
		c["scope"] = "excluded"
		c["properties"] = []any{prop("koto:role", "guest.img build toolchain (fcguest/guest/rpms.lock), not shipped"), prop("koto:rpm-source", r.src)}
		d.dep(ociRef, d.add(c))
	}
	fmt.Fprintf(os.Stderr, "sbomagg: guest.img: %d toolchain rpms, base %s\n", len(rp), dg[:12])
	return gRef
}

// --- main ------------------------------------------------------------------

func main() {
	flag.Parse()
	if *parts == "" || *out == "" || *version == "" {
		die("usage: sbomagg -parts DIR -out FILE -version VER -commit SHA -epoch SECS [-root DIR] [-arch A] [-cargo-lock F]")
	}
	must(hex40, *commit, "-commit")
	if *epoch <= 0 {
		die("-epoch must be the commit time in unix seconds")
	}
	*version = strings.TrimPrefix(*version, "v")
	if strings.Contains(*version, "_") {
		die("version %q contains '_'", *version)
	}
	d := &doc{comps: map[string]obj{}, deps: map[string]map[string]bool{}}

	rootRef := "pkg:github/jpzk/koto@v" + *version
	kc := comp("application", rootRef, "koto", *version, rootRef)
	kc["properties"] = []any{prop("koto:source-commit", *commit), prop("koto:arch", *arch)}
	kc["externalReferences"] = []any{obj{"type": "vcs", "url": "https://github.com/jpzk/koto"}}

	kotoRef := goPart(filepath.Join(*parts, "koto.cdx.json"), "koto", d)
	tuiRef := goPart(filepath.Join(*parts, "koto-tui.cdx.json"), "koto-tui", d)
	agentRef := goPart(filepath.Join(*parts, "fc-agent.cdx.json"), "fc-agent", d)

	fcf := filepath.Join(*root, "fcguest/build-firecracker.sh")
	fp := readPins(fcf, shPin)
	need(fp, fcf, "FC_VERSION", "FC_COMMIT")
	fcCommit := must(hex40, fp["FC_COMMIT"], "FC_COMMIT")
	var lock []byte
	if *cargoArg != "" {
		var err error
		if lock, err = os.ReadFile(*cargoArg); err != nil {
			die("%v", err)
		}
	} else {
		lock = fetch("https://raw.githubusercontent.com/firecracker-microvm/firecracker/" + fcCommit + "/Cargo.lock")
	}
	fcRef := firecracker(d, lock, fp["FC_VERSION"], fcCommit)

	kf := filepath.Join(*root, "fcguest/build-kernel.sh")
	kp := readPins(kf, shPin)
	need(kp, kf, "KERNEL_TAG", "KERNEL_COMMIT")
	kr := "pkg:github/amazonlinux/linux@" + must(hex40, kp["KERNEL_COMMIT"], "KERNEL_COMMIT")
	vm := comp("operating-system", kr, "vmlinux", kp["KERNEL_TAG"], kr)
	vm["properties"] = []any{prop("koto:asset", "fcassets/vmlinux"), prop("koto:kernel-tag", kp["KERNEL_TAG"])}
	vmRef := d.add(vm)

	gRef := guest(d, *root, agentRef)
	d.dep(rootRef, kotoRef, tuiRef, fcRef, vmRef, gRef)

	// assemble, sorted
	refs := make([]string, 0, len(d.comps))
	for r := range d.comps {
		if r != rootRef {
			refs = append(refs, r)
		}
	}
	sort.Strings(refs)
	comps := make([]any, 0, len(refs))
	for _, r := range refs {
		comps = append(comps, d.comps[r])
	}
	var deps []any
	dk := make([]string, 0, len(d.deps))
	for r := range d.deps {
		dk = append(dk, r)
	}
	sort.Strings(dk)
	known := func(r string) bool { _, ok := d.comps[r]; return ok || r == rootRef }
	for _, r := range dk {
		if !known(r) {
			die("dependency graph references unknown bom-ref %q", r)
		}
		on := make([]string, 0, len(d.deps[r]))
		for t := range d.deps[r] {
			if !known(t) {
				die("%s dependsOn unknown bom-ref %q", r, t)
			}
			on = append(on, t)
		}
		sort.Strings(on)
		deps = append(deps, obj{"ref": r, "dependsOn": on})
	}
	if _, ok := d.deps[rootRef]; !ok {
		die("root has no dependencies")
	}

	sum := sha256.Sum256([]byte("koto-sbom\x00" + *version + "\x00" + *arch + "\x00" + *commit))
	sum[6] = sum[6]&0x0f | 0x50 // RFC 9562 v5-style layout over SHA-256
	sum[8] = sum[8]&0x3f | 0x80
	u := hex.EncodeToString(sum[:16])
	serial := "urn:uuid:" + u[0:8] + "-" + u[8:12] + "-" + u[12:16] + "-" + u[16:20] + "-" + u[20:32]

	bom := obj{
		"$schema":      "http://cyclonedx.org/schema/bom-1.6.schema.json",
		"bomFormat":    "CycloneDX",
		"specVersion":  "1.6",
		"serialNumber": serial,
		"version":      1,
		"metadata": obj{
			"timestamp": time.Unix(*epoch, 0).UTC().Format(time.RFC3339),
			"lifecycles": []any{obj{"phase": "build"}},
			"tools": obj{"components": []any{
				obj{"type": "application", "name": "cyclonedx-gomod", "version": "v1.12.0", "purl": "pkg:golang/github.com/CycloneDX/cyclonedx-gomod@v1.12.0"},
				obj{"type": "application", "name": "sbomagg", "version": *commit, "description": "scripts/supplychain/sbomagg (koto)"},
			}},
			"component": kc,
			"properties": []any{prop("koto:source-commit", *commit), prop("koto:source-epoch", strconv.FormatInt(*epoch, 10))},
		},
		"components":   comps,
		"dependencies": deps,
	}
	b, err := json.MarshalIndent(bom, "", "  ")
	if err != nil {
		die("%v", err)
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		die("%v", err)
	}
	fmt.Fprintf(os.Stderr, "sbomagg: %s: %d components, %d dependency entries\n", *out, len(comps), len(deps))
}
