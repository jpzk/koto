package ociimage

// tools.img: the read-only koto runtime every image group boots from
// (root device; the guest init bind-mounts it at /opt/koto inside the OCI
// root). It carries koto's own binaries only, never distro packages.
//
// Production is behind ToolsImage so the source can change without
// touching callers. Today (RootfsTools) it is DERIVED on the host from the
// shipped, already-verified fcassets rootfs.img via `debugfs dump` (no
// downloads, no root, no loop mounts). Follow-up: ship tools.img as its own
// release artifact and swap the implementation.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ToolsLabel is the ext4 label of tools.img; guest init checks it.
const ToolsLabel = "kototools"

// toolsLayout versions the file layout below; bump it when it changes so
// every host rebuilds.
const toolsLayout = "v1"

// ToolsImage produces (or reuses) tools.img under cache and returns its path.
type ToolsImage interface {
	Ensure(cache string, logf func(string, ...any)) (string, error)
}

// ToolFile maps a path inside tools.img to its source path inside rootfs.img.
type ToolFile struct {
	Dst, Src string
	Mode     os.FileMode
	Optional bool
}

// RootfsToolFiles is the v1 layout. /sidecar is copied whole.
var RootfsToolFiles = []ToolFile{
	{"/bin/fc-agent", "/usr/local/bin/fc-agent", 0o755, false},
	{"/bin/claude", "/usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe", 0o755, false},
	{"/bin/codex", "/usr/local/bin/codex", 0o755, false},
	{"/bin/codex-code-mode-host", "/usr/local/bin/codex-code-mode-host", 0o755, true},
	{"/etc/ssl/ca-bundle.crt", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", 0o644, false},
	{"/etc/codex/managed_config.toml", "/etc/codex/managed_config.toml", 0o644, true},
}

// ToolsSidecarDir is copied recursively from rootfs.img into tools.img.
const ToolsSidecarDir = "/sidecar"

// toolsMountpoints are empty dirs guest init mounts onto before pivoting.
var toolsMountpoints = []string{"/proc", "/dev", "/sys", "/run", "/tmp", "/mnt", "/newroot"}

// RootfsTools derives tools.img from a verified rootfs.img. Agent, when
// set, replaces /bin/fc-agent with a host file (dev builds / e2e).
type RootfsTools struct {
	Rootfs string
	Agent  string
}

var toolsMu sync.Mutex

// ToolsPath is where a tools image with key k lives.
func ToolsPath(cache, key string) string { return filepath.Join(cache, "tools", key+".img") }

// fileSha hashes p, memoised in memo by (size, mtime) so a 2 GiB rootfs is
// hashed once per change, not once per boot.
func fileSha(p, memo string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	stamp := fmt.Sprintf("%s %d %d", p, fi.Size(), fi.ModTime().UnixNano())
	if b, err := os.ReadFile(memo); err == nil {
		if s, h, ok := strings.Cut(strings.TrimSpace(string(b)), "\t"); ok && s == stamp && len(h) == 64 {
			return h, nil
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	_ = os.MkdirAll(filepath.Dir(memo), 0o755)
	_ = os.WriteFile(memo, []byte(stamp+"\t"+sum+"\n"), 0o644)
	return sum, nil
}

// Key identifies the tools image content: layout + rootfs sha (+ agent sha).
func (t RootfsTools) Key(cache string) (string, error) {
	rs, err := fileSha(t.Rootfs, filepath.Join(cache, "tools", ".rootfs.sha"))
	if err != nil {
		return "", fmt.Errorf("tools.img source: %w", err)
	}
	k := toolsLayout + "-" + rs[:16]
	if t.Agent != "" {
		as, err := fileSha(t.Agent, filepath.Join(cache, "tools", ".agent.sha"))
		if err != nil {
			return "", fmt.Errorf("tools.img agent override: %w", err)
		}
		k += "-a" + as[:12]
	}
	return k, nil
}

// Ensure builds tools/<key>.img if missing and returns its path.
func (t RootfsTools) Ensure(cache string, logf func(string, ...any)) (string, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	toolsMu.Lock()
	defer toolsMu.Unlock()
	key, err := t.Key(cache)
	if err != nil {
		return "", err
	}
	out := ToolsPath(cache, key)
	if fi, err := os.Stat(out); err == nil && fi.Mode().Perm()&0o222 == 0 {
		if lbl, _, err := ReadSuper(out); err == nil && lbl == ToolsLabel {
			return out, nil
		}
	}
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			return "", fmt.Errorf("tools.img needs e2fsprogs on the host (%s not found in PATH)", tool)
		}
	}
	t0 := time.Now()
	work := filepath.Join(cache, "work", "tools-"+key)
	_ = os.RemoveAll(work)
	stage := filepath.Join(work, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	type staged struct {
		dst, host string
		mode      os.FileMode
	}
	var files []staged
	var total int64
	for i, f := range RootfsToolFiles {
		host := filepath.Join(stage, fmt.Sprint(i))
		src := f.Src
		if f.Dst == "/bin/fc-agent" && t.Agent != "" {
			if err := copyFile(t.Agent, host); err != nil {
				return "", fmt.Errorf("agent override: %w", err)
			}
		} else if err := debugfsDump(t.Rootfs, src, host); err != nil {
			if f.Optional {
				logf("tools.img: optional %s not in rootfs, skipped", src)
				continue
			}
			return "", fmt.Errorf("tools.img: extract %s from %s: %w", src, t.Rootfs, err)
		}
		fi, _ := os.Stat(host)
		total += fi.Size()
		files = append(files, staged{f.Dst, host, f.Mode})
	}
	// /sidecar: whole directory (scripts + helper js), modes preserved.
	sc := filepath.Join(stage, "sidecar")
	req, err := debugfsReq("rdump", ToolsSidecarDir, stage)
	if err != nil {
		return "", fmt.Errorf("tools.img: rdump: %w", err)
	}
	if b, err := exec.Command("debugfs", "-R", req, t.Rootfs).CombinedOutput(); err != nil {
		return "", fmt.Errorf("tools.img: rdump %s: %v: %s", ToolsSidecarDir, err, b)
	}
	ents, err := os.ReadDir(sc)
	if err != nil || len(ents) == 0 {
		return "", fmt.Errorf("tools.img: %s missing or empty in %s", ToolsSidecarDir, t.Rootfs)
	}
	for _, de := range ents {
		fi, err := de.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue // flat dir today; anything else is not ours to copy
		}
		total += fi.Size()
		files = append(files, staged{ToolsSidecarDir + "/" + de.Name(), filepath.Join(sc, de.Name()), fi.Mode().Perm() | 0o444})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].dst < files[j].dst })

	sp := filepath.Join(work, "build.debugfs")
	sf, err := os.Create(sp)
	if err != nil {
		return "", err
	}
	s := &script{w: bufio.NewWriter(sf), nodes: map[string]*node{"/": {typ: 'd'}}, links: map[string]int{}}
	s.emit("rmdir /lost+found")
	for _, d := range toolsMountpoints {
		s.ensureDir(d)
	}
	for _, f := range files {
		s.ensureDir(filepath.Dir(f.dst))
		qh, err := q(f.host)
		if err != nil {
			sf.Close()
			return "", err
		}
		s.mk(f.dst, 'f', "write %[2]s %[1]s", qh)
		s.meta(f.dst, 0, 0, int64(0o100000|f.mode), time.Unix(1700000000, 0))
	}
	if s.err == nil {
		s.err = s.w.Flush()
	}
	sf.Close()
	if s.err != nil {
		return "", s.err
	}
	sizeMB := (total*11/10)>>20 + 16
	img := out + ".part"
	_ = os.MkdirAll(filepath.Dir(out), 0o755)
	_ = os.Remove(img)
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		return "", err
	}
	if err := os.Truncate(img, sizeMB<<20); err != nil {
		return "", err
	}
	env := append(os.Environ(), "E2FSPROGS_FAKE_TIME="+fakeTime)
	mk := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-b", "4096", "-m", "0", "-L", ToolsLabel,
		"-O", "^has_journal", "-E", "root_owner=0:0", img)
	mk.Env = env
	if b, err := mk.CombinedOutput(); err != nil {
		os.Remove(img)
		return "", fmt.Errorf("mke2fs: %v: %s", err, b)
	}
	if err := runDebugfs(sp, img, env); err != nil {
		os.Remove(img)
		return "", err
	}
	if b, err := exec.Command("e2fsck", "-fn", img).CombinedOutput(); err != nil {
		os.Remove(img)
		return "", fmt.Errorf("e2fsck: %v\n%s", err, b)
	}
	if err := os.Chmod(img, 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(img, out); err != nil {
		return "", err
	}
	logf("tools.img %s built (%d files, %d MiB) in %s", key, len(files), sizeMB, time.Since(t0).Round(time.Millisecond))
	return out, nil
}

