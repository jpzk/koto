package main

// OCI image boot wiring (docs/images.md). A group whose config.json carries
// image=<catalog name> + image_digest=<pinned digest> boots that image as its
// root instead of the shared Fedora rootfs:
//
//	vda tools.img   ro, root device (fc-agent, claude, codex, /sidecar)
//	vdb workspace   rw, the group's existing workspace.img, untouched
//	vdc sys.img     rw, per group: overlay upper for the image root
//	vdd.. layers    ro, one ext4 per diff_id, bottom -> top
//
// The drive order is built FROM ociimage.FixedDevices, so the layer slot
// guard (ociimage.CheckLayerSlots) and the real device list cannot drift.
// Groups without image= never reach this file: fcVMConfigFor(nil) is the
// byte-identical develop config.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"koto/ociimage"
)

// fcImagePlan is everything one image boot needs, resolved on the host.
// Host paths here; the chroot-relative names come from fcImageJail*.
type fcImagePlan struct {
	Name    string
	Digest  string
	Created time.Time
	Tools   string   // host tools.img
	Sys     string   // host sys.img (rw, chowned to the jail uid)
	Layers  []string // host layer ext4 files, bottom -> top, deduped by diff_id
	DiffIDs []string // matching diff_ids
}

// Chroot-relative drive paths (bind targets staged by fcStageJailFor).
const (
	fcJailTools = "/a/tools.img"
	fcJailSys   = "/a/sys.img"
)

func fcJailLayer(i int) string { return fmt.Sprintf("/a/L%02d.ext4", i) }

// fcImagesDir is the host-wide image store: catalog.json, blobs/, layers/,
// tools/. Under the daemon state dir, so an isolated KOTO_HOME is isolated.
func fcImagesDir() string { return filepath.Join(HERE, "images") }

func fcImageStore() *ociimage.Store { return ociimage.NewStore(fcImagesDir()) }

// fcImageAgentEnv names an operator override for the fc-agent baked into
// tools.img (dev builds and e2e: boot a new guest agent without rebuilding
// the 2G rootfs). Unset in production.
const fcImageAgentEnv = "KOTO_IMAGE_AGENT"

// fcImageDedup keeps one entry per diff_id, at its TOPMOST position: a
// repeated layer re-applies its content above whatever came between.
func fcImageDedup(layers []ociimage.Layer) []ociimage.Layer {
	last := map[string]int{}
	for i, l := range layers {
		last[l.DiffID] = i
	}
	var out []ociimage.Layer
	for i, l := range layers {
		if last[l.DiffID] == i {
			out = append(out, l)
		}
	}
	return out
}

// fcImageBootArgs is the image-mode cmdline. The koto.* keys are the
// contract with fcguest/image.go (parseImageArgs): layers bottom -> top as
// the first 32 hex chars of each diff_id, catalog name, pinned digest,
// RFC3339 created (omitted when unknown).
func fcImageBootArgs(p *fcImagePlan) string {
	hs := make([]string, len(p.DiffIDs))
	for i, d := range p.DiffIDs {
		hs[i] = strings.TrimPrefix(d, "sha256:")[:32]
	}
	s := "console=ttyS0 reboot=k panic=1 pci=off quiet init=/bin/fc-agent" +
		" koto.layers=" + strings.Join(hs, ",") +
		" koto.image=" + p.Name + " koto.digest=" + p.Digest
	if !p.Created.IsZero() {
		s += " koto.created=" + p.Created.UTC().Format(time.RFC3339)
	}
	return s
}

// fcImageDrives builds the image-mode drive list in ociimage.FixedDevices
// order, then one read-only drive per layer. An entry it does not know how
// to wire is an error, so growing FixedDevices without wiring it here fails
// loudly (and the slot guard shrinks with it).
func fcImageDrives(p *fcImagePlan, wsPath string, rl func() map[string]any) ([]map[string]any, error) {
	if len(p.Layers) != len(p.DiffIDs) || len(p.Layers) == 0 {
		return nil, fmt.Errorf("image %s: bad plan (%d layers, %d diff_ids)", p.Name, len(p.Layers), len(p.DiffIDs))
	}
	if err := ociimage.CheckLayerSlots(len(p.Layers)); err != nil {
		return nil, fmt.Errorf("image %s: %w", p.Name, err)
	}
	var drives []map[string]any
	for _, d := range ociimage.FixedDevices {
		switch d {
		case "tools.img":
			drives = append(drives, map[string]any{"drive_id": "tools", "path_on_host": fcJailTools, "is_root_device": true, "is_read_only": true, "rate_limiter": rl()})
		case "workspace.img":
			drives = append(drives, map[string]any{"drive_id": "workspace", "path_on_host": wsPath, "is_root_device": false, "is_read_only": false, "rate_limiter": rl()})
		case "sys.img":
			drives = append(drives, map[string]any{"drive_id": "sys", "path_on_host": fcJailSys, "is_root_device": false, "is_read_only": false, "rate_limiter": rl()})
		case "vsock", "reserve":
			// not drives: vsock is its own config section; reserve is headroom
		default:
			return nil, fmt.Errorf("image boot: fixed device %q has no drive wiring", d)
		}
	}
	for i := range p.Layers {
		drives = append(drives, map[string]any{"drive_id": fmt.Sprintf("layer%02d", i), "path_on_host": fcJailLayer(i), "is_root_device": false, "is_read_only": true, "rate_limiter": rl()})
	}
	return drives, nil
}

