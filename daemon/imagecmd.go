package main

// `koto image` (docs/images.md): the operator's image catalog verbs, served
// by the admin-only Image rpc. Output is streamed as text lines so a long
// pull shows progress. Group moves go through setImageConfig (config_image.go)
// and take effect at the group's next boot (a running group is restarted);
// the workspace disk is never touched by any of them.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"koto-protocol/pb"
	"koto/ociimage"
)

// imageEnv is what imageRun needs from the daemon; tests swap it.
type imageEnv struct {
	store   *ociimage.Store
	client  *ociimage.Client
	groups  func() []string
	running func(g string) bool
	restart func(g string) error
	tools   func() string // basename of the tools image in use ("" = unknown)
	now     func() time.Time
}

func defaultImageEnv(ctx context.Context) imageEnv {
	return imageEnv{
		store:  fcImageStore(),
		client: &ociimage.Client{Ctx: ctx},
		groups: func() []string {
			var out []string
			for g := range readGroups() {
				out = append(out, g)
			}
			sort.Strings(out)
			return out
		},
		running: fcRunning,
		restart: func(g string) error { _, err := restartCtx(ctx, g); return err },
		tools: func() string {
			k, err := ociimage.RootfsTools{Rootfs: fcRootfsPath(), Agent: os.Getenv(fcImageAgentEnv)}.Key(fcImagesDir())
			if err != nil {
				return ""
			}
			return k + ".img"
		},
		now: time.Now,
	}
}

func (s *kotoServer) Image(r *pb.ImageReq, stream pb.Koto_ImageServer) error {
	out := func(f string, a ...any) {
		_ = stream.Send(&pb.ImageEvent{Event: "data", Data: fmt.Sprintf(f, a...) + "\n"})
	}
	emitLogf("image", "info", "image %s %s", r.Op, strings.Join(r.Args, " "))
	if err := imageRun(defaultImageEnv(stream.Context()), r.Op, r.Args, r.All, out); err != nil {
		emitLogf("image", "warn", "image %s: %v", r.Op, err)
		return stream.Send(&pb.ImageEvent{Event: "error", Error: err.Error()})
	}
	return stream.Send(&pb.ImageEvent{Event: "end"})
}

// imageRefs builds the in-use view from every group's config and sys disks.
func imageRefs(env imageEnv, c *ociimage.Catalog) []ociimage.GroupRef {
	var refs []ociimage.GroupRef
	for _, g := range env.groups() {
		cfg := loadGroupConfig(g)
		prev := cfg.str(cfgImagePrev)
		if prev == ociimage.DefaultName {
			prev = ""
		}
		if d := ociimage.GroupDigests(c, vol(g), cfg.str(cfgImageDigest), prev); len(d) > 0 {
			refs = append(refs, ociimage.GroupRef{Group: g, Digests: d})
		}
	}
	return refs
}

func imageArgs(op string, args []string, n int, usage string) error {
	if len(args) != n {
		return fmt.Errorf("usage: koto image %s %s", op, usage)
	}
	return nil
}

func fmtSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func imageRun(env imageEnv, op string, args []string, all bool, out func(string, ...any)) error {
	switch op {
	case "add":
		if err := imageArgs(op, args, 2, "<name> <ref>"); err != nil {
			return err
		}
		return imageFetch(env, args[0], args[1], false, out)
	case "update":
		if len(args) < 1 || len(args) > 2 {
			return fmt.Errorf("usage: koto image update <name> [<ref>]")
		}
		ref := ""
		if len(args) == 2 {
			ref = args[1]
		}
		return imageFetch(env, args[0], ref, true, out)
	case "ls":
		return imageLs(env, out)
	case "inspect":
		if err := imageArgs(op, args, 1, "<name>"); err != nil {
			return err
		}
		return imageInspect(env, args[0], out)
	case "rm":
		if err := imageArgs(op, args, 1, "<name>"); err != nil {
			return err
		}
		return env.store.Update(func(c *ociimage.Catalog) error {
			dg := c.Names[args[0]]
			if err := c.Remove(args[0], imageRefs(env, c)); err != nil {
				return err
			}
			out("removed %s (%s); run koto image gc to free its files", args[0], ociimage.Short(dg))
			return nil
		})
	case "gc":
		if err := imageArgs(op, args, 0, ""); err != nil {
			return err
		}
		c, err := env.store.Load()
		if err != nil {
			return err
		}
		var keep []string
		if t := env.tools(); t != "" {
			keep = append(keep, t)
		}
		rep, err := env.store.GC(imageRefs(env, c), keep...)
		if err != nil {
			return err
		}
		for _, d := range rep.Entries {
			out("dropped unreferenced image %s", ociimage.Short(d))
		}
		out("gc: removed %d file(s), %s freed", len(rep.Files), fmtSize(rep.Bytes))
		return nil
	case "apply":
		return imageApply(env, args, all, out)
	case "rollback":
		if err := imageArgs(op, args, 1, "<group>"); err != nil {
			return err
		}
		return imageRollback(env, args[0], out)
	case "":
		return errors.New("usage: koto image add|ls|inspect|rm|gc|update|apply|rollback")
	}
	return fmt.Errorf("unknown image op %q (add|ls|inspect|rm|gc|update|apply|rollback)", op)
}

