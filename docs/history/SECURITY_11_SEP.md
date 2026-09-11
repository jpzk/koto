# koto security assessment triage — 2026-09-11

Source: an automated assessment producing 339 findings (2 high, 166 medium,
171 low) against the daemon, guest agent, sidecar scripts and TUI.

This file is the ledger: every finding gets a verdict, and a verdict of
*accepted* or *not a finding* carries its reasoning, because an unexplained
"won't fix" is indistinguishable from an oversight on the next audit.

Verdicts:

- **fixed** — code changed, pinned by a test in `daemon/audit_fixes_test.go`.
- **accepted** — real, but the risk is the trust model's stated position.
- **not a finding** — the precondition does not exist, or the claim is wrong.

---

## high

### H1 — Legacy `workspace.img` symlink enables cross-boundary VM disk redirection (`daemon/fc.go`) — **fixed**

`fcEnsureWorkspaceImg` accepted the image with `os.Stat`, which follows
symlinks, and every consumer downstream works on the PATH: `os.Truncate` +
`e2fsck` + `resize2fs` in the grow path, `os.Chown` in `fcJailFixupPerms`, and
the jail shim's bind mount, which hands the resolved inode to the guest as a
read-write block device — where the guest's mount fallback (`mkfs.ext4 -F` on
an unmountable `/dev/vdb`) would format it.

Planting the symlink needs write access to `groups/<g>/`, which under
Firecracker is tier 1 (there is no shared filesystem). The one exception is
the case the finding names: a **podman-era** workspace, which the migration
path deliberately treats as pre-existing untrusted input, and which a podman
sidecar had RW.

Fixed by `os.Lstat` + a regular-file check before anything touches the path.
Refusing the whole class beats reasoning about who wrote it.
`TestWorkspaceImgRefusesNonRegularFile`.

### H2 — Jailed Firecracker can impersonate its guest on the privileged host control plane (`daemon/fc.go`) — **accepted**

True as stated, unclosable, and not an escalation.

Firecracker's hybrid vsock makes every guest→host connection a `connect(2)`
**by the VMM process** to `<uds>_<port>` (see the channel map at the top of
`fc.go`). The VMM is definitionally the peer, so `SO_PEERCRED` cannot
distinguish "VMM relaying its guest" from "VMM acting alone", and a per-VM MAC
would have to be handed to the guest through that same VMM.

What the channel grants is exactly the authority the group's own guest already
holds — and a VMM compromise is reached *through* that guest, via a
virtio/vsock device-model bug, so the attacker held it already. The jailer's
actual claim is narrower and still holds: a VMM escape reaches no credentials,
no network, no other group's sockets, and no host filesystem outside its
chroot. Recorded in the comment at `fcJailFixupPerms`.

---

## medium

### M3 — DNS rebinding bypasses the HTTP egress destination policy (`daemon/proxy.go`) — **fixed**

`egressConnect` pinned its dial to the vetted IP; `egressHTTP` did not — it
handed the original URL to a shared `http.Client`, whose transport re-resolved
the name inside `Do()`. The comment there acknowledged the window and deferred
it on the grounds that plain-HTTP egress is rare. It is still a wide window,
because the daemon's own dial is not behind the guest's L3 frame filter: a
guest controlling DNS for a name it is allowed to reach could steer the
DAEMON's connection at a destination its profile forbids.

Fixed with a per-request `http.Transport` whose `DialContext` ignores the
address the transport resolved and dials the vetted `ip:port`. The Host header
still rides `r.URL`, so the origin sees the name it was asked for. The dial
guard `egressConnect` already had (never dial the control plane, whatever the
check said) is applied on this path too, and `serveEgress` now defaults a
missing port from the URL scheme so the vetted address is a complete `ip:port`.
`TestEgressHTTPDialsVettedIP`.

### M6 — IPv6 AWS metadata endpoint bypasses the guest egress filter (`daemon/fcnet.go`) — **fixed**

`169.254.169.254` — the metadata endpoint for AWS, GCP, Azure and most others
— is link-local, so `fcClassifyDst` already returned `fcDstCtl` for it. AWS's
IPv6 IMDS is `fd00:ec2::254`, a ULA, which `IsPrivate()` classes as LAN: a
`lan` or `full` guest on a dual-stack host could read the instance's role
credentials. `Ec2MetadataAccess: false` does not cover it — that governs the
netstack's own answer, not a host-routed dial.

Classified as control plane, so it is denied under every profile, on both the
frame filter and the L7 proxy. `TestIPv6MetadataIsControlPlane`.

### M10 — Independent DNS lookups bypass the boot-time egress profile (`daemon/proxy.go`) — **fixed**

`serveEgress` called `egressTargetAllowed` twice — once for the config profile,
once for the booted one — and kept the vetted IP from the first. Each call did
its own `egressLookupIP`, so a rebinding name answering LAN then WAN passed
`full` on lookup one and `wan` on lookup two while the dial went to the LAN
address the booted profile forbids.

Fixed by intersecting the two profiles first (`egressIntersect`) and checking
once, which also makes "the stricter of the two" a property of the value rather
than of the call sequence. `wan ∩ lan` is `none`, which is correct: a group
that booted `wan` and now reads `lan` reaches nothing until the `/restart`.
`TestEgressProfilesIntersectOnOneLookup`.

### M2 — Unauthenticated local vsock peers can invoke the guest agent as root (`fcguest/main.go`) — **fixed (defense in depth)**

Already mitigated, and the mitigation is load-bearing enough to deserve a
second layer. `fc-agent` binds `VMADDR_CID_ANY` and its `Exec`/`ExecStream`/
`ShellAttach`/`Init`/`Shutdown` ops run as PID 1, i.e. guest root — so anything
in-guest that could reach vsock would defeat the `root=no` profile outright.
Nothing can: `build-kernel.sh` refuses to produce a kernel with
`CONFIG_VSOCKETS_LOOPBACK=y` or `CONFIG_VHOST_VSOCK=y` (audit I1).

But that is a property of an ASSET — `fcassets/vmlinux` — which an operator can
replace, and the agent itself took the peer on faith. `vsockAcceptLoop` now
keeps the `SockaddrVM` that `accept` returns and refuses any peer whose CID is
not `VMADDR_CID_HOST`. The kernel guard stays the primary defense; this makes
it a backstop rather than the only one.

A per-VM shared secret was the other half of the suggested remediation and is
deliberately not taken: the secret would have to reach the guest over the same
vsock channel it is meant to protect, so it buys nothing against a peer that
can already speak on that channel.

### M4 — AttachShell permits cross-session tmux attachment within an authorized group (`daemon/grpc_server.go`) — **fixed (hygiene), claim partly rejected**

`open.Session` went to the guest unvalidated and became `tmux new-session -A -s
<value>`. Now pinned to `shellSessionRE` — `koto-shell` plus an optional
`-<chat session>`, which is exactly the namespace `shellSessionName` (TUI) and
`turn.go` (guest) mint. That closes the two real consequences: an arbitrary
selector created an unmanaged tmux session outside the group's namespace, and a
newline in it forged daemon log records.

The access-control framing does not hold. `attach_shell` on a group yields an
interactive shell as the guest worker uid, from which `tmux attach -t` reaches
every session in that VM — so restricting the selector cannot be the boundary
between conversations, and nothing here claims it is. Cross-conversation
isolation inside one group is a VM-level property koto does not offer; the
boundary is the group.

### M7 — Group-scoped Config permission bypasses field-level posture authorization (`daemon/grpc_server.go`) — **fixed**

Real, and the reasoning already existed on the other plane. `ConfigReq` carries
the delegable settings (`model`, `effort`, `provider`) and the posture keys
(`network`/`internet`, `root`, `ports`, `size`, `autostart`) in one message,
and the interceptor did one coarse `config` check against the group. So a role
granted "may set this group's model" could also give the group WAN egress,
passwordless guest sudo, a published host port, more of the host's RAM, or a
boot at daemon start.

`postureVerb` (auth.go) re-labels a posture-setting request as the synthetic
verb `config_posture`, which is in `adminOnlyVerbs` — so no acl.json grant
reaches it, `"*"` included. Reads and delegable-key writes stay `config`. This
is the gRPC twin of the ctl plane's posture refusal (audit H1, 2026-09-05);
the invariant is the same one, that `network=none` must not be voidable by
anything the default contains. `TestConfigPostureIsAdminOnly`.

### M5 — Security-sensitive helpers are resolved from an untrusted PATH (`daemon/install.go`) — **not a finding**
### M8 — `koto tui` executes an attacker-selected executable with administrator credentials (`daemon/tui_cmd.go`) — **not a finding**

Both reduce to "an attacker can write to a directory on the operator's PATH".
That attacker already executes as the operator on their next shell command —
`ls`, `git`, `make` — with no koto involved. koto's trust model names the host
user as tier 1 by definition; a defense here would protect nothing, because the
thing it protects runs as the principal it is defending against.

The one part of M8 with a distinct shape — resolving `koto-tui` from somewhere
the DAEMON can write, which is a tier-2→tier-1 crossing rather than a tier-1
one — was found and closed by the previous audit (L10, 2026-09-06): the
`<state>/koto-tui` fallback is gone, and the surviving dev-clone convenience
requires a cwd carrying a koto checkout's markers, which `ProtectSystem=strict`
keeps the daemon out of. The rationale is in the comment at `tuiMain`.

### M11 — Unvalidated cs-job IDs enable path traversal outside the jobs directory (`sidecar/cs-job`) — **fixed (hygiene)**

