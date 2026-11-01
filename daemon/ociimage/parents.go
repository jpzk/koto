package ociimage

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// CheckImpliedParents refuses an image where a layer creates a file under a
// directory it has no header for (an "implied parent") while a lower layer
// has that path as a non-directory, e.g. bin/tool over a base whose /bin is
// a symlink to usr/bin.
//
// Layers are converted per diff_id (one ext4 shared by every image that
// carries it), so BuildLayer cannot know the lower stack: it makes implied
// parents real root:root 0755 directories, which is also what moby's
// overlay2 unpack does. Over a lower directory that is the reference
// behaviour; over a lower symlink or file it would silently hide the base's
// entry (and everything reached through it) in the merged rootfs. That case
// is refused, fail closed, before the image enters the catalog.
func CheckImpliedParents(cache string, layers []Layer) error {
	lower := map[string]bool{} // path -> isDir, merged state of layers below
	for i, l := range layers {
		ents, wh, opq, err := scanHeaders(BlobPath(cache, l.Digest), l.MediaType)
		if err != nil {
			return fmt.Errorf("layer %d %s: %w", i+1, Short(l.DiffID), err)
		}
		// this layer's whiteouts / opaque dirs apply to the lower state first
		// Every recorded path has all its ancestors recorded too, so only a
		// path present in lower as a directory can have a subtree: skip the
		// O(len(lower)) scan otherwise (it ran once per entry before).
		for _, d := range opq {
			if lower[d] {
				dropUnder(lower, d, false)
			}
		}
		for _, w := range wh {
			if isDir, ok := lower[w]; ok && isDir {
				dropUnder(lower, w, true)
			} else {
				delete(lower, w)
			}
		}
		// whiteout markers live in their parent dir too (BuildLayer creates it)
		check := make([]string, 0, len(ents)+len(wh)+len(opq))
		for p := range ents {
			check = append(check, p)
		}
		check = append(check, wh...)
		for _, d := range opq {
			check = append(check, d+"/.wh..wh..opq")
		}
		for _, p := range check {
			for a := path.Dir(p); a != "/" && a != "."; a = path.Dir(a) {
				if _, explicit := ents[a]; explicit {
					continue
				}
				if isDir, ok := lower[a]; ok && !isDir {
					return fmt.Errorf("layer %d %s: %s is only implied as a directory (no header) "+
						"but a lower layer has it as a non-directory (symlink or file); "+
						"converting it would hide the lower entry: rebuild the image so the layer "+
						"writes through the real path", i+1, Short(l.DiffID), a)
				}
			}
		}
		for p, isDir := range ents {
			if wasDir := lower[p]; !isDir && wasDir {
				dropUnder(lower, p, false) // a non-dir replaces a lower dir subtree
			}
			lower[p] = isDir
			for a := path.Dir(p); a != "/" && a != "."; a = path.Dir(a) {
				if _, ok := lower[a]; !ok {
					lower[a] = true
				}
			}
		}
	}
	return nil
}

// dropUnder deletes everything below p (and p itself when self).
func dropUnder(m map[string]bool, p string, self bool) {
	if self {
		delete(m, p)
	}
	pre := p + "/"
	for k := range m {
		if strings.HasPrefix(k, pre) {
			delete(m, k)
		}
	}
}

// scanHeaders reads one layer's tar headers: entries (path -> isDir),
// whiteout targets and opaque directories.
func scanHeaders(blob, mediaType string) (ents map[string]bool, wh, opq []string, err error) {
	f, err := os.Open(blob)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(mediaType, "gzip") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, nil, nil, err
		}
		defer gz.Close()
		r = gz
	}
	ents = map[string]bool{}
	tr := tar.NewReader(&countReader{r: r, max: maxLayerBytes})
	for n := 0; ; n++ {
		if n > maxEntries {
			return nil, nil, nil, fmt.Errorf("more than %d entries", maxEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return ents, wh, opq, nil
		}
		if err != nil {
			return nil, nil, nil, err
		}
		p, err := cleanPath(h.Name)
		if err != nil {
			return nil, nil, nil, err
		}
		if p == "/" {
			continue
		}
		dir, base := path.Dir(p), path.Base(p)
		switch {
		case base == ".wh..wh..opq":
			opq = append(opq, dir)
		case strings.HasPrefix(base, ".wh."):
			wh = append(wh, path.Join(dir, strings.TrimPrefix(base, ".wh.")))
		default:
			ents[p] = h.Typeflag == tar.TypeDir
		}
	}
}