// debugfsReq builds one debugfs -R request: cmd followed by every argument
// through q. A path that cannot be quoted is an error, never a silently
// split or truncated request.
func debugfsReq(cmd string, args ...string) (string, error) {
	parts := []string{cmd}
	for _, a := range args {
		qa, err := q(a)
		if err != nil {
			return "", err
		}
		parts = append(parts, qa)
	}
	return strings.Join(parts, " "), nil
}

func debugfsDump(img, src, dst string) error {
	req, err := debugfsReq("dump", src, dst)
	if err != nil {
		return err
	}
	b, err := exec.Command("debugfs", "-R", req, img).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, b)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 {
		return errors.New("not found or empty")
	}
	return nil
}

func runDebugfs(scriptPath, img string, env []string) error {
	db := exec.Command("debugfs", "-w", "-f", scriptPath, img)
	db.Env = env
	var stderr strings.Builder
	db.Stdout = io.Discard
	db.Stderr = &stderr
	if err := db.Run(); err != nil {
		return fmt.Errorf("debugfs: %v", err)
	}
	var bad []string
	for _, ln := range strings.Split(stderr.String(), "\n") {
		if ln != "" && !strings.HasPrefix(ln, "debugfs ") {
			bad = append(bad, ln)
		}
	}
	if len(bad) > 0 {
		if len(bad) > 10 {
			bad = bad[:10]
		}
		return fmt.Errorf("debugfs reported errors:\n  %s", strings.Join(bad, "\n  "))
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
