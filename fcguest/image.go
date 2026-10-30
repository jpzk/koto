package main

// OCI image boot (docs/images.md). A group with image=<name> boots with
// tools.img as the read-only root device and this agent as init; the kernel
// cmdline names the layer stack:
//
//	koto.layers=<hex32>,<hex32>,...   bottom -> top (overlay order), the first
//	                                  32 hex chars of each layer's diff_id
//	koto.image=<catalog name>  koto.digest=sha256:<hex64>  koto.created=<RFC3339>
//
// Drives, in the order the daemon builds them from ociimage.FixedDevices:
// vda tools (label kototools, this root), vdb workspace (unchanged: mounted
// by mountWorkspace as on every other group), vdc sys (label kotosys, the
// overlay upper) and vdd.. one read-only ext4 per layer. Layers are matched
// to the cmdline BY SUPERBLOCK (label "L"+hex[:15] AND uuid = hex[:32]),
// never by device order, so a reordered or substituted drive fails the boot
// instead of silently stacking the wrong filesystem.
//
// imageRoot then stacks overlay lowerdir=top:..:bottom with upper/work on
// sys, pivots into it with tools landing at /opt/koto, and injects what the
// agent needs from any base (uid 1000, resolv.conf, hostname, CA bundle,
// /sidecar, /etc/koto/image). Without koto.layers= none of this runs and the
// default rootfs boot path is byte-for-byte what it was.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	toolsLabel = "kototools" // ociimage.ToolsLabel
	sysLabel   = "kotosys"   // ociimage.SysLabel
	toolsDev   = "/dev/vda"
	toolsDir   = "/opt/koto"
	// maxImageLayers bounds what the guest accepts from the cmdline. The
	// host's slot guard (ociimage.CheckLayerSlots) is the real limit; this
	// only keeps a malformed cmdline from driving unbounded work here.
	maxImageLayers = 16
	// provisionMarker lives in the sys upper: a fresh sys.img (image apply)
	// has none, so provision.sh runs once per base; a rollback restores the
	// archived sys.img together with its marker.
	provisionMarker = "/var/lib/koto/provisioned"
	provisionScript = "/workspace/.koto/provision.sh"
	provisionLog    = "/var/log/koto-provision.log"
	provisionMax    = 30 * time.Minute
	// provisionRunning exists (tmpfs /run) while provision.sh runs.
	provisionRunning = "/run/koto-provisioning"
)

// imageMode is set once imageRoot has pivoted; read-only afterwards.
var imageMode *imageArgs

// workerPATH is the worker's PATH. Image mode adds the tools bin dir.
var workerPATH = "/sidecar:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

type imageArgs struct {
	Layers  []string // bottom -> top, 32 lowercase hex each
	Name    string
	Digest  string
	Created string
}

var (
	reHex32   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reName    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	reDigest  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reCreated = regexp.MustCompile(`^[0-9A-Za-z:.+-]{1,40}$`)
)

// parseImageArgs reads the koto.* image keys from a kernel cmdline. It
// returns nil, nil when there is no koto.layers= (default rootfs boot).
func parseImageArgs(cmdline string) (*imageArgs, error) {
	kv := map[string]string{}
	for _, f := range strings.Fields(cmdline) {
		k, v, ok := strings.Cut(f, "=")
		if !ok || !strings.HasPrefix(k, "koto.") {
			continue
		}
		if _, dup := kv[k]; dup {
			return nil, fmt.Errorf("cmdline: %s given twice", k)
		}
		kv[k] = v
	}
	raw, ok := kv["koto.layers"]
	if !ok {
		return nil, nil
	}
	a := &imageArgs{Name: kv["koto.image"], Digest: kv["koto.digest"], Created: kv["koto.created"]}
	if raw == "" {
		return nil, errors.New("cmdline: koto.layers= is empty")
	}
	seen := map[string]bool{}
	for i, l := range strings.Split(raw, ",") {
		if !reHex32.MatchString(l) {
			return nil, fmt.Errorf("cmdline: layer %d %q is not 32 lowercase hex chars", i+1, l)
		}
		if seen[l] {
			// An image may legitimately repeat a diff_id (empty layers), but
			// the host dedups drives per diff_id and must dedup the cmdline
			// too: one drive cannot be two lowerdirs' worth of verification.
			return nil, fmt.Errorf("cmdline: layer %s listed twice", l)
		}
		seen[l] = true
		a.Layers = append(a.Layers, l)
	}
	if len(a.Layers) > maxImageLayers {
		return nil, fmt.Errorf("cmdline: %d layers, guest accepts at most %d", len(a.Layers), maxImageLayers)
	}
	if !reName.MatchString(a.Name) {
		return nil, fmt.Errorf("cmdline: koto.image=%q is not a catalog name", a.Name)
	}
	if !reDigest.MatchString(a.Digest) {
		return nil, fmt.Errorf("cmdline: koto.digest=%q is not sha256:<64 hex>", a.Digest)
	}
	if a.Created != "" && !reCreated.MatchString(a.Created) {
		return nil, fmt.Errorf("cmdline: koto.created=%q malformed", a.Created)
	}
	return a, nil
}

