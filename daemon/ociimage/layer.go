package ociimage

// Layer converter: OCI layer tar(.gz) -> ext4 image, without root, without
// podman, without `mke2fs -d`. Tar paths are never materialized on the host:
// regular-file contents are staged under stage/<sha256> and a generated
// debugfs script (mkdir/write/symlink/mknod/ln/sif/ea_set) builds the tree
// inside an empty, freshly made filesystem. OCI whiteouts become overlayfs
// whiteouts (.wh.x -> char 0/0 x; .wh..wh..opq -> trusted.overlay.opaque=y).

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxEntries    = 200_000
	maxPathLen    = 1024
	maxNameLen    = 255
	maxLayerBytes = 4 << 30 // decompressed tar stream
	maxXattrBytes = 64 << 10
	// maxLayerXattrBytes caps the sum of xattr values in one layer: each value is
	// staged as a host file during conversion and lands in ext4 blocks, so the
	// per-value cap alone let a layer stage unbounded data (qodo #5).
	maxLayerXattrBytes = 16 << 20
	fakeTime           = "1700000000" // E2FSPROGS_FAKE_TIME: deterministic fs timestamps
)

// maxLayerXattrs caps xattr ENTRIES per layer: each one, empty or not, is a
// staged host file plus a debugfs command (codeant r5). One per tar entry on
// average (a label on every file) still fits. A var so tests can lower it.
var maxLayerXattrs = maxEntries

type node struct {
	typ  byte   // 'd' 'f' 'l' 'c' 'b' 'p' 'h' 'w'
	hard string // for typ h: the regular file this links to
}

// LayerLabel / LayerUUID derive the ext4 identity from the diff_id, so the
// guest can check it is mounting exactly the layer the host intended.
func LayerLabel(diffID string) string { return "L" + strings.TrimPrefix(diffID, "sha256:")[:15] }
func LayerUUID(diffID string) string {
	h := strings.TrimPrefix(diffID, "sha256:")
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
func LayerFile(cache, diffID string) string {
	return filepath.Join(cache, "layers", strings.TrimPrefix(diffID, "sha256:")+".ext4")
}

// q quotes one debugfs argument (script line or -R request). debugfs' ss
// parser splits on unquoted whitespace; inside "..." the only escape is a
// doubled quote, and a newline always ends the request. We do not rely on
// that: control characters (incl. NUL/newline), DEL, '"' and '\' are refused
// (fail closed); everything else, spaces and single quotes included, is
// passed through verbatim inside double quotes. Every path that reaches a
// debugfs command line MUST go through q.
func q(s string) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return "", fmt.Errorf("unsupported character %q in path %q", r, s)
		}
	}
	return `"` + s + `"`, nil
}

func cleanPath(name string) (string, error) {
	if len(name) > maxPathLen {
		return "", fmt.Errorf("path too long (%d)", len(name))
	}
	// ".." is refused on the RAW name: path.Clean would silently rebase
	// "../../etc/x" to "/etc/x", hiding a malformed or hostile layer.
	for _, c := range strings.Split(name, "/") {
		if c == ".." {
			return "", fmt.Errorf("path escapes root: %q", name)
		}
	}
	p := path.Clean("/" + name)
	for _, c := range strings.Split(p, "/") {
		if len(c) > maxNameLen {
			return "", fmt.Errorf("name too long in %q", name)
		}
	}
	return p, nil
}

type script struct {
	w     *bufio.Writer
	cwd   string
	nodes map[string]*node
	links map[string]int // path -> link count (hardlinks)
	err   error
}

func (s *script) emit(f string, a ...any) {
	if s.err == nil {
		_, s.err = fmt.Fprintf(s.w, f+"\n", a...)
	}
}

func (s *script) cd(dir string) {
	if s.cwd == dir {
		return
	}
	qd, err := q(dir)
	if err != nil {
		s.err = err
		return
	}
	s.emit("cd %s", qd)
	s.cwd = dir
}

