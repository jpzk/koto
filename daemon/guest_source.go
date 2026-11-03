package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"koto/ociimage"
)

// fcGuestPath is the shipped guest.img (release asset guest.img_<ver>_x86_64.zst).
func fcGuestPath() string { return filepath.Join(fcAssetsDir(), "guest.img") }

// fcGuestSource picks what every image boot attaches as vda:
//   - fcassets/guest.img when present (the normal case since guest.img);
//   - else, for one release, the 1.8.0 way: derive it from fcassets/rootfs.img
//     (a 1.8.0 tree whose binaries were swapped by hand);
//   - else nothing can boot, and the error names the missing asset.
func fcGuestSource() (ociimage.ToolsImage, string, error) {
	agent := os.Getenv(fcImageAgentEnv)
	if fileExists(fcGuestPath()) {
		return ociimage.ShippedGuest{Path: fcGuestPath(), Agent: agent}, "guest.img", nil
	}
	if fileExists(fcRootfsPath()) {
		return ociimage.RootfsTools{Rootfs: fcRootfsPath(), Agent: agent}, "legacy rootfs.img", nil
	}
	return nil, "", fmt.Errorf("guest.img missing: %s (reinstall or run koto upgrade)", fcGuestPath())
}

// fcLegacyToolsKey is the 1.8.0 tools.img key that image gc must keep, or ""
// when guest.img is shipped (gc then leaves <cache>/tools alone, which also
// keeps a 1.8.0 rollback's derived image).
func fcLegacyToolsKey() string {
	src, _, err := fcGuestSource()
	if err != nil {
		return ""
	}
	rt, ok := src.(ociimage.RootfsTools)
	if !ok {
		return ""
	}
	k, err := rt.Key(fcImagesDir())
	if err != nil {
		return ""
	}
	return k + ".img"
}

// fcDefaultImageFor resolves a no-image group to the catalog default
// (ensureDefaultImage). ok=false means boot the legacy rootfs instead: only
// when the default cannot be produced (offline first boot) AND rootfs.img is
// still on disk. config.json is never rewritten.
func fcDefaultImageFor(g string) (name, dg string, ok bool, err error) {
	logf := func(f string, a ...any) { emitLogfG("fc", g, "info", "[%s] image: "+f, append([]any{g}, a...)...) }
	name, dg, err = fcEnsureDefault(logf)
	if err == nil {
		return name, dg, true, nil
	}
	if fileExists(fcRootfsPath()) {
		emitLogfG("fc", g, "warn", "[%s] image: default image unavailable (%v); booting the legacy rootfs.img this time", g, err)
		return "", "", false, nil
	}
	return "", "", false, fmt.Errorf("group %s: %w", g, err)
}

// fcEnsureDefault is ensureDefaultImage on the daemon's own store; a var so
// tests can stub the network. Set in init: the real one reaches restart ->
// spawn -> fcImagePlanFor, which a package-level initializer may not.
var fcEnsureDefault func(logf func(string, ...any)) (string, string, error)

func init() {
	fcEnsureDefault = func(logf func(string, ...any)) (string, string, error) {
		return ensureDefaultImage(defaultImageEnv(context.Background()), logf)
	}
}