Correct as a bug, not as a boundary: `cs-job` runs as the guest worker in the
guest's own workspace, so `cs-job rm ../..` destroys exactly what `rm -rf
/workspace` already would. Fixed anyway, because no verb here has business
resolving outside `$JOBS`, and the realistic trigger is a model repeating a
malformed id back at itself rather than an adversary.

`chkid` rejects anything but a bare alphanumeric before the path is built, on
`_notify`, `wait`, `peek`, `status`, `logs` and `rm`. It is called as a
STATEMENT, never inside `$(...)`: a command substitution runs in a subshell, so
an `exit` there ends only the subshell and the caller proceeds with an empty id
— which for `rm -rf "$JOBS/$id"` is worse than the traversal. (Written the
wrong way first; the smoke test caught it.)

### M12 — Claude worker permanently bypasses all permission checks (`fcguest/turn.go`) — **accepted**

`--dangerously-skip-permissions` is not an oversight, it is the product. koto
runs unattended agents; an approval prompt with nobody to answer it is a hang,
and the flag is why every group is a microVM with no NIC by default rather than
a process on the host. The boundary is KVM plus the `network` and `root`
profiles, not the model's own tool gate — which is exactly why those two
profiles default closed and why posture is admin-only (M7).

The workspace-destruction consequence is the group's own workspace, which the
agent is supposed to own. The remediation's "run workers in least-privilege
disposable workspaces, keep network access disabled by default" is a
description of what koto already does.

### M13 — Root agent follows worker-controlled temporary-file symlinks (`fcguest/main.go`) — **fixed**

Real, and it crosses the one boundary inside the guest that `root=no` claims to
hold. `writeWorkerFile` wrote `path + ".tmp"` with `os.WriteFile` (which follows
symlinks) and then `os.Chown`ed it (which also follows), under directories that
are chowned to uid 1000 — so the worker could pre-create the predictable
temporary name as a symlink, have PID 1 truncate an arbitrary root-owned guest
file, and then take ownership of it.

Now `O_CREAT|O_EXCL|O_NOFOLLOW` — which refuses an existing entry of any kind,
closing the race rather than sampling it — plus `fchown` on the descriptor
rather than on a name that can change underneath. The `os.Rename` needed no
change: it replaces `path`, it does not follow it.

### M14 — Destroyed groups retain executable schedules and can resurrect a reused group (`daemon/groups.go`) — **fixed**

Real, and not only as a security matter: `destroy()` cancelled goals and
disarmed the report window but left schedules alone, and a schedule fire is an
`enqueueSend` whose `sendNow` calls `ensure()` — so a destroyed group's cron
line rebuilt its VM and a blank workspace minutes after the operator deleted
it, and a later group reusing the name inherited the old group's schedules.

`delSchedsFor(g)` in `destroy()`, next to `disarmReport` and the event-ring
drop, which close the same name-reuse hazard for their own state. The generation
-id scheme the remediation proposes is not needed once the records are gone
with the group.

### M16 — Concurrent whole-file config updates can restore revoked group security posture (`daemon/config.go`) — **fixed**

Three callers — `configCmd`, `seedSpawnConfig`, `ensureProviderConfig` — each
parsed the whole `config.json`, changed a key or two, and wrote the whole
document back, with no lock between read and write. The last writer's stale
copy of every OTHER key wins, so an operator lowering `network` to none could
have it undone seconds later by a spawn seeding `provider` from a snapshot
taken before the change. Posture is admin-only (M7) exactly so it cannot be
lowered by a lesser principal; restoring it through a stale rewrite is the same
outcome by another route.

`updateGroupConfig` is now the only writer: a per-group mutex held across read,
mutate and commit, and a commit by rename so a reader sees one document or the
other. (`os.WriteFile` truncates in place, so readers could catch half a file.
`groupNetwork`/`groupRoot` fail CLOSED on a parse error, so that window was
never an escalation — but a config read silently answering "default" because it
caught a write mid-flight is not something to leave in.)
`TestGroupConfigUpdatesAreSerialized`.

### M17 — Send and schedule paths can provision arbitrary unregistered groups (`daemon/send.go`) — **fixed**

Real, and the root of M14's other half. `ensure()` created whatever
syntactically valid name it was handed, and it is reached from `send`, `clear`,
`restart`, a schedule fire and a shell attach — none of which carry spawn
authority. So a principal with `send` on `"*"` provisioned groups without
`spawn`, and the ctl plane's `ctlMaxSpawn` cap was reachable around rather than
through.

Split: `ensure()` boots a group that is registered in `groups.json` and errors
on an unknown name; `spawnEnsure()` is the creating path, called from exactly
three admission points — the ctl `spawn` verb, the `Spawn` RPC, and the
daemon's own boot of `main`. The cap moved with it: the `Spawn` RPC enforces
`ctlMaxSpawn` too, since `spawn` is an ordinary grantable verb and a non-admin
role holding it on `"*"` previously had no bound at all.
`TestEnsureDoesNotProvision`.

### M20 — Main-to-peer goals can bypass the mandatory human-approval gate (`daemon/goals.go`) — **fixed, partly rejected**

The ctl half is real. A main caller's `goal_set` always targets a peer (its own
group is refused), and plan-first on that path IS the human gate:
`goal_approve` is self-only precisely so main cannot approve a plan it set on a
peer. An explicit `"plan": false` went straight to `goalStatusRunning` and
started an autonomous, self-judged, multi-iteration loop on another group with
nobody in the loop. Now forced to plan-first, with a log line saying so. A
group setting a goal on ITSELF keeps `plan=false`: approving its own plan is
allowed, so it is the same authority by a shorter route.

The gRPC half is rejected. `GoalSet` over gRPC is the OPERATOR — the human the
gate exists to involve — so `plan=false` there is that human approving up
front, not a bypass. A gate that the person it defers to cannot also skip is
not a gate, it is an obstacle.
`TestMainPeerGoalsAreAlwaysPlanFirst`.

### M15 — Proxy concurrency limits do not impose an aggregate request-body memory budget (`daemon/proxy.go`) — **fixed**

The previous audit bounded one body (`proxyMaxBody`, 64 MiB) and how many
(32 per group / 128 global). Their PRODUCT is the number that matters: ~8 GiB
of live heap in the process that also holds the OAuth token, every group's log
tailer and the whole control plane — and the bodies stay live across up to 7
retry attempts, with `injectThinkingDisplay` unmarshalling one on top. No
exploit is needed, only large valid-looking requests in parallel, which
`cs-subagent` already fans out.

`proxyBodyBudget` (512 MiB) is charged as the body is READ — not guessed from
`Content-Length`, which the guest sets and a chunked request omits — and
returned when the body stops being referenced. Over budget answers 503, the
same "come back" the slot wait already answers with. Both providers go through
one `readRequestBody`; the Venice path buffers and retries identically and had
the identical hole. Deliberately far above real traffic (a turn's body is
kilobytes; 512 MiB still admits sixteen simultaneous maximum-size ones): an OOM
backstop like the semaphores beside it, never a scheduler.
`TestProxyBodyBudgetBounded`.

### M18 — List and WatchState bypass group ACLs and disclose unauthorized job metadata (`daemon/grpc_server.go`) — **fixed**

`list` and `watch_state` are verb-only in the ACL, so the interceptor
authorized the CALL and the handler then serialized the whole fleet — every
group, and with it every group's job records: command text, session, rc,
timings, output size. A role confined to one group by `"send": ["main"]` still
enumerated and continuously watched everything.

Rather than invent a second grammar, these two verbs now PROJECT through their
own grant's target set — something `acl.json` could already express and which
every seeded role writes as `"*"`, so a broad grant behaves exactly as before
and `"list": ["main"]` finally means what it reads as. Jobs are narrowed a
second time by the `jobs` grant: seeing that a group exists and reading the
command lines running inside it are different asks.

Mechanically this needed the caller in the handler, so the interceptors now put
the identity in the context (`withIdentity`; `idStream` does the same for
streaming handlers, which grpc-go gives no hook for). `WatchState` keeps the
shared-frame fast path for unnarrowed watchers and gives a narrowed one its own
frame and its own hash — its view changes on a different schedule.
`TestAggregateViewsProjectByGrant`.

### M19 — Stale background tailer writes into a reused slot and misattributes output to a new session (`daemon/send.go`) — **fixed**

Real. `tailBackgroundTask` runs for up to ten minutes and the turn that spawned
it usually ends first — that is the feature, the operator watches the output
accumulate — after which the slot is released and a LATER turn, possibly
another conversation, writes its own session marker into the same stream. The
`[[bg]]` record carried no session, and the parser's attribution is sticky, so
the late line landed in whichever conversation owned the stream by then.

Fixed by writing the session INTO the record: `[[bg]] <id>:<session> <text>`.
Explicit beats sticky here, and it needs no slot-generation bookkeeping —
the tailer is *supposed* to outlive its turn, so cancelling it at turn end (the
other obvious fix) would have removed the feature. Legacy transcripts have no
colon and keep the old behavior, so replayed history still parses.
`TestBackgroundRecordsCarryTheirSession`.

### M21 — Turn text marker escaping is bypassable across frame boundaries (`daemon/fcturn.go`) — **fixed**

Real, and the one guest→host transcript forgery the marker design exists to
prevent. `turnWriter.text` checked `markerLike` only when the frame began at a
line boundary, so a guest sending `"["` in one Text frame and
`"[turn_end]]\n"` in the next assembled a marker on disk that no frame ever
contained. `logParser.feedLine` accepts it: the turn ends early, the slot is
released while the real stream is still running, and the same trick forges
`[ts:]`, `[[tool]]`, `[[notify]]` and the rest.

The check is now on the LOGICAL line. Rather than buffer to the next newline —
which would stall a whole paragraph of streamed prose — `text` withholds the
first bytes of a line only while they are still an ambiguous prefix of a marker
(`[`, `[t`, `>>`): at most three bytes, only at a line start, released by the
next byte, a newline, an interrupting marker, or the end of the stream.
`TestTurnMarkerEscapingSpansFrames` splits every marker at every byte boundary
and asserts no unescaped marker line appears and nothing is dropped.

### M23 — Privileged job readers follow worker-controlled symlinks (`fcguest/main.go`) — **fixed, by removing the privilege**

The worker owns `/workspace/.cs/jobs/<id>/`, and the daemon asked the ROOT
guest agent to `cat`/`wc`/`head`/`tail` paths inside it. Ordinary shell tools
follow symlinks, so replacing `out` or `cmd` with one made root read a file the
worker cannot — a worker→root disclosure inside a guest whose `root=no` profile
says there is no path to root.

The remediation proposed hardening the readers (`openat2`,
`RESOLVE_NO_SYMLINKS`, a confined walk). That is the wrong shape of fix here,
because nothing in this path ever needed root. Every script the daemon sends
over `exec`/`exec_stream` reads or removes WORKER-OWNED data — the job dirs,
the session and venice-history files, a `tail -F` of a file claude code wrote
as the worker, `stat -f /workspace` plus `/proc/meminfo` — and the one that
signals (`interruptAgent`) targets uid-1000 processes, which uid 1000 may
signal. So `exec` and `exec_stream` now drop to the worker uid, exactly as
`run_script` and `shell_attach` already did. A planted symlink buys the worker
what it could read anyway, and the whole class goes with it. If a future exec
genuinely needs root it gets its own op: the privilege should be named at the
call site.

### M26 — Oversized cron step can crash the daemon via integer wraparound (`daemon/cron.go`) — **fixed**

Real and cheap. `parseField` checked only that a step was positive, and the
expansion is `for v := from; v <= to; v += step` — so a step near `MaxInt64`
wrapped `v` NEGATIVE on the second iteration, left the loop condition true, and
panicked on `mask[v]`. Reachable from every schedule-add path, the guest ctl
plane's `sched_add` included, none of which recovers: one malformed cron line
from a compromised guest takes the daemon down.

Steps are now bounded by the field's own span. Rejected rather than clamped: a
step wider than the range can only ever select `from`, so anything above the
span is a typo and accepting it silently would hide the typo.
`TestCronStepBounded`.

### M22 — Guest-facing framed channels permit resource exhaustion with incomplete frames (`daemon/fcframe.go`) — **fixed**
### M27 — Guest-facing vsock listeners allow unbounded resource retention (`daemon/fc.go`) — **fixed**

One finding twice. `fcAcceptLoop` spawned an untracked goroutine per accepted
connection with no admission limit, and the framed readers allocate whatever
length the peer declares (16 MiB on the turn channel) and then block in
`ReadFull` with no deadline. A guest could open connections in a loop,
declare a large frame on each, send nothing, and pin host goroutines,
descriptors and heap for the VM's lifetime.

Two bounds, chosen so each covers what the other cannot:

- **`fcMaxConnsPerGroup` (64), per group.** Every cost here is per connection,
  so capping connections caps all of them at once — which is why there is no
  separate frame-memory budget. Real traffic is one proxy connection per
  upstream request, one ctl connection, one gateway link and up to `groupSlots`
  (10) turn streams, so 64 is a wide margin. Per GROUP, so one guest cannot
  starve another; published-port connections get their own bucket because they
  arrive from the host side, and charging them here would let an outside caller
  starve the group's control channels.
- **`fcFrameBodyWait` (60s), from header to payload.** The header is waited on
  with NO deadline, deliberately: both channels are legitimately idle between
  frames — the ctl connection for the VM's lifetime, a turn stream for as long
  as the model thinks — so an idle timeout there would kill working
  connections. Once a header is read the payload is already allocated and no
  honest peer pauses mid-frame (the writer emits header and body in one write),
  so the deadline starts exactly where the peer's obligation does.
  `fcReadFrameBounded` peeks the header rather than reading it, which keeps the
  shared framing code byte-identical to the guest's.

`TestGuestVsockConnectionsBounded`.

### M24 — Recursive shared root mount allows worker mount events to propagate into fc-agent (`fcguest/main.go`) — **accepted**

True, and largely moot after M23. `earlyInit` marks `/` `MS_REC|MS_SHARED`
because rootless podman in the guest requires it ("/ is not a shared mount"),
and the worker has the userns prerequisites to create a mount namespace — so a
mount it makes can propagate back to the peer group fc-agent lives in. There is
no mount-namespace separation between the two inside the guest; there never
was.

It mattered because root-side code read paths the worker controls. It no longer
does: `exec`/`exec_stream` run as the worker (M23), and `writeWorkerFile` uses
`O_CREAT|O_EXCL|O_NOFOLLOW` (M13), which refuses a path a mount has made point
at an existing inode just as it refuses a symlink. What remains is a guest-
internal `root=no` weakening with no identified consumer, contained by KVM.

Narrowing the shared subtree is the right long-term shape, but it cannot be
changed blind: the constraint comes from podman's storage layout, and verifying
it needs a rootfs build and a booted VM with containers actually running. Not
worth breaking the guest's container support on an untested guess for a hole
whose consumers have all been closed by other means.

### M25 — Long-lived gRPC streams retain access after authorization revocation (`daemon/auth.go`) — **fixed**

Real, and pointed at exactly the streams revocation is usually about. A
server-streaming RPC receives its one request and never calls `RecvMsg` again,
so every check the interceptor made happened at connect time: revoking a role,
deleting a token, or narrowing `acl.json` left every attached `SubscribeGroup`,
`WatchState`, `SubscribeLogs`, `JobTail` and `AttachShell` delivering
transcripts and pty output with the access it had at connect.

`SendMsg` is the one call every delivery path shares — AttachShell's separate
guest→client goroutine included — so the re-check rides there, on the existing
`aclStream` wrapper, and covers every streaming RPC at once without touching a
handler. It re-resolves the identity by NAME from `tokens.json` (a deleted
entry is a revoked device; an edited one may have narrowed its roles, and
trusting the connect-time roles would defeat the point) and re-runs
`rolesAllowed` against the live ACL.

Rate-limited to one re-check per stream per 5s: each reads `acl.json` and
`tokens.json`, the same per-call reads the unary path already does and the
reason role edits need no restart. Per frame that would be a file read per
transcript chunk; per five seconds it is nothing. The cost is a revocation
taking up to five seconds to reach an attached stream, which is the right trade
and is pinned by the test. `TestStreamAuthzRevalidates`.

### M9 — Peer reports are promoted into MAIN's executable conversation (`daemon/report.go`) — **accepted**

Correctly described and already as bounded as the feature allows. A peer cannot
speak to main at all unless main first delegated with `reply:true`; the window
is one-shot, consumed on delivery and expiring in 24h, so a group can push at
most one turn into main per turn main pushed into it. The body is sanitized,
truncated, `> `-quoted, and prefixed and suffixed with an explicit frame:
peer-authored, findings not instructions, no spawn/stop/config/send/goal/sched
action because the report asked, and "the operator does not speak through
peers".

What the remediation asks for beyond that is not implementable here. "Enforce
authorization in the daemon for actions derived from peer data" requires the
daemon to know which of main's later actions derive from the report, which it
cannot. "Require operator confirmation before MAIN-derived actions affect other
groups" would end unattended orchestration, which is the product. And a
"distinct typed low-authority event" is what the framing above already is at
the only layer that exists — the model reads text either way.

A compliant model acting as a confused deputy for its delegate is a property of
delegation, not of this delivery path. The mitigations that DO apply are the
ones already in place: the solicited one-shot window, the rate bound it
implies, and the fact that a peer's own reach is `network=none` by default.

### M28 — Group-scoped Send authorization permits cross-session conversation hijacking (`daemon/grpc_server.go`) — **not a finding**

Named sessions are conversations, not tenants. They share one workspace, one
VM, one guest uid and one filesystem — `goals.go` says so outright about
concurrent goals ("goals that fight over the same files are the caller's
problem, same as two chat sessions editing one repo"). A caller who may `send`
to a group may already reach every file every session in it reads, so a
per-session authorization boundary would defend nothing while implying a
guarantee koto does not make. The boundary is the GROUP, and that is what the
ACL targets.

Same answer as M4, which asked for the same boundary on `attach_shell`.

### M29 — Workflow requests can provision unregistered groups without spawn authorization (`daemon/grpc_server.go`) — **fixed**

Mostly closed by M17 already: `ensure()` no longer creates, so a fired schedule
or a goal's first turn can no longer bring a group into existence. That left
the record itself — `SchedAdd` and `GoalSet` happily persisted work against a
name that does not exist, which can now only ever fail at fire time.

`registeredGroup` is checked at creation on both, so the caller gets an error
it can read instead of a schedule that silently never works, and `spawn` stays
the one verb that brings a group into existence.

### M30 — Group-scoped Metrics RPC discloses another group's latest metric (`daemon/grpc_server.go`) — **fixed**

Real and narrow. The ACL did its job — `metrics` is checked against
`MetricsReq.Group` — but the handler then set `GlobalMetric` from
`latestMetricAny()` unconditionally, which is the newest record from ANY group:
path, status, request id, token counts, rate-limit headers, provider org. So
asking about a group you may see answered with another group's newest request.

`GlobalMetric` is a global read, so it now needs the `"*"` target the
untargeted form of this verb already needs. `TestScopedMetricsOmitGlobal`.

### M33 — Provider-controlled session ID enables privileged path traversal during session clearing (`daemon/groups.go`) — **fixed**

Real. The id is captured off the PROVIDER's stream, stored in a file the worker
can also rewrite, and then substituted into
`rm -f /workspace/.claude/projects/*/"$(cat "$I")".jsonl`. Quoting stops
metacharacters; it does not stop `..`, so `../other/transcript` walks out of
this session's project directory into another's.

Validated on both sides, because the two are different times and different
writers: `fc-agent` refuses a non-conforming id on the way in (`validSessionID`
— Claude's are UUIDs, so hex and dashes), and the clear script re-checks the
file's content before it becomes a path component, since the file is writable
by something other than the code that wrote it.

### M34 — Host-address isolation fails open on stale or failed self-address refresh (`daemon/fcnet.go`) — **fixed**

Real, and backwards in the dangerous direction. `fcSelfIPs` is a DENYLIST —
the host's own interface addresses, which no guest may reach — and the refresh
discarded `net.InterfaceAddrs`' error, then stored the (empty) result as fresh.
A failed netlink dump therefore unblocked every host address for a full TTL, on
the L3 filter and the L7 proxy alike, since they share `fcClassifyDst`.

The last known-good list is now kept on error and `at` is left alone so the
next call retries immediately rather than after another TTL.
`TestSelfIPRefreshFailsClosed`.

### M35 — Empty or null ACL targets authorize global cross-group reads (`daemon/acl.go`) — **fixed**

Real, and a one-line hole with a wide answer. `json.Unmarshal` of `null` into a
string SUCCEEDS and leaves `""`, as does an explicit `""` — and the scalar
branch of `parseTargets` stored that as `names[""]`. The empty target is what
`targetOf` returns for a group-scoped request that OMITS the group: the
read-across-every-group form, documented to need `"*"`. So a malformed or
migrated grant read as a global one. (The list branch already skipped empties;
only the scalar branch did not, which is why it survived review.)

Empty and whitespace-only scalars now grant nothing. `TestEmptyACLTargetGrantsNothing`.

### M37 — gRPC lifecycle paths bypass the daemon-wide group quota (`daemon/groups.go`) — **fixed (with M17/M29)**

The same finding as M17 from the lifecycle side, and closed by the same split:
`ensure()` no longer provisions, so `Send` and `Restart` cannot bring a group
into existence, and the `ctlMaxSpawn` cap now applies to the `Spawn` RPC as well
as the ctl verb. `SchedAdd`/`GoalSet` refuse an unregistered target (M29).

The remainder of the remediation — per-identity quotas, rate limits on
lifecycle grants, separate accounting for ports/queues/workspaces,
transactional rollback of a failed provision — is a capacity-planning wish
list, not this finding. `fcHostMemAdmit` already refuses a spawn that does not
fit beside the running VMs, which is the bound that actually protects the host.

### M38 — gRPC GoalSet bypasses the protected main-group autonomous-goal restriction (`daemon/grpc_server.go`) — **fixed**

Real. `ctlDispatch` refused a goal on `main`; the `GoalSet` RPC did not, and
`goalSet` persisted and started the driver regardless. `main` is the group
holding the cross-group orchestration verbs — spawn, stop, send, config, sched
— so an autonomous, self-judged, multi-iteration loop there is the
judge-and-iterate machinery pointed at the fleet.

The check moved INTO `goalSet`, the creation boundary every caller shares, so
no plane can differ from another again.  `TestNoGoalOnMain`.

### M39 — Daemon-writable TUI path enables symlink redirection into operator-owned files (`tui/persist.go`) — **fixed**

Real, and a crossing in the wrong direction. `<state>/run/tui` is the one path
systemd's `ReadWritePaths=` leaves the daemon (tier 2) able to write, while the
TUI runs as the operator (tier 1), unconfined — so the daemon names the files
the operator opens for writing. A symlink dropped at `tui-state.json` or
`tui.log` redirected that write onto an operator-owned file. Same family as
audit L10, which removed `<state>/koto-tui` as an executable lookup path.

Every TUI open under that directory now uses `O_NOFOLLOW` (state load and save,
the debug log, its rotation). This does not harden every component of the path
— the run directory is the daemon's by design — but it closes the sink the
daemon can reach without also being able to replace its own state root.
`TestStateWriteRefusesSymlink`.

### M31 — Goal dispatch proceeds after failed conversation reset (`daemon/goals.go`) — **fixed**

Real, and it breaks the loop's central claim. `clearGoalSession` logged
`clearSessionContext`'s failure and returned normally, so the plan, worker and
judge dispatchers enqueued their turn regardless — and the enqueue can still
succeed, so a failed reset did not reliably stop delivery.

A fresh context per turn is the design, not a nicety: the worker iterates with
the filesystem as its memory, and the judge must review WITHOUT having watched
the work. A failed reset lets the plan phase inherit an execution context, an
iteration inherit the last one's, and — the one that matters — the judge
inherit the worker's own reasoning and rubber-stamp it, so the loop can end on
an unverified self-report. That is precisely what the judge exists to prevent.

The error now propagates and each of the three dispatchers pauses the goal with
`stalled` and a reason naming the reset, which also raises the operator
notification the pause path already carries. Pausing for a human beats running
without the isolation the loop advertises. `TestGoalTurnAbortsOnFailedReset`.

### M40 — Slot lifecycle races allow stale cleanup and VM-exit timing to corrupt or strand allocations (`daemon/queue.go`) — **fixed**

Two real windows, one missing concept. `sendNow` deferred `releaseSlot(g,
slot)` with no record of WHICH acquisition it was releasing:

- A stalled turn quarantines its slot and still runs that deferred release. If
  the quarantine was lifted in between — by the VM-exit reaper, or by a
  successful self-heal restart — the release saw an unquarantined slot, freed
  it, and deleted the busy flag of whichever waiter had since acquired it. Two
  live turns on one stream, which is the exact corruption slots exist to
  prevent.
- In the other order, the reaper cleared the quarantine before the stall path
  set it; a failed self-heal then left the slot quarantined-and-busy with no
  owner to free it. One of ten slots gone until the daemon restarted.

`acquireSlot` now returns a `slotHold` carrying a per-slot generation counter,
and `releaseSlot`/`quarantineSlot` take the hold and act only while it still
owns the slot. Both stale operations become no-ops.
`TestSlotHoldGenerations`.

### M32 — Install migration resurrects revoked client credentials from stale clone (`daemon/install.go`) — **fixed**

Real, and it undoes a security action. Revoking a device is deleting its line
from `clients.allow` and its entry from `tokens.json`. The additive merge ran on
every install, so an upgrade from a clone that predates the revocation put both
back — and the `client-*`/`token-*` files are copied too, so possession of the
old certificate and bearer token was enough to authenticate again with the old
roles. The previous audit (L10) had noticed the behavior and settled for
printing a line about it; printing is not a control.

The merge now runs only on a FIRST install, which is the migration it exists
for. Once the installed registries exist they are AUTHORITATIVE — and they will
exist, because the wizard mints into the state dir, so a clone-side identity is
legacy by construction. Clone identities the installed registry lacks are
NAMED, with the command that would add one back deliberately: silence would be
the worse failure of the two, since an operator who really did mint in the clone
needs to know why it does not work, and one who revoked a device needs to know
it stayed revoked. `TestInstallDoesNotResurrectRevokedIdentities`.

### M36 — Group-wide attachment spool can deliver orphaned uploads to unrelated turns (`daemon/fc.go`) — **fixed**

Real, and three bugs in one mechanism. Uploads were selected by a group-wide
mtime WATERMARK: every file newer than the last delivery rode along with
whichever turn went next. With concurrent sessions in one group that means two
turns can both select a file before either advances the watermark (one
session's image reaches another session's agent), the turn that was supposed to
carry it can find it already deleted, and a file whose `Send` was refused after
staging — queue full, validation error — is orphaned yet still delivered later
to an unrelated turn.

The message already carried the answer, which is why this needed no claim
protocol and no plumbing through the queue: `processAttachments` embeds
`[image: .cs/uploads/<name>]` for exactly the files that turn is about, and
`fcSendMsg` already receives the message. `fcTurnUploads` reads ownership out
of the text — exact, per turn, and unaffected by concurrency. The watermark is
gone.

Two supporting pieces. The names become `tar -C uploads` arguments and the
message text is not always the operator's (a ctl `send` from main carries
arbitrary text), so a reference must resolve to a plain basename that already
exists as a regular file in that group's own uploads dir. And staging is rolled
back when the enqueue is refused, with a 24h sweep for the one orphan that
remains possible — a daemon restart between staging and delivery — so the
pending-upload quota cannot be held by a file no turn will ever claim.
`TestUploadsAreOwnedByTheirTurn`.

### M41 — IPv4-mapped IPv6 loopback destinations bypass the host control-plane filter (`daemon/fcnet.go`) — **not a finding (pinned by a test)**

The premise is wrong, and measurably so. `net.IP.IsLoopback` does not only
recognize `::1`: it calls `To4()` first and, on success, tests `ip4[0] == 127`.
The same is true of `IsPrivate` and `IsLinkLocalUnicast`. So `::ffff:127.0.0.2`
— including the raw 16-byte form the frame filter actually sees — already
classifies as `fcDstCtl`, and the mapped v4 IMDS address already classifies as
link-local.

Verified against the stdlib rather than reasoned about, then pinned:
`TestMappedIPv4DestinationsClassifyAsIPv4` covers the finding's exact case plus
the mapped forms of loopback, unspecified, IMDS, RFC1918, tailnet and public.
The property is load-bearing and invisible in the code — `fcClassifyDst` reads
as though it only handled the v6 spellings — which is presumably how the
finding arose, and is reason enough for the test to exist.

### M42 — Guest-controlled job_done callbacks can inject turns into same-group sessions and exhaust notification state (`daemon/ctl.go`) — **fixed (the two halves that matter)**

Two real defects, both closed; the third ask is declined with a reason.

**The authorization half is real and specific.** `job_done` is self-attributed —
any group may report its OWN job — so the group is not the problem. The SESSION
is: it comes off the guest and was only charset-normalized. The ordinary send
path refuses reserved `goal-*` names; this callback did not, so a forged
completion landed in a live goal's worker or judge conversation. Those are the
sessions the design calls follow-only, and the judge's independence rests on
nobody else writing into them. Reserved names now fold into the group's default
session, with a warn line — folding rather than dropping, because the job result
still belongs to the operator.

**The exhaustion half is real.** The per-key buffer was bounded by the previous
audit (M4); the number of KEYS was not, and the name is the guest's to choose.
Each distinct one costs a pending slice, a debounce timer, and after the flush a
send queue and a worker goroutine that live for the daemon's lifetime.
`notifyMaxKeysPerGroup` (32 named sessions per group — a group runs at most
`groupSlots` = 10 concurrent turns) folds the overflow into the default session.
The default key is exempt from the count, since it is the fold target.

**Declined: authenticating the job ID.** The remediation wants the daemon to
record each job at mint and reject unknown ids. It cannot: under Firecracker the
job dir lives inside `workspace.img`, which is why the rc and output tail travel
IN the payload at all — `daemon/jobs.go` mirrors guest state on a TTL and is
routinely stale, so "require a matching live job" would drop legitimate
completions whenever the mirror had not refreshed. A callback token would have
to be minted by the guest-side `cs-job` and handed back by the same guest, which
authenticates nothing. What a forged `job_done` can now do is make its own group
wake its own default session with text the operator sees quoted and fenced —
which is what `cs-notify` already permits by design.
`TestJobDoneSessionGuards`.

### M44 — Goal session namespace collision permits cross-goal state and execution interference (`daemon/goals.go`) — **fixed**

Real. `resolveGoalName` reserved only the WORK session of each existing goal,
and only for goals with a Name. Two consequences:

- With `foo` live, the name `foo-judge` was accepted — and
  `goalWorkSessionFor("foo-judge")` is byte-identical to
  `goalJudgeSessionFor("foo")`.
- Unnamed legacy records were skipped entirely, even though `goalSessionSlug`
  falls back to their ID, so their sessions were occupied and invisible.

Not cosmetic: the derived `(group, session)` pair keys the send queue, the
per-turn context reset, the handoff capture, the transcript and the
reserved-session checks. A collision interleaves two goals' turns — and in the
`foo-judge` case, interleaves one goal's worker with another's JUDGE.

The occupied set is now built over both sessions of every goal in the group, by
effective slug. `TestGoalNamesReserveBothSessions`.

### M47 — Concurrent GoalSet requests can allocate the same run name (`daemon/goals.go`) — **fixed**

Real, and it produces exactly the collision M44 describes by a different route.
`resolveGoalName` took `goalLock`, built the occupied set, released it, and
`goalSet` took the lock again to insert — so two concurrent callers could both
find a name free and both take it.

Split into `resolveGoalNameLocked`, which `goalSet` calls inside its existing
critical section, so the check and the insert happen under one acquisition.
`listSessions` reads a file and is gathered outside the lock
(`groupSessionSet`). `resolveGoalName` survives as the unlocked wrapper.
`TestConcurrentGoalSetsGetDistinctNames`.

### M49 — Guest ctl plane can approve delegated goals without human authorization (`daemon/ctl.go`) — **fixed**

The most consequential finding in the batch, and the one that made M20's fix
decorative. The ctl handler refused `goal_approve` when the request named a
group other than the caller — but a goal MAIN delegated to a peer is
self-targeted from the peer's side. So the delegate approved its own delegated
plan, and the human gate that plan-first exists to open never existed:
`goals.go`'s own header says "park at awaiting_approval until a HUMAN approves
(GoalApprove has no ctl-plane counterpart)", and in practice it had one.

Self-targeted was never the question; who SET it is. `GoalItem.CreatedBy`
records the group that asked — `""` for the operator over gRPC — and the ctl
plane approves only what the caller itself set. A group's own plan-first goal
is still its own to approve, which is how a coordinator starts plan-first work
autonomously; a delegated or operator-set plan needs the human, who has
`GoalApprove` on the RPC.

A record written before the field existed has no creator and so needs the
operator too: the safe reading of "unknown provenance", and self-limiting since
goals turn over. The field is loop-internal and deliberately absent from the pb
conversions — it is an authorization fact, not something a client renders.
`TestCtlApprovesOnlySelfSetGoals`.

### M50 — Destroyed group goals remain readable after group-name reuse (`daemon/grpc_server.go`) — **fixed**

Real, and the same name-reuse hazard M14 found for schedules. `destroy()` called
`goalCancelOnDestroy`, which moves non-terminal records to `cancelled` — and
leaves them in `goals.json`. `GoalList` serves cancelled records, and group
names are reusable: the workspace and the port allocation go, the global goal
store did not. Both the ACL and the store key on the NAME, so a later group of
the same name inherited the old one's goal text, acceptance criteria, plans and
judge feedback, all agent-authored.

`delGoalsFor(g)` runs right after the cancel — cancel first so live drivers see
a terminal status and exit through their normal path, then remove. It sits
beside `delSchedsFor`, `disarmReport` and the event-ring drop, which close the
same hazard for their own state. `TestDestroyDropsGoalRecords`.

### M48 — Unbounded streamed transcript blocks permit daemon and TUI resource exhaustion (`daemon/logparse.go`) — **fixed**

Real. Every other limit around the parser is per line, per frame, per rate or
per file; none capped the BODY one open thinking or tool-output block
accumulates. On close `strings.Join` materializes a second full-size copy, and
that body then rides the event into the replay ring, History and the TUI — so
an unterminated (or merely enormous) block grew the daemon's heap by the size
of the guest's output, twice, per open block, with a copy retained downstream.

`blockBodyMax` (1 MiB) bounds the retained body. Past the budget the block keeps
STREAMING — the per-line `thinking` and `tool_result` events are unaffected, so
the operator still watches it arrive — and only the accumulation stops, ending
with an explicit `…[truncated]` marker so a clipped body is never mistaken for
a complete one. The budget is released on every exit from a block: the close
marker, and a `[[turn_end]]` arriving mid-block.
`TestOpenBlockBodiesAreBounded`.

### M46 — Mutable package resolution can poison the shared golden rootfs during a root build (`fcguest/Dockerfile.rootfs`) — **partly fixed**

The npm half is real and was inconsistent with the file it sits in: the Fedora
base is pinned by DIGEST, with a comment explaining why, and the line below it
installed `@anthropic-ai/claude-code` at whatever `latest` resolved to that day.
npm runs lifecycle scripts as root during the build, before the `node` account
exists, and the result is exported as the read-only root drive shared by every
VM — so a compromised publish would have entered every guest on the next
rebuild, and two rebuilds a week apart produced different images with no record
of the difference. Pinned to `2.1.268` via an `ARG`, with the recorded integrity
hash and the bump procedure in the comment.

The RPM half is **not taken**. Fedora offers no first-class repository
snapshot, so "pin every RPM version" means either hand-maintaining ~25 NEVRAs
that go stale the moment the mirror garbage-collects them — turning every
rootfs rebuild into a dependency-resolution puzzle — or standing up a private
mirror, which is infrastructure this project does not have. `dnf` already
verifies package signatures against the Fedora keys baked into the pinned base
image, which is the control the finding is really asking for; what is missing
is reproducibility, not authenticity.

`--ignore-scripts` is also not taken: claude-code's install needs its lifecycle
scripts, and disabling them would trade a working rootfs for a hardening step
the version pin already covers for the realistic threat (a malicious publish
landing silently).

**Not rebuilt.** `make rootfs` was not run — it would replace the live
`fcassets/rootfs.img` on a host with running groups. The pinned version matches
what the operator's own `claude` reports (2.1.268) and the spec resolves, but
the first rebuild is the real check.

### M43 — Installer executes a PATH-resolved `git` after privileged operations (`daemon/setup.go`) — **not a finding**

Same shape as M5 and M8: an attacker who can write a directory on the
operator's PATH already executes as the operator on their next shell command.
The "after sudo, so cached authorization multiplies it" framing does not add
anything — the attacker does not need koto's sudo timestamp when they control
what the operator runs next. The repository-local `git` config angle is real in
general but subordinate: it requires the operator to run the installer inside a
hostile checkout, which is the same tier-1 premise.

### M45 — Named Claude session state is readable and writable by every worker (`fcguest/turn.go`) — **not a finding**

Sessions are conversations, not tenants — the same answer as M4 and M28. Every
worker in a group runs as the same uid in the same VM with the same
`/workspace`, and that is the documented design: `goals.go` says so about
concurrent goals ("goals that fight over the same files are the caller's
problem, same as two chat sessions editing one repo"). A worker that can read
`sessions/<name>.id` can already read that session's transcript, its files and
its git history directly.

The remediation — a uid or user namespace per session, with the mapping held
daemon-side and an opaque per-turn handle — would be a real feature (in-group
multi-tenancy) rather than a fix. It is not in koto's model: the isolation
boundary is the GROUP, and the way to isolate two workloads is to give them two
groups, which costs one microVM and is what `/new` is for.

### M55 — Concurrent attachment uploads bypass the per-group pending-disk quota (`daemon/attachments.go`) — **fixed**

Real. `saveImage` read the spool's size with `dirBytes`, compared it, and then
wrote — a check and a use with nothing between them, while gRPC serves unary
RPCs concurrently. N image-bearing Sends could all read the same under-quota
total and all write, putting the spool arbitrarily far past
`maxUploadsPending` and consuming shared host storage, which is the fleet-wide
failure the quota exists to prevent.

One mutex around the check and the write. The write is tens of kilobytes and
the contention is per group, so nothing here wants a reservation counter.
`TestUploadQuotaHoldsUnderConcurrency` runs 64 concurrent senders at a
sixteenth of the budget each — four times the quota if the window were open.

### M56 — Guest-controlled job metadata can inject model notifications and host prompt logs (`daemon/notify.go`) — **fixed**

Real, and it slipped through because the obvious field was already handled. A
job's OUTPUT is sanitized and `> `-quote-fenced before it reaches the model —
the id and rc beside it were neither. `flushNotify` formats them as
`[job %s rc=%s]` into text the model reads, and `notifyDeliver` mirrors the same
string into the host log, so a newline in either forges further lines in both:
daemon-looking context inside the model's turn, and forged records for anything
reading `koto ctl logs`.

Both fields have narrow real shapes — `cs-job` mints ids from `mktemp`, an rc is
a wait status — so `ctlJobField` clamps them to bare alphanumerics plus `-_.`
and replaces anything else with `?`. Replacing rather than refusing: a result
with an unreadable id is still worth delivering, and rejecting the message would
let one malformed field lose the whole batch.

The finding's other half — that this path does not reject reserved `goal-*`
sessions — was fixed under M42. `TestJobMetadataFieldsAreClamped`.

### M51 — Unbounded Firecracker console output can exhaust the host filesystem (`daemon/fc.go`) — **fixed**

Real. The guest boots with `console=ttyS0`, and the VMM's stdout and stderr
were the console log file itself, opened `O_APPEND`. So anything running in a
guest could write to `/dev/ttyS0` forever and the bytes landed straight on the
state filesystem — the one every workspace image lives on, and whose exhaustion
remounts every guest read-only, which is the fleet-wide failure this project
has already been bitten by. `logSinkAppend`'s token bucket and 1 GiB ceiling
guard the TURN stream; nothing guarded this one.

`fcConsoleSink` hands the VMM the write end of a pipe and drains it into the
log, writing at most 8 MiB. Two decisions worth naming:

- The copier keeps READING after the cap and merely stops writing. That is the
  load-bearing half — a pipe whose reader walks away blocks its writer, and the
  writer is the VMM, so capping by closing would wedge the VM instead of its
  log.
- Truncated per boot rather than capped cumulatively. The console is boot
  debugging, so the boot that just happened is the one worth keeping, and a
  cumulative cap would leave a crash-looping VM's later boots writing nothing —
  exactly when someone is reading the file.

`TestConsoleSinkIsBounded` writes far past the cap and fails if the writer ever
blocks.

### M54 / M75 — Authorized Send callers can exhaust daemon resources with unbounded session creation (`daemon/send.go`, `daemon/queue.go`) — **fixed**

One finding, filed twice. The session name is caller-chosen and validated only
for shape, and nothing reclaimed what a name allocated: `enqueue` created a
buffered channel and a worker goroutine per `(group, session)` that lived for
the daemon's lifetime, and `registerSession` appended to a file that is
rewritten in full on every new name, copied into every group snapshot and
folded into the state hash — so an accumulated registry is work on every tick,
not just bytes on disk.

**Reclamation, not a cap, for the queues.** A quota alone would have been the
wrong fix: a group that legitimately uses many session names over weeks would
eventually wedge on it, and the failure would look like "sends stopped
working". `sendWorker` now retires its queue after `sessionIdleMax` (30 min)
with nothing to do, and `enqueue` recreates it on demand — so the steady state
is bounded by concurrency rather than by vocabulary. The teardown is safe
because `enqueue` holds `queuesMu` across BOTH the map lookup and the channel
send, so a job cannot slip into a queue being retired, and a queue with
anything buffered is never retired.

**A cap for the registry**, which is different in kind: it is advisory UI state
whose job is to populate a tree, so `sessionRegMax` (256) simply stops listing
past the cap. The session still works.

`TestIdleSessionsAreReclaimed`, `TestSessionRegistryIsBounded`.

### M52 — Symlinked group state escapes the trusted state root during VM staging (`daemon/fc.go`) — **not a finding (with one part already fixed)**

Every sink it names requires write access to the state tree or to the clone —
`groups/`, a group directory, `prompt.md`, the migration source. That is tier 1
by definition: the state dir is `0750` and owned by the operator, and a clone
is the operator's checkout. An attacker who can plant a symlink under either
can also edit `koto.env`, replace the binary, or read `creds/` directly. The
premise "a malicious checkout or state tree that an operator installs" is the
operator installing an attacker's software, which no path check survives.

The one part that was NOT tier 1 — `workspace.img` itself, which the podman-era
migration path deliberately treats as pre-existing untrusted input — is H1, and
is fixed.

`stateDirTrusted` is worth being clear about: it checks the state ROOT, and
that is all it claims to check (audit L9 added it for a pre-created `/tmp`
directory wearing the name). Extending it to a recursive descendant walk would
suggest a guarantee the trust model does not make, and would still lose to a
symlink planted after the check.

### M53 — Unbound guest turn streams let a compromised guest spoof turn lifecycle (`daemon/fcturn.go`) — **accepted**

Accurate, and already narrowed once. The previous audit (L4) closed the part
that was a boundary: a guest used to pick any slot in `[0, groupSlots)`, so
`fcExpectTurn`/`fcConsumeExpectedTurn` now require the daemon to have handed
that slot out, and a stream for an unissued slot is refused.

What remains is a guest forging its OWN group's transcript. That is not a
crossing: every byte in that transcript is guest-authored to begin with — the
guest runs the model, the tools and the output — so "forge text, tool and error
frames" describes a guest lying about work it alone performs, which no frame
protocol can prevent. The cross-SESSION effects stay inside one group, and
sessions are conversations sharing a VM, a uid and a filesystem, not tenants
(M4, M28, M45). Ending its own turn early or holding its own stream open is
self-inflicted: it costs that group a slot, and the slot pool, the stall
timeout and the quarantine path already bound that.

The per-turn capability would need a new field in `MsgReq` — a proto change and
a regenerated contract across daemon, guest and the out-of-tree Android client
— to authenticate the guest to itself. Declined on that basis, not on effort:
there is no second principal here for the capability to distinguish.

### M57 — Non-admin ACL grants can stop the protected main orchestrator (`daemon/grpc_server.go`) — **not a finding**

`stop: ["main"]` in `acl.json` is the operator writing down that this role may
stop main. Honouring it is not a bypass. The protection the finding compares
against is on the ctl plane, where the principal is a GROUP — a peer must not be
able to stop the orchestrator — and it still holds.

`destroy` refuses `main` because it is irreversible and takes the workspace
with it. `stop` is neither: the next send boots the group again, and the
operator's own TUI `/stop` with `main` focused is a routine action that this
change would break. Different verbs, different answers, for a reason.

### M62 — Legacy JobTail raw frames allow terminal escape injection (`tui/daemon.go`) — **fixed**

Real, and the compatibility branch is what makes it real. The current daemon
sanitizes every guest-authored byte it relays — but `startJobTail`'s `"data"`
case exists precisely for daemons that DON'T: one predating
`JobTailReq.parsed` ignores the flag and streams raw frames. The raw peek
renderer then hands the line to ANSI-aware wrapping and lipgloss, both of which
PRESERVE control sequences, so a job whose output contains OSC 52, a title set,
cursor control or a private-mode switch had it interpreted by the operator's
terminal the moment they hovered the row.

`scrubVT` — the same policy `daemon/sanitize.go` applies, already used for the
shell pane — now runs on every raw frame. A client cannot rely on the
sanitization of a peer it is explicitly written to be compatible with.
`TestJobTailRawFramesAreScrubbed`.

### M58 — Prompt data is inherited by Venice workspace commands (`sidecar/venice_stream.js`) — **fixed (with a named residual)**

Real. The turn's message and system prompt arrive as `MSG_B64`/`SP_B64`, and
the bash tool spawned with `env: process.env` handed both to every command it
ran and to all their descendants. That tool runs attacker-influenceable
workspace code — a repository's build script, an npm `postinstall` — which
could read the operator's system prompt (`prompts/global.md` + the group's
`prompt.md` + memory) out of its own environment without the model ever
choosing to reveal it, and exfiltrate it over whatever egress the group's
`network` profile allows. Base64 is an encoding, not a confidentiality control.

Both variables are now deleted from `process.env` immediately after they are
read. Deleting rather than filtering at the spawn site keeps it true for every
child, including ones added later. Verified by loading the script with the
variables set and checking they are gone by the time it exits.

**Residual — POSTPONED by the operator (2026-09-11).** The claude path has the
same exposure by a different route: `runClaude` passes the composed system
prompt as `--append-system-prompt <text>` on ARGV, which any process of the
same uid reads from `/proc/<pid>/cmdline` for the turn's duration. (The message
body goes over stdin and is not exposed.)

Not pursued, deliberately. Closing it needs the CLI to accept the prompt from a
file or stdin, which is an upstream capability question rather than a change
koto can make on its own — and the exposure is same-uid inside one guest, i.e.
the reach a worker already has to that session's transcript, files and git
history (see M45). Revisit if claude-code grows a file/stdin form of the flag.

### M59 — Stale Firecracker reaper corrupts replacement VM state after restart (`daemon/fc.go`) — **fixed**

Real, and the same missing concept as M40 one level up. Each VM's reaper
goroutine waits on `cmd.Wait` and then calls `abortInflightTurn`,
`releaseGroupQuarantine` and `clearGroupStalls` — all keyed on the GROUP alone.
`fcStop` deletes the registry entry and polls pid liveness without joining that
goroutine, so a `/restart` can register a replacement while the old reaper is
still pending. The old callback then lands on the new VM: completing a turn it
is still running (and `sendNow` treats any completion token as success, so the
queue advances before the real `turn_end`), freeing slots it owns, and clearing
stall flags it raised.

`fcVM.gen` identifies the BOOT rather than the group, and the reaper stands down
when `fcGenSuperseded` says a different VM is registered. Deliberately not "no
VM registered" — an ordinary stop or crash with no replacement still needs those
cleanups, and they are what wakes a turn parked on a `[[turn_end]]` that can
never come. `TestStaleVMReaperIsFenced`.

### M61 — Cross-session stream confusion after ambiguous agent delivery (`daemon/queue.go`) — **fixed**

Real, and the asymmetry is the point: fc-agent starts `runTurn` BEFORE it
sends its RPC response, so an `fcSendMsg` error — transport failure, lost
response, deadline — does not prove the turn was not accepted. `sendNow`
returned on that error and its deferred `releaseSlot` put the slot straight
back in the pool, where another session could take it while a guest-side
writer was still live on that stream. That is the exact interleaving slots
exist to prevent.

The stall path already had the right answer for the same reason, so the
delivery error now takes it too: quarantine, not release. The slot comes back
when the VM's death proves no writer survived
(`releaseGroupQuarantine`), not before. With M40's holds in place this composes
correctly — `sendNow`'s deferred release still runs and is correctly a no-op.
`TestAmbiguousDeliveryQuarantinesTheSlot`.

### M63 — Stop/Destroy can be bypassed by work admitted across the group lifecycle barrier (`daemon/queue.go`) — **fixed**

Real, and it undermines a property the docs already promise ("a stop DISCARDS
the group's pending traffic — so the VM stays down"). `stopGroup` drains the
queues and cancels in-flight turns, but nothing closed ADMISSION across that
window:

- A producer enqueuing after the drain gets a worker whose first act is
  `sendNow`'s `ensure()`, which boots the VM the operator just powered off.
- A job pulled off the channel but not yet recorded in `inFlightSess` is
  invisible to BOTH `dropQueued` and `inFlightSessions`, so it escapes the
  drain and the cancel alike.

`groupBarrier` closes admission for the duration. Checked in `enqueue` (which
already holds `queuesMu`) and again in `sendWorkerTurn`, because the second
check is the one that covers the channel-pull gap.

A COUNTER rather than a flag: `destroy` wraps `stopGroup`, and the barrier has
to survive the inner call and span the workspace removal and the `groups.json`
delete that follow it — the window where an escaped turn would re-register the
very name it is being removed under. `TestStopBarrierClosesAdmission`.

### M60 — Clear reports success without fencing queued and active work (`daemon/groups.go`) — **fixed**

Real, and it made `/clear` a suggestion rather than an operation. Both the
scoped and group-wide paths deleted the guest's conversation state and
truncated the host transcript with everything still live, so the reset need not
hold: a queued message ran against the conversation that had just been
forgotten, an in-flight turn still held the old claude session id and wrote it
back into `sessions/<name>.id` AFTER the `rm`, and a background tailer kept
appending to the log that had just been truncated. A clear the next turn undoes
is not a clear.

`clearFence` closes admission (the `groupBarrier` M63 added), discards what is
queued in scope, cancels the scope's in-flight turns and WAITS for them to
retire — bounded at 10s, because a turn that will not die inside that window is
already the stall path's problem and blocking `/clear` forever is worse than
one that logs which writer it could not fence.

Cancelling the in-flight turn is a deliberate part of the contract rather than
a side effect: "forget this conversation" while a turn OF that conversation is
running and will re-create its id is incoherent. The barrier is per group,
because admission is; the drain and the cancel honour the requested scope, so a
scoped clear leaves the group's other conversations alone.
`TestClearFencesQueuedAndActiveWork`.

### M67 — Unvalidated installer values allow systemd unit and EnvironmentFile injection (`daemon/install.go`) — **fixed**

Real, with the finding's own caveat worth keeping: an unrestricted sudo user
already has this authority. The case that matters is delegated sudo or
privileged automation supplying `-state`, where the caller is meant to choose a
DIRECTORY, not the unit's contents. `renderUnit` interpolates the value into
`WorkingDirectory`, `Environment=HOME` and `ReadWritePaths`, and `writeEnvFile`
emits raw `KEY=VALUE` lines; both are installed through sudo and read by the
privileged service manager, so a newline writes additional directives.

`unitSafeValue` rejects newline, carriage return and NUL on `-state` and on the
resolved claude path. Deliberately ONLY control characters: spaces, quotes and
backslashes are legal in a path and are the renderers' quoting problem, not an
injection — refusing them would turn a real directory name into an install
failure. Values PRESERVED from an existing `koto.env` need no check:
`readEnvFile` parses line by line, so a newline was already a line boundary
there. `TestInstallerValuesRejectControlCharacters`.

### M68 — Stale Firecracker PID can be reused to kill or misidentify another process (`daemon/fc.go`) — **fixed**

Real. The reaper reaped the child but left `fcVMs[g]` in place with its pid
still set, and every registry-first path — `fcRunning`, `fcPidOf`,
`fcHostMemCommittedMiB`, the resource sampler — asked only `pidAlive`, which
answers "is this number signalable". After reuse the daemon can call an
unrelated process this group's VM: report a dead group as UP (so the lazy boot
never fires and the jobs refresher hammers a dead vsock), count its memory
against the fleet cap and block legitimate spawns, sample its `/proc`, and — in
`fcStop` — SIGKILL it. The stronger `pidIsFirecracker` check guarded only the
pidfile path, and would not have helped between two VMMs anyway.

Two changes. `fcVM.start` records the process's start tick
(`/proc/<pid>/stat` field 22) at spawn, and `vmAlive` requires pid AND start to
match on every one of those paths — including immediately before the SIGKILL,
not just in the wait loop, since the VMM can exit in that gap. And the reaper
now DELETES its own registry entry and pidfile, so there is no reaped-but-still-
registered state to misread; it deletes only if the entry is still its own
generation, so a replacement's entry survives (M59).

An entry with no recorded start time falls back to `pidAlive`, so an upgrade
does not declare every running VM dead. `pidStartTime` parses from the last
`)`, because field 2 is the comm and may contain spaces and parens — the detail
behind a long line of `/proc` parsing bugs.
`TestVMIdentityGuardsAgainstPIDReuse`.

### M64 — Transcript clear does not invalidate in-memory replay and subscriber delivery (`daemon/events.go`) — **fixed**

Real, and the complement to M60: that one fenced the WRITERS, this one is the
second copy. The clear handlers rewrote the persistent logs and deleted the
guest's conversation state, and left the replay ring alone — so a client
reconnecting with a pre-clear `since_seq` was replayed exactly the frames the
clear had just erased, out of memory, with nothing on disk to show for them.
`destroy` already dropped the ring; `clear` did not, and `clear` is the one
people run *expecting* the transcript to be gone.

`clearEventRing` drops the ring and raises `ringFloor` to the current seq, so
any older cursor replays as `gap` — the client's cue to drop its view and
refetch `History`, which is exactly right because History now reflects the
cleared log. Dropped rather than filtered, session-scoped clears included: a
partial frame's session attribution is sticky, so a half-filtered ring would be
a worse answer than a refetch. `TestClearInvalidatesTheReplayRing`.

The finding's last clause — frames already queued in a subscriber's channel —
is left alone deliberately. Those are at most a buffer's worth of frames the
client had already been sent moments earlier, the `gap` that follows tells it
to discard its view anyway, and reaching into live subscriber channels to
rewrite history in flight buys nothing for the complexity.

### M65 — Group-only report capability accepts stale or canceled delegation results (`daemon/report.go`) — **partly fixed, partly rejected**

Three claims, three different answers.

**Fixed: the window outliving its work.** A stop discards the group's queued
messages and cancels its in-flight turn, so an armed window then describes
delegated work that will never run — while keeping a one-turn channel into main
open for up to 24h. `destroy` already disarmed for exactly this reason; `stop`
is the other lifecycle edge that discards the traffic, and now does too.
`TestStopDisarmsTheReportWindow`. Not `/restart`: it deliberately KEEPS the
backlog, so a queued delegation still runs and its window is still owed.

**Already fixed elsewhere: the destroy race.** "Destroy disarms before teardown
is complete, so a concurrent reply-enabled send can re-arm the same group name"
— the ctl `send` arms atomically with its enqueue, and M63 now closes admission
for the whole of destroy, so that enqueue fails and nothing arms.

**Rejected: the delegation nonce.** A delayed report from delegation A being
routed to delegation B's session is the DOCUMENTED semantics — one window per
peer, newest delegation wins — and both sessions are main's own, so nothing
crosses a boundary; the misattribution is between two of the orchestrator's
conversations about the same peer. Making it exact would require the peer to
ECHO a token it was handed, and the peer is a language model: a capability an
LLM must copy back out of its prompt is not a capability, it is a suggestion
with extra steps. The invariant that actually matters — a group can push at
most one turn into main per turn main pushed into it — holds on the group key
alone.

### M66 — Untrusted goal and agent output is replayed as trusted instructions across agent sessions (`daemon/goals.go`) — **fixed for the agent-authored fields**

The part that is real is the AGENT-authored text, and it is real: the judge's
feedback, the previous worker turn's closing handoff, and the worker's evidence
note were interpolated raw into a LATER agent's instruction stream. A label
like `REVIEWER FEEDBACK:` is not a boundary — the next worker reads the block
with the same weight as the harness's own words — and the handoff is captured
out of a transcript that a prompt-injected worker (a hostile repo, a poisoned
tool result) can write into. The two coordinator messages are the sharpest: they
land in MAIN's session, which holds the cross-group verbs.

All four are now fenced with `reportQuoteBody` — the same `> ` quoting main's
delegation callbacks already use — and labelled as quoted agent text, a claim
to verify rather than an instruction. That is not a guarantee; no in-band
framing is, against an instruction-following model. It is the boundary this
codebase already had for exactly this problem, and it was missing here.
`TestGoalPromptsFenceAgentAuthoredText`.

The goal's own `Text` and `Criteria` stay unfenced, deliberately: they ARE the
instruction, supplied by the operator or by main under `goal_set`, which is
authorized. Fencing them would mean the worker has no instruction left.

### M73 — JSON-escaped terminal controls bypass event sanitization in tool rendering (`tui/view.go`, fixed in `tui/model.go`) — **fixed**

Real, and a genuinely good catch: the sanitizer ran on the wrong side of a
decoding boundary. `sanitizeEvent` scrubs `Event.Input` while it is still
SERIALIZED JSON, and in JSON an escape is the six printable bytes `\u001b` —
nothing for a terminal-control scrub to find. `formatTool` then
`json.Unmarshal`s it, turning them back into real control bytes, and drops the
value straight into the rendered tool line, which lipgloss preserves verbatim.
So a Bash command, a file path or a WebFetch url could carry OSC 52, a title
set or cursor control through to the operator's terminal.

Fixed at the decoding boundary — `pick` scrubs every value it extracts — which
is the one place guaranteed to run after the decode and before the format. The
fallback path that renders raw JSON needs nothing: there the escapes are still
literal `\u001b` text, which is exactly what should be displayed.
`TestToolInputControlsAreScrubbedAfterDecoding`.

### M70 — Unbounded send payloads exhaust daemon memory through per-session queues (`daemon/queue.go`) — **fixed**

Real, and the gap the previous bounds left open. `sendQueueDepth` (64) is per
SESSION, and after M54 the number of live sessions is bounded by idle
reclamation rather than by a cap — so a caller with Send permission could
multiply retained payload across session names while turns were slow, with no
limit on any individual message either.

Two bounds at the one admission point both planes share:
`sendMsgMax` (1 MiB) per message — far beyond any real prompt, and well under
the proxy's own 64 MiB request cap that this sits upstream of — and
`sendQueuedBytesMax` (16 MiB) for everything one group holds queued across all
its sessions. Every path that takes a job OFF a queue releases its bytes, the
worker's receive and the drain alike, so the accounting cannot drift from the
channels. `TestSendPayloadsAreBounded`.

### M69 — Parsed JobTail retains unbounded open blocks in the daemon (`daemon/logparse.go`) — **already fixed (M48)**

Same defect, reached through `JobTail`'s parsed mode instead of the group
tailer: both feed the same `logParser`, and the cumulative block budget M48
added (`blockBodyMax`) applies to every caller of `feedLine`. Nothing further
to do; noted so the two reports are not mistaken for two bugs.

### M71 — Pre-queue attachment persistence can orphan uploads and reuse them in later turns (`daemon/grpc_server.go`) — **already fixed (M36)**

The same finding as M36, filed from the lifecycle end, and closed by the same
change: uploads are owned by the turn whose message references them
(`fcTurnUploads`), staging is rolled back when the enqueue is refused, and a
24h sweep collects the one orphan a daemon restart can still leave. The
group-wide mtime watermark it describes no longer exists.

### M72 — Credential and private-key files retain or inherit non-private permissions (`daemon/install.go`) — **fixed (the migration path); the rest declined with reasons**

The primary defect is real and specific: `copyFile` passes
`info.Mode().Perm()` to the destination, which is right for the guest assets —
the firecracker binary has to stay executable — and wrong for secrets. A clone
whose `creds/` was created under a loose umask, or copied off another machine,
materialized a group- or world-readable CA key, client private key or bearer
token in the installed state dir. A destination mode is a property of what the
file IS, not of where it came from.

`copySecretIfAbsent` writes credentials at a fixed `0600`, with
`O_EXCL|O_NOFOLLOW` — "create it fresh or not at all" is both the containment
and the already-present check. `tightenSecretDir` forces `0700` on an existing
`creds/`, which `MkdirAll` leaves alone, and the directory mode is the last
line of defense for every file under it.
`TestCredentialCopiesAreOwnerOnly`.

Declined, with reasons. **Readers validating owner/mode before loading**: the
state dir is `0750` and owned by the operator, and `stateDirTrusted` already
checks the ROOT at install time (audit L9); a per-read stat would be a check
against a use that follows it, and the thing it defends against — another local
user writing into the operator's state dir — is tier 1 (see M52). **The
"creation mode only" overwrite paths**: every one of them writes to a path the
installer or the PKI created at `0600` in the same run, so there is no
pre-existing inode with looser bits to inherit; making each one `fchmod` would
add ceremony without changing an outcome.

### M74 — Guest-controlled daemon error text reaches terminal output without sanitization (`daemon/grpc_server.go`) — **fixed**

Real, and backwards from where the care already was. RunScript's DATA frames go
through `newChunkSanitizer`; its ERROR frame went straight to `fail(k.Error)`.
An error is the frame a guest can produce on demand — by making the operation
fail — and it reaches the operator's terminal and the TUI's debug log, where
nothing downstream strips control sequences (lipgloss styling and ANSI-aware
width measurement are not sanitizers).

Sanitized in three places, chosen so the whole class is covered once:
`fcAgentCall`'s `resp.Error` at the trust boundary — which catches every
handler that turns a guest error into a protobuf error field, not just this one
— plus RunScript's and AttachShell's error frames, which arrive on their own
streams. AttachShell's DATA stays raw deliberately: that is pty output the TUI
renders through a terminal emulator, whereas the error string is a daemon
message the client prints directly. `TestGuestErrorTextIsSanitized`.

### M77 — Installer accepts attacker-controlled descendant symlinks in the state directory (`daemon/install.go`) — **not a finding**

M52 by another route. Every descendant it names lives inside a `0750`
directory owned by the operator, so pre-seeding one requires the write access
that already implies replacing the binary or reading `creds/` outright. And a
no-follow walk of the descendants would not settle it anyway: the check would
still precede the use, and `stateDirTrusted` deliberately validates the ROOT —
the property that actually holds — rather than implying a guarantee about a
tree that can change underneath it.

One part of the description is simply wrong about the consequence: `.claude` is
a symlink to `creds/` the installer creates, and `claude_login.go` already
treats an existing non-symlink there as the normal dev-clone case and resolves
credentials the way the proxy does rather than trusting the path.

### M76 — Background subagents omit the harness system prompt (`sidecar/cs-job`, fixed in `sidecar/cs-subagent`) — **fixed**

Real. `--system` was optional and `cs-job spawn` passed none, so a background
subagent ran with NO harness policy — no `prompts/global.md`, no per-group
`prompt.md`, no memory — while still carrying
`--dangerously-skip-permissions` on the claude path and tool execution on the
venice path. A delegated prompt is exactly the content most likely to carry an
injection, and it was the one call that ran unguided.

`cs-subagent` now defaults to the TURN's composed prompt, which `fc-agent`
already writes per session as `.cs/system-prompt-<session>.md` before every
turn, selected by `$KOTO_SESSION` from the turn env. An explicit `--system`
still wins. Verified against a stubbed provider: default session, named
session, and explicit override all resolve correctly.

The severity framing in the finding is right and worth keeping: this is
behavioral guidance, not an authorization boundary. What contains a subagent is
the microVM, the `network` profile and the `root` profile, exactly as for every
other turn. But "the harness is the only source of context" is a stated
property of this system — `--bare` exists for it — and a background job is not
an exception to it.

### M78 — Authenticated JobTail streams can exhaust daemon and guest resources (`daemon/grpc_server.go`) — **fixed**

Real. Each live tail costs four things — a daemon goroutine, an HTTP/2 stream,
a vsock connection, and a `tail -f` PROCESS in the guest — and cleanup is tied
to the individual RPC context ending, with no admission limit anywhere. An
authorized reader reopening streams for a known job accumulated all four.

`jobTailMaxPerGroup` (8) and `jobTailMaxGlobal` (64), released on every
termination path. Per group as well as globally, because the guest-side cost
lands in `fcMaxConnsPerGroup` (M22/M27) — a budget SHARED with that group's ctl
and turn channels, so letting job tails exhaust it would take the group's
control plane down with them. The numbers are an exhaustion backstop, not a
scheduler: the TUI opens one tail per hovered row, so several attached
operators stay far below. `TestJobTailsAreBounded`.

### M81 — Concurrent session clearing can discard active log writes and misattribute later output (`daemon/sessions.go`) — **fixed**

Real. `filterLogSession` is a read-rewrite-rename TRANSACTION on a file that
the turn-frame appenders write concurrently — and it took none of the per-path
locks they use. A line appended after the snapshot is dropped by the rename,
and losing a `[[turn_end]]` that way parks its send worker until the stall
timeout, which is the expensive failure this codebase keeps running into.

Now held under the same `logWriteLock(path)` every append takes, across the
READ as well as the write: the snapshot is what the rename asserts is current,
so it has to be inside the transaction.

The shared `<path>.tmp` is gone too. The lock serializes clears of one STREAM,
but a group has ten, and two clears racing through one shared temp name could
install each other's stale content — `os.CreateTemp` costs nothing and removes
the case. `TestFilterLogSessionIsSerializedWithAppends` runs 200 concurrent
appends against a filter and fails if any completed append is lost.

The finding's last clause — a targeted clear removing a session marker while
that session has an ACTIVE turn — is closed from the other end by M60, which
cancels the scope's in-flight turns and waits for them before deleting
anything.

### M82 — Cumulative partial-line events amplify guest output into resource exhaustion (`daemon/logtail.go`) — **fixed**

Real, and an amplifier rather than a leak: the retained buffer was already
bounded (`tailMaxPartial`, audit L5), but the whole cumulative buffer was
turned into an EVENT on every read — rebuilt, protobuf-converted, sequenced,
and fanned out to every subscriber. A newline-free stream therefore cost
multi-megabyte frames per read iteration, multiplied by subscriber count,
against a 1 MiB/s input throttle.

Fixed by bounding what goes on the WIRE (`tailMaxLiveEvent`, 64 KiB) while
leaving the parse buffer large, since a real line that eventually ends still
has to parse.

Truncated rather than sent as a delta, deliberately: the frame's semantics are
REPLACE — `recordEvent` keeps one live partial per session and supersedes it,
and the TUI assigns rather than appends (`streamBuf[key] = ev.Text`) — so a
delta would render as a fragment for anyone who joined mid-line. The tail is
what a client can display of an unterminated line anyway. Cut on a rune
boundary, so a truncated partial never carries half a code point into a
renderer. `TestPartialLineEventsAreBounded`.

### M80 — Authorized job-observability RPCs permit unbounded guest-agent execution (`daemon/grpc_server.go`) — **fixed (concurrency); context propagation declined**

Two real halves.

**Duplicate execs for the same group.** `Jobs` called `refreshJobs` straight
through, bypassing the in-flight marker the background refresher uses — so N
concurrent callers asking about the SAME group each issued their own guest
exec: N shells in the guest, N host connections, N goroutines, all producing
the same answer. `refreshJobs` now takes that marker and serves the current
snapshot when a refresh is already running, which is both cheaper and no
staler than waiting for a duplicate of it. (`refreshJobsNow` is the unguarded
body, for the background path that has already claimed the marker.)

**Aggregate concurrency.** The per-call timeout bounds ONE operation, not how
many. `jobQueryMaxGlobal` (16) covers `Jobs` and `JobLogs` together — the
remaining fan-out is across DIFFERENT groups, since same-group duplicates now
collapse.

**Declined: threading the RPC context into the guest call.** It would let a
client's cancellation free a slot sooner. But the work it would cancel is
already bounded by a 15s per-call timeout and now by the admission cap, so what
it buys is latency on slot return, not a bound — and the cost is `context` on
`fcExec`, `fcAgentCall` and `fcHostDial`, i.e. the whole guest-call path, for
that. Recorded rather than done.

`TestJobQueriesAreBounded`, `TestRefreshJobsSharesTheInFlightGuard`.

### M83 — Detached jobs can outlive turns and exhaust persistent workspace resources (`sidecar/cs-job`) — **partly fixed, partly by design**

**Fixed: `rm` now terminates before it forgets.** A detached job outliving its
turn is the FEATURE — `setsid` is there so a turn's process-group kill does not
take the background work with it — but deleting only the bookkeeping left the
job running against a directory nobody tracks: still burning CPU and workspace
disk, still writing to a now-unreferenced `out` file, and no longer listed by
`cs-job list`. The mint records the job's pgid (the `setsid` child is its own
session and group leader, so its pid IS the pgid), and `rm` sends TERM then
KILL to the negative pid so descendants go too. Verified against a running job,
a completed one, and a legacy dir with no pgid file.

**By design: the rest.** "Run jobs under a daemon-owned supervisor" with
non-overridable admission, wall-clock deadlines and cgroup limits describes a
different product. The guest IS the supervision boundary here: a job's CPU,
memory and disk are the microVM's, bounded by the `size` preset and the
host-side IO/cpu/memory limits, and guest filesystem fullness raises an operator
alert at 80/90% (`resources.go`). `CS_MAX_JOBS` being caller-overridable is
real and deliberate — it is the agent's own concurrency knob inside its own VM,
not an authorization boundary, and an agent that wants more parallelism in its
own sandbox may have it.

### M79 — Untrusted background-task paths are opened by the guest-root agent (`daemon/send.go`) — **premise removed by M23**

The finding is "a worker-controlled path is opened by a ROOT process". That
privilege is gone: `exec`/`exec_stream` now run as the worker (M23), so the
background tailer opens what the worker could already open, named in text the
worker itself wrote, and copies it into the worker's own conversation — which
it could do with `cat`. No boundary is crossed.

What remains is not a privilege issue: a FIFO or other special file could block
the tailer, bounded by the existing 10-minute cap and now by
`fcMaxConnsPerGroup` (M22/M27), and the content lands in the group's own
transcript, whose growth is bounded by `fcLogSinkWait` and the 1 GiB ceiling.

### M84 — Restart authorization can register arbitrary new groups (`daemon/groups.go`) — **already fixed (M17)**

`restart` goes through `ensureLocked`, which since M17 refuses a name that is
not registered in `groups.json`. Creating a group is `spawnEnsure`, reachable
only from the three admission points, and the `Spawn` RPC now enforces
`ctlMaxSpawn` as well. Noted so the report is not mistaken for a separate bug.

### M86 — `koto ctl shell` forwards guest terminal-control bytes to the operator terminal (`daemon/ctl_cli.go`) — **fixed**

Real, and the gap is exactly where the finding says: the TUI renders guest pty
bytes through a terminal EMULATOR — they become a screen grid and only safe
cell content reaches the real terminal — while the CLI attach puts the terminal
in raw mode and relays bytes straight to `os.Stdout`, because that is what makes
vim, tmux and colours work. So a process in the guest could drive the
operator's terminal directly: set the clipboard through OSC 52, retitle the
window, or provoke a query reply, which makes the terminal write attacker-chosen
bytes back INTO the guest's stdin.

Blanket sanitization is the wrong fix — strip CSI and the attach stops being a
terminal. `shellFilter` removes only the classes that are never needed to DRAW
and are the ones that do harm: OSC, DCS/APC/PM/SOS (and their C1 forms), and the
CSI queries `n` (Device Status Report) and `c` (Device Attributes). Cursor
movement, modes, scrolling regions and SGR pass through byte for byte.

Stateful, because a chunk boundary falls wherever the vsock read ended — the
test splits a hostile sequence at every byte offset and asserts the same output
each time — and the CSI buffer is bounded so a malformed sequence cannot buffer
without limit or swallow the rest of the stream.

The two other `os.Stdout.Write(ev.Chunk)` sites in this file need nothing:
`runscript` is sanitized daemon-side unless `-raw` (audit M9a) and `job-tail`
always is.

### M88 — Preflight remediation makes /dev/kvm globally writable (`daemon/setup_checks.go`) — **fixed**

Real, and it is koto's own preflight doing the asking. `checkKVMAt` told the
operator to install `KERNEL=="kvm", GROUP="kvm", MODE="0666"` and said nothing
about what that grants. On Fedora it changes nothing — 0666 is the distro
default — but on a host shipping `0660 root:kvm` it opens the KVM interface to
every local account.

The requirement itself is not negotiable and the comment explains why: the VMM
runs as a per-VM uid inside the daemon's user namespace with a deliberately
empty supplementary group set, and `newgidmap` can map only the operator's own
gid and their `/etc/subgid` range — the host's `kvm` gid is in neither, so no
group membership reaches the jailed process. What DOES reach it is a POSIX ACL
naming the host uids the VMM actually runs as, and those are a contiguous band:
the operator's subuid base plus `fcJailBaseUID`, one per possible group port.

So the narrow grant leads now, with the real numbers filled in from
`/etc/subuid` (rendered on this host as `554288..554388`), and the world-access
rule is still offered — it is most hosts' status quo — but labelled with its
cost. `TestKVMRemediationLeadsWithTheNarrowGrant` pins the ordering, the
presence of both, and that the band is concrete rather than a placeholder when
the subuid range is readable.

### M89 — Concurrent group recreation during destroy can corrupt replacement VM state (`daemon/groups.go`) — **fixed**

Real, and the complement to M63. That closed admission on the SEND queue;
spawn and restart come in through `ensure()`, which serializes on `groupOpMu`
instead — and `stopGroup` took that mutex, powered the VM off, and RELEASED it
before `destroy` had removed anything. In the window, `ensure()` saw a group
that was merely not running, created the workspace, allocated a port and
registered a replacement VM; `destroy` then continued on the group NAME alone
and deleted the replacement's workspace and runtime artifacts while its VM
stayed registered and alive.

`destroy` now holds `groupOpMu` across its whole cleanup, so the group cannot
be recreated until its name is gone from `groups.json`. That needed `stopGroup`
split into `stopGroupPrepare` (pause goals, discard queued traffic, disarm the
report window, cancel in-flight turns) and `stopGroupLocked` (the power-off),
since Go mutexes are not reentrant and `destroy` calls both from inside the
lock. `TestDestroyHoldsTheGroupLockThroughout`.

### M85 — Interrupt can cancel a subsequent session turn via a TOCTOU race (`daemon/queue.go`) — **fixed**

Real. Turn state is keyed by `(group, session)` and REUSED, and `Interrupt`
asked `sessionBusy` under one lock acquisition and cancelled under another. If
the observed turn retired in the gap and the worker had already started the
next queued prompt, the cancel closed the NEW turn's channel — aborting a
prompt nobody asked to interrupt.

The turn now has an IDENTITY the interrupt can name: `sessionTurn` returns the
running turn's cancel channel (made fresh per turn by `sendWorkerTurn`), and
`cancelTurn` closes it only while it is still the current one. A successor
holding the key is left alone, and the RPC reports "no turn in flight", which
is the truthful answer about the turn the caller meant.
`TestInterruptCancelsOnlyTheObservedTurn`.

`requestTurnCancel` survives unchanged for `stopGroup` and `clearFence`, which
genuinely mean "whatever is running now" — they are cancelling the session, not
one turn of it.

**Residual, stated:** `interruptAgent` signals the guest by SESSION, matching
worker processes rather than a pid, so the same race could in principle deliver
a SIGINT to a successor turn's worker. Narrowing it needs the guest to report
the turn's pid back to the daemon — a protocol change — and the window is
small, same-session and same-caller-authority. The channel half, which is what
actually discards a queued prompt, is closed.

### M90 — Unbounded Venice transcript can exhaust a session workspace and deny turns (`sidecar/venice_stream.js`) — **fixed**

Real, and the failure mode is the notable part: Venice's chat API is stateless,
so the WHOLE transcript is replayed on every request. An unbounded one grows
the session workspace, grows every request, and eventually exceeds the proxy's
64 MiB body cap — at which point that session can make no further Venice turns
at all until someone clears it. Silent up to that point, then total.

`trimHistory` caps the serialized transcript at 4 MiB, far more than any Venice
model accepts, so it discards nothing the provider would have used. Applied on
SAVE and on LOAD, because a transcript written before the cap existed must not
be replayed whole either.

Oldest-first, which matches how the model's own context window behaves and what
a turn is likely to need. Whole entries only, never partial — a truncated
`tool_calls` entry without its matching `role:"tool"` reply is a malformed
conversation the API rejects, so a naive byte cap would turn a large session
into a broken one; the trim keeps dropping past orphaned tool replies until the
surviving head is coherent. Verified directly against the function: a 20 MB
transcript trims to 4.1 MB, keeps the newest turn, and leaves no leading
orphan.

### M87 — SchedAdd permits unbounded persistent schedule growth across group names (`daemon/grpc_server.go`) — **fixed**

Half of it was already closed by M29: `SchedAdd` now requires the target group
to be registered, so the set of usable names is bounded by `ctlMaxSpawn` rather
than by the caller's imagination.

What that leaves is still worth bounding. 100 groups × `schedMaxPerGroup` is
10,000 records, and every one of them is marshaled and rewritten to
`schedules.json` on each add, copied and sorted on each list, and walked by
`cronLoop` every single minute. `schedMaxTotal` (2000) is the ceiling on that
walk. `TestScheduleStoreHasADaemonWideCap`.

The remaining suggestions — per-caller ownership of schedules, rate limiting
creation, bounding due work per tick — are product design rather than this
finding. A group's schedules fire through `enqueueSend`, which is already
bounded per session and per group (M70) and closed during stop/destroy (M63).

### M91 — Clearing the replacement network policy leaves legacy WAN egress active (`daemon/groups.go`, fixed in `daemon/config.go`) — **fixed**

Real, and it fails in the direction that matters: an operator REVOKING egress
gets told it worked while the guest keeps it. `applyConfig`'s clear deleted
only the key it was given, and `groupConfig.network()` falls back to the legacy
`internet` key — so `-network=` on a pre-migration config left
`internet:"full"` resolving to `wan`, and both the frame filter and the L7
proxy gate read that resolution.

Clearing either spelling now clears both, and SETTING `network` deletes the
legacy key too, so no stale spelling survives for a later read to resolve
through. The legacy write path already migrated forward; it stays that way.
`TestClearingNetworkClearsTheLegacyKey`.

### M93 — Claude executable parent directories bypass the daemon's ProtectHome boundary (`daemon/install.go`) — **fixed**

Real, and the unit's own comment made a claim the code did not keep: "Nothing
else of $HOME is visible". `claudeBindDirs` returned `filepath.Dir(bin)`
unconditionally, so a claude at `/home/<user>/claude` bound the WHOLE home
read-only into the service namespace. Read-only is not containment here — the
daemon is tier 2 and the home is tier 1's — so every unrelated credential,
repository and ssh key in it became readable by the process that already holds
the OAuth token.

`protectHomeRoot` refuses to bind a whole protected home: `/home/<user>`,
`/root`, `/run/user/<uid>` and their parents. The native installer's
`~/.local/bin` is unaffected, and a symlinked binary still gets its target's
directory bound, which is what keeps a version update working without a restart.

The failure mode is deliberate and LOUD rather than silent: refusing the bind
means the daemon cannot exec claude, and a silent refusal would reproduce
exactly the outage this binding was added to fix (three OAuth expiries in 36h,
2026-09-05). `installClaudeBin` now says so at install time and names the fix —
move the binary — instead of quietly handing over the home.
`TestClaudeBindRefusesAWholeHome`.

### M94 — Age-trimmed partial events remain pinned by ringPartial (`daemon/events.go`) — **fixed**

Real. `ringPartial[g][session]` indexes a session's live partial BY POINTER so
the next frame can supersede it, and the age trim dropped events off the front
of the ring without touching that index. A partial whose event had aged out
therefore kept the event — and its payload, which before M82 could be
megabytes — alive until another event for that exact session arrived. For a
session whose turn ended without one (a stopped, wedged or restarted guest)
that is never, and session names are caller-chosen.

The trim now reconciles: any indexed partial found in the prefix being
discarded is dropped with it, and an empty per-group index is removed. A
partial still IN the ring keeps its entry, because supersession depends on it.
`TestAgeTrimReleasesStalePartials`.

### M95 — Authorized clients can exhaust daemon capacity with unbounded streaming RPCs (`daemon/auth.go`) — **fixed**

Real, and the finding is right that the control belongs at the shared boundary.
`authStream` authenticated, verb-checked and handed straight to the handler —
and each handler then retains something for the life of the call: a subscriber
registration and its buffered channel (`SubscribeGroup`, `WatchState`,
`SubscribeLogs`), or guest-side execution and an attachment (`JobTail`,
`AttachShell`). Nothing bounded how many an authenticated caller could open.
gRPC keepalives police dead CONNECTIONS, not live streams.

Admission now rides `authStream`, so every streaming RPC is covered by one
change rather than five. Per-identity as well as global, so one client cannot
crowd out the rest. Deliberately generous — a single TUI holds a
`SubscribeGroup` per group plus `WatchState` and `SubscribeLogs`, so a full
fleet is already ~100 streams for one identity and several operators share the
`tui` identity; the test pins that four operators on a full fleet still fit.

`JobTail`'s own tighter cap (M78) stays: it bounds a guest-side `tail -f`
process and a vsock connection, which this does not distinguish.
`TestStreamAdmissionIsBounded`.

### M96 — TUI lifecycle commands omit the selected goal name (`tui/daemon.go`) — **fixed**

Real, and not a corner case: a group runs several goals at once by design, so
this is the ordinary shape. `goalOpCmd` put the selected run in `extra["name"]`
and `GoalApprove`/`Pause`/`Interrupt`/`Resume`/`Cancel` built their requests
with only the group — the daemon then fell back to implicit selection, so
`/goals cancel` aimed at the row the operator picked could resolve to a
different eligible run, or fail as ambiguous while a run sat right there
selected.

All five now pass `Name`. Pinned by a test that reads the call sites, because
the failure is an omitted FIELD — nothing a behavioural test of the RPC would
catch, since a single-goal group resolves the same either way.
`TestGoalLifecycleRPCsCarryTheName`.

### M92 — Guest disconnects do not cancel credentialed upstream proxy work (`daemon/proxy.go`) — **fixed**

Real, and the slot accounting is what makes it matter. The upstream request
builders used `http.NewRequest`, so the call did not inherit `r.Context()`, and
`doWithRetry`'s backoff was an unconditional `time.Sleep`. A guest that sent a
complete body and then disconnected left the proxy running credentialed work
against the provider on its behalf — and `proxyAcquire`'s slot is released when
the handler RETURNS, so one of the group's 32 slots stayed held for the whole
backoff, which with `retry-after` reaches tens of seconds.

Both upstream builders now use `http.NewRequestWithContext(r.Context(), …)`,
`doWithRetry` takes the context, checks it before starting another attempt, and
waits on a timer selected against `ctx.Done()` instead of sleeping. A
disconnected guest cancels the provider call it was paying for and frees its
slot immediately. `TestUpstreamWorkFollowsTheGuestContext`.

(This is the same propagation M80 declined for the JOB-observability path, and
the difference is the reason: there the guest call is bounded by a 15s timeout
and an admission cap, so the context buys latency on slot return. Here the wait
is attacker-influenced through `retry-after` and the slot is one of 32.)

### M98 — Destroyed group job cache is reused after same-name recreation (`daemon/groups.go`) — **fixed**

The fourth member of the name-reuse family (after the report window, the
schedules M14, the goal records M50, and the event ring). `jobsCache` is keyed
by group NAME and carries no incarnation, and `destroy` did not clear it — so a
later group reusing the name inherited the destroyed one's job list, command
text included. Two publication paths reach it without a guest read:
`listGroups` attaches `jobsSnapshot` to every configured group without a
synchronous refresh, and `refreshJobs` deliberately serves the retained cache
when the guest read fails.

`dropJobsCache` in `destroy`, alongside `delSchedsFor`, `delGoalsFor`,
`disarmReport` and the event-ring drop. The in-flight refresh marker goes with
it, so a refresh from the destroyed incarnation cannot repopulate the
replacement's cache. `TestDestroyDropsTheJobCache`.

### M101 — Concurrent ctl spawn requests bypass the group-count cap (`daemon/ctl.go`) — **fixed**

Real. Both spawn admission points read the registry and compared its length
before calling `ensure`, while registration happened later inside `allocPort`
under `groupsLock` — so concurrent spawns with distinct names all passed the
check while the registry was still below the limit, and every one of them then
registered. The ctl plane handles each guest connection in its own goroutine,
so a compromised `main` needed only to issue its spawns in parallel.

The cap now lives WITH the registration, inside `allocPort`'s critical section,
which makes it true rather than advisory and covers every present and future
caller. `allocPort` returns an error instead of a bare int; the call-site checks
stay, because they produce a specific message before any workspace is created.
`TestGroupCapIsAtomicWithRegistration` runs 200 concurrent allocations and
asserts exactly `ctlMaxSpawn` are admitted.
