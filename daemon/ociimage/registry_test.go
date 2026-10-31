package ociimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRegistry is an in-memory OCI distribution endpoint (no network).
type fakeRegistry struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	docs      map[string][]byte // digest -> bytes (manifests, configs, blobs)
	types     map[string]string // digest -> content type (manifests)
	tags      map[string]string // tag -> digest
	corrupt   map[string]bool   // serve flipped bytes for this digest
	fail5xx   map[string]int    // path -> remaining 503s
	lieHeader bool              // wrong Docker-Content-Digest on tag fetch
	token     string            // non-empty: require Bearer auth
	hits      map[string]int    // path -> GET count
	realm     string            // non-empty: Www-Authenticate realm override
	redirect  map[string]string // path -> 302 Location (after auth)
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{t: t, docs: map[string][]byte{}, types: map[string]string{}, tags: map[string]string{},
		corrupt: map[string]bool{}, fail5xx: map[string]int{}, hits: map[string]int{}, redirect: map[string]string{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func (f *fakeRegistry) client(logs *[]string) *Client {
	c := &Client{Sleep: func(time.Duration) {},
		Logf: func(s string, a ...any) {
			if logs != nil {
				*logs = append(*logs, fmt.Sprintf(s, a...))
			}
		}}
	c.testTrust(f.pool(), f.allow)
	return c
}

// pool trusts the fake registry's certificate (httptest certs are shared,
// so it also covers sibling httptest TLS servers).
func (f *fakeRegistry) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(f.srv.Certificate())
	return p
}

// allow lets exactly the fake registry's loopback listener past the policy.
func (f *fakeRegistry) allow(ap netip.AddrPort) bool {
	return ap.String() == f.host()
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		if r.URL.Query().Get("scope") != "repository:test/img:pull" {
			http.Error(w, "bad scope", 400)
			return
		}
		fmt.Fprintf(w, `{"token":%q}`, f.token)
		return
	}
	f.hits[r.URL.Path]++
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		realm := f.srv.URL + "/token"
		if f.realm != "" {
			realm = f.realm
		}
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s",service="fake"`, realm))
		http.Error(w, "auth", 401)
		return
	}
	if loc, ok := f.redirect[r.URL.Path]; ok {
		http.Redirect(w, r, loc, http.StatusFound)
		return
	}
	if n := f.fail5xx[r.URL.Path]; n > 0 {
		f.fail5xx[r.URL.Path] = n - 1
		http.Error(w, "busy", 503)
		return
	}
	const pre = "/v2/test/img/"
	if !strings.HasPrefix(r.URL.Path, pre) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, pre)
	var key string
	switch {
	case strings.HasPrefix(rest, "manifests/"):
		key = strings.TrimPrefix(rest, "manifests/")
		if dg, ok := f.tags[key]; ok {
			key = dg
			hd := dg
			if f.lieHeader {
				hd = Sha([]byte("lie"))
			}
			w.Header().Set("Docker-Content-Digest", hd)
		}
		w.Header().Set("Content-Type", f.types[key])
	case strings.HasPrefix(rest, "blobs/"):
		key = strings.TrimPrefix(rest, "blobs/")
	}
	b, ok := f.docs[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.corrupt[key] {
		b = append([]byte(nil), b...)
		b[len(b)/2] ^= 0xff
	}
	w.Write(b)
}

func (f *fakeRegistry) put(b []byte, ct string) string {
	dg := Sha(b)
	f.docs[dg] = b
	if ct != "" {
		f.types[dg] = ct
	}
	return dg
}

type tfile struct {
	name, body string
	typ        byte
	mode       int64
}

func mkTar(t *testing.T, files []tfile) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: f.mode, Typeflag: f.typ, Size: int64(len(f.body))}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(f.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// addImage publishes an index -> amd64 manifest -> config + layers under tag.
func (f *fakeRegistry) addImage(tag string, created time.Time, layers ...[]byte) (index string, ls []string) {
	var descs []map[string]any
	var diffIDs []string
	for _, raw := range layers {
		blob := gz(raw)
		dg := f.put(blob, "")
		ls = append(ls, dg)
		diffIDs = append(diffIDs, Sha(raw))
		descs = append(descs, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest": dg, "size": len(blob)})
	}
	cfg, _ := json.Marshal(map[string]any{"created": created, "architecture": "amd64", "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs}})
	cdg := f.put(cfg, "")
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": MTManifest,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cdg, "size": len(cfg)},
		"layers": descs})
	mdg := f.put(man, MTManifest)
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": MTIndex, "manifests": []any{
		map[string]any{"mediaType": MTManifest, "digest": Sha([]byte("arm")), "size": 1,
			"platform": map[string]any{"architecture": "arm64", "os": "linux"}},
		map[string]any{"mediaType": MTManifest, "digest": mdg, "size": len(man),
			"platform": map[string]any{"architecture": "amd64", "os": "linux"}},
	}})
	index = f.put(idx, MTIndex)
	f.tags[tag] = index
	return index, ls
}

