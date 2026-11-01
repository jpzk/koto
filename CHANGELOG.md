# Changelog

## Unreleased

### Added
- OCI images for groups: `koto image add|ls|inspect|rm|gc|update|apply|rollback`,
  `koto spawn <g> --image <name>` and the per-group `image=` config key (catalog
  names only, pinned by digest). Image groups boot one read-only ext4 drive per
  OCI layer (shared host-wide, cached by diff_id, mode 0444), a read-only
  `tools.img` derived from the shipped `rootfs.img`, a per-group `sys.img`
  overlay upper and the unchanged `/workspace` disk. `apply` archives
  `sys.img` and runs `/workspace/.koto/provision.sh` once per new base;
  `rollback` restores the previous digest and its `sys.img`. Images are
  limited to 12 layers (firecracker device budget). No age gate; age is
  logged. Groups without `image=` boot exactly as before. See
  [docs/images.md](docs/images.md).
- fcjail: the read-only bind remount keeps the locked nosuid/nodev/noexec/atime
  flags of the source mount (EPERM inside the user namespace otherwise).

### Follow-up
- Build `tools.img` in the release pipeline instead of deriving it on each host.