func layerLabel(h string) string { return "L" + h[:15] }
func layerUUID(h string) string {
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

type superblock struct{ dev, label, uuid string }

// readSuperblock reads an ext4 superblock's label and uuid straight off the
// device (no blkid in tools.img).
func readSuperblock(dev string) (superblock, error) {
	f, err := os.Open(dev)
	if err != nil {
		return superblock{}, err
	}
	defer f.Close()
	b := make([]byte, 1024)
	if _, err := f.ReadAt(b, 1024); err != nil {
		return superblock{}, err
	}
	return parseSuperblock(dev, b)
}

func parseSuperblock(dev string, b []byte) (superblock, error) {
	if len(b) < 0x88 || b[0x38] != 0x53 || b[0x39] != 0xEF {
		return superblock{}, errors.New("no ext4 magic")
	}
	u := b[0x68:0x78]
	return superblock{dev, strings.TrimRight(string(b[0x78:0x88]), "\x00"),
		fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])}, nil
}

// matchLayers maps each wanted layer (bottom -> top) to exactly one drive
// whose label AND uuid both derive from that layer's diff_id. Device order
// is irrelevant. A label hit with the wrong uuid, a layer on two drives or a
// missing layer all refuse the boot.
func matchLayers(want []string, drives []superblock) ([]string, error) {
	devs := make([]string, len(want))
	for i, h := range want {
		lbl, uuid := layerLabel(h), layerUUID(h)
		var hit []superblock
		for _, d := range drives {
			if d.label == lbl {
				hit = append(hit, d)
			}
		}
		switch {
		case len(hit) == 0:
			return nil, fmt.Errorf("layer %d/%d (%s): no drive carries label %s", i+1, len(want), h, lbl)
		case len(hit) > 1:
			return nil, fmt.Errorf("layer %d/%d (%s): label %s on %d drives", i+1, len(want), h, lbl, len(hit))
		case hit[0].uuid != uuid:
			return nil, fmt.Errorf("layer %d/%d: %s has label %s but uuid %s, want %s (substituted drive?)",
				i+1, len(want), hit[0].dev, lbl, hit[0].uuid, uuid)
		}
		devs[i] = hit[0].dev
	}
	return devs, nil
}

// lowerdirOpt builds overlay's lowerdir: it lists the TOP layer first.
func lowerdirOpt(bottomToTop []string) string {
	r := make([]string, len(bottomToTop))
	for i, d := range bottomToTop {
		r[len(bottomToTop)-1-i] = d
	}
	return "lowerdir=" + strings.Join(r, ":")
}

func byLabel(drives []superblock, label string) []superblock {
	var out []superblock
	for _, d := range drives {
		if d.label == label {
			out = append(out, d)
		}
	}
	return out
}

