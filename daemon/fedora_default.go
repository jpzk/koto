package main

import (
	"fmt"
	"sync"

	"koto/ociimage"
)

// The default base image. Since guest.img, a group with no image= (or an
// empty one) no longer boots the monolithic rootfs: it boots the catalog's
// default name (ociimage.Catalog.DefaultImage, "fedora" unless the operator
// set another) through the OCI image path, with guest.img as vda. The
// group's config.json is not rewritten, so a 1.8.0 rollback still sees a
// no-image group and boots it on rootfs.img.

// defaultImageMu serialises the first-boot add, so two groups booting at
// once pull the base once.
var defaultImageMu sync.Mutex

// ensureDefaultImage returns the catalog name and digest a no-image group
// boots. When the default is fedora and the catalog does not have it yet
// (a fresh install, or the first boot after an upgrade from 1.8.0), it is
// added from the digest-pinned ociimage.FedoraRef exactly like
// `koto image add fedora`. Any other default name must already be in the
// catalog: the operator put it there.
func ensureDefaultImage(env imageEnv, out func(string, ...any)) (name, digest string, err error) {
	defaultImageMu.Lock()
	defer defaultImageMu.Unlock()
	look := func() (string, string, bool, error) {
		c, err := env.store.Load()
		if err != nil {
			return "", "", false, err
		}
		n := c.DefaultImage()
		dg, ok := c.Names[n]
		return n, dg, ok, nil
	}
	name, dg, ok, err := look()
	if err != nil {
		return "", "", fmt.Errorf("image catalog: %w", err)
	}
	if ok {
		return name, dg, nil
	}
	if name != ociimage.FedoraName {
		return "", "", fmt.Errorf("default image %q is not in the catalog (koto image ls)", name)
	}
	out("default image %s is not in the catalog yet: adding %s", name, ociimage.FedoraRef)
	if err := imageFetch(env, name, ociimage.FedoraRef, false, out); err != nil {
		return "", "", fmt.Errorf("add default image %s: %w (when online: koto image add %s)", name, err, name)
	}
	name, dg, ok, err = look()
	if err != nil {
		return "", "", fmt.Errorf("image catalog: %w", err)
	}
	if !ok {
		return "", "", fmt.Errorf("default image %s vanished from the catalog after add", name)
	}
	return name, dg, nil
}
