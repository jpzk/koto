package ociimage

// sys.img: the per-group writable overlay upper (rw, ext4, label kotosys).
// It belongs to exactly one image digest; sys.digest records which. When
// the group's digest changes the old sys.img is archived as
// sys-<olddigest12>.img, and the new base gets either its archive back
// (rollback) or a fresh filesystem (apply). The group's workspace.img is
// never touched here.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SysLabel is the ext4 label of sys.img; guest init checks it.
const SysLabel = "kotosys"

// DefaultSysSize is the sparse size of a fresh sys.img.
const DefaultSysSize int64 = 4 << 30

// SysMode picks what a digest change does with the new base's sys.img.
type SysMode int

const (
	SysFresh   SysMode = iota // apply: always a new, empty sys.img
	SysRestore                // rollback: the archived sys-<new12>.img if present
)

var sysArchRe = regexp.MustCompile(`^sys-([a-f0-9]{12})\.img$`)

func sysPath(dir string) string       { return filepath.Join(dir, "sys.img") }
func sysDigestPath(dir string) string { return filepath.Join(dir, "sys.digest") }

// SysArchive is the archive path for digest dg in group dir.
func SysArchive(dir, dg string) string {
	return filepath.Join(dir, "sys-"+short12(dg)+".img")
}

func short12(dg string) string {
	h := strings.TrimPrefix(dg, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// SysDigest returns the digest the current sys.img belongs to ("" if none).
func SysDigest(dir string) string {
	b, err := os.ReadFile(sysDigestPath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SysArchives lists the 12-hex digest prefixes of archived sys images.
func SysArchives(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if m := sysArchRe.FindStringSubmatch(e.Name()); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// SysAction describes what PrepareSys did, for logs and the CLI.
type SysAction struct {
	Kept, Archived, Restored, Created bool
	ArchivePath                       string
}

func (a SysAction) String() string {
	var p []string
	if a.Kept {
		p = append(p, "kept")
	}
	if a.Archived {
		p = append(p, "archived old as "+filepath.Base(a.ArchivePath))
	}
	if a.Restored {
		p = append(p, "restored archive")
	}
	if a.Created {
		p = append(p, "fresh")
	}
	return strings.Join(p, ", ")
}

// PrepareSys makes dir/sys.img belong to digest dg. Same digest: kept as is.
// Different (or unrecorded) digest: the existing sys.img is archived under
// its own digest, then the archive for dg is restored (SysRestore) or a
// fresh one is made. size <= 0 uses DefaultSysSize.
func PrepareSys(dir, dg string, mode SysMode, size int64) (SysAction, error) {
	var a SysAction
	if !ValidDigest(dg) {
		return a, fmt.Errorf("sys.img: invalid digest %q", dg)
	}
	cur := SysDigest(dir)
	_, err := os.Stat(sysPath(dir))
	exists := err == nil
	if exists && cur == dg {
		a.Kept = true
		return a, nil
	}
	if exists {
		if ValidDigest(cur) {
			a.ArchivePath = SysArchive(dir, cur)
			if err := os.Rename(sysPath(dir), a.ArchivePath); err != nil {
				return a, err
			}
			a.Archived = true
		} else if err := os.Remove(sysPath(dir)); err != nil { // unowned leftover: nothing to roll back to
			return a, err
		}
	}
	_ = os.Remove(sysDigestPath(dir))
	arch := SysArchive(dir, dg)
	_, aerr := os.Stat(arch)
	restore := aerr == nil && mode == SysRestore
	// Commit the digest BEFORE sys.img appears. sys.img is absent here, so a
	// failed digest write leaves the archive where it was, and a crash after
	// the write but before the rename/mkfs just repeats this path next boot.
	// The reverse order could leave a sys.img with no digest, which the next
	// call treats as an unowned leftover and deletes — losing the rollback.
	if err := writeAtomic(sysDigestPath(dir), dg+"\n"); err != nil {
		return a, err
	}
	if restore {
		if err := os.Rename(arch, sysPath(dir)); err != nil {
			_ = os.Remove(sysDigestPath(dir))
			return a, err
		}
		a.Restored = true
		return a, nil
	}
	if err := MakeSys(sysPath(dir), size); err != nil {
		_ = os.Remove(sysDigestPath(dir))
		return a, err
	}
	a.Created = true
	return a, nil
}

// ArchiveSys parks the group's current sys.img under its digest (the same
// archive PrepareSys would make) and forgets the digest. The default-rootfs
// boot calls it, so a later return to the same image is a real transition:
// image_sys=fresh then gets a new sys.img instead of the same-digest keep,
// and a rollback (image_sys=restore) finds the archive. No sys.img, or one
// without a valid digest, is left alone. dir missing is not an error.
func ArchiveSys(dir string) (SysAction, error) {
	var a SysAction
	cur := SysDigest(dir)
	if !ValidDigest(cur) {
		return a, nil
	}
	if _, err := os.Stat(sysPath(dir)); err != nil {
		_ = os.Remove(sysDigestPath(dir)) // digest without a disk: stale record
		return a, nil
	}
	a.ArchivePath = SysArchive(dir, cur)
	// sys.img goes first: a crash between the two leaves a stale digest with
	// no disk, which PrepareSys ignores (it only trusts the digest when
	// sys.img exists).
	if err := os.Rename(sysPath(dir), a.ArchivePath); err != nil {
		return a, err
	}
	a.Archived = true
	return a, os.Remove(sysDigestPath(dir))
}

// MakeSys creates an empty ext4 sys.img (sparse) owned by root inside.
func MakeSys(p string, size int64) error {
	if size <= 0 {
		size = DefaultSysSize
	}
	if _, err := exec.LookPath("mke2fs"); err != nil {
		return errors.New("sys.img needs e2fsprogs on the host (mke2fs not found in PATH)")
	}
	tmp := p + ".part"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, nil, 0o644); err != nil {
		return err
	}
	if err := os.Truncate(tmp, size); err != nil {
		os.Remove(tmp)
		return err
	}
	if b, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-m", "0", "-L", SysLabel,
		"-E", "root_owner=0:0", tmp).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("mke2fs sys.img: %v: %s", err, b)
	}
	return os.Rename(tmp, p)
}

func writeAtomic(p, s string) error {
	tmp := p + ".part"
	if err := os.WriteFile(tmp, []byte(s), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// GroupDigests returns every image digest group dir pins: the given current
// and previous digests plus the base of each archived sys image, expanded
// to full digests via the catalog (an archive whose base is no longer in
// the catalog cannot be booted and pins nothing). Feed it to GroupRef.
func GroupDigests(c *Catalog, dir string, current, prev string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	add(current)
	add(prev)
	add(SysDigest(dir))
	for _, p := range SysArchives(dir) {
		for dg := range c.Images {
			if short12(dg) == p {
				add(dg)
			}
		}
	}
	return out
}

func sysRootPath(dir string) string { return filepath.Join(dir, "sys.root") }

// MarkSysRoot records that the group's image-mode system disks are written
// by a guest that has root (root=yes). The sys overlay persists across
// boots and is parked/restored across image changes, so anything a root
// worker planted there (sudoers rules, setuid binaries, edited units) would
// survive a later downgrade to root=no. The marker lets DropRootSys catch
// that downgrade. Idempotent.
func MarkSysRoot(dir string) error {
	if _, err := os.Stat(sysRootPath(dir)); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeAtomic(sysRootPath(dir), "yes\n")
}

// DropRootSys is called on an image-mode boot with root=no. If MarkSysRoot
// recorded a root=yes era, every system disk of that era is discarded:
// sys.img, sys.digest and all parked sys-*.img archives (a rollback must
// not bring a root-written overlay back either). The marker goes last, so
// a crash midway repeats the purge. Returns how many disks were removed;
// 0 with no marker. Fail closed: any removal error is returned and the
// caller refuses the boot.
func DropRootSys(dir string) (int, error) {
	if _, err := os.Stat(sysRootPath(dir)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	paths := []string{sysPath(dir)}
	for _, h := range SysArchives(dir) {
		paths = append(paths, filepath.Join(dir, "sys-"+h+".img"))
	}
	for _, p := range paths {
		err := os.Remove(p)
		if err == nil {
			n++
		} else if !errors.Is(err, os.ErrNotExist) {
			return n, err
		}
	}
	if err := os.Remove(sysDigestPath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return n, err
	}
	return n, os.Remove(sysRootPath(dir))
}