// imageRoot assembles the image root and pivots into it. Called as PID 1
// before earlyInit; on return / is the overlay and tools is at /opt/koto.
func imageRoot(a *imageArgs) error {
	t0 := time.Now()
	mnt := func(src, dst, typ string, flags uintptr, data string) error {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(src, dst, typ, flags, data); err != nil && err != unix.EBUSY {
			return fmt.Errorf("mount %s on %s (%s): %w", src, dst, typ, err)
		}
		return nil
	}
	// tools.img is read-only: these mountpoints are baked into it.
	if _, err := os.Stat("/proc/self"); err != nil {
		if err := mnt("proc", "/proc", "proc", 0, ""); err != nil {
			return err
		}
	}
	if err := mnt("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil {
		return err
	}
	if err := mnt("tmpfs", "/mnt", "tmpfs", 0, "mode=0700,size=1m"); err != nil {
		return err
	}
	devs, _ := filepath.Glob("/dev/vd[a-z]*")
	var drives []superblock
	for _, d := range devs {
		if d == wsDev {
			continue // the workspace is never inspected by image boot
		}
		s, err := readSuperblock(d)
		if err != nil {
			logf("image: drive %s: %v", d, err)
			continue
		}
		drives = append(drives, s)
	}
	var tools *superblock
	for i := range drives {
		if drives[i].dev == toolsDev {
			tools = &drives[i]
		}
	}
	if tools == nil || tools.label != toolsLabel {
		return fmt.Errorf("root drive %s is not tools.img (label %v, want %s)", toolsDev, tools, toolsLabel)
	}
	sys := byLabel(drives, sysLabel)
	if len(sys) != 1 {
		return fmt.Errorf("need exactly one %s drive, found %d", sysLabel, len(sys))
	}
	layerDevs, err := matchLayers(a.Layers, drives)
	if err != nil {
		return err
	}
	var lowers []string
	for i, d := range layerDevs {
		dst := fmt.Sprintf("/mnt/l/%02d", i)
		if err := mnt(d, dst, "ext4", unix.MS_RDONLY, "noload"); err != nil {
			return fmt.Errorf("layer %d/%d: %w", i+1, len(layerDevs), err)
		}
		lowers = append(lowers, dst)
	}
	if err := mnt(sys[0].dev, "/mnt/sys", "ext4", 0, ""); err != nil {
		return fmt.Errorf("sys: %w", err)
	}
	for _, d := range []string{"/mnt/sys/upper", "/mnt/sys/work"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("sys: %w", err)
		}
	}
	if err := mnt("overlay", "/newroot", "overlay", 0,
		lowerdirOpt(lowers)+",upperdir=/mnt/sys/upper,workdir=/mnt/sys/work"); err != nil {
		return err
	}
	if err := os.MkdirAll("/newroot"+toolsDir, 0o755); err != nil {
		return err
	}
	if err := os.Chdir("/newroot"); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", strings.TrimPrefix(toolsDir, "/")); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	_ = os.Chdir("/")
	// The overlay holds its own references to the layer and sys mounts;
	// drop the helper mounts from the old root so /opt/koto is just tools.
	for _, l := range lowers {
		_ = unix.Unmount(toolsDir+l, unix.MNT_DETACH)
	}
	for _, m := range []string{"/mnt/sys", "/mnt", "/proc", "/dev"} {
		_ = unix.Unmount(toolsDir+m, unix.MNT_DETACH)
	}
	_ = unix.Mount("", toolsDir, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, "")
	if err := injectImage("/", a); err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	imageMode = a
	workerPATH = "/sidecar:" + toolsDir + "/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	_ = os.Setenv("PATH", toolsDir+"/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	logf("image %s@%s: %d layers + sys %s, root ready in %s", a.Name, shortDigest(a.Digest),
		len(lowers), sys[0].dev, time.Since(t0).Round(time.Millisecond))
	return nil
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// caBundles are the paths common bases read their trust store from.
var caBundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"}

// injectImage writes what the agent needs into the image root (all of it
// lands in the sys upper, never in a layer). root is "/" in the guest and a
// temp dir in tests.
func injectImage(root string, a *imageArgs) error {
	p := func(s string) string { return filepath.Join(root, s) }
	for _, d := range []string{"/etc", "/etc/koto", "/var/lib/koto", "/var/log", "/workspace"} {
		if err := os.MkdirAll(p(d), 0o755); err != nil {
			return err
		}
	}
	if err := ensureWorker(root); err != nil {
		return err
	}
	// net.go writes /run/resolv.conf (tmpfs); make the image read it there.
	_ = os.RemoveAll(p("/etc/resolv.conf"))
	if err := os.Symlink("/run/resolv.conf", p("/etc/resolv.conf")); err != nil {
		return err
	}
	if err := os.WriteFile(p("/etc/hostname"), []byte("koto-vm\n"), 0o644); err != nil {
		return err
	}
	hosts, _ := os.ReadFile(p("/etc/hosts"))
	if !strings.Contains(string(hosts), "koto-vm") {
		if len(hosts) > 0 && hosts[len(hosts)-1] != '\n' {
			hosts = append(hosts, '\n')
		}
		hosts = append(hosts, "127.0.0.1 localhost koto-vm\n"...)
		if err := os.WriteFile(p("/etc/hosts"), hosts, 0o644); err != nil {
			return err
		}
	}
	hasCA := false
	for _, c := range caBundles {
		if fi, err := os.Stat(p(c)); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			hasCA = true
			break
		}
	}
	if !hasCA {
		if err := os.MkdirAll(p("/etc/ssl/certs"), 0o755); err != nil {
			return err
		}
		_ = os.Remove(p(caBundles[0]))
		if err := os.Symlink(toolsDir+"/etc/ssl/ca-bundle.crt", p(caBundles[0])); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(p("/etc/codex/managed_config.toml")); errors.Is(err, os.ErrNotExist) {
		_ = os.MkdirAll(p("/etc/codex"), 0o755)
		_ = os.Symlink(toolsDir+"/etc/codex/managed_config.toml", p("/etc/codex/managed_config.toml"))
	}
	// /sidecar is on every worker PATH and referenced by absolute path.
	_ = os.RemoveAll(p("/sidecar"))
	if err := os.Symlink(toolsDir+"/sidecar", p("/sidecar")); err != nil {
		return err
	}
	_ = os.Remove(p("/etc/koto/image"))
	return os.WriteFile(p("/etc/koto/image"), []byte(etcKotoImage(a)), 0o444)
}

