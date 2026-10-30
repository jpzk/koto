# OCI images for groups

By default every group boots the shipped Fedora rootfs
(`fcassets/rootfs.img`, the built-in image `koto-default`). A group can
instead boot any linux/amd64 OCI image (Debian, Ubuntu, your own) while koto
keeps its own guest tools, its `/workspace` disk and its turn loop unchanged.

Groups without `image=` boot exactly as before: same drives, same kernel
command line, same rootfs (`fcVMConfig` in `daemon/fc.go`; golden test
`daemon/fcimage_golden_test.go`).

## Quick start

```sh
koto image add debian docker.io/library/debian@sha256:<digest>
koto spawn mygroup --image debian          # or: koto ctl spawn mygroup -image debian
koto ctl config mygroup -image debian      # existing group, applies at next restart
```

`image add` resolves a tag to a digest ONCE and stores the catalog entry by
digest; groups are pinned to that digest, never to a moving tag.

## Commands (`koto image`, admin-only rpc `Image`)

Source: `daemon/image_cli.go` (CLI), `daemon/imagecmd.go` (daemon side).

- `add <name> <ref>`: pull `<ref>` (tag or `@sha256:` digest), verify every
  blob's sha256, convert layers, and print digest, created time and age,
  arch, layer count (new / shared with already-cached images), libc
  (`glibc` / `musl`, with a warning for musl because the bundled claude
  binary needs glibc) and compressed size. Refuses an existing name (use
  `update`).
- `ls`: catalog names, digests and the groups using them, plus digests kept
  only because a group or an archived `sys.img` still references them.
- `inspect <name>`: layers (cached or missing), created, libc, groups.
- `rm <name>`: refused while any group references the digest (current
  image, previous image for rollback, or an archived `sys-<digest12>.img`).
- `gc`: deletes blobs and layer drives no catalog name or group needs.
- `update <name> [<ref>]`: re-resolve (ref defaults to the one used at
  `add`), move the catalog name to the new digest, print `old -> new` and
  the groups still on the old digest. Groups are NOT moved.
- `apply <name> <group..>` or `apply <name> --all`: move groups to the
  name's current digest. Running groups restart; stopped ones apply at next
  start.
- `rollback <group>`: back to the previous digest and its archived
  `sys.img` (or back to `koto-default` if that was the previous image).

Image age is logged, never enforced: koto has no age gate (`ociimage.Pull`).

## Group config

Per-group key `image=<catalog name>` (`daemon/config_image.go`):

- accepted: a catalog name or `koto-default` (empty clears it). Raw
  references (anything with `/`, `:` or `@`) are rejected with a readable
  error; only operator-curated catalog names are accepted, also via
  in-guest `config_set` from main.
- at set time the group config records `image_digest` (the pin),
  `image_prev` (for rollback) and whether `sys.img` must be fresh or
  restored.
- takes effect on the group's next (re)start.

## VM layout in image mode

Drives (`daemon/fcimage.go`, `ociimage.FixedDevices`):

- `vda` `tools.img` read-only: koto guest agent (`fc-agent`), claude, codex
  and sidecar scripts, mounted at `/opt/koto`.
- `vdb` `workspace.img` read-write: the existing `/workspace` disk. Image
  operations never touch it.
- `vdc` `sys.img` read-write, per group: the overlay upper layer (your
  `apt install`s and `/etc` edits live here).
- `vdd..` one read-only ext4 drive per OCI layer, bottom to top.

The guest init (`fcguest/image.go`) stacks
`overlay lowerdir=Ln:..:L1,upperdir=sys`, verifies every layer drive's
ext4 label and UUID against the `koto.layers=` kernel command line (boot
fails on mismatch, never relies on `/dev` order), binds the tools at
`/opt/koto`, injects the worker user (uid 1000), `HOME=/workspace`,
`resolv.conf`, hostname and a CA bundle if the image has none, and writes
`/etc/koto/image` (`name=`, `digest=`, `created=`).

### Layer limit

Firecracker attaches at most 17 virtio-mmio devices on x86
(`ociimage.FCMaxMMIODevices`, measured). koto uses 5 of them (tools,
workspace, sys, vsock, one reserve), so an image may have at most
**12 layers** (`ociimage.LayerSlots`). Larger images are refused at
`add` / `update` with an error naming the limit; rebuild them with fewer
layers. koto does not squash.

## Host cache

Under the daemon state dir (`images/`, `fcImagesDir`):

