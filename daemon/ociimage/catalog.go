package ociimage

// Image catalog: the operator-facing name -> digest map plus the verified
// description of every digest still kept on disk.
//
//	<root>/catalog.json        names, images by digest, default pointer
//	<root>/blobs/sha256/<hex>   manifests, configs, layer blobs (0444)
//	<root>/layers/<hex>.ext4    converted layers keyed by diff_id (0444)
//	<root>/tools/<key>.img      tools.img (see tools.go)
//
// A name is resolved to a digest ONCE (add/update); groups record the
// digest, so moving a name never moves a group. Digests that lost their
// name stay in the catalog while any group (current, previous, archived
// sys.img) still references them, so rollback works offline.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// DefaultName is the built-in image: today's shipped rootfs, booted through
// the unchanged default path. It is never stored, pulled or removed.
const DefaultName = "koto-default"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether s is acceptable as a catalog name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Entry is one image digest kept in the catalog.
type Entry struct {
	Digest         string    `json:"digest"`
	Ref            string    `json:"ref"` // repo@digest, as pinned at add/update
	ManifestDigest string    `json:"manifest_digest,omitempty"`
	ConfigDigest   string    `json:"config_digest,omitempty"`
	Created        time.Time `json:"created"`
	Added          time.Time `json:"added"`
	Arch           string    `json:"arch"`
	Libc           string    `json:"libc"`
	Size           int64     `json:"size"` // compressed layer bytes
	Layers         []Layer   `json:"layers"`
	Builtin        bool      `json:"builtin,omitempty"`
}

// Catalog is the on-disk document. Names may share a digest.
type Catalog struct {
	Version int               `json:"version"`
	Default string            `json:"default,omitempty"` // "" == DefaultName
	Names   map[string]string `json:"names"`             // name -> digest
	Images  map[string]*Entry `json:"images"`            // digest -> entry
	// Sources is the reference each name was added/updated from (a tag or a
	// pinned ref), so `koto image update <name>` can re-resolve it.
	Sources map[string]string `json:"sources,omitempty"`
}

// GroupRef is what one group pins: its current digest, its rollback
// digest and the bases of archived sys images. The catalog never reads
// group config itself; the daemon hands these in.
type GroupRef struct {
	Group   string
	Digests []string
}

var builtin = &Entry{Builtin: true, Arch: "amd64", Libc: LibcGlibc}

// DefaultImage returns the name new groups get when they ask for "the default".
func (c *Catalog) DefaultImage() string {
	if c.Default == "" {
		return DefaultName
	}
	return c.Default
}

// Lookup resolves a catalog name. DefaultName resolves to the built-in entry.
func (c *Catalog) Lookup(name string) (*Entry, bool) {
	if name == DefaultName {
		return builtin, true
	}
	dg, ok := c.Names[name]
	if !ok {
		return nil, false
	}
	e, ok := c.Images[dg]
	return e, ok
}

// ByDigest returns the entry kept for dg (named or not).
func (c *Catalog) ByDigest(dg string) (*Entry, bool) {
	e, ok := c.Images[dg]
	return e, ok
}

