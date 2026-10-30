package ociimage

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// convertMu serialises layer builds host-wide: two images sharing a layer
// must not race on the same work dir / .part file.
var convertMu sync.Mutex

// ReadSuper returns the ext4 volume label and UUID straight from the superblock.
func ReadSuper(p string) (label, uuid string, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sb := make([]byte, 1024)
	if _, err := f.ReadAt(sb, 1024); err != nil {
		return "", "", err
	}
	if sb[0x38] != 0x53 || sb[0x39] != 0xEF {
		return "", "", fmt.Errorf("%s: not ext4", p)
	}
	u := sb[0x68:0x78]
	uuid = fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	return strings.TrimRight(string(sb[0x78:0x88]), "\x00"), uuid, nil
}

// LayerOK reports whether the cached ext4 for l exists, is read-only and
// carries the label/UUID derived from its diff_id.
func LayerOK(cache string, l Layer) bool {
	p := LayerFile(cache, l.DiffID)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o222 != 0 {
		return false
	}
	lbl, uuid, err := ReadSuper(p)
	return err == nil && lbl == LayerLabel(l.DiffID) && uuid == LayerUUID(l.DiffID)
}

// EnsureLayers converts every layer of img that is not already cached.
// It refuses images over the slot budget before doing any work. Returns the
// number of layers built vs reused (shared with other images).
func EnsureLayers(cache string, img *Image, logf func(string, ...any)) (built, reused int, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := CheckLayerSlots(len(img.Layers)); err != nil {
		return 0, 0, err
	}
	convertMu.Lock()
	defer convertMu.Unlock()
	for i, l := range img.Layers {
		if LayerOK(cache, l) {
			reused++
			logf("layer %d/%d %s cached (%s)", i+1, len(img.Layers), Short(l.DiffID), LayerLabel(l.DiffID))
			continue
		}
		t := time.Now()
		st, err := BuildLayer(cache, l)
		if err != nil {
			return built, reused, fmt.Errorf("layer %d %s: %w", i+1, Short(l.DiffID), err)
		}
		built++
		logf("layer %d/%d %s converted in %s: %d entries (%d files, %d dirs, %d symlinks, %d whiteouts, %d opaque)",
			i+1, len(img.Layers), Short(l.DiffID), time.Since(t).Round(time.Millisecond),
			st.Entries, st.Files, st.Dirs, st.Symlinks, st.Whiteouts, st.Opaque)
	}
	return built, reused, nil
}

// Libc values reported by DetectLibc.
const (
	LibcGlibc   = "glibc"
	LibcMusl    = "musl"
	LibcUnknown = "unknown"
)

// DetectLibc scans the layer tars (headers only, bottom -> top, honouring
// whiteouts) for the dynamic loader: ld-linux-x86-64.so.2 means glibc,
// ld-musl-*.so.1 means musl. glibc wins when both exist (gcompat images).
func DetectLibc(cache string, layers []Layer) (string, error) {
	present := map[string]string{} // path -> libc
	for _, l := range layers {
		if err := scanLayer(BlobPath(cache, l.Digest), l.MediaType, present); err != nil {
			return LibcUnknown, fmt.Errorf("libc scan %s: %w", Short(l.Digest), err)
		}
	}
	musl := false
	for _, v := range present {
		if v == LibcGlibc {
			return LibcGlibc, nil
		}
		musl = musl || v == LibcMusl
	}
	if musl {
		return LibcMusl, nil
	}
	return LibcUnknown, nil
}

func loaderLibc(base string) string {
	switch {
	case base == "ld-linux-x86-64.so.2":
		return LibcGlibc
	case strings.HasPrefix(base, "ld-musl-") && strings.HasSuffix(base, ".so.1"):
		return LibcMusl
	}
	return ""
}

func scanLayer(blob, mediaType string, present map[string]string) error {
	f, err := os.Open(blob)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(mediaType, "gzip") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(&countReader{r: r, max: maxLayerBytes})
	for n := 0; ; n++ {
		if n > maxEntries {
			return fmt.Errorf("more than %d entries", maxEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := cleanPath(h.Name)
		if err != nil {
			return err
		}
		dir, base := path.Dir(p), path.Base(p)
		if base == ".wh..wh..opq" {
			for k := range present {
				if strings.HasPrefix(k, dir+"/") {
					delete(present, k)
				}
			}
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			tgt := path.Join(dir, strings.TrimPrefix(base, ".wh."))
			for k := range present {
				if k == tgt || strings.HasPrefix(k, tgt+"/") {
					delete(present, k)
				}
			}
			continue
		}
		if lc := loaderLibc(base); lc != "" && h.Typeflag != tar.TypeDir {
			present[p] = lc
		}
	}
}

// LibcWarning is the operator-facing warning for a detected libc, or "".
func LibcWarning(libc string) string {
	switch libc {
	case LibcMusl:
		return "WARNING: musl image: koto's claude binary is built for glibc and will not run in this guest; " +
			"use a glibc-based image (debian, ubuntu, fedora) for claude groups"
	case LibcUnknown:
		return "WARNING: no dynamic loader found in the image (static or distroless?): claude will not run"
	}
	return ""
}