func TestParseRef(t *testing.T) {
	dg := "sha256:" + strings.Repeat("a", 64)
	for _, c := range []struct{ in, host, repo, tag, dg string }{
		{"debian", "registry-1.docker.io", "library/debian", "latest", ""},
		{"debian:trixie", "registry-1.docker.io", "library/debian", "trixie", ""},
		{"docker.io/library/debian@" + dg, "registry-1.docker.io", "library/debian", "", dg},
		{"ghcr.io/o/r:v1.2", "ghcr.io", "o/r", "v1.2", ""},
		{"127.0.0.1:5000/test/img:x", "127.0.0.1:5000", "test/img", "x", ""},
		{"localhost/a@" + dg, "localhost", "a", "", dg},
		// F34 (codeant r5): "__" and "-+" are legal OCI component separators
		{"ghcr.io/o/foo__bar:v1", "ghcr.io", "o/foo__bar", "v1", ""},
		{"ghcr.io/o/foo--bar", "ghcr.io", "o/foo--bar", "latest", ""},
		{"ghcr.io/a-b_c.d/e---f", "ghcr.io", "a-b_c.d/e---f", "latest", ""},
	} {
		r, err := ParseRef(c.in)
		if err != nil || r.Host != c.host || r.Repo != c.repo || r.Tag != c.tag || r.Digest != c.dg {
			t.Errorf("ParseRef(%q) = %+v, %v", c.in, r, err)
		}
	}
	for _, bad := range []string{"", "debian:trixie@" + dg, "debian@sha256:abc", "Debian", "a b", "x/../y", "debian:-bad",
		"ghcr.io/o/a___b", "ghcr.io/o/a.._b", "ghcr.io/o/_a", "ghcr.io/o/a_", "ghcr.io/o//a", "ghcr.io/o/a-", "ghcr.io/o/a._b"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}

func TestResolveTagToDigest(t *testing.T) {
	f := newFakeRegistry(t)
	idx, _ := f.addImage("trixie", time.Now().Add(-90*24*time.Hour), mkTar(t, []tfile{{name: "etc/os-release", body: "ID=debian\n"}}))
	r, err := ParseRef(f.host() + "/test/img:trixie")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.client(nil).Resolve(r)
	if err != nil || got != idx {
		t.Fatalf("Resolve = %q, %v; want %q", got, err, idx)
	}
	// digest refs never touch the network
	before := len(f.hits)
	r2, _ := ParseRef(f.host() + "/test/img@" + idx)
	if got, err := f.client(nil).Resolve(r2); err != nil || got != idx || len(f.hits) != before {
		t.Fatalf("digest Resolve = %q, %v (hits %d->%d)", got, err, before, len(f.hits))
	}
	// a registry header that disagrees with the bytes is refused
	f.lieHeader = true
	if _, err := f.client(nil).Resolve(r); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("lying Docker-Content-Digest accepted: %v", err)
	}
	// unknown tag: 404, not retried
	r3, _ := ParseRef(f.host() + "/test/img:nope")
	if _, err := f.client(nil).Resolve(r3); err == nil || f.hits["/v2/test/img/manifests/nope"] != 1 {
		t.Fatalf("unknown tag: err=%v hits=%d", err, f.hits["/v2/test/img/manifests/nope"])
	}
}

