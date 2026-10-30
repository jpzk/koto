// Package ociimage pulls OCI / Docker v2 images from a registry and turns
// their layers into read-only ext4 drives for Firecracker guests.
//
// registry.go is the registry client. It is stdlib only (no new go.mod
// require lines) and trusts nothing it downloads: every manifest, config and
// layer blob is re-hashed against the digest that names it before it is
// used or cached. Tags are resolved to a digest exactly once (koto image
// add / update); everything after that works by digest. Image age is logged,
// never enforced: koto has no age gate, operators pin what they trust.
package ociimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	MTIndex     = "application/vnd.oci.image.index.v1+json"
	MTManifest  = "application/vnd.oci.image.manifest.v1+json"
	MTDList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	MTDManifest = "application/vnd.docker.distribution.manifest.v2+json"

	maxManifest = 4 << 20
	maxConfig   = 4 << 20
	// MaxBlob bounds one compressed layer blob.
	MaxBlob = 2 << 30
	// MaxLayers bounds the manifest before the (smaller) slot guard applies.
	MaxLayers = 64

	acceptAll = MTIndex + "," + MTManifest + "," + MTDList + "," + MTDManifest
)

var (
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	repoRe   = regexp.MustCompile(`^[a-z0-9]+([._/-][a-z0-9]+)*$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	authKV   = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// ValidDigest reports whether s is a well-formed sha256 digest.
func ValidDigest(s string) bool { return digestRe.MatchString(s) }

// Ref is a parsed image reference. Exactly one of Tag / Digest is set.
type Ref struct {
	Host, Repo, Tag, Digest string
}

// Name is host/repo in the user-facing form (docker.io library/ prefix kept).
func (r Ref) Name() string {
	h := r.Host
	if h == "registry-1.docker.io" {
		h = "docker.io"
	}
	return h + "/" + r.Repo
}

// Pinned returns name@digest for a resolved digest.
func (r Ref) Pinned(digest string) string { return r.Name() + "@" + digest }

// ParseRef parses [host/]repo[:tag] or [host/]repo@sha256:<hex>.
// Bare names default to the "latest" tag; tag+digest is refused as ambiguous.
func ParseRef(s string) (Ref, error) {
	if s == "" || len(s) > 512 || strings.ContainsAny(s, " \t\n") {
		return Ref{}, fmt.Errorf("bad image reference %q", s)
	}
	name, tag, dg := s, "", ""
	if at := strings.LastIndex(s, "@"); at >= 0 {
		name, dg = s[:at], s[at+1:]
		if !digestRe.MatchString(dg) {
			return Ref{}, fmt.Errorf("refusing %q: digest must be sha256:<64 hex>", s)
		}
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
		if dg != "" {
			return Ref{}, fmt.Errorf("refusing %q: tag+digest is ambiguous, give one", s)
		}
		if !tagRe.MatchString(tag) {
			return Ref{}, fmt.Errorf("bad tag %q", tag)
		}
	}
	if dg == "" && tag == "" {
		tag = "latest"
	}
	host, repo := "docker.io", name
	if i := strings.Index(name, "/"); i > 0 && (strings.ContainsAny(name[:i], ".:") || name[:i] == "localhost") {
		host, repo = name[:i], name[i+1:]
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	if !repoRe.MatchString(repo) {
		return Ref{}, fmt.Errorf("bad repository %q", repo)
	}
	return Ref{Host: host, Repo: repo, Tag: tag, Digest: dg}, nil
}

// Client talks to registries over HTTPS. Zero value is usable.
type Client struct {
	HTTP  *http.Client         // default: 10 min timeout
	Logf  func(string, ...any) // progress log; default: discard
	Sleep func(time.Duration)  // retry backoff; default time.Sleep
	Now   func() time.Time     // age logging; default time.Now
	Tries int                  // attempts for transient errors; default 3
	tok   map[string]string    // host/repo -> bearer token
}

func (c *Client) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

func (c *Client) hc() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	return c.HTTP
}

func (c *Client) tries() int {
	if c.Tries <= 0 {
		return 3
	}
	return c.Tries
}

func (c *Client) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) auth(r Ref, resp *http.Response) error {
	h := resp.Header.Get("Www-Authenticate")
	if !strings.HasPrefix(h, "Bearer ") {
		return fmt.Errorf("registry %s: unsupported auth challenge %q", r.Host, h)
	}
	p := map[string]string{}
	for _, m := range authKV.FindAllStringSubmatch(h, -1) {
		p[m[1]] = m[2]
	}
	u, err := url.Parse(p["realm"])
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("registry %s: bad token realm %q", r.Host, p["realm"])
	}
	q := u.Query()
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	q.Set("scope", "repository:"+r.Repo+":pull")
	u.RawQuery = q.Encode()
	tr, err := c.hc().Get(u.String())
	if err != nil {
		return err
	}
	defer tr.Body.Close()
	if tr.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: %s", tr.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tr.Body, 1<<20)).Decode(&t); err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if c.tok == nil {
		c.tok = map[string]string{}
	}
	c.tok[r.Host+"/"+r.Repo] = t.Token
	return nil
}

// statusErr marks HTTP failures so 5xx can be retried and 4xx not.
type statusErr struct {
	path string
	code int
	text string
}

func (e *statusErr) Error() string { return fmt.Sprintf("GET %s: %s", e.path, e.text) }

func (c *Client) get(r Ref, path, accept string) (*http.Response, error) {
	for try := 0; try < 2; try++ {
		req, err := http.NewRequest(http.MethodGet, "https://"+r.Host+"/v2/"+r.Repo+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if t := c.tok[r.Host+"/"+r.Repo]; t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := c.hc().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			err = c.auth(r, resp)
			resp.Body.Close()
			if err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, &statusErr{path, resp.StatusCode, resp.Status}
		}
		return resp, nil
	}
	return nil, errors.New("registry auth loop")
}

// transient: network-level failures worth a bounded retry. Digest, size,
// auth and 4xx errors are never retried.
func transient(err error) bool {
	var se *statusErr
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") || strings.Contains(s, "timeout") ||
		strings.Contains(s, "connection refused") || strings.Contains(s, "EOF")
}

func (c *Client) retry(what string, f func() error) error {
	var err error
	for try := 1; ; try++ {
		if err = f(); err == nil || !transient(err) || try >= c.tries() {
			return err
		}
		c.logf("retry %d/%d %s: %v", try, c.tries(), what, err)
		c.sleep(time.Duration(try) * 2 * time.Second)
	}
}

// Sha returns "sha256:<hex>" of b.
func Sha(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

// Short is the first 12 hex chars of a digest (sys-<digest12>.img naming).
func Short(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// BlobPath is the content-addressed cache path for digest dg under root.
func BlobPath(root, dg string) string {
	return filepath.Join(root, "blobs", strings.Replace(dg, ":", "/", 1))
}

// VerifyFile re-hashes a cached file; the cache is never trusted blindly.
func VerifyFile(p, dg string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return "sha256:"+hex.EncodeToString(h.Sum(nil)) == dg
}

// Resolve turns a tag into a manifest digest by fetching the manifest and
// hashing its bytes (the Docker-Content-Digest header is checked, not
// trusted). A digest ref is returned as-is without network.
func (c *Client) Resolve(r Ref) (string, error) {
	if r.Digest != "" {
		return r.Digest, nil
	}
	var dg string
	err := c.retry(r.Name()+":"+r.Tag, func() error {
		resp, err := c.get(r, "/manifests/"+r.Tag, acceptAll)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
		if err != nil {
			return err
		}
		if len(b) > maxManifest {
			return fmt.Errorf("manifest %s:%s exceeds %d bytes", r.Name(), r.Tag, maxManifest)
		}
		dg = Sha(b)
		if h := resp.Header.Get("Docker-Content-Digest"); h != "" && h != dg {
			return fmt.Errorf("resolve %s:%s: registry says %s but manifest hashes to %s", r.Name(), r.Tag, h, dg)
		}
		return nil
	})
	return dg, err
}

// fetchDoc returns a small document named by dg: from the cache when the
// cached copy re-hashes to dg (offline rollback), else from the registry,
// verified, then cached 0444.
func (c *Client) fetchDoc(cache string, r Ref, path, accept, dg string, max int64) ([]byte, string, error) {
	if cache != "" {
		p := BlobPath(cache, dg)
		if st, err := os.Stat(p); err == nil && st.Size() <= max {
			if b, err := os.ReadFile(p); err == nil && Sha(b) == dg {
				return b, "", nil
			}
		}
	}
	var b []byte
	var ct string
	err := c.retry(Short(dg), func() error {
		resp, err := c.get(r, path, accept)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, err = io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > max {
			return fmt.Errorf("%s exceeds %d bytes", path, max)
		}
		if got := Sha(b); got != dg {
			return fmt.Errorf("digest mismatch for %s: got %s", path, got)
		}
		ct = resp.Header.Get("Content-Type")
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if cache != "" {
		p := BlobPath(cache, dg)
		if os.MkdirAll(filepath.Dir(p), 0o755) == nil && os.WriteFile(p+".part", b, 0o444) == nil {
			_ = os.Rename(p+".part", p)
		}
	}
	return b, ct, nil
}

// blob ensures layer blob dg is in the cache (fetching only if missing or
// corrupt) and returns its path and whether it was already cached.
func (c *Client) blob(cache string, r Ref, dg string, size int64) (string, bool, error) {
	p := BlobPath(cache, dg)
	if VerifyFile(p, dg) {
		return p, true, nil
	}
	if size <= 0 || size > MaxBlob {
		return "", false, fmt.Errorf("blob %s size %d out of bounds", dg, size)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", false, err
	}
	err := c.retry(Short(dg), func() error {
		resp, err := c.get(r, "/blobs/"+dg, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		tmp := p + ".part"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n != size {
			err = fmt.Errorf("blob %s: got %d bytes, manifest says %d", dg, n, size)
			if n < size {
				err = fmt.Errorf("%w: %v", io.ErrUnexpectedEOF, err)
			}
		}
		if err == nil && "sha256:"+hex.EncodeToString(h.Sum(nil)) != dg {
			err = fmt.Errorf("blob %s: sha256 mismatch, rejected", dg)
		}
		if err == nil {
			err = os.Chmod(tmp, 0o444)
		}
		if err == nil {
			err = os.Rename(tmp, p)
		}
		if err != nil {
			os.Remove(tmp)
		}
		return err
	})
	if err != nil {
		return "", false, err
	}
	return p, false, nil
}

type desc struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

type manifest struct {
	MediaType string `json:"mediaType"`
	Config    desc   `json:"config"`
	Layers    []desc `json:"layers"`
	Manifests []desc `json:"manifests"`
}

type imgConfig struct {
	Created      time.Time `json:"created"`
	Architecture string    `json:"architecture"`
	OS           string    `json:"os"`
	Config       struct {
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// Layer is one image layer, bottom -> top order in Image.Layers.
type Layer struct {
	Digest    string `json:"digest"`
	DiffID    string `json:"diff_id"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

// Image is the pulled, verified description of one image.
type Image struct {
	Ref            string    `json:"ref"`             // name@digest (pinned)
	Digest         string    `json:"digest"`          // what the user pinned (index or manifest)
	ManifestDigest string    `json:"manifest_digest"` // linux/amd64 manifest
	ConfigDigest   string    `json:"config_digest"`
	Created        time.Time `json:"created"`
	Arch           string    `json:"arch"`
	Env            []string  `json:"env,omitempty"`
	Layers         []Layer   `json:"layers"`
	// Fetched / Cached count layer blobs downloaded vs already present.
	Fetched int `json:"-"`
	Cached  int `json:"-"`
}

// CompressedSize sums the layer blob sizes.
func (i *Image) CompressedSize() int64 {
	var n int64
	for _, l := range i.Layers {
		n += l.Size
	}
	return n
}

var layerTypes = map[string]bool{
	"application/vnd.oci.image.layer.v1.tar+gzip":       true,
	"application/vnd.docker.image.rootfs.diff.tar.gzip": true,
	"application/vnd.oci.image.layer.v1.tar":            true,
	"application/vnd.docker.image.rootfs.diff.tar":      true,
}

// Pull fetches image r (resolving a tag first if needed) into cache:
// manifests + config + every layer blob, digest-verified, only what is
// missing. The image's age is logged, never enforced.
func (c *Client) Pull(r Ref, cache string) (*Image, error) {
	dg, err := c.Resolve(r)
	if err != nil {
		return nil, err
	}
	b, ct, err := c.fetchDoc(cache, r, "/manifests/"+dg, acceptAll, dg, maxManifest)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.MediaType == "" {
		m.MediaType = ct
	}
	mdg := dg
	if m.MediaType == MTIndex || m.MediaType == MTDList || len(m.Manifests) > 0 {
		var pick *desc
		for i, d := range m.Manifests {
			if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" {
				pick = &m.Manifests[i]
				break
			}
		}
		if pick == nil || !digestRe.MatchString(pick.Digest) {
			return nil, errors.New("image index has no linux/amd64 manifest")
		}
		c.logf("index %s -> linux/amd64 manifest %s", Short(dg), Short(pick.Digest))
		mdg = pick.Digest
		if b, _, err = c.fetchDoc(cache, r, "/manifests/"+mdg, acceptAll, mdg, maxManifest); err != nil {
			return nil, err
		}
		m = manifest{}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
	}
	if !digestRe.MatchString(m.Config.Digest) {
		return nil, fmt.Errorf("manifest: bad config digest %q", m.Config.Digest)
	}
	if len(m.Layers) == 0 || len(m.Layers) > MaxLayers {
		return nil, fmt.Errorf("manifest: %d layers (want 1..%d)", len(m.Layers), MaxLayers)
	}
	cb, _, err := c.fetchDoc(cache, r, "/blobs/"+m.Config.Digest, "", m.Config.Digest, maxConfig)
	if err != nil {
		return nil, err
	}
	var cfg imgConfig
	if err := json.Unmarshal(cb, &cfg); err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}
	if cfg.OS != "linux" || cfg.Architecture != "amd64" {
		return nil, fmt.Errorf("image config is %s/%s, want linux/amd64", cfg.OS, cfg.Architecture)
	}
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		return nil, fmt.Errorf("image config: %d diff_ids vs %d layers", len(cfg.RootFS.DiffIDs), len(m.Layers))
	}
	img := &Image{Ref: r.Pinned(dg), Digest: dg, ManifestDigest: mdg, ConfigDigest: m.Config.Digest, Created: cfg.Created,
		Arch: cfg.Architecture, Env: cfg.Config.Env}
	c.logf("image %s created %s (%s old, logged only), %d layers", Short(dg), fmtCreated(cfg.Created),
		Age(cfg.Created, c.now()), len(m.Layers))
	for i, l := range m.Layers {
		if !digestRe.MatchString(l.Digest) || !digestRe.MatchString(cfg.RootFS.DiffIDs[i]) {
			return nil, fmt.Errorf("layer %d: bad digest", i)
		}
		if !layerTypes[l.MediaType] {
			return nil, fmt.Errorf("layer %d: unsupported media type %q (gzip or plain tar only)", i, l.MediaType)
		}
		t := time.Now()
		_, hit, err := c.blob(cache, r, l.Digest, l.Size)
		if err != nil {
			return nil, err
		}
		if hit {
			img.Cached++
			c.logf("blob %d/%d %s cached (re-verified)", i+1, len(m.Layers), Short(l.Digest))
		} else {
			img.Fetched++
			c.logf("blob %d/%d %s fetched %d bytes in %s, sha256 ok", i+1, len(m.Layers), Short(l.Digest),
				l.Size, time.Since(t).Round(time.Millisecond))
		}
		img.Layers = append(img.Layers, Layer{l.Digest, cfg.RootFS.DiffIDs[i], l.MediaType, l.Size})
	}
	return img, nil
}

func fmtCreated(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// Age renders now-created in days (or "unknown"). Informational only.
func Age(created, now time.Time) string {
	if created.IsZero() {
		return "unknown age"
	}
	d := now.Sub(created)
	if d < 0 {
		return "0d (created in the future)"
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
