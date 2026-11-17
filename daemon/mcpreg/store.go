package mcpreg

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// maxFile bounds the registry file so a corrupted/hostile file cannot make
// the daemon allocate unboundedly.
const maxFile = 4 << 20

// Store owns the registry file. Current() re-stats the file and reloads on
// change, so an operator edit (or a revoke written by another daemon
// process) takes effect on the next request. A failed reload keeps the last
// good registry and surfaces the error via LastError.
type Store struct {
	path string

	mu      sync.Mutex
	cur     *Registry
	stamp   fileStamp
	lastErr error
}

type fileStamp struct {
	ino   uint64
	size  int64
	mtime time.Time
}

// Open loads path (a missing file is an empty registry). A load error on the
// first open is returned: there is no last-good to fall back on, and the
// caller must not run with an unknown policy.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	r, st, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.cur = New()
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.cur, s.stamp = r, st
	return s, nil
}

// Path is the backing file.
func (s *Store) Path() string { return s.path }

// LastError is the most recent reload failure (nil after a good reload).
func (s *Store) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Current returns the live registry, reloading if the file changed. The
// returned value must be treated as read-only.
func (s *Store) Current() *Registry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return s.cur
}

func (s *Store) refreshLocked() {
	st, err := stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		// a removed file means "no connections", which is deny-all; that is
		// the safe direction, so it is not treated as an error.
		if s.stamp != (fileStamp{}) {
			s.cur, s.stamp, s.lastErr = New(), fileStamp{}, nil
		}
		return
	}
	if err == nil && st == s.stamp {
		return
	}
	r, st2, err := readFile(s.path)
	if err != nil {
		s.lastErr = err // keep last good
		return
	}
	s.cur, s.stamp, s.lastErr = r, st2, nil
}

// Update applies fn to a copy of the current registry, validates it and
// persists atomically. On any error nothing changes.
func (s *Store) Update(fn func(*Registry) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if s.lastErr != nil {
		return fmt.Errorf("mcp registry on disk is invalid, fix it first: %w", s.lastErr)
	}
	next := s.cur.Clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	b, err := next.Marshal()
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, b); err != nil {
		return err
	}
	st, err := stat(s.path)
	if err != nil {
		return err
	}
	s.cur, s.stamp = next, st
	return nil
}

func stat(path string) (fileStamp, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return fileStamp{}, err
	}
	var ino uint64
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = sys.Ino
	}
	return fileStamp{ino: ino, size: fi.Size(), mtime: fi.ModTime()}, nil
}

// readFile opens with O_NOFOLLOW and checks the OPENED file (fstat), so a
// swap between check and open cannot slip a symlink or a world-readable file
// past the loader.
func readFile(path string) (*Registry, fileStamp, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fileStamp{}, fmt.Errorf("mcp registry %s: refusing symlink", path)
		}
		return nil, fileStamp{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fileStamp{}, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fileStamp{}, fmt.Errorf("mcp registry %s: not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fileStamp{}, fmt.Errorf("mcp registry %s: mode %04o is group/world accessible, want 0600", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, fileStamp{}, err
	}
	if len(b) > maxFile {
		return nil, fileStamp{}, fmt.Errorf("mcp registry %s: larger than %d bytes", path, maxFile)
	}
	r, err := Parse(b)
	if err != nil {
		return nil, fileStamp{}, err
	}
	var ino uint64
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = sys.Ino
	}
	return r, fileStamp{ino: ino, size: fi.Size(), mtime: fi.ModTime()}, nil
}

// writeAtomic: 0700 parent (not a symlink), temp O_EXCL|O_NOFOLLOW 0600,
// fsync, rename over, fsync dir.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(dir); err != nil {
		return err
	} else if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("mcp registry dir %s: not a real directory", dir)
	}
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("mcp registry %s: refusing to replace a non-regular file", path)
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // umask-proof
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