func etcKotoImage(a *imageArgs) string {
	return "name=" + a.Name + "\ndigest=" + a.Digest + "\ncreated=" + a.Created + "\n"
}

// ensureWorker makes sure uid/gid 1000 exist. Every koto runtime runs the
// worker as uid 1000 named "node" with HOME=/workspace; an image that already
// has uid 1000 keeps its own name.
func ensureWorker(root string) error {
	pw := filepath.Join(root, "etc/passwd")
	gr := filepath.Join(root, "etc/group")
	uid := strconv.Itoa(workerUID)
	if !hasID(pw, uid) {
		if !hasID(pw, "0") {
			if err := appendLine(pw, "root:x:0:0:root:/root:/bin/sh"); err != nil {
				return err
			}
		}
		if err := appendLine(pw, fmt.Sprintf("node:x:%d:%d:koto worker:%s:/bin/sh", workerUID, workerGID, wsDir)); err != nil {
			return err
		}
		if fi, err := os.Stat(filepath.Join(root, "etc/shadow")); err == nil && fi.Mode().IsRegular() {
			if err := appendLine(filepath.Join(root, "etc/shadow"), "node:!:20000::::::"); err != nil {
				return err
			}
		}
	}
	if !hasID(gr, strconv.Itoa(workerGID)) {
		if !hasID(gr, "0") {
			if err := appendLine(gr, "root:x:0:"); err != nil {
				return err
			}
		}
		if err := appendLine(gr, fmt.Sprintf("node:x:%d:", workerGID)); err != nil {
			return err
		}
	}
	return nil
}

// hasID reports whether a passwd/group style file has an entry with field 3
// (the numeric id) equal to id.
func hasID(path, id string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) > 2 && f[2] == id {
			return true
		}
	}
	return false
}

func appendLine(path, line string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	b = append(b, line+"\n"...)
	return os.WriteFile(path, b, 0o644)
}

// provisionOnce runs /workspace/.koto/provision.sh the first time a base
// boots (marker absent in the sys upper). It is started from handleInit, after
// the network is up and the root posture is applied, so a script that needs
// apt/dnf has a route. The script is worker-writable, so it runs with the
// group's posture: as root only when the group has root=yes, as the worker
// (uid/gid 1000) otherwise — root=no must not be bypassable by planting a
// script. Runs in the background so a slow script never holds the agent's RPC
// listener back; /run/koto-provisioning exists while it runs so work that
// depends on it can wait. Output goes to the sys upper (root-owned), not to
// the worker-writable workspace. Only a run that exits 0 (or a base without a
// script) writes the marker; a failed, refused or killed run retries on the
// next boot.
func provisionOnce(root bool) {
	if imageMode == nil {
		return
	}
	if _, err := os.Stat(provisionMarker); err == nil {
		return
	}
	fi, err := os.Lstat(provisionScript)
	if errors.Is(err, os.ErrNotExist) {
		writeMarker("none")
		return
	}
	if err != nil || !fi.Mode().IsRegular() {
		logf("provision: %s is not a regular file (%v), not running it", provisionScript, err)
		return
	}
	_ = os.WriteFile(provisionRunning, nil, 0o644)
	go func() {
		defer os.Remove(provisionRunning)
		t0 := time.Now()
		_ = os.MkdirAll(filepath.Dir(provisionLog), 0o755)
		lf, err := os.OpenFile(provisionLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|unix.O_NOFOLLOW, 0o644)
		if err != nil {
			logf("provision: log: %v", err)
			return
		}
		defer lf.Close()
		_ = lf.Chmod(0o644) // root-owned; readable so a root=no worker sees its output
		cmd := provisionCmd(root)
		if cmd.Dir == "/root" {
			_ = os.MkdirAll("/root", 0o700)
		}
		cmd.Stdout, cmd.Stderr = lf, lf
		who := provisionWho(cmd)
		fmt.Fprintf(lf, "# koto: running %s as %s\n", provisionScript, who)
		pid, ch, err := startTracked(cmd)
		if err != nil {
			logf("provision: start as %s: %v", who, err)
			return
		}
		logf("provision: running %s as %s (pid %d)", provisionScript, who, pid)
		var rc int
		select {
		case ws := <-ch:
			rc = ws.ExitStatus()
		case <-time.After(provisionMax):
			_ = unix.Kill(-pid, unix.SIGKILL)
			_ = unix.Kill(pid, unix.SIGKILL)
			<-ch
			rc = -1
			logf("provision: killed after %s", provisionMax)
		}
		logf("provision: rc=%d as %s in %s", rc, who, time.Since(t0).Round(time.Millisecond))
		if rc != 0 {
			logf("provision: not marking %s done; it runs again on the next boot", imageMode.Digest)
			return
		}
		writeMarker("rc=0 user=" + who)
	}()
}