func TestPullVerifiesAndFetchesOnlyMissing(t *testing.T) {
	f := newFakeRegistry(t)
	l1 := mkTar(t, []tfile{{name: "bin/", typ: tar.TypeDir, mode: 0o755}, {name: "bin/sh", body: "#!sh", mode: 0o755}})
	l2 := mkTar(t, []tfile{{name: "etc/os-release", body: "PRETTY_NAME=\"Debian\"\n"}})
	idx, ls := f.addImage("a", time.Now().Add(-60*24*time.Hour), l1, l2)
	cache := t.TempDir()
	r, _ := ParseRef(f.host() + "/test/img:a")
	img, err := f.client(nil).Pull(r, cache)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != idx || len(img.Layers) != 2 || img.Fetched != 2 || img.Cached != 0 || img.Arch != "amd64" {
		t.Fatalf("img = %+v", img)
	}
	if img.Layers[0].DiffID != Sha(l1) || img.Layers[1].DiffID != Sha(l2) {
		t.Fatal("diff_ids out of order")
	}
	if !strings.HasSuffix(img.Ref, "/test/img@"+idx) {
		t.Fatalf("Ref not pinned: %s", img.Ref)
	}
	for _, dg := range ls {
		st, err := os.Stat(BlobPath(cache, dg))
		if err != nil || st.Mode().Perm() != 0o444 {
			t.Fatalf("blob %s: %v %v", dg, err, st)
		}
	}
	// second image shares l1: only l3 is fetched; layer blobs for l1 untouched
	l3 := mkTar(t, []tfile{{name: "etc/new", body: "x"}})
	_, ls2 := f.addImage("b", time.Now().Add(-50*24*time.Hour), l1, l3)
	r2, _ := ParseRef(f.host() + "/test/img:b")
	img2, err := f.client(nil).Pull(r2, cache)
	if err != nil {
		t.Fatal(err)
	}
	if img2.Fetched != 1 || img2.Cached != 1 || f.hits["/v2/test/img/blobs/"+ls2[0]] != 1 {
		t.Fatalf("shared layer refetched: fetched=%d cached=%d hits=%d", img2.Fetched, img2.Cached, f.hits["/v2/test/img/blobs/"+ls2[0]])
	}
	// offline re-pull by digest works from the cache (rollback path)
	f.srv.Close()
	r3, _ := ParseRef(f.host() + "/test/img@" + idx)
	if img3, err := f.client(nil).Pull(r3, cache); err != nil || img3.Cached != 2 {
		t.Fatalf("offline pull: %v %+v", err, img3)
	}
}

func TestBlobShaMismatchRejected(t *testing.T) {
	f := newFakeRegistry(t)
	_, ls := f.addImage("a", time.Now().Add(-60*24*time.Hour), mkTar(t, []tfile{{name: "f", body: strings.Repeat("z", 4096)}}))
	f.corrupt[ls[0]] = true
	cache := t.TempDir()
	r, _ := ParseRef(f.host() + "/test/img:a")
	_, err := f.client(nil).Pull(r, cache)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("corrupt blob accepted: %v", err)
	}
	if f.hits["/v2/test/img/blobs/"+ls[0]] != 1 {
		t.Fatalf("digest mismatch was retried (%d GETs)", f.hits["/v2/test/img/blobs/"+ls[0]])
	}
	left, _ := filepath.Glob(BlobPath(cache, ls[0]) + "*")
	if len(left) > 0 {
		t.Fatalf("rejected blob left on disk: %v", left)
	}
	// a corrupted manifest document is rejected too
	f2 := newFakeRegistry(t)
	idx, _ := f2.addImage("a", time.Now(), mkTar(t, []tfile{{name: "f", body: "x"}}))
	f2.corrupt[idx] = true
	r2, _ := ParseRef(f2.host() + "/test/img@" + idx)
	if _, err := f2.client(nil).Pull(r2, t.TempDir()); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("corrupt manifest accepted: %v", err)
	}
}

