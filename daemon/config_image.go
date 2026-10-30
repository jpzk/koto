package main

// image= group config (docs/images.md). The ONLY values accepted are names in
// the host image catalog (koto image ls) and the built-in koto-default; a raw
// registry reference is refused with a readable error, so nothing that can
// write config (ctl config_set from main, a delegated `config` grant, spawn)
// can make the daemon pull or boot an image the operator did not add.
//
// The name is resolved to its digest ONCE, at set time, and that digest is
// what the group boots (image_digest) — a later `koto image update` moves the
// catalog name without moving any group. The previous pin is kept for
// rollback (image_prev / image_prev_name), and image_sys=fresh tells the next
// boot to archive the old base's sys.img and start the new base on a fresh one
// (fcImagePlanFor → ociimage.PrepareSys). The workspace disk is never touched.

import (
	"encoding/json"
	"fmt"
	"strings"

	"koto/ociimage"
)

// Config keys owned by the image path. Only setImageConfig writes them.
const (
	cfgImage         = "image"
	cfgImageDigest   = "image_digest"
	cfgImagePrev     = "image_prev"      // previous digest, or koto-default
	cfgImagePrevName = "image_prev_name" // catalog name the previous digest was set as
	cfgImageSys      = "image_sys"       // fresh | restore (rollback)
)

// imagePin is a validated image= request: Name is a catalog name (or
// koto-default, with Digest "").
type imagePin struct {
	Name   string
	Digest string
}

// resolveImageName validates a requested image value against the catalog.
// "" means koto-default (clearing image= returns the group to the shipped
// rootfs).
func resolveImageName(v string) (imagePin, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == ociimage.DefaultName {
		return imagePin{Name: ociimage.DefaultName}, nil
	}
	if strings.ContainsAny(v, "/:@") {
		return imagePin{}, fmt.Errorf("image %q looks like a registry reference; only catalog names are accepted — add it first with `koto image add <name> <ref>`, then use <name>", truncateRunes(v, 80))
	}
	if !ociimage.ValidName(v) {
		return imagePin{}, fmt.Errorf("image %q is not a valid catalog name ([a-z0-9][a-z0-9._-]{0,63})", truncateRunes(v, 80))
	}
	cat, err := fcImageStore().Load()
	if err != nil {
		return imagePin{}, fmt.Errorf("image catalog: %w", err)
	}
	e, ok := cat.Lookup(v)
	if !ok || e.Builtin {
		return imagePin{}, fmt.Errorf("image %q is not in the image catalog (koto image ls)", v)
	}
	return imagePin{Name: v, Digest: e.Digest}, nil
}

// imagePinFromRaw is resolveImageName for a configReq field. ok=false means
// the field was absent.
func imagePinFromRaw(raw json.RawMessage) (p imagePin, ok bool, err error) {
	if len(raw) == 0 {
		return imagePin{}, false, nil
	}
	if isClear(raw) {
		return imagePin{Name: ociimage.DefaultName}, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return imagePin{}, true, fmt.Errorf("image must be a string (a catalog name)")
	}
	p, err = resolveImageName(s)
	return p, true, err
}

// setImageConfig applies a validated pin to a config document. Re-setting the
// digest the group already runs changes nothing but the recorded name (no
// sys reset, rollback target kept).
func setImageConfig(cfg map[string]any, p imagePin) {
	cur, _ := cfg[cfgImageDigest].(string)
	curName, _ := cfg[cfgImage].(string)
	if p.Name == ociimage.DefaultName {
		if cur == "" {
			delete(cfg, cfgImage) // already on the default rootfs
			return
		}
		cfg[cfgImagePrev] = cur
		cfg[cfgImagePrevName] = curName
		delete(cfg, cfgImage)
		delete(cfg, cfgImageDigest)
		delete(cfg, cfgImageSys)
		return
	}
	if cur == p.Digest {
		cfg[cfgImage] = p.Name
		return
	}
	if cur == "" {
		cfg[cfgImagePrev] = ociimage.DefaultName
		cfg[cfgImagePrevName] = ociimage.DefaultName
	} else {
		cfg[cfgImagePrev] = cur
		cfg[cfgImagePrevName] = curName
	}
	cfg[cfgImage] = p.Name
	cfg[cfgImageDigest] = p.Digest
	cfg[cfgImageSys] = "fresh"
}
