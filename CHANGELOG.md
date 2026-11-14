# Changelog

## Unreleased

### Added
- Confirm-mode goals. `goal_set` takes `mode` (`yolo` default, or `confirm`).
  The same option is `koto ctl goal set -mode` and `/goals set mode=confirm`.
  - A goal set from a confirm or plan session is always confirm.
  - Tool calls wait as approval cards in the goal session, answered oldest
    first (`i/N`). There is no auto-deny by default
    (`KOTO_GOAL_APPROVAL_TIMEOUT`), and the turn clocks are paused while a
    card waits.
  - `goal_status` reports `waiting_confirm` / `pending`, and the goal view
    shows "waiting for confirmation (N)".
- Goal steering.
  - Allow+message (`a`) joins Deny+message (`m`) on approval cards.
  - Messages on goal approvals, decision answers and `/steer <text>`
    (`koto ctl goal steer`, operator-only `GoalSteer` RPC) go into a
    host-held steering log.
  - Every later iteration reads that log as `OPERATOR STEER (authoritative)`.
  - Workers ask the operator a decision with `cs-ask` (ctl `ask`), shown as
    "waiting for decision".
  - See [docs/groups-goals-schedules.md](docs/groups-goals-schedules.md).
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
