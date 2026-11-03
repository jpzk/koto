package ociimage

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

// EnsureLayers converts every distinct layer of img that is not already
// cached. A repeated diff_id is one ext4 (the daemon's fcImageDedup attaches
// it once), so the slot budget and the work both count unique diff_ids. It
// refuses images over the slot budget before doing any work. Returns the
// number of distinct layers built vs reused (shared with other images). A
// reused layer's mtime is refreshed so a concurrent GC's grace period covers
// it until the catalog entry that keeps it is written.
func EnsureLayers(cache string, img *Image, logf func(string, ...any)) (built, reused int, err error) {
	built, reused, _, err = EnsureLayersCtx(context.Background(), cache, img, logf)
	return built, reused, err
}

// FreshLayer is a layer file one EnsureLayersCtx call converted, with the
// mtime it had when the build finished: a later TouchCached (another add
// reusing it) moves the mtime, which is how RemoveFresh tells a layer only
// the cancelled add knows about from one somebody else now relies on.
type FreshLayer struct {
	Path  string
	Mtime time.Time
}

// EnsureLayersCtx is EnsureLayers that honours ctx (nil = never cancelled):
// it checks ctx before each layer, and when ctx is cancelled it stops,
// removes the layers this call built (RemoveFresh) and returns ctx.Err().
// A conversion already running finishes first (it is one mke2fs/debugfs
// pass, not interruptible) and is then removed. The returned fresh list
// lets the caller undo the build if it is cancelled later, before the
// catalog entry that would keep the layers is written.
func EnsureLayersCtx(ctx context.Context, cache string, img *Image, logf func(string, ...any)) (built, reused int, fresh []FreshLayer, err error) {
	return EnsureLayersProgress(ctx, cache, img, logf, nil)
}

// layerCached and layerBuilder are LayerOK and BuildLayerProgress; tests
// swap them to drive the convert loop without e2fsprogs on the PATH.
var (
	layerCached  = LayerOK
	layerBuilder = BuildLayerProgress
)

// EnsureLayersProgress is EnsureLayersCtx that also reports progress per
// distinct layer i/n: PhaseLayerOK for a layer whose ext4 image is already
// cached, else PhaseConvert from 0 to the blob size as the tar is consumed
// (BuildLayerProgress; throttled, final done == total). prog may be nil.
func EnsureLayersProgress(ctx context.Context, cache string, img *Image, logf func(string, ...any), prog func(Progress)) (built, reused int, fresh []FreshLayer, err error) {
	if prog == nil {
		prog = func(Progress) {}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ls := UniqueLayers(img.Layers)
	if err := CheckLayerSlots(len(ls)); err != nil {
		return 0, 0, nil, err
	}
	convertMu.Lock()
	defer convertMu.Unlock()
	cancelled := func() error {
		if ctx == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			removeFreshLocked(fresh, logf)
			return err
		}
		return nil
	}
	for i, l := range ls {
		if err := cancelled(); err != nil {
			return built, reused, nil, err
		}
		if layerCached(cache, l) {
			prog(Progress{Phase: PhaseLayerOK, Layer: i + 1, Layers: len(ls), Done: l.Size, Total: l.Size})
			TouchCached(LayerFile(cache, l.DiffID))
			reused++
			logf("layer %d/%d %s cached (%s)", i+1, len(ls), Short(l.DiffID), LayerLabel(l.DiffID))
			continue
		}
		t := time.Now()
		prog(Progress{Phase: PhaseConvert, Layer: i + 1, Layers: len(ls), Total: l.Size})
		st, err := layerBuilder(cache, l, func(done, total int64) {
			prog(Progress{Phase: PhaseConvert, Layer: i + 1, Layers: len(ls), Done: done, Total: total})
		})
		if err != nil {
			return built, reused, fresh, fmt.Errorf("layer %d %s: %w", i+1, Short(l.DiffID), err)
		}
		p := LayerFile(cache, l.DiffID)
		if fi, serr := os.Stat(p); serr == nil {
			fresh = append(fresh, FreshLayer{Path: p, Mtime: fi.ModTime()})
		}
		built++
		logf("layer %d/%d %s converted in %s: %d entries (%d files, %d dirs, %d symlinks, %d whiteouts, %d opaque)",
			i+1, len(ls), Short(l.DiffID), time.Since(t).Round(time.Millisecond),
			st.Entries, st.Files, st.Dirs, st.Symlinks, st.Whiteouts, st.Opaque)
	}
	if err := cancelled(); err != nil {
		return built, reused, nil, err
	}
	return built, reused, fresh, nil
}

// RemoveFresh deletes the layers a cancelled add built, except any whose
// mtime moved since (another add reused it via TouchCached and may be about
// to write a catalog entry naming it). Taken under convertMu, so it cannot
// interleave with another EnsureLayers checking or touching the same file.
func RemoveFresh(fresh []FreshLayer, logf func(string, ...any)) {
	if len(fresh) == 0 {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	convertMu.Lock()
	defer convertMu.Unlock()
	removeFreshLocked(fresh, logf)
}

func removeFreshLocked(fresh []FreshLayer, logf func(string, ...any)) {
	for _, f := range fresh {
		fi, err := os.Stat(f.Path)
		if err != nil || !fi.ModTime().Equal(f.Mtime) {
			continue
		}
		if err := os.Remove(f.Path); err == nil {
			logf("cancelled: removed layer %s", path.Base(f.Path))
		}
	}
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
	added := map[string]string{} // this layer's loaders
	wh, opq := map[string]bool{}, map[string]bool{}
	for n := 0; ; n++ {
		if n > maxEntries {
			return fmt.Errorf("more than %d entries", maxEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			for k := range present {
				if hiddenBy(k, wh, opq) {
					delete(present, k)
				}
			}
			for k, v := range added {
				present[k] = v
			}
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
		// Whiteouts hide LOWER layers only (OCI layer spec), so they are
		// collected here and applied once per layer: one ancestor walk per
		// loader path instead of a scan of every loader per whiteout, which
		// was quadratic on a layer of many loaders and whiteouts (codeant r6).
		if base == ".wh..wh..opq" {
			opq[dir] = true
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			wh[path.Join(dir, strings.TrimPrefix(base, ".wh."))] = true
			continue
		}
		if lc := loaderLibc(base); lc != "" && h.Typeflag != tar.TypeDir {
			added[p] = lc
		}
	}
}

// hiddenBy reports whether p (or an ancestor) is whited out by wh, or sits
// below an opaque directory in opq.
func hiddenBy(p string, wh, opq map[string]bool) bool {
	if wh[p] {
		return true
	}
	for d := path.Dir(p); ; d = path.Dir(d) {
		if wh[d] || opq[d] {
			return true
		}
		if d == "/" || d == "." {
			return false
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

// UniqueLayers keeps the first occurrence of every diff_id, in order. Only
// the set matters here (conversion is per diff_id); the boot order of
// repeated layers is the daemon's fcImageDedup.
func UniqueLayers(layers []Layer) []Layer {
	seen := map[string]bool{}
	var out []Layer
	for _, l := range layers {
		if !seen[l.DiffID] {
			seen[l.DiffID] = true
			out = append(out, l)
		}
	}
	return out
}

// TouchCached refreshes the mtime of a reused cache file so Store.GC's grace
// period protects it while an `image add` that reuses it is in flight (the
// catalog entry that keeps it is written last). Best effort: a failed touch
// only shrinks the protection back to the file's real age.
func TouchCached(p string) {
	now := time.Now()
	_ = os.Chtimes(p, now, now)
}