// imageFetch is add (new name) and update (existing name, moves it). The
// reference is resolved to a digest exactly once, here; groups are untouched.
func imageFetch(env imageEnv, name, refStr string, update bool, out func(string, ...any)) error {
	if name == ociimage.DefaultName {
		return fmt.Errorf("%q is the built-in image", name)
	}
	if !ociimage.ValidName(name) {
		return fmt.Errorf("invalid image name %q (want [a-z0-9][a-z0-9._-]{0,63})", name)
	}
	c, err := env.store.Load()
	if err != nil {
		return err
	}
	oldDg, exists := c.Names[name]
	switch {
	case !update && exists:
		return fmt.Errorf("image %q already exists (%s); use koto image update %s [<ref>]", name, ociimage.Short(oldDg), name)
	case update && !exists:
		return fmt.Errorf("no image named %q (koto image add %s <ref>)", name, name)
	case update && refStr == "":
		refStr = c.Sources[name]
		if refStr == "" {
			return fmt.Errorf("image %q has no recorded source reference; pass one: koto image update %s <ref>", name, name)
		}
	}
	ref, err := ociimage.ParseRef(refStr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(env.store.Root, 0o755); err != nil {
		return err
	}
	cl := env.client
	cl.Logf = func(f string, a ...any) { out("  "+f, a...) }
	out("pulling %s", refStr)
	img, err := cl.Pull(ref, env.store.Root)
	if err != nil {
		return err
	}
	if err := ociimage.CheckLayerSlots(len(fcImageDedup(img.Layers))); err != nil {
		return fmt.Errorf("image %s: %w", name, err)
	}
	built, reused, err := ociimage.EnsureLayers(env.store.Root, img, cl.Logf)
	if err != nil {
		return err
	}
	libc, lerr := ociimage.DetectLibc(env.store.Root, img.Layers)
	if lerr != nil {
		out("  %v", lerr)
	}
	var prev string
	if err := env.store.Update(func(c *ociimage.Catalog) error {
		if cur, ok := c.Names[name]; ok != update {
			if ok {
				return fmt.Errorf("image %q was added concurrently (%s)", name, ociimage.Short(cur))
			}
			return fmt.Errorf("image %q was removed concurrently", name)
		}
		p, err := c.Put(name, img, libc, env.now())
		if err != nil {
			return err
		}
		prev = p
		if c.Sources == nil {
			c.Sources = map[string]string{}
		}
		c.Sources[name] = refStr
		return nil
	}); err != nil {
		return err
	}
	out("%s: %s", name, img.Ref)
	out("  digest   %s", img.Digest)
	out("  created  %s (%s, not enforced)", fmtTime(img.Created), ociimage.Age(img.Created, env.now()))
	out("  arch     %s", img.Arch)
	out("  layers   %d (%d new, %d shared)", len(img.Layers), built, reused)
	out("  libc     %s", libc)
	out("  size     %s compressed", fmtSize(img.CompressedSize()))
	if w := ociimage.LibcWarning(libc); w != "" {
		out("%s", w)
	}
	if update {
		if prev == img.Digest {
			out("%s unchanged (%s)", name, ociimage.Short(img.Digest))
			return nil
		}
		out("%s: %s -> %s", name, ociimage.Short(prev), ociimage.Short(img.Digest))
		var on []string
		for _, g := range env.groups() {
			if loadGroupConfig(g).str(cfgImageDigest) == prev {
				on = append(on, g)
			}
		}
		if len(on) > 0 {
			out("groups still on %s: %s (move them with koto image apply %s <group..>|--all)",
				ociimage.Short(prev), strings.Join(on, ", "), name)
		} else {
			out("no group is on %s", ociimage.Short(prev))
		}
	}
	return nil
}

func imageLs(env imageEnv, out func(string, ...any)) error {
	c, err := env.store.Load()
	if err != nil {
		return err
	}
	refs := imageRefs(env, c)
	out("%s  (built-in, shipped rootfs)", ociimage.DefaultName)
	for _, n := range c.SortedNames() {
		e, _ := c.Lookup(n)
		line := fmt.Sprintf("%s  %s  %s  %d layers  %s  %s", n, ociimage.Short(e.Digest),
			e.Created.UTC().Format("2006-01-02"), len(e.Layers), e.Libc, fmtSize(e.Size))
		if u := ociimage.Users(e.Digest, refs); len(u) > 0 {
			line += "  used by " + strings.Join(u, ",")
		}
		out("%s", line)
	}
	var orphans []string
	for dg := range c.Images {
		if len(c.NamesFor(dg)) == 0 {
			orphans = append(orphans, dg)
		}
	}
	sort.Strings(orphans)
	for _, dg := range orphans {
		out("<unnamed>  %s  kept for %s", ociimage.Short(dg), strings.Join(ociimage.Users(dg, refs), ","))
	}
	return nil
}

func imageInspect(env imageEnv, name string, out func(string, ...any)) error {
	c, err := env.store.Load()
	if err != nil {
		return err
	}
	e, ok := c.Lookup(name)
	if !ok {
		return fmt.Errorf("no image named %q", name)
	}
	if e.Builtin {
		out("%s: built-in, boots the shipped rootfs through the default path", name)
		return nil
	}
	out("name      %s", name)
	if s := c.Sources[name]; s != "" {
		out("source    %s", s)
	}
	out("ref       %s", e.Ref)
	out("digest    %s", e.Digest)
	out("manifest  %s", e.ManifestDigest)
	out("config    %s", e.ConfigDigest)
	out("created   %s (%s)", fmtTime(e.Created), ociimage.Age(e.Created, env.now()))
	out("added     %s", fmtTime(e.Added))
	out("arch      %s", e.Arch)
	out("libc      %s", e.Libc)
	out("size      %s compressed", fmtSize(e.Size))
	for i, l := range e.Layers {
		st := "missing"
		if ociimage.LayerOK(env.store.Root, l) {
			st = "ok"
		}
		out("layer %2d  %s  diff %s  %s  %s", i+1, ociimage.Short(l.Digest), ociimage.Short(l.DiffID), fmtSize(l.Size), st)
	}
	if u := ociimage.Users(e.Digest, imageRefs(env, c)); len(u) > 0 {
		out("groups    %s", strings.Join(u, ", "))
	}
	return nil
}

// imageApplyOne pins g to pin and restarts it if it is running.
func imageApplyOne(env imageEnv, g string, pin imagePin, out func(string, ...any)) error {
	before := loadGroupConfig(g).str(cfgImageDigest)
	if _, err := updateGroupConfig(g, func(cfg map[string]any) { setImageConfig(cfg, pin) }); err != nil {
		return fmt.Errorf("%s: %w", g, err)
	}
	if before == pin.Digest {
		out("%s: already on %s@%s", g, pin.Name, ociimage.Short(pin.Digest))
		return nil
	}
	out("%s: %s -> %s@%s (fresh sys.img, provision.sh runs once)", g, shortOrDefault(before), pin.Name, ociimage.Short(pin.Digest))
	return imageRestart(env, g, out)
}

func shortOrDefault(dg string) string {
	if dg == "" {
		return ociimage.DefaultName
	}
	return ociimage.Short(dg)
}

func imageRestart(env imageEnv, g string, out func(string, ...any)) error {
	if !env.running(g) {
		out("%s: not running; applies at its next start", g)
		return nil
	}
	out("%s: restarting", g)
	if err := env.restart(g); err != nil {
		return fmt.Errorf("%s: restart: %w", g, err)
	}
	out("%s: up", g)
	return nil
}

func imageGroupKnown(env imageEnv, g string) error {
	if !validGroupName(g) {
		return fmt.Errorf("invalid group name %q", g)
	}
	for _, k := range env.groups() {
		if k == g {
			return nil
		}
	}
	return fmt.Errorf("no group %q", g)
}

func imageApply(env imageEnv, args []string, all bool, out func(string, ...any)) error {
	if len(args) < 1 || (all && len(args) != 1) || (!all && len(args) < 2) {
		return fmt.Errorf("usage: koto image apply <name> <group..>|--all")
	}
	pin, err := resolveImageName(args[0])
	if err != nil {
		return err
	}
	groups := args[1:]
	if all {
		if pin.Name == ociimage.DefaultName {
			return fmt.Errorf("--all moves the groups set to a catalog name; name the groups to move to %s", ociimage.DefaultName)
		}
		for _, g := range env.groups() {
			if loadGroupConfig(g).str(cfgImage) == pin.Name {
				groups = append(groups, g)
			}
		}
		if len(groups) == 0 {
			out("no group is set to %s", pin.Name)
			return nil
		}
	}
	for _, g := range groups {
		if err := imageGroupKnown(env, g); err != nil {
			return err
		}
	}
	var errs []string
	for _, g := range groups {
		if err := imageApplyOne(env, g, pin, out); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// imageRollback swaps a group's current and previous pins and restores the
// previous base's archived sys.img (image_sys=restore → PrepareSys).
func imageRollback(env imageEnv, g string, out func(string, ...any)) error {
	if err := imageGroupKnown(env, g); err != nil {
		return err
	}
	c, err := env.store.Load()
	if err != nil {
		return err
	}
	var from, to string
	var verr error
	_, err = updateGroupConfig(g, func(cfg map[string]any) {
		cur, _ := cfg[cfgImageDigest].(string)
		curName, _ := cfg[cfgImage].(string)
		prev, _ := cfg[cfgImagePrev].(string)
		prevName, _ := cfg[cfgImagePrevName].(string)
		if prev == "" {
			verr = fmt.Errorf("group %s has no previous image to roll back to", g)
			return
		}
		from, to = ociimage.DefaultName, ociimage.DefaultName
		if cur == "" {
			cur, curName = ociimage.DefaultName, ociimage.DefaultName
		} else {
			from = curName + "@" + ociimage.Short(cur)
		}
		if prev == ociimage.DefaultName {
			delete(cfg, cfgImage)
			delete(cfg, cfgImageDigest)
			delete(cfg, cfgImageSys)
		} else {
			e, ok := c.ByDigest(prev)
			if !ok || e.Builtin {
				verr = fmt.Errorf("group %s: previous image %s is no longer in the catalog", g, ociimage.Short(prev))
				return
			}
			if !ociimage.ValidName(prevName) || prevName == ociimage.DefaultName {
				if n := c.NamesFor(prev); len(n) > 0 {
					prevName = n[0]
				} else {
					prevName = "rollback-" + strings.TrimPrefix(ociimage.Short(prev), "sha256:")
				}
			}
			cfg[cfgImage] = prevName
			cfg[cfgImageDigest] = prev
			cfg[cfgImageSys] = "restore"
			to = prevName + "@" + ociimage.Short(prev)
		}
		cfg[cfgImagePrev] = cur
		cfg[cfgImagePrevName] = curName
	})
	if verr != nil {
		return verr
	}
	if err != nil {
		return err
	}
	out("%s: rollback %s -> %s (previous sys.img restored if archived)", g, from, to)
	return imageRestart(env, g, out)
}