func (s *script) ensureDir(p string) {
	if p == "/" {
		return
	}
	if n, ok := s.nodes[p]; ok {
		if n.typ != 'd' {
			s.err = fmt.Errorf("%s: parent is not a directory", p)
		}
		return
	}
	s.ensureDir(path.Dir(p))
	s.mk(p, 'd', "mkdir %s")
}

// mk creates basename(p) inside dirname(p) using a cwd-relative command.
func (s *script) mk(p string, typ byte, cmd string, extra ...any) {
	s.cd(path.Dir(p))
	qb, err := q(path.Base(p))
	if err != nil {
		s.err = err
		return
	}
	s.emit(cmd, append([]any{qb}, extra...)...)
	s.nodes[p] = &node{typ: typ}
}

func (s *script) meta(p string, uid, gid int, mode int64, mtime time.Time) {
	qp, err := q(p)
	if err != nil {
		s.err = err
		return
	}
	s.emit("sif %s uid %d", qp, uid)
	s.emit("sif %s gid %d", qp, gid)
	s.emit("sif %s mode 0%o", qp, mode)
	if !mtime.IsZero() && mtime.Unix() > 0 {
		s.emit("sif %s mtime %s", qp, mtime.UTC().Format("20060102150405"))
	}
}

type LayerStats struct {
	Entries, Files, Dirs, Symlinks, Hardlinks, Whiteouts, Opaque, Devices, Xattrs int
	TarBytes, ContentBytes, XattrBytes                                            int64
	XattrValueBytes                                                               int64 // actual xattr value bytes (XattrBytes is the 4K-rounded disk estimate)
}

