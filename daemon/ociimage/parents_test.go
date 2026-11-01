package ociimage

import (
	"archive/tar"
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"
)

func hdrTar(t *testing.T, hs ...*tar.Header) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hs {
		if h.Mode == 0 {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	return buf.Bytes()
}

// F05: an implied parent over a lower symlink would hide the base's
// entry; refuse. Implied over a lower dir, explicit dir headers, whiteouts
// that remove the lower symlink first, and paths through unrelated dirs pass.
func TestCheckImpliedParents(t *testing.T) {
	cache := t.TempDir()
	reg := func(n string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeReg} }
	dir := func(n string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeDir} }
	base := cacheLayer(t, cache, hdrTar(t, dir("usr/"), dir("usr/bin/"), reg("usr/bin/sh"),
		&tar.Header{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "usr/bin"},
		dir("home/"), dir("home/node/"), reg("etc/passwd")))

	cases := []struct {
		name string
		top  []*tar.Header
		bad  string // "" = accept, else the refused path
	}{
		{"implied over symlink", []*tar.Header{reg("bin/tool")}, "/bin"},
		{"implied over symlink, deep", []*tar.Header{reg("bin/x/y")}, "/bin"},
		{"implied over file", []*tar.Header{reg("etc/passwd/x")}, "/etc/passwd"},
		{"whiteout under symlink", []*tar.Header{reg("bin/.wh.sh")}, "/bin"},
		{"explicit dir replaces symlink", []*tar.Header{dir("bin/"), reg("bin/tool")}, ""},
		{"implied over lower dir", []*tar.Header{reg("home/node/.npmrc")}, ""},
		{"implied new dir", []*tar.Header{reg("opt/app/run")}, ""},
		{"whiteout removes symlink first", []*tar.Header{reg(".wh.bin"), reg("bin/tool")}, ""},
		{"through real path", []*tar.Header{reg("usr/bin/tool")}, ""},
	}
	for _, c := range cases {
		top := cacheLayer(t, cache, hdrTar(t, c.top...))
		err := CheckImpliedParents(cache, []Layer{base, top})
		switch {
		case c.bad == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad+" is only implied")):
			t.Errorf("%s: want refusal of %s, got %v", c.name, c.bad, err)
		}
	}
	// a middle layer turning the symlink into a dir makes the top layer fine
	mid := cacheLayer(t, cache, hdrTar(t, reg(".wh.bin"), dir("bin/")))
	top := cacheLayer(t, cache, hdrTar(t, reg("bin/tool2")))
	if err := CheckImpliedParents(cache, []Layer{base, mid, top}); err != nil {
		t.Errorf("dir from middle layer: %v", err)
	}
}

// Round-2 review: the subtree drop must still happen when a non-dir or a
// whiteout replaces a lower dir, but must not scan lower once per entry.
func TestCheckImpliedParentsReplaceAndScale(t *testing.T) {
	cache := t.TempDir()
	reg := func(n string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeReg} }
	dir := func(n string) *tar.Header { return &tar.Header{Name: n, Typeflag: tar.TypeDir} }
	base := cacheLayer(t, cache, hdrTar(t, dir("srv/"), dir("srv/a/"), reg("srv/a/f")))
	// srv replaced by a file: a later implied srv/x must be refused
	mid := cacheLayer(t, cache, hdrTar(t, reg("srv")))
	top := cacheLayer(t, cache, hdrTar(t, reg("srv/x")))
	if err := CheckImpliedParents(cache, []Layer{base, mid, top}); err == nil || !strings.Contains(err.Error(), "/srv is only implied") {
		t.Fatalf("file over dir then implied: %v", err)
	}
	// srv whited out, then recreated by an explicit dir: srv/a is gone, so
	// an implied srv/a/y is a new dir, accepted
	mid = cacheLayer(t, cache, hdrTar(t, reg(".wh.srv"), dir("srv/")))
	top = cacheLayer(t, cache, hdrTar(t, reg("srv/a/y")))
	if err := CheckImpliedParents(cache, []Layer{base, mid, top}); err != nil {
		t.Fatalf("whiteout dir then recreate: %v", err)
	}

	// scale: n files over n lower files was n*n prefix checks (~10^9 at
	// n=32k, tens of seconds); now linear.
	const n = 32000
	hs := []*tar.Header{dir("d/")}
	for i := 0; i < n; i++ {
		hs = append(hs, reg("d/f"+strconv.Itoa(i)))
	}
	lo := cacheLayer(t, cache, hdrTar(t, hs...))
	hs = hs[:1]
	for i := 0; i < n; i++ {
		hs = append(hs, reg("d/f"+strconv.Itoa(i)), reg("d/.wh.g"+strconv.Itoa(i)))
	}
	hi := cacheLayer(t, cache, hdrTar(t, hs...))
	start := time.Now()
	if err := CheckImpliedParents(cache, []Layer{lo, hi}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("CheckImpliedParents %d over %d took %v (quadratic?)", n, n, d)
	}
}