// provisionCmd builds the provision.sh command for the group's root posture:
// root=yes → root (HOME=/root), otherwise the worker uid/gid 1000 with
// supplementary groups dropped (HOME=/workspace).
func provisionCmd(root bool) *exec.Cmd {
	cmd := exec.Command("/bin/sh", provisionScript)
	path := "PATH=" + toolsDir + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	var name, digest string
	if imageMode != nil {
		name, digest = imageMode.Name, imageMode.Digest
	}
	img := []string{"LANG=C.UTF-8", "KOTO_IMAGE=" + name, "KOTO_IMAGE_DIGEST=" + digest}
	if root {
		cmd.Dir = "/root"
		cmd.Env = append([]string{path, "HOME=/root", "USER=root", "LOGNAME=root"}, img...)
		return cmd
	}
	cmd.Dir = wsDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: workerUID, Gid: workerGID, Groups: []uint32{}},
	}
	cmd.Env = append([]string{path, "HOME=" + wsDir, "USER=node", "LOGNAME=node"}, img...)
	return cmd
}

func provisionWho(cmd *exec.Cmd) string {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
		return fmt.Sprintf("uid %d", cmd.SysProcAttr.Credential.Uid)
	}
	return "root"
}

func writeMarker(state string) {
	_ = os.MkdirAll(filepath.Dir(provisionMarker), 0o755)
	_ = os.WriteFile(provisionMarker, []byte(fmt.Sprintf("digest=%s\nstate=%s\nat=%s\n",
		imageMode.Digest, state, time.Now().UTC().Format(time.RFC3339))), 0o644)
}

// bootImage runs imageRoot when the cmdline asks for an image. A broken
// image boot powers the VM off (reboot=k makes firecracker exit) so the
// daemon fails the start fast with the console line, instead of a hung VM.
func bootImage() {
	mounted := false
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		// /proc is not mounted yet on the tools root; imageRoot mounts it,
		// but the cmdline decides whether we are in image mode at all.
		_ = os.MkdirAll("/proc", 0o755)
		_ = unix.Mount("proc", "/proc", "proc", 0, "")
		mounted = true
		b, err = os.ReadFile("/proc/cmdline")
	}
	if err != nil {
		return // default rootfs boot: earlyInit mounts /proc
	}
	a, err := parseImageArgs(string(b))
	if a == nil && err == nil {
		if mounted {
			// default boot: leave /proc exactly as earlyInit expects it
			_ = unix.Unmount("/proc", 0)
		}
		return
	}
	if err == nil {
		err = imageRoot(a)
	}
	if err != nil {
		logf("FATAL image boot: %v", err)
		unix.Sync()
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
		select {}
	}
}

const (
	imageSudoers   = "/etc/sudoers.d/node"
	imageSudoGrant = "#1000 ALL=(ALL) NOPASSWD: ALL\n"
)

// revokeImageSudo removes koto's root=yes grant from an image root. The sys
// upper persists, so without this a group moved from root=yes to root=no kept
// passwordless sudo. Only a file that is exactly koto's grant is removed.
func revokeImageSudo(path string) {
	b, err := os.ReadFile(path)
	if err != nil || string(b) != imageSudoGrant {
		return
	}
	if err := os.Remove(path); err != nil {
		logf("root=no: remove %s: %v", path, err)
		return
	}
	logf("root=no: removed koto sudo grant %s", path)
}