// BuildLayer converts one cached blob into cache/layers/<diff_id>.ext4.
func BuildLayer(cache string, l Layer) (LayerStats, error) {
	var st LayerStats
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			return st, fmt.Errorf("layer conversion needs e2fsprogs on the host (%s not found in PATH)", tool)
		}
	}
	out := LayerFile(cache, l.DiffID)
	work := filepath.Join(cache, "work", strings.TrimPrefix(l.DiffID, "sha256:")[:16])
	_ = os.RemoveAll(work)
	stage := filepath.Join(work, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return st, err
	}
	defer os.RemoveAll(work)

	f, err := os.Open(BlobPath(cache, l.Digest))
	if err != nil {
		return st, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(l.MediaType, "gzip") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return st, err
		}
		r = gz
	}
	dh := sha256.New()
	cr := &countReader{r: io.TeeReader(r, dh), max: maxLayerBytes}
	tr := tar.NewReader(cr)

	sf, err := os.Create(filepath.Join(work, "build.debugfs"))
	if err != nil {
		return st, err
	}
	s := &script{w: bufio.NewWriterSize(sf, 1<<20), cwd: "", nodes: map[string]*node{"/": {typ: 'd'}}, links: map[string]int{}}
	type metaRec struct {
		p        string
		uid, gid int
		mode     int64
		mtime    time.Time
	}
	s.emit("rmdir /lost+found") // a layer must not inject lost+found into the merged root
	// dirMeta: last header per directory wins (tar semantics), applied after
	// every child exists, deepest path first, so nothing created later in
	// the script touches a directory whose metadata is already final.
	dirMeta := map[string]metaRec{}
	xn := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, fmt.Errorf("tar: %w", err)
		}
		st.Entries++
		if st.Entries > maxEntries {
			return st, fmt.Errorf("more than %d entries", maxEntries)
		}
		p, err := cleanPath(h.Name)
		if err != nil {
			return st, err
		}
		dir, base := path.Dir(p), path.Base(p)
		if p != "/" {
			s.ensureDir(dir)
		}
		// whiteouts
		if base == ".wh..wh..opq" {
			s.ensureDir(dir)
			qd, err := q(dir)
			if err != nil {
				return st, err
			}
			s.emit("ea_set %s trusted.overlay.opaque y", qd)
			st.Opaque++
			continue
		}
		if strings.HasPrefix(base, ".wh..wh.") {
			// other AUFS metadata (.wh..wh.plnk, .wh..wh.aufs): not content, never a whiteout
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			tgt := path.Join(dir, strings.TrimPrefix(base, ".wh."))
			if _, ok := s.nodes[tgt]; ok {
				return st, fmt.Errorf("whiteout %s collides with entry in same layer", p)
			}
			s.mk(tgt, 'w', "mknod %s c 0 0")
			s.meta(tgt, 0, 0, 0o20000, time.Time{})
			st.Whiteouts++
			continue
		}
		mode := h.Mode & 0o7777
		if old, ok := s.nodes[p]; ok && p != "/" {
			// later entry wins (tar semantics); dirs merge
			if !(old.typ == 'd' && h.Typeflag == tar.TypeDir) {
				// links_count is fixed up at the end per origin path; replacing
				// either side of a hardlink pair would leave it wrong. Refuse.
				if old.typ == 'h' || s.links[p] > 0 {
					return st, fmt.Errorf("%s: replacing a hardlinked entry in the same layer is unsupported", p)
				}
				if old.typ == 'd' {
					for np := range s.nodes {
						if strings.HasPrefix(np, p+"/") {
							return st, fmt.Errorf("%s: replacing a non-empty directory in the same layer is unsupported", p)
						}
					}
					for dp := range dirMeta {
						if dp == p || strings.HasPrefix(dp, p+"/") {
							delete(dirMeta, dp)
						}
					}
				}
				s.cd(dir)
				qb, err := q(base)
				if err != nil {
					return st, err
				}
				if old.typ == 'd' {
					s.emit("rmdir %s", qb)
				} else {
					s.emit("rm %s", qb)
				}
				delete(s.nodes, p)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if p != "/" {
				if _, ok := s.nodes[p]; !ok {
					s.mk(p, 'd', "mkdir %s")
				}
			}
			dirMeta[p] = metaRec{p, h.Uid, h.Gid, 0o40000 | mode, h.ModTime}
			st.Dirs++
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > maxLayerBytes {
				return st, fmt.Errorf("%s: bad size %d", p, h.Size)
			}
			hh := sha256.New()
			tmp := filepath.Join(stage, "incoming")
			tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return st, err
			}
			n, err := io.Copy(io.MultiWriter(tf, hh), tr)
			tf.Close()
			if err != nil || n != h.Size {
				return st, fmt.Errorf("%s: short read %d/%d: %v", p, n, h.Size, err)
			}
			staged := filepath.Join(stage, hex.EncodeToString(hh.Sum(nil)))
			if _, err := os.Stat(staged); err == nil {
				os.Remove(tmp)
			} else if err := os.Rename(tmp, staged); err != nil {
				return st, err
			}
			s.cd(dir)
			qb, err := q(base)
			if err != nil {
				return st, err
			}
			qs, err := q(staged)
			if err != nil {
				return st, err
			}
			s.emit("write %s %s", qs, qb)
			s.nodes[p] = &node{typ: 'f'}
			s.meta(p, h.Uid, h.Gid, 0o100000|mode, h.ModTime)
			st.Files++
			st.ContentBytes += h.Size
		case tar.TypeSymlink:
			if len(h.Linkname) > maxPathLen {
				return st, fmt.Errorf("%s: symlink target too long", p)
			}
			qt, err := q(h.Linkname)
			if err != nil {
				return st, err
			}
			s.mk(p, 'l', "symlink %s "+strings.ReplaceAll(qt, "%", "%%"))
			s.meta(p, h.Uid, h.Gid, 0o120000|0o777, h.ModTime)
			st.Symlinks++
		case tar.TypeLink:
			tgt, err := cleanPath(h.Linkname)
			if err != nil {
				return st, err
			}
			tn, ok := s.nodes[tgt]
			if ok && tn.typ == 'h' {
				// link to a link: point at the original regular file
				tgt = tn.hard
				tn, ok = s.nodes[tgt]
			}
			if !ok || tn.typ != 'f' {
				return st, fmt.Errorf("%s: hardlink target %s not a regular file in this layer", p, tgt)
			}
			qt, err := q(tgt)
			if err != nil {
				return st, err
			}
			s.mk(p, 'h', "ln "+strings.ReplaceAll(qt, "%", "%%")+" %s")
			if n := s.nodes[p]; n != nil {
				n.hard = tgt
			}
			if s.links[tgt] == 0 {
				s.links[tgt] = 1
			}
			s.links[tgt]++
			st.Hardlinks++
		case tar.TypeChar, tar.TypeBlock:
			t, bits := "c", int64(0o20000)
			if h.Typeflag == tar.TypeBlock {
				t, bits = "b", 0o60000
			}
			s.mk(p, t[0], "mknod %s "+t+" %d %d", h.Devmajor, h.Devminor)
			s.meta(p, h.Uid, h.Gid, bits|mode, h.ModTime)
			st.Devices++
		case tar.TypeFifo:
			s.mk(p, 'p', "mknod %s p")
			s.meta(p, h.Uid, h.Gid, 0o10000|mode, h.ModTime)
			st.Devices++
		default:
			return st, fmt.Errorf("%s: unsupported tar type %q", p, h.Typeflag)
		}
		// xattrs (e.g. security.capability) from PAX records
		keys := make([]string, 0)
		for k := range h.PAXRecords {
			if strings.HasPrefix(k, "SCHILY.xattr.") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := h.PAXRecords[k]
			if len(v) > maxXattrBytes {
				return st, fmt.Errorf("%s: xattr too large", p)
			}
			// The caps count what is staged: actual value bytes (greptile r5:
			// the 4K disk estimate made 4097 tiny labels hit a 16 MiB cap) and
			// entries, since an empty value still stages a host file and a
			// debugfs command (codeant r5).
			if st.XattrValueBytes+int64(len(v)) > maxLayerXattrBytes {
				return st, fmt.Errorf("%s: layer xattrs exceed %d bytes in total", p, maxLayerXattrBytes)
			}
			if st.Xattrs >= maxLayerXattrs {
				return st, fmt.Errorf("%s: layer has more than %d xattrs", p, maxLayerXattrs)
			}
			st.XattrValueBytes += int64(len(v))
			// sizing: a value that does not fit in the inode takes its own
			// block(s); count every one as such (round up to 4K).
			st.XattrBytes += (int64(len(v)) + 4095) / 4096 * 4096
			xn++
			xf := filepath.Join(work, fmt.Sprintf("x%d", xn))
			if err := os.WriteFile(xf, []byte(v), 0o600); err != nil {
				return st, err
			}
			qp, err := q(p)
			if err != nil {
				return st, err
			}
			name := strings.TrimPrefix(k, "SCHILY.xattr.")
			qn, err := q(name)
			if err != nil {
				return st, err
			}
			qx, err := q(xf)
			if err != nil {
				return st, err
			}
			s.emit("ea_set -f %s %s %s", qx, qp, qn)
			st.Xattrs++
		}
		if s.err != nil {
			return st, s.err
		}
	}
	// drain trailing padding so the diff_id covers the whole stream
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return st, err
	}
	st.TarBytes = cr.n
	if got := "sha256:" + hex.EncodeToString(dh.Sum(nil)); got != l.DiffID {
		return st, fmt.Errorf("diff_id mismatch: config says %s, layer is %s", l.DiffID, got)
	}
	for p, n := range s.links {
		qp, err := q(p)
		if err != nil {
			return st, err
		}
		s.emit("sif %s links_count %d", qp, n)
	}
	dirs := make([]string, 0, len(dirMeta))
	for p := range dirMeta {
		dirs = append(dirs, p)
	}
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if dirs[i] == "/" {
			di = 0
		}
		if dirs[j] == "/" {
			dj = 0
		}
		if di != dj {
			return di > dj
		}
		return dirs[i] < dirs[j]
	})
	for _, dp := range dirs {
		m := dirMeta[dp]
		s.meta(m.p, m.uid, m.gid, m.mode, m.mtime)
	}
	if s.err == nil {
		s.err = s.w.Flush()
	}
	sf.Close()
	if s.err != nil {
		return st, s.err
	}

	// size: content rounded to 4K + xattr blocks + per-inode overhead + one
	// data block per directory (implied parents included) + dirent bytes,
	// +25% + 8M slack. Without the per-directory block a dir-heavy layer ran
	// out of blocks inside debugfs (codeant r6).
	ents := int64(len(s.nodes)) + 16
	var ndirs, dirent int64
	for np, n := range s.nodes {
		if n.typ == 'd' {
			ndirs++
		}
		dirent += (8 + int64(len(path.Base(np))) + 3) &^ 3
	}
	blocks := (st.ContentBytes+st.XattrBytes+int64(st.Files)*4096)/4096 + ents/8 + ndirs + dirent/4096 + 64
	sizeMB := (blocks*4096*5/4)>>20 + 8
	inodes := ents + ents/4 + 64
	img := out + ".part"
	_ = os.MkdirAll(filepath.Dir(out), 0o755)
	_ = os.Remove(img)
	// Every failure below removes the (up to GiB-sized, sparse) .part; only
	// the final rename keeps it (codeant r5: mke2fs/debugfs/truncate errors
	// left it in the cache).
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(img)
		}
	}()
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		return st, err
	}
	if err := os.Truncate(img, sizeMB<<20); err != nil {
		return st, err
	}
	env := append(os.Environ(), "E2FSPROGS_FAKE_TIME="+fakeTime)
	uuid := LayerUUID(l.DiffID)
	mk := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-b", "4096", "-I", "256",
		"-N", fmt.Sprint(inodes), "-m", "0", "-L", LayerLabel(l.DiffID), "-U", uuid,
		"-O", "^has_journal,^resize_inode", "-E", "hash_seed="+uuid+",root_owner=0:0,lazy_itable_init=0", img)
	mk.Env = env
	if b, err := mk.CombinedOutput(); err != nil {
		return st, fmt.Errorf("mke2fs: %v: %s", err, b)
	}
	db := exec.Command("debugfs", "-w", "-f", filepath.Join(work, "build.debugfs"), img)
	db.Env = env
	var stderr strings.Builder
	db.Stdout = io.Discard
	db.Stderr = &stderr
	if err := db.Run(); err != nil {
		return st, fmt.Errorf("debugfs: %v", err)
	}
	var bad []string
	for _, ln := range strings.Split(stderr.String(), "\n") {
		if ln == "" || strings.HasPrefix(ln, "debugfs ") {
			continue
		}
		bad = append(bad, ln)
	}
	if len(bad) > 0 {
		if len(bad) > 10 {
			bad = bad[:10]
		}
		os.Remove(img)
		return st, fmt.Errorf("debugfs reported errors:\n  %s", strings.Join(bad, "\n  "))
	}
	if b, err := exec.Command("e2fsck", "-fn", img).CombinedOutput(); err != nil {
		os.Remove(img)
		return st, fmt.Errorf("e2fsck: %v\n%s", err, b)
	}
	if err := os.Chmod(img, 0o444); err != nil {
		return st, err
	}
	if err := os.Rename(img, out); err != nil {
		return st, err
	}
	renamed = true
	return st, nil
}

type countReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, errors.New("decompressed layer exceeds bound")
	}
	return n, err
}
