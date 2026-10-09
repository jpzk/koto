# koto fuzzing campaign — 2026-10-09

Scope: the 1.0.3 release plus the 2026-10-09 dependency bump (`d2b4202`),
fuzzed in isolated dev daemons that each had their own state dir, ports and
scratch groups. No production or dev-fleet group was touched. Three streams:

- **Daemon API**: every gRPC verb driven with malformed, boundary and
  concurrent input, under the admin identity and under a minted scoped one
  (`fzagent` holding the `fzrole` ACL).
- **TUI pty**: `koto-tui` driven in a pty under a VT emulator, with hostile
  transcript content, resize storms and the mono/vt100 profiles.
- **Guest frames / turn stream**: offline Go fuzz targets over the turn-frame
  renderer and the log writers (in-tree style, not committed).

None of the findings came from the dependency bump; every one reproduces on
1.0.3.

Verdicts, as in `SECURITY_11_SEP.md`:

- **fixed**: the code changed, and the fix is pinned by a test in `daemon/audit_fixes_test.go`.
- **open**: real, but not fixed yet.
- **not a finding**: the precondition does not exist.

---

### F1 — Transcript marker forgery through the `[[bg]]` writer (`daemon/send.go`, `fcturn.go`) — **fixed** `2a2d891`

The turn writer cached for itself whether its stream was mid-line. The
background tailer's appends never updated that cache, so after a
`[[bg]]` line the writer escaped turn text against the wrong line state.
Guest-authored text could then land at the start of a line and read as a
daemon `[[marker]]`, which could switch the session or reattribute a turn.
Every writer of a slot log now reads the line state from the file's last
byte under `logWriteLock` (`logEndsMidline`).
`TestTurnMarkerEscapingSurvivesInterleavedBgLines`.

### F2 — `koto ctl shell` output filter can be evaded (`daemon/shellfilter.go`) — **open**

The M86 filter does not drop everything it is meant to drop from guest shell
output. This affects `koto ctl shell` only; the TUI renders the shell through
a cell grid (`tui/shell_view.go`) and never writes raw guest bytes. Details
are not recorded here. It is deferred to a separate change.

### F3 — `Resources` and `SubscribeLogs` ignored target scoping (`daemon/grpc_server.go`) — **fixed** `5bc3648`

Both verbs are untargeted, and the handlers answered for the whole fleet. A
role confined to one group still read every group's disk, RSS, CPU and guest
memory, and every group's fc/egress/send log lines. Both now project through
the caller's own grant, as List and WatchState do (M18):

- Host sums are recomputed from the visible groups.
- Lines with no group attribution are withheld from a scoped role.

`TestResourcesAndLogsProjectByGrant`.

### F4 — Abandoned Restart/Spawn/Destroy acted minutes later (`daemon/groups.go`) — **fixed** `87c3736`

Restart ignored its context. Spawn and Destroy checked theirs only before
`groupOpLock`, which waits for as long as the holder takes. So a timed-out
client's Restart stopped and rebooted the VM once the lock came free, and a
backlog of abandoned calls replayed in sequence. `groupOpLockCtx` now waits
for the lock or the context. Once the lock is held, the operation is
committed. `TestGroupOpsHonourCancelWhileWaitingOnLock`.

### F5 — VMM sometimes runs at nice 0 (`daemon/fcjail.go`) — **open, fix in progress**

The VMM's nice value is set on a thread that the Go scheduler is free to
migrate, so the setting sometimes misses the thread that forks the VMM.
Posture verification (`nice ≥10`) flags it. A concurrent change pins the OS
thread (`runtime.LockOSThread`). It will land with that work.

### F6 — `koto ctl ask` hangs about 1 run in 6 (`daemon/ctl_cli.go`) — **open**

`ask` sends before the server has registered its `SubscribeGroup` stream, so
a fast turn's `turn_end` can come before the subscription and the client
waits forever. The fix is to wait for the stream's first frame (or the
synthetic seq-0 activity frame) before `Send`.

### F7 — `[[session]]` glued onto a dead stream's last line (`daemon/send.go`) — **fixed** `2a2d891`

When a turn died mid-line, the next turn's `[[session]]` header was appended
to that line, so replay attributed the next turn to the previous session.
`sendNow` now writes the header under the log lock and starts a fresh line
when the file is mid-line. `TestTurnHeaderAfterDeadStreamStartsFreshLine`.

### F8 — Uploads of discarded turns leak for 24h (`daemon/attachments.go`) — **open**

Images staged for a turn that is later drained, stopped or cleared stay in
`.cs/uploads` until the 24h sweep. They count against the per-group pending
quota meanwhile, so attachments are refused.

### F9 — Shell filter swallows non-ASCII output (`daemon/shellfilter.go`) — **open**

This is part of F2: Cyrillic and Latin-1 text is dropped from `koto ctl
shell` output. It is deferred together with F2.

### F10 — Mono mode: SGR inside markdown emphasis breaks the frame (`tui/markdown.go`, `tui/mono.go`) — **open**

In the ascii style, emphasis emits SGR sequences that the mono filter's
`scanEsc` does not fully consume. Under vt100/dumb the leftover bytes shift
the row, and the frame wraps.

### F11 — Quadratic render on long unbroken tokens (`tui/markdown.go`) — **open**

Glamour/reflow word-wrap is quadratic in token length. A single multi-KB
token, such as base64 or a minified line, stalls the render for seconds.
The fix is to pre-break tokens longer than the width before glamour sees them.

### F12 — Width mismatches: Hangul jamo, VS16 emoji (`tui/width.go`, `tui/view.go`) — **open**

Conjoining jamo sequences and VS16-promoted emoji are measured differently by
koto's width function and by the terminal, so the rows that contain them are
off by one or more cells.

### F13 — Misleading in-band responses (`daemon/grpc_server.go`, `config.go`, `acl.go`) — **open**

- Destroy/Stop of a nonexistent group returns `ok:true`.
- Clear reports "stopping".
- Invalid `Config` values are silently dropped instead of refused.
- `AclSetRole` accepts unknown verbs.

None of these is a privilege issue. Each makes a mistake look like success.

---

## Also noted (no action)

- **FuzzWrapLine crasher**: the bug was in the test oracle, not the product.
- **The `[[bg]]` session name is not normalized** (`logparse.go`). This is
  defence in depth only: F1 was the reachable path.
- **The guest's `writeFrame` lacks the M117 size check**, so its comment
  claims a check that is not there. The host side enforces the check.
- **NAT64 / 6to4 addresses classify as WAN** (`fcClassifyDst`). They are
  consistent with the network profiles' intent, but worth a comment.
- **Admission and refused spawns**: with `KOTO_HOST_MEM_MIB=4096`, only two
  small VMs fit. A Spawn refused by admission still registers the group.
- **Stale `run/fc/` sockets** are left behind after a VMM dies uncleanly.