// NamesFor lists the names currently pointing at dg, sorted.
func (c *Catalog) NamesFor(dg string) []string {
	var out []string
	for n, d := range c.Names {
		if d == dg {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// SortedNames returns every stored name (the built-in excluded), sorted.
func (c *Catalog) SortedNames() []string {
	out := make([]string, 0, len(c.Names))
	for n := range c.Names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Put records img (pulled + converted) under name and returns the digest
// the name pointed at before ("" if new). It never touches groups.
func (c *Catalog) Put(name string, img *Image, libc string, now time.Time) (string, error) {
	if name == DefaultName {
		return "", fmt.Errorf("%q is the built-in image and cannot be replaced", DefaultName)
	}
	if !ValidName(name) {
		return "", fmt.Errorf("invalid image name %q (want [a-z0-9][a-z0-9._-]{0,63})", name)
	}
	if !ValidDigest(img.Digest) {
		return "", fmt.Errorf("invalid digest %q", img.Digest)
	}
	old := c.Names[name]
	if _, ok := c.Images[img.Digest]; !ok {
		c.Images[img.Digest] = &Entry{Digest: img.Digest, Ref: img.Ref, ManifestDigest: img.ManifestDigest,
			ConfigDigest: img.ConfigDigest, Created: img.Created, Added: now.UTC(), Arch: img.Arch,
			Libc: libc, Size: img.CompressedSize(), Layers: img.Layers}
	}
	c.Names[name] = img.Digest
	return old, nil
}

// Users lists the groups referencing dg, sorted.
func Users(dg string, refs []GroupRef) []string {
	var out []string
	for _, r := range refs {
		for _, d := range r.Digests {
			if d == dg {
				out = append(out, r.Group)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// ErrInUse is returned (wrapped) when rm hits a referenced image.
var ErrInUse = errors.New("image in use")

// Remove deletes name. It is refused while any group references the
// digest (current, rollback or archived sys.img) and no OTHER name still
// points at it; in that case nothing changes. The digest entry is dropped
// once neither a name nor a group needs it (blobs go at gc).
func (c *Catalog) Remove(name string, refs []GroupRef) error {
	if name == DefaultName {
		return fmt.Errorf("%q is built in and cannot be removed", DefaultName)
	}
	dg, ok := c.Names[name]
	if !ok {
		return fmt.Errorf("no image named %q", name)
	}
	others := len(c.NamesFor(dg)) > 1
	if u := Users(dg, refs); len(u) > 0 && !others {
		return fmt.Errorf("%w: %s (%s) is used by group(s) %s; move them first (koto image apply / rollback)",
			ErrInUse, name, Short(dg), strings.Join(u, ", "))
	}
	delete(c.Names, name)
	delete(c.Sources, name)
	if c.Default == name {
		c.Default = ""
	}
	if len(c.NamesFor(dg)) == 0 && len(Users(dg, refs)) == 0 {
		delete(c.Images, dg)
	}
	return nil
}

// GCReport says what a sweep removed.
type GCReport struct {
	Entries []string // digests dropped from the catalog
	Files   []string // paths removed (blobs, layers, tools)
	Bytes   int64
}

// Store is the image state directory with a locked catalog.
// Grace protects files younger than it from GC: a concurrent `image add`
// fetches blobs before it records the catalog entry that keeps them.
type Store struct {
	Root  string
	Grace time.Duration
}

// NewStore returns the store rooted at dir (created on first write).
func NewStore(dir string) *Store { return &Store{Root: dir, Grace: time.Hour} }

func (s *Store) catalogPath() string { return filepath.Join(s.Root, "catalog.json") }

func emptyCatalog() *Catalog {
	return &Catalog{Version: 1, Names: map[string]string{}, Images: map[string]*Entry{}}
}

// Load reads the catalog without locking (a consistent snapshot thanks to
// atomic rename on save). A missing file is an empty catalog.
func (s *Store) Load() (*Catalog, error) {
	b, err := os.ReadFile(s.catalogPath())
	if errors.Is(err, os.ErrNotExist) {
		return emptyCatalog(), nil
	}
	if err != nil {
		return nil, err
	}
	c := emptyCatalog()
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", s.catalogPath(), err)
	}
	if c.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported catalog version %d", s.catalogPath(), c.Version)
	}
	if c.Names == nil {
		c.Names = map[string]string{}
	}
	if c.Images == nil {
		c.Images = map[string]*Entry{}
	}
	// json accepts "<digest>": null; every caller dereferences entries, so a
	// nil one is corruption, refused here rather than a nil deref at spawn.
	for d, e := range c.Images {
		if e == nil {
			return nil, fmt.Errorf("%s: image %s has a null entry", s.catalogPath(), d)
		}
	}
	for n, d := range c.Names {
		if _, ok := c.Images[d]; !ok {
			return nil, fmt.Errorf("%s: name %q points at unknown digest %s", s.catalogPath(), n, d)
		}
	}
	return c, nil
}

// lock takes an exclusive flock on <root>/.lock (cross-process: CLI vs daemon).
func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Root, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Update runs fn on the current catalog under the lock and saves the
// result atomically if fn returns nil.
func (s *Store) Update(fn func(*Catalog) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	c, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		return err
	}
	return s.save(c)
}

func (s *Store) save(c *Catalog) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.catalogPath() + ".part"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err == nil {
		_ = f.Sync()
		f.Close()
	}
	return os.Rename(tmp, s.catalogPath())
}

// GC drops catalog entries that no name and no group references, then
// deletes every blob / layer / tools image not needed by what is left.
// keepTools names the tools image file(s) in use (basename); others go.
func (s *Store) GC(refs []GroupRef, keepTools ...string) (GCReport, error) {
	var rep GCReport
	err := s.Update(func(c *Catalog) error {
		for dg := range c.Images {
			if len(c.NamesFor(dg)) == 0 && len(Users(dg, refs)) == 0 {
				delete(c.Images, dg)
				rep.Entries = append(rep.Entries, dg)
			}
		}
		sort.Strings(rep.Entries)
		blobs, layers := map[string]bool{}, map[string]bool{}
		for _, e := range c.Images {
			for _, d := range []string{e.Digest, e.ManifestDigest, e.ConfigDigest} {
				if d != "" {
					blobs[BlobPath(s.Root, d)] = true
				}
			}
			for _, l := range e.Layers {
				blobs[BlobPath(s.Root, l.Digest)] = true
				layers[LayerFile(s.Root, l.DiffID)] = true
			}
		}
		tools := map[string]bool{}
		for _, t := range keepTools {
			tools[filepath.Join(s.Root, "tools", filepath.Base(t))] = true
		}
		sweep := func(dir string, keep map[string]bool) error {
			ents, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			for _, de := range ents {
				p := filepath.Join(dir, de.Name())
				if keep[p] || de.IsDir() {
					continue
				}
				if fi, err := de.Info(); err == nil {
					if time.Since(fi.ModTime()) < s.Grace {
						continue
					}
					rep.Bytes += fi.Size()
				}
				if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				rep.Files = append(rep.Files, p)
			}
			return nil
		}
		if err := sweep(filepath.Join(s.Root, "blobs", "sha256"), blobs); err != nil {
			return err
		}
		if err := sweep(filepath.Join(s.Root, "layers"), layers); err != nil {
			return err
		}
		if len(keepTools) > 0 {
			if err := sweep(filepath.Join(s.Root, "tools"), tools); err != nil {
				return err
			}
		}
		return nil
	})
	return rep, err
}