func TestYoungImageAcceptedAgeLogged(t *testing.T) {
	f := newFakeRegistry(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f.addImage("fresh", now.Add(-time.Hour), mkTar(t, []tfile{{name: "f", body: "x"}}))
	var logs []string
	c := f.client(&logs)
	c.Now = func() time.Time { return now }
	r, _ := ParseRef(f.host() + "/test/img:fresh")
	img, err := c.Pull(r, t.TempDir())
	if err != nil {
		t.Fatalf("1h-old image refused (koto has no age gate): %v", err)
	}
	if !img.Created.Equal(now.Add(-time.Hour)) {
		t.Fatalf("created = %v", img.Created)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "1.0h old, logged only") || !strings.Contains(joined, "2026-10-30T11:00:00Z") {
		t.Fatalf("age not logged:\n%s", joined)
	}
}

func TestTransientRetryAndAuth(t *testing.T) {
	f := newFakeRegistry(t)
	f.token = "s3cret"
	_, ls := f.addImage("a", time.Now(), mkTar(t, []tfile{{name: "f", body: "x"}}))
	f.fail5xx["/v2/test/img/blobs/"+ls[0]] = 2
	var logs []string
	r, _ := ParseRef(f.host() + "/test/img:a")
	img, err := f.client(&logs).Pull(r, t.TempDir())
	if err != nil || img.Fetched != 1 {
		t.Fatalf("pull with 2x503 + bearer auth: %v", err)
	}
	if n := strings.Count(strings.Join(logs, "\n"), "retry "); n != 2 {
		t.Fatalf("want 2 retries logged, got %d:\n%s", n, strings.Join(logs, "\n"))
	}
	// exhausted retries surface the error
	f2 := newFakeRegistry(t)
	_, ls2 := f2.addImage("a", time.Now(), mkTar(t, []tfile{{name: "f", body: "y"}}))
	f2.fail5xx["/v2/test/img/blobs/"+ls2[0]] = 5
	r2, _ := ParseRef(f2.host() + "/test/img:a")
	if _, err := f2.client(nil).Pull(r2, t.TempDir()); err == nil || f2.hits["/v2/test/img/blobs/"+ls2[0]] != 3 {
		t.Fatalf("want failure after 3 tries: %v hits=%d", err, f2.hits["/v2/test/img/blobs/"+ls2[0]])
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	if s := Age(now.Add(-50*24*time.Hour), now); s != "50d" {
		t.Fatal(s)
	}
	if s := Age(time.Time{}, now); s != "unknown age" {
		t.Fatal(s)
	}
}

// TestConcurrentSameBlobPull: several pulls of the same image into one cache
// (two groups, image add racing image update) all hold their blob temp file
// open at the same moment; none may truncate, rename or delete another's
// partial file, all must succeed, and the cache ends with one verified blob
// per digest and no temp leftovers.
func TestConcurrentSameBlobPull(t *testing.T) {
	f := newFakeRegistry(t)
	_, ls := f.addImage("a", time.Now().Add(-60*24*time.Hour),
		mkTar(t, []tfile{{name: "f", body: strings.Repeat("q", 64<<10)}}))
	cache := t.TempDir()
	r, _ := ParseRef(f.host() + "/test/img:a")
	const n = 4
	var opened sync.WaitGroup
	opened.Add(n)
	all := make(chan struct{})
	go func() { opened.Wait(); close(all) }()
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		c := f.client(nil)
		var once sync.Once
		c.tempOpen = func() {
			once.Do(opened.Done)
			select { // every puller has its temp open before any writes
			case <-all:
			case <-time.After(10 * time.Second):
			}
		}
		go func() { _, err := c.Pull(r, cache); errs <- err }()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent pull %d: %v", i, err)
		}
	}
	select {
	case <-all:
	default:
		t.Fatal("pulls did not overlap; test proves nothing")
	}
	if !VerifyFile(BlobPath(cache, ls[0]), ls[0]) {
		t.Fatal("cached blob does not verify")
	}
	left, _ := filepath.Glob(filepath.Join(cache, "blobs", "sha256", "*.part*"))
	if len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// F14: the slot guard runs after manifest+config and BEFORE any layer blob
// is requested; repeated diff_ids count once (they collapse to one drive).
func TestPullRefusesTooManyLayersBeforeBlobs(t *testing.T) {
	f := newFakeRegistry(t)
	n := LayerSlots() + 1
	var layers [][]byte
	for i := 0; i < n; i++ {
		layers = append(layers, mkTar(t, []tfile{{name: fmt.Sprintf("f%d", i), body: fmt.Sprint(i)}}))
	}
	_, ls := f.addImage("big", time.Now(), layers...)
	r, _ := ParseRef(f.host() + "/test/img:big")
	_, err := f.client(nil).Pull(r, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("want slot refusal, got %v", err)
	}
	for _, dg := range ls {
		if h := f.hits["/v2/test/img/blobs/"+dg]; h != 0 {
			t.Fatalf("layer blob %s fetched %d times before slot refusal", Short(dg), h)
		}
	}
	// LayerSlots() distinct layers + one repeat: fits after dedup.
	dup := append(append([][]byte(nil), layers[:n-1]...), layers[0])
	f.addImage("dup", time.Now(), dup...)
	r2, _ := ParseRef(f.host() + "/test/img:dup")
	img, err := f.client(nil).Pull(r2, t.TempDir())
	if err != nil {
		t.Fatalf("dedup-fitting image refused: %v", err)
	}
	if len(img.Layers) != n {
		t.Fatalf("layers = %d, want %d (dedup is the daemon's job)", len(img.Layers), n)
	}
}
