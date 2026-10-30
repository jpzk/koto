package ociimage

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
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