- `blobs/sha256/<hex>`: compressed blobs, digest-verified, mode 0444.
- `layers/<diff_id>.ext4`: one ext4 image per layer, mode 0444, shared by
  every image and group that uses that layer. Only missing layers are
  fetched and converted.
- `catalog.json`: name to digest, plus kept manifests and configs so a
  rollback works offline.
- `tools/<key>.img`: `tools.img`, see below.

Layer conversion (`ociimage.BuildLayer`) runs `mke2fs` plus a generated
`debugfs` script: no root, no podman, and tar paths are never materialized
on the host. OCI whiteouts (`.wh.<name>`) become overlayfs whiteouts (char
device 0/0) and opaque markers (`.wh..wh..opq`) become
`trusted.overlay.opaque=y`. Bounds: 200 000 entries, 1024-byte paths,
4 GiB decompressed per layer, 2 GiB per compressed blob, 64 manifest layers.

### Registry outbound policy

The registry picks the next places the daemon connects to: the token realm
in `Www-Authenticate` and 30x redirects to blob CDNs. Every connection the
daemon makes during a pull goes through the same policy
(`daemon/ociimage/netpolicy.go`):

- https only, for the registry, the token realm and every redirect hop.
  A realm that carries userinfo is refused.
- Addresses are checked after DNS resolution, on the address actually
  dialled. The policy refuses loopback, RFC 1918 / ULA private,
  link-local (this covers the 169.254.169.254 metadata service), CGNAT,
  multicast, unspecified, reserved and documentation ranges, and NAT64
  addresses that embed any of them. So **registries on localhost or a
  private LAN are not supported**. Mirror the image to a public registry,
  or pin it by digest from one.
- Environment proxies (`HTTPS_PROXY`) are ignored, because a proxy would
  move the dial away from the address that was checked.
- At most 5 redirects. The registry's bearer token is dropped on any hop
  that leaves the registry's host:port.
- There is no whole-request deadline, so a large layer on a slow link
  finishes. Dial and TLS handshakes are bounded at 30 s and response
  headers at 60 s. A body that sends no byte for 2 minutes is aborted and
  retried, up to 3 attempts. Cancelling the `koto image` call (Ctrl-C, or
  the client disconnecting) cancels the requests in flight.

## tools.img

`tools.img` is derived on the host from the shipped, already-verified
`fcassets/rootfs.img` (`ociimage.RootfsTools`): the needed files are dumped
with `debugfs` and written into a small read-only ext4. Nothing is
downloaded. It is keyed by the rootfs hash (and an optional `KOTO_IMAGE_AGENT`
override for a development fc-agent), so a koto update rebuilds it.

Follow-up: build and sign `tools.img` in the release pipeline next to
`rootfs.img` instead of deriving it on each host.

## Updating an image, provisioning, rollback

```sh
koto image update debian docker.io/library/debian@sha256:<new>
koto image apply debian mygroup
koto image rollback mygroup
```

`apply` archives the group's `sys.img` as `sys-<olddigest12>.img` and boots
the new base with a fresh `sys.img`, so system changes made on the old base
are gone while `/workspace` is untouched. If `/workspace/.koto/provision.sh`
exists it runs on the first boot of each new base, after the network is up
(marker `/var/lib/koto/provisioned` inside `sys.img`, log
`/var/log/koto-provision.log`); put your `apt install` lines there.

The script lives in the worker-writable workspace, so it runs with the
group's root posture: as root only when the group has `root=yes`, otherwise
as the worker (uid/gid 1000, `HOME=/workspace`). The log's first line and the
agent console say which. With `root=no` a script cannot install system
packages; set `root=yes` for that. Only a run that exits 0 writes the
marker; a failing or killed run (30 min cap) runs again on the next boot.
It runs in the background: the agent is reachable while it runs, and
`/run/koto-provisioning` exists until it finishes
(`while [ -e /run/koto-provisioning ]; do sleep 2; done`). Switching a group
from `root=yes` to `root=no` removes koto's sudo grant from `sys.img` on the
next boot.

`rollback` restores the previous digest together with its archived
`sys.img`.

## Limitations in image mode

- musl images (Alpine): `add` warns; claude (glibc) will not run.
- providers that need `node` (venice, pi) need `node` in the image.
- shell attach (`/shell`, `koto ctl shell`) needs `tmux` in the image.
- `root=yes` needs `sudo` in the image (the root filesystem is writable via
  the sys overlay anyway).