// fcImageJailBinds are the image-mode binds replacing the rootfs bind.
func fcImageJailBinds(p *fcImagePlan) []fcBind {
	b := []fcBind{{Src: p.Tools, Dst: fcJailTools, RO: true}, {Src: p.Sys, Dst: fcJailSys}}
	for i, l := range p.Layers {
		b = append(b, fcBind{Src: l, Dst: fcJailLayer(i), RO: true})
	}
	return b
}

// fcImagePlanFor resolves group g's image boot. nil, nil = default rootfs
// (no image= or the built-in koto-default). Errors are readable and refuse
// the boot: a group pinned to an image the host cannot produce must not
// silently fall back to a different root.
func fcImagePlanFor(g string) (*fcImagePlan, error) {
	cfg := loadGroupConfig(g)
	name := cfg.str("image")
	if name == "" || name == ociimage.DefaultName {
		// Park a previous image's sys.img so that coming back to it later is a
		// real transition (fresh apply = new sys.img; rollback = restore).
		act, err := ociimage.ArchiveSys(vol(g))
		if err != nil {
			return nil, fmt.Errorf("group %s: archive sys.img: %w", g, err)
		}
		if act.Archived {
			emitLogfG("fc", g, "info", "[%s] image: default rootfs; sys.img %s", g, act)
		}
		return nil, nil
	}
	dg := cfg.str("image_digest")
	if !ociimage.ValidName(name) {
		return nil, fmt.Errorf("group %s: image=%q is not a catalog name", g, name)
	}
	if !ociimage.ValidDigest(dg) {
		return nil, fmt.Errorf("group %s: image=%s has no valid image_digest (%q); set it again with koto image apply", g, name, dg)
	}
	st := fcImageStore()
	cat, err := st.Load()
	if err != nil {
		return nil, fmt.Errorf("group %s: image catalog: %w", g, err)
	}
	e, ok := cat.ByDigest(dg)
	if !ok || e.Builtin {
		return nil, fmt.Errorf("group %s: image %s@%s is not in the catalog (koto image ls)", g, name, ociimage.Short(dg))
	}
	layers := fcImageDedup(e.Layers)
	if err := ociimage.CheckLayerSlots(len(layers)); err != nil {
		return nil, fmt.Errorf("group %s: image %s: %w", g, name, err)
	}
	logf := func(f string, a ...any) { emitLogfG("fc", g, "info", "[%s] image: "+f, append([]any{g}, a...)...) }
	// Layer files are shared and 0444; a missing one (gc'd, disk loss) is
	// rebuilt from its cached blob, or the boot is refused.
	if _, _, err := ociimage.EnsureLayers(st.Root, &ociimage.Image{Layers: layers}, logf); err != nil {
		return nil, fmt.Errorf("group %s: image %s layers: %w (re-run koto image add)", g, name, err)
	}
	tools, err := ociimage.RootfsTools{Rootfs: fcRootfsPath(), Agent: os.Getenv(fcImageAgentEnv)}.Ensure(st.Root, logf)
	if err != nil {
		return nil, fmt.Errorf("group %s: tools.img: %w", g, err)
	}
	mode := ociimage.SysFresh
	if cfg.str("image_sys") == "restore" {
		mode = ociimage.SysRestore
	}
	act, err := ociimage.PrepareSys(vol(g), dg, mode, 0)
	if err != nil {
		return nil, fmt.Errorf("group %s: sys.img: %w", g, err)
	}
	if !act.Kept {
		logf("sys.img for %s: %s", ociimage.Short(dg), act)
	}
	p := &fcImagePlan{Name: name, Digest: dg, Created: e.Created, Tools: tools, Sys: filepath.Join(vol(g), "sys.img")}
	// Same rule as workspace.img: the rw bind and the chown act on a PATH.
	if fi, err := os.Lstat(p.Sys); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("group %s: sys.img is not a regular file — refusing to use it as a VM disk", g)
	}
	for _, l := range layers {
		p.Layers = append(p.Layers, ociimage.LayerFile(st.Root, l.DiffID))
		p.DiffIDs = append(p.DiffIDs, l.DiffID)
	}
	logf("booting %s@%s (%d layers)", name, ociimage.Short(dg), len(layers))
	return p, nil
}
