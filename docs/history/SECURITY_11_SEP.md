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

### M99 — Unbounded token-rate sample retention enables daemon resource exhaustion (`daemon/tokrate.go`) — **fixed**

Real, and the trigger is the ordinary headless case. `tokRates` was the only
thing that pruned aged-out samples, and it runs from `stateWatchLoop`, which
skips its tick entirely when no client is watching — so a daemon nobody has a
TUI attached to accumulated one sample per retired proxy request, forever, and
a guest in a running group produces those at will.

Pruning now happens at INSERTION, amortized: a full scan on every add would be
O(n) per request under the global lock, so it runs once the slice passes
`tokSamplesPruneAt`, with `tokSamplesMax` as a hard ceiling for the case where
everything in the window is genuinely live.

The prune also COPIES into a right-sized slice instead of `tokSamples[:0]`,
which the finding was right to call out separately: reslicing kept the largest
backing array the daemon had ever needed for the life of the process, so a
burst's peak capacity was retained even after its samples aged out.
`TestTokSamplesArePrunedWithoutAReader`.

### M97 — ctl ask can substitute another session's response (`daemon/ctl_cli.go`) — **fixed for the cross-session case; the same-session case named**

Real. `SubscribeGroup` carries the whole GROUP — every session of it — and
`ctlAsk` started capturing at the first `prompt` whose text equalled its own
and stopped at the first unqualified `turn_end`. A client with send access to
the group could pre-stage or race an identical prompt in a DIFFERENT session
and hand the victim's synchronous command the wrong conversation's output,
leaving the intended turn unconsumed.

Both the start and the end are now qualified by session, which the events
already carry, with the default session's three spellings (`""`, `-`,
`default`) folded together so the filter matches what the daemon stamps.

**Named, not pretended away:** two IDENTICAL prompts queued back to back in the
SAME session remain indistinguishable on the wire. Within one session the
daemon serializes turns, so the next matching `prompt` is this turn under
ordinary use — but separating a deliberate duplicate needs a server-issued turn
id propagated onto every lifecycle event, which is a proto change and a
contract change for every client. The cross-session substitution, which is the
part another principal can cause, is closed.
`TestAskSessionNormalization`.

### M100 — Fallback scanner container can read and exfiltrate the entire checkout (`tools/hooks/pre-commit`) — **fixed**

Real, and it is the confidentiality half that the existing precautions do not
cover. The scanner needs the repo WITH `.git` to read the index, so it can read
the whole checkout — git history, ignored files, and `creds/` among them — and
a read-only mount prevents writes, not reads. The digest pin stops a mutable
tag being swapped, but pinning is not a confidentiality boundary: if the image,
its build pipeline or the registry account were compromised, the only thing
between it and the operator's CA key and bearer tokens was somewhere to send
them.

`--network=none` is the load-bearing flag, and it now has nowhere. Verified,
not assumed: the container comes up with `lo` alone and no DNS resolution. The
rest is ordinary least privilege for a process that only reads files —
`--cap-drop=ALL`, `--security-opt=no-new-privileges`, a read-only root with a
tmpfs for scratch (also verified: a write to `/` is refused, `/tmp` works).

`label=disable` stays: Fedora's SELinux policy denies the bind mount otherwise,
which is the same trade the project documents everywhere else. The staged-only
snapshot the remediation prefers would change what `--staged` reads; with no
network, reading the checkout costs nothing.

### M104 — Notification and tailer state survives group destruction and name reuse (`daemon/logtail.go`) — **fixed**

Three leaks with one cause: `destroy` cleaned up by group NAME, and these are
not all keyed that way. The tail-claim bug is the sharpest and is not only a
leak — it is a functional break nobody would have attributed:

- **Tail claims are keyed by PATH** (`markTail`), and `destroy` did
  `delete(tails, g)` with the bare name. Every claim for `groupLogPath(g)` and
  the ten slot paths survived, so a group recreated under the same name could
  never start a tailer — `markTail` returned false forever and the group
  streamed no events at all.
- **`notifyQueue[g]`** holds undelivered markers that `tryFlushNotify` appends
  to whatever `groupLogPath(g)` names WHEN IT RUNS, which after recreation is
  the replacement's log.
- **`notifyExpectN[g]`** is the allowlist deciding whether a `[[notify]]` line
  may become a live notification. A stale expectation authorizes a matching
  marker in the replacement's stream — the exact forgery that allowlist exists
  to stop.

`dropGroupTailState` clears all three, called from `destroy` beside the other
name-reuse cleanups. The test asserts the replacement can claim its tails,
which is the half that would otherwise have read as "the new group is broken".
`TestDestroyReleasesTailAndNotifyState`.

### M102 — Plan phase treats aborted or self-healed turns as completed (`daemon/goals.go`) — **fixed**

Real, and it is the same gate M20 and M49 are about — so a fail-open here made
both of those partly decorative. `goalPlanPhase` does check for a stall, but
both failure paths defeated it:

- `abortInflightTurn` (the VM exited mid-turn) wakes the blocked sender through
  the SAME channel a real `[[turn_end]]` uses, and `sendNow` returned nil.
- On timeout `sendNow` marks the session stalled, calls `selfHeal`, and returns
  nil — and a SUCCESSFUL heal clears the stall flags before the caller reads
  `isStalled`.

Either way the plan phase concluded the turn ran and moved the goal to
`awaiting_approval` with no plan behind it: a human is shown a plan to review
that was never written, and a group that set its own goal can approve and
execute one.

Fixed where the ambiguity is, not where it surfaces: the completion channel now
carries a `turnOutcome`, so a VM exit is `turnAborted` and a real `turn_end` is
`turnCompleted`, and `sendNow` returns an error for the abort and for the stall.
The plan phase's existing `if terr != nil` branch then pauses the goal, and
every other caller of a turn gets the truth for free.

**A rejected first attempt, recorded because the reasoning matters:** requiring
a captured HANDOFF as proof of a completed plan seemed neater and is wrong. The
handoff is a best-effort read of the transcript, so a legitimate plan whose
closing report does not match the expected shape would pause the goal — the
harness surfaced exactly that, failing ten goal tests at once. Absence of a
handoff is not evidence of absence of a plan; the wakeup's provenance is.
`TestTurnOutcomeDistinguishesAbortFromCompletion`.

### M103 — Unbounded guest-to-egress proxy tunnels enable shared resource exhaustion (`daemon/proxy.go`) — **fixed (admission); idle timeouts declined**

Real, and the reason it slipped past the M2 work is structural: `ServeHTTP`
dispatches CONNECT and absolute-form requests to `serveEgress` and RETURNS
before `proxyAcquire` is ever reached — those semaphores guard the LLM leg
only. So a process in a networked guest could open policy-permitted tunnels in
a loop and leave them idle, each costing a guest-side bridge with two copy
goroutines, a vsock connection, a unix socket and a host dial, with a CONNECT
tunnel held for as long as either end wants it.

`egressAcquire` bounds them per group (64) and globally (512), released by a
`defer` that fires when the tunnel ends — `splice` blocks for its whole life,
so the reservation covers exactly the right span. Per group as well as
globally, so one guest cannot crowd out another's egress. Generous because real
traffic is parallel: npm and git open many connections at once. The
guest-side vsock count is separately capped by `fcMaxConnsPerGroup` (M22/M27);
this bounds the HOST end, which that cap does not reach.

**Declined: idle read/write deadlines and a maximum tunnel lifetime.** A
`network=full` group is meant to be able to hold a long-lived connection — an
ssh session, a websocket, a long poll — and those idle by design between
keepalives. A deadline tuned to catch a parked tunnel would kill working ones
intermittently, which is the worst kind of bug to attribute, and the
per-connection cost is already bounded by the admission limit.
`TestEgressConnectionsAreBounded`.

### M105 — Tool transcript renders unredacted tool arguments (`tui/model.go`) — **not a finding**

The transcript is the AUDIT LOG. `Bash $ <command>` exists so the operator can
see what the agent actually ran, and a redacted one would defeat the purpose of
the pane it lives in — "the operator can't tell what the agent did" is a worse
failure than the one being described.

The premise also does not hold here. The proposed disclosure is to "other users
with legitimate transcript access", but a group's transcript is readable
exactly by principals the ACL grants `subscribe_group`/`history` on that group
— the same principals who can `send` to it, attach its shell, and read its
workspace. There is no audience that can read the transcript but should not see
what ran in it.

Where a redaction rule WOULD have bitten, the boundary is elsewhere and is
already drawn: secrets never reach an agent's tool arguments in the first place
(the proxy holds the credentials; guests get `ANTHROPIC_API_KEY=proxied`), and
the pre-commit scan plus `make secrets-scan` exist to keep the operator's own
secrets out of the repository the agent works in.

The one adjacent thing that IS a real problem — control characters in those
same arguments reaching the terminal — is M73, and is fixed.

### M106 — Group-scoped interrupt authorization permits cross-session turn cancellation (`daemon/grpc_server.go`) — **not a finding**

The fourth report of the same shape (M4, M28, M45), and the answer does not
change: sessions are conversations, not tenants. A caller authorized to
`interrupt` a group can already `send` to it, attach its shell, and read its
workspace — cancelling a turn in one of its conversations is strictly less than
any of those.

The finding is careful and correct that this is "the missing authorization
binding" rather than injection; the disagreement is only about whether that
binding should exist. It should not: koto's isolation boundary is the GROUP,
and two workloads that need isolating get two groups, which costs one microVM.

The genuinely wrong behaviour in this area was an interrupt hitting a turn the
caller never observed — a race, not a permission — and that is M85, fixed.

### M108 — Makefile PKI generation leaves private keys readable by other local users (`Makefile`) — **fixed**

Real. `openssl` writes a key with whatever the caller's umask allows, and the
common 022 left `ca.key`, `server.key` and every `client-*.key` mode 0644. The
CA key is the whole trust root: a local user holding it mints a client cert the
daemon accepts. `pki.go` already writes 0600, so the Makefile route was the
only one exposed — and it is the route `make pki-init` / `make pki-client`
documents.

Three changes: `creds/` is created 0700, each key-generating line is prefixed
`umask 077 &&`, and every key is `chmod 600`'d afterwards.

The umask is prefixed PER LINE rather than set once, and that matters: make
runs each recipe line in its own shell (there is no `.ONESHELL` here), so a
standalone `umask 077` line changes nothing for the lines after it. I wrote it
that way first. The explicit chmod is the belt to that brace and also repairs a
key generated before this.

Verified end to end under `umask 022` in a scratch tree: after `pki-init` and
`pki-client`, `creds/` is 0700, every `.key` and token is 0600, and the
certificates stay world-readable as they should.

### M109 — Worker-controlled status object can wedge Firecracker VM initialization (`fcguest/turn.go`) — **fixed**

Real, and the blast radius is larger than "a bad read": `reconcileOrphanJobs`
runs inside `handleInit` while it holds `initMu`, so a poisoned entry wedges
the GUEST'S INITIALIZATION — and the job tree lives in `workspace.img`, so it
survives the restart and does it again on every later boot. The group stays
dead until someone repairs the workspace by hand.

`os.ReadFile` on a worker-owned path offered three ways in: a FIFO with no
writer blocks forever, a symlink to `/dev/zero` buffers until the agent dies,
and a large regular file does the same more slowly.

`jobStatusIsRunning` opens `O_RDONLY|O_NOFOLLOW|O_NONBLOCK` — no symlink, no
blocking on a FIFO — then requires a regular file via `fstat` on the
DESCRIPTOR (not an `Lstat` on the path, which the worker could swap
afterwards), and reads at most 64 bytes, since a status marker is one short
word. The write-back is `O_NOFOLLOW` too. `TestJobStatusReadIsDefensive` covers
the FIFO (with a timeout, because the failure mode is a hang), the symlink, a
directory, an oversized file, and that genuine markers still parse.

### M110 — Memory admission can be bypassed by racing mutable VM configuration (`daemon/fc.go`) — **fixed**

Real. `fcSpawn` read the machine shape for `fcHostMemAdmit`, then read it AGAIN
— inside `fcVMConfig`, for `vm.memMiB`, and for the cgroup — after listener,
network, jail and image setup had run. `configCmd` writes `config.json` without
`groupOpMu`, and `seedSpawnConfig` writes it before `ensure` takes that lock, so
a concurrent authorized Config or Spawn could raise the preset in between. The
VM and its cgroup were then built from the LARGER value while the fleet
reservation still held the smaller — the accounting is wrong either way, and
without the optional per-VM cgroups it directly overcommits host memory.

One resolution now, taken before admission and threaded through: `fcVMConfig`
takes the vCPU count, memory and IO budget as PARAMETERS rather than re-reading
`config.json`, which also makes it a pure function of its arguments. The IO
budget joined the snapshot for the same reason.

The workspace DISK size is still resolved separately, in
`fcEnsureWorkspaceImg` before this point, and deliberately left that way: that
path only ever grows the image, so a preset raised underneath it produces a
larger disk, never a mismatch anything relies on.
`TestVMConfigUsesTheAdmittedShape`.

### M111 — Untrusted TUI content can inject terminal control sequences (`tui/theme.go`) — **fixed for the reachable class; the blanket render-boundary sanitizer declined**

The finding's framing is right — `reassertBg`, `stripSGRColor` and `foldASCII`
are styling transformations, not safety boundaries, and nothing should be
relying on them as such. But the concrete paths it names have been closed one
at a time over this audit, at the producer rather than the renderer:
`AgentFrame_Error` at the daemon's trust boundary (M74), the legacy raw JobTail
frames (M62), decoded tool arguments (M73), and the shell pane through
`scrubVT` all along. Chat content arrives already sanitized by the daemon's
`sanitizeEvent`.

What was left is the awkward class: ERROR text. It is assembled on the CLIENT
from gRPC status messages, daemon errors and guest-influenced errors, and it
lands in both the rendered frame and the debug log, neither of which strips
anything. `addLine` is the one place every error line passes through, so the
scrub goes there — covering every producer, present and future, without
touching the streaming paths where throughput matters.
`TestErrorLinesAreScrubbed`.

**Declined: one sanitizer over the finished `View()` frame.** It sounds like
the strictly safer design and is not. The frame at that point contains
glamour's markdown rendering and lipgloss's layout, and an allowlist tight
enough to be a boundary would have to enumerate what those libraries emit —
so the failure mode is a silently mangled UI on a library upgrade, traded for
coverage of paths that are already sanitized at the producer, where the code
actually knows whether the bytes are trusted. The existing frame-level pass
(`monoFrame`) is a deliberate exception: it STRIPS colour rather than judging
sequences, so it cannot mangle anything.

### M116 — Installer promotes unverified Firecracker runtime and guest assets (`daemon/install.go`) — **fixed**

Real, and backwards in a way worth naming: `verifyArtifacts` checked `koto` and
`koto-tui` and stopped, while the manifest vouches for FIVE files — and the
other three are the runtime boundary itself. The Firecracker binary is exec'd
and bind-mounted into the jail; `vmlinux` and `rootfs.img` are the VM's boot
inputs, and the guest kernel is what makes `CONFIG_VSOCKETS_LOOPBACK=n` (M2)
true. Verifying the two that run as the operator but not the three that define
the sandbox is the wrong half.

Every artifact the manifest covers is now held to it. Two non-failures are kept
deliberately, both already documented in the manifest's own header:

- **No entry → skipped, not refused.** `vmlinux` and `rootfs.img` embed build
  timestamps and resolved package versions, so they do not reproduce
  bit-for-bit. Failing closed would break `make build` → `make install`, the
  build-from-source route the project offers on purpose.
- **No entries at all → not an error.** The manifest is committed but empty
  until the first release.

What changed about those cases is that they are now SAID: the installer
reports how many artifacts it verified and names the ones it could not, because
"nothing was verified" and "everything verified" should not look identical to
an operator. `TestArtifactVerificationCoversEveryManifestEntry` tampers with
each of the five in turn.

### M113 — Transcript files are created with permissions readable by other local users (`daemon/logtail.go`) — **fixed**

Real, and uniform: every writer asked for 0644 and every directory for 0755 —
the umask's choice rather than a decision. A transcript holds prompts, model
output, tool output, command output, proxy errors and notifications, and
nothing about it wants to be group-readable.

Changed across all of the paths the finding identifies, since fixing one would
leave the file at whichever mode created it first: the turn sink
(`logSinkAppend`), the tailer's create, the prompt echo, the clear/rewrite
truncate, the session-filter rewrite, the session registry, and `config.json`.
`.cs/` itself is 0700 — it holds the transcripts, the config and the uploads.

The exposure was to the operator's GROUP rather than the world (the state dir
is 0750 and owner-owned), which is why this is a tightening rather than an
emergency. `TestTranscriptFilesAreOwnerOnly`.

### M114 — Published-port forwarding permits unbounded idle connection exhaustion (`daemon/fc.go`) — **already fixed (M22/M27)**

The published-port accept loop is one of the `fcAcceptLoop` call sites, and it
was given its OWN admission bucket when that cap went in — deliberately
separate from the guest's, because these connections arrive from the host side
and charging them to the guest's budget would let an outside caller starve the
group's ctl and turn channels. So the unbounded goroutine-per-client is bounded
at `fcMaxConnsPerGroup`, released on every handler exit including a dial
failure.

An inactivity deadline on established streams is declined for the same reason
as M103: a published port exists to carry a real service, and a connection idle
between requests is the normal case for one.

### M115 — Unsolicited guest report requests trigger repeatable host-daemon resource consumption (`daemon/ctl.go`) — **fixed**

Two real orderings, both "expensive work before the authorization decision".

**The report body.** An unsolicited `report` was base64-decoded (up to a 1 MiB
frame), scrubbed rune by rune by `sanitize`, trimmed and truncated — and THEN
refused by `deliverReport` for having no armed window. A guest could spend the
daemon's CPU in a loop on a verb it is not authorized to use at all.
`reportArmed` is now checked first and is a map lookup. It asks only whether an
entry EXISTS, deliberately: an expired window carries bookkeeping
`deliverReport` owns — delete it, log the expiry, answer with the reason — and
short-circuiting that here would swap a specific answer for a generic one and
skip the cleanup. It consumes nothing, so `deliverReport` stays authoritative.

**The log line.** `ctlDispatchPB` logged the complete converted request before
dispatch, and that line is mirrored to stderr, retained in the log ring and
fanned out to every `SubscribeLogs` subscriber — a megabyte-per-frame amplifier
on the daemon's own logging path, again before any authorization. Requests are
now logged in full up to `ctlLogMax` (512 bytes) and elided to verb plus length
beyond it, which is where a payload stops being a command.
`TestUnsolicitedReportIsRefusedCheaply`.

### M112 — Purge can be redirected by a path-substitution race (`daemon/uninstall.go`) — **fixed**

Real, and the confirmation prompt is what makes the window wide: `purgeRefusal`
validates a PATHNAME — `EvalSymlinks`, marker checks, ancestry — and then
`os.RemoveAll` resolves that name AGAIN, with a human's answer in between. With
a custom `-state` under a writable parent, the parent can be renamed and
replaced with a symlink in the meantime, so the prompt shows one directory and
the `rm -rf` walks another.

`dirIdentity` takes the directory's `(device, inode)` before the guards and
re-checks it immediately before the deletion; a mismatch refuses and says why.
`Lstat`, not `Stat`, so a symlink swapped in for the validated directory reads
as a different object rather than as whatever it points at.

This does not make the operation race-free in the abstract — only a
descriptor-relative deletion would, and Go's `RemoveAll` takes a path. It does
mean the substitution has to win a syscall-width race instead of a
human-width one, and that the check the operator was shown is the check that
holds at deletion time. `TestPurgeIdentityDetectsSubstitution`.

### M107 — Sensitive prompts are persisted in job command metadata (`sidecar/cs-job`) — **accepted**

Two claims, and the answers are ones this ledger has already reached.

The GROUP-scoped disclosure is M105/M106 again: `cs-job list` and the Jobs RPCs
are readable by principals who can also send to that group, attach its shell
and read its workspace. A job's command line is not something they are being
kept from.

The argv persistence is M58's residual, which the operator POSTPONED on
2026-09-11 — the same shape (a prompt on a command line, visible in
`/proc/<pid>/cmdline` and in the job's `cmd` file), and the same fix would be
needed: a delivery path that is not argv. `cs-job run` already accepts the
stdin form; making `spawn` use it would change the documented agent-facing
interface, which is not a change to make inside an audit pass without the
operator's call. Recorded here so the two are tracked together.

### M119 — Guest-triggered proxy errors can grow the host group log without bound (`daemon/proxy.go`) — **fixed**

Real, and the exempt writer was the one a guest could drive hardest.
`logSinkAppend` gives the guest TURN sink a 1 GiB per-file ceiling and a
per-group byte-rate bucket; `streamLogAppend` — the group stream, carrying
proxy error records and notifications — was a bare `O_APPEND` write with
neither. Every non-200/non-404 upstream response writes a line, and the
per-group concurrency semaphore bounds simultaneous REQUESTS rather than
cumulative responses, so a guest could sustain failing requests to an
allowlisted endpoint and grow that group's log until the shared filesystem gave
out — which is the fleet-wide failure (every guest remounts read-only).

Both writers now go through `logSinkAppend` under the path's write lock, and an
append failure is logged (deduped) rather than silently swallowed: a full
filesystem used to make "the transcript stopped" indistinguishable from
"nothing happened".

`tryFlushNotify` already holds that lock across its whole flush, so it calls a
`logAppendLocked` variant — Go mutexes are not reentrant, and taking it twice
deadlocked the daemon. The test suite caught that as a hang, which is how it
should be caught. `TestGroupStreamAppendsAreBounded`.

### M118 — PID 1 follows a worker-controlled `/workspace/.cs` symlink during privileged setup (`fcguest/main.go`) — **fixed**
### M120 — Worker-controlled `ctl.out` FIFO can block the guest control plane (`fcguest/main.go`) — **fixed**

The same directory, two ways in, and both turn on the fact that `/workspace` is
worker-owned and PERSISTS across boots — so uid 1000 can stage something and
wait for the next VM start, when PID 1 walks it.

**M118, the symlink.** `setupCS` used `MkdirAll`, `Chown`, `Stat`, `Remove`,
`Mkfifo` and `OpenFile` on pathname strings under `csDir`. Replace `.cs` with a
symlink and all of that happens wherever it points, as PID 1: a directory of
the worker's choosing gets chowned to the worker, with `ctl` and `ctl.out`
created inside it. `/run` is the obvious target — a tmpfs the agent set up, and
handing it to uid 1000 is a worker→PID-1 escalation inside the guest. `.cs` is
now `Lstat`ed and replaced if it is not a real directory, and the setup aborts
rather than continuing on a path it could not establish.

**M120, the FIFO.** `.cs` is chowned to the worker by design, so `ctl.out`
itself can be unlinked and replaced with a FIFO. Opening an existing FIFO
write-only BLOCKS until a reader appears — at boot that means `setupCS` never
returns, so the agent RPC and the ctl forwarder never start and the group is
dead on every subsequent boot; at runtime it stops the single response loop.
`openCtlOut` opens `O_NOFOLLOW|O_NONBLOCK`, `fstat`s the DESCRIPTOR and
requires a regular file, and `setupCS` replaces a bad endpoint and retries
once. `O_NONBLOCK` is cleared afterwards, since a regular file should be
written the ordinary blocking way.

Both fixes are the shape M13, M23 and M109 established: no privileged operation
on a worker-controlled pathname without a no-follow open and a descriptor-based
type check. `TestOpenCtlOutRefusesNonRegularFiles` covers the FIFO with a
timeout, because the failure is a hang.

### M121 — Unbounded forged background-task notices enable resource exhaustion (`daemon/logtail.go`) — **fixed**

Real, and the provenance point is unanswerable rather than an oversight: the
trigger IS a regex match in a tool_result, because claude code announces a
backgrounded command in prose and there is no structured signal to prefer. So
the bound has to be on the resource.

Each accepted notice opened an exec stream, started a `tail -F` process in the
guest, allocated host reader state and armed a ten-minute timer. `bgActive`
deduped by `(group, id)` — which is exactly what distinct ids evade — and
nothing else counted them.

`bgTailMaxPerGroup` (8) and `bgTailMaxGlobal` (64). Deliberately small, unlike
the other caps in this audit: a turn backgrounds a handful of commands at most,
and the ten-minute lifetime makes the ceiling per ten minutes rather than per
turn. Refusing the excess costs a live view of one background job's output — a
feature degrading, not a turn failing. `TestBackgroundTailersAreBounded`.

### M117 — Pending upload quota permits oversized host-to-guest frames and wedges group delivery (`daemon/fc.go`) — **fixed**

Real, and the wedge is the part that matters: the guest refuses an oversize
frame BEFORE unmarshalling, and the uploads were only cleared after a
successful agent call — so an oversize set left the turn undeliverable and the
files pending, and every later turn in that group retried the same doomed
frame. The group stops accepting work until someone clears the spool by hand.

The budget is now taken against the SERIALIZED `MsgReq` (`proto.Size`), which is
what the channel limit applies to: `maxUploadsPending` bounds raw file bytes on
disk, while tar headers, block padding, the end blocks, the message, the system
prompt and config.json all land on top.

Over the limit, the turn is delivered WITHOUT its attachments and the files stay
pending, with a log line saying so. Delivering a degraded turn beats failing
delivery: the alternative is the wedge above, and the operator can see what
happened.

`fcWriteFrame` also refuses an oversize frame outright, as the remediation asks
— both peers reject a declared length over their channel maximum, so writing
one produces a peer that hangs up rather than an error naming the size.
(M36 already narrowed the exposure considerably: uploads are per-turn now, so
only files the message references are packed, not everything newer than a
group-wide watermark.) `TestOversizeFramesAreRefusedBeforeSending`.

### M124 — Authenticated wildcard subscriber can permanently allocate tailers for nonexistent groups (`daemon/grpc_server.go`) — **fixed**
### M125 — Wildcard-authorized Send can exhaust shared storage across group buckets (`daemon/attachments.go`) — **fixed**

The same mistake in two RPCs: spending daemon resources on a group NAME before
establishing that the group exists. The ACL authorizes a verb against a target
PATTERN, and a role holding `"*"` — which the seeded `agent` role does — can
legitimately name anything; nothing else asserted the target was real.

- **Subscribe** called `ensureTail` first. `tails` is process-wide and
  permanent, and `tailFile` CREATES the log file and then polls it forever, with
  nothing tying the tailer to the stream that asked. Unique invented names left
  a descriptor, a goroutine and a poll loop behind for each.
- **Send** processed attachments first. `saveImage` creates
  `<ROOT>/<group>/.cs/uploads` and writes the bytes before `enqueueSend` gets a
  say, and the pending quota is per DIRECTORY — so the per-group allowance
  multiplied across the namespace, in shared state, for groups that do not
  exist.

Both now require `registeredGroup` first. This completes what M17 started:
`ensure()` refuses to PROVISION an unregistered group, and these refuse to spend
a descriptor or a byte of disk on one. `TestRPCsRefuseUnregisteredGroups`.

### M123 — Clear RPC ignores guest deletion failures and reports success (`daemon/groups.go`) — **fixed**

Real, and it makes `/clear` lie in the one direction that matters. `fcExec`
returns the guest shell's exit status SEPARATELY from the Go error — a
successful agent response carrying `rc=1` is a failed deletion — and both clear
paths discarded it. A read-only guest filesystem, or anything else that makes
`rm` fail, told the operator the conversation was forgotten while it was still
in the VM, ready to be resumed by `--resume` on the next turn.

The session script also ended in `true`, which forced a zero status over
whatever the `rm`s did — so the caller could not have checked even if it had
looked. That is gone; the one place a non-zero status is EXPECTED (an id file
that was never written, i.e. the normal first-turn case) is now an explicit
`if`, not a `&&` whose false branch would fail the script.

Both callers check `rc` and report the guest's output on failure. Verified
against the three shapes by hand: no id file → 0, a valid id → 0 with the
transcript gone, and an undeletable transcript → 1 with the operator told.

### M126 — OAuth login executes an untrusted PATH-selected `claude` binary (`daemon/claude_login.go`) — **fixed (the check-to-use gap)**

The PATH premise is tier 1, as in M5 and M8: an attacker who can write a
directory on the operator's PATH already executes as the operator on their next
shell command. That part does not change.

What IS a defect on its own terms is the shape: `exec.LookPath("claude")`
followed by `exec.Command("claude", …)` does the lookup TWICE, and the second
happens when the command is built — so the binary that was checked and the
binary that runs need not be the same file. A check that does not constrain the
thing it precedes means less than it reads as, and this particular child
inherits the operator's terminal for an interactive credential flow.

Resolved once, and the resolved path is what runs. Cheap, and it makes the
existing check honest. (`KOTO_CLAUDE_BIN`, which the daemon's own refresh exec
uses, does not cover this interactive path and is not meant to — the wizard
resolves in the operator's shell on purpose.)

### M122 — Unbounded transcript retention can exhaust the operator TUI (`tui/model.go`) — **fixed**

Partly closed already, by the daemon-side work in this audit: a parser block is
capped at 1 MiB (M48), a live partial at 64 KiB on the wire (M82), and the log
sink has a 1 MiB/s bucket and a 1 GiB ceiling. Those bound what ARRIVES.

What they do not cover is the TUI's own retention, and `maxLines` caps the line
COUNT: 50k lines of one word is nothing, 50k lines of a megabyte each is not. A
line count times a per-line size nobody bounds is not a budget.

`maxLineBytes` (64 MiB) is the second cap, kept as a running sum so it costs an
add per append rather than a walk. Every path that mutates `m.lines` goes
through `trimLines` now — the append, the live-batch append, and the
history-page prepend, which REPLACES the slice wholesale and so recounts.
Oldest-first, because a transcript is read from the bottom. The test asserts
both caps bind independently and that the counter never drifts from what is
actually held — a drift would silently stop the budget working.

The remaining suggestions — byte-bounding the render cache, replacing string
concatenation with builders, truncating before markdown rendering — are
performance work on paths whose INPUT is now bounded at 1 MiB per block, and
are not tracked here as security findings.

### M127 — Shell error text bypasses TUI terminal-output sanitization (`tui/shell_view.go`) — **fixed**

Real, and it is the same seam M111 describes from the other end: the PTY path
is deliberately safe — raw guest bytes go through the terminal EMULATOR and
then `scrubVT` — while the ended-shell ERROR is a guest-authored string
concatenated into the hint line and handed to `lipgloss.Render`, which styles
content without neutralizing what is in it.

`scrubVT` on both error sinks: the `ShellFrame.Error` the guest sends, and the
transport error beside it, which is assembled on the client and can carry a
server message through. `TestShellErrorTextIsScrubbed`.

### M128 — State directory validation is not bound to subsequent writes (`daemon/install.go`) — **not a finding (M52/M77/M112 again)**

The third report of the check-then-use shape against the state root, and the
answer is the one M52 and M77 already got: every operation it names needs write
access to the state tree or its parent, and the default `/var/lib/koto` sits
under a parent no unprivileged user can write. `stateDirTrusted` validates the
ROOT and claims nothing more (audit L9 added it for a pre-created `/tmp`
directory wearing the name).

Where the same shape had a consequence I could not dismiss — `--purge`, which
is an `rm -rf` gated by a prompt a human takes seconds to answer — it IS fixed,
by binding the deletion to the directory's `(device, inode)` (M112). The
difference is the width of the window and what is on the other side of it.

### M129 — Guest-controlled Venice proxy lacks per-group spend or request-rate enforcement (`daemon/proxy.go`) — **accepted**

Accurate, and describes the product rather than a defect. The proxy exists to
hold the credential and inject it for the group's own inference; a guest
spending the account's quota on inference is the feature, and the same is true
of the Anthropic leg, which this finding does not single out. What bounds it
today is concurrency (32 per group, 128 global), the body budget (M15), the
endpoint allowlist (M3's sibling, which keeps the credential to inference and
model metadata), and the operator's own per-group `/config model`.

A cumulative spend budget per group is a real feature and a good one —
`metrics.jsonl` already records per-request token counts per group, so the data
is there — but it is a product decision about defaults, refusal behaviour and
operator visibility, not a fix to apply inside an audit pass. Recorded for the
operator rather than invented.

## M130 — Goal interruption can fail to cancel a reserved turn — FIXED

`daemon/goals.go`, `goalInterrupt`.

**Confirmed.** The verb did two things: transition the goal to `paused`, and —
only when `sessionBusy` said the goal's worker session had a turn in flight —
call `interruptAgent(g, work)`, a best-effort SIGINT to the guest process. It
never touched the turn-cancellation machinery the ordinary `Interrupt` RPC goes
through, so:

- **Pre-delivery.** A turn that is in flight but has not reached the guest —
  booting the VM, waiting on one of the group's ten slots, mid-`fcSendMsg` —
  has no process to signal. `interruptAgent` was a no-op against it and the
  iteration ran to completion after the goal was already paused.
- **Post-delivery.** One SIGINT is all the guest ever got. The re-signal and
  SIGKILL escalation live in `sendNow`'s abort loop, which it enters only once
  the turn's cancel channel is closed; a worker that ignored the first signal
  was never escalated on. The explicit cancellation checks in `sendNow` that
  DISCARD the prompt likewise never fired.

**Fix:** `goalInterrupt` now calls `requestTurnCancel(g, work)` before the
signal, and keeps the signal as the fast path for an already-delivered process.
`requestTurnCancel` rather than the identity-bound `cancelTurn` (M85) on
purpose: the meaning here is "stop whatever this goal's session is running" —
the same meaning `stopGroup` and `clearFence` carry — not "stop the turn I just
observed", so there is no observed identity to bind to.

**Not a gap, and left alone:** a turn still *queued* when the interrupt lands.
`sendWorker` re-checks `goalTurnShouldRun` at delivery and drops a turn whose
goal is no longer `running`/`planning`, so that half already worked (it is what
`TestGoalQueuedTurnSkippedAfterCancel` pins).

Test: `TestGoalInterruptCancelsTheReservedTurn` in `daemon/audit_fixes_test.go`
takes the in-flight turn's channel identity via `sessionTurn`, interrupts, and
asserts that exact channel is closed. Verified to fail with the
`requestTurnCancel` call removed.

## M131 — Untrusted custom-theme filenames reach terminal output — FIXED

`tui/theme.go`, `themeNames`.

**Confirmed.** `themeNames` accepted any non-directory entry in the drop-in
directory whose name ended `.svg`, with no name policy applied — while
`loadTheme` enforced `themeNameRE` + a `..` check on the same string. So the
listed set was strictly larger than the loadable set, and the extra entries
were arbitrary bytes from a filename.

Both consumers render the name before anything tries to load it:

- `themeListing` concatenates it into a `sys` transcript line. `addLine` scrubs
  `err` lines only (M111), so `sys` text reaches the frame as written, and
  `themeFrame`/`monoFrame` rewrite SGR sequences only — OSC, CSI, CR and the
  rest pass through to the terminal.
- The `/themes` picker renders it as a row title through lipgloss, same story.

Demonstrated in the negative control: a file named
`$'\e]0;pwned\aplain.svg'` put a live OSC title-set sequence into the `/themes
list` output, and `a\e[2J\e[Hclear.svg` an erase-display. Reach is a local
writer of `run/tui/themes` (the TUI's one writable mount), so this is terminal
manipulation and visual spoofing against the operator, not code execution.

**Fix:** one policy, `themeNameOK`, applied at enumeration as well as at load,
so the list is exactly what can be selected; a rejected file is named in the
TUI debug log (`%q`, so the log line is escaped too) rather than dropped in
silence. `loadTheme` now calls the same helper, which is what keeps the two
from drifting again.

Test: `TestThemeNamesRejectHostileDropInFilenames` in `tui/theme_test.go`
writes hostile filenames into a drop-in directory and asserts both that no
listed name carries a control byte and that the rendered listing string does
not either. Verified to fail with the check removed.

## M132 — Daemon can read and replace the TUI's administrator credentials — ACCEPTED, no change

`daemon/install.go` (unit), `daemon/tui_cmd.go`.

**The facts are right and the conclusion is wrong.** The unit runs the daemon
as the operator account with `ReadWritePaths=<state>`, and `creds/` lives under
the state dir, so yes: the daemon process can read `client-tui.key` and
`token-tui` despite their 0600 modes, and can rewrite `ca.crt` before a later
TUI launch. The 0600 bits were never the boundary — same uid.

What the finding claims this buys is "administrative access beyond the
already-compromised daemon", and there is no such beyond:

- **The stolen identity authenticates TO the daemon.** A client cert plus
  bearer token is only ever presented to the koto gRPC server, and the
  compromised process IS that server. Every verb the TUI identity could invoke,
  the daemon already executes. Replaying `client-tui` against "reachable
  endpoints" means replaying it against itself.
- **Replacing the CA is the same non-gain.** A rewritten `ca.crt` makes the TUI
  trust a rogue server; the attacker already holds `server.key` and answers on
  `:8443`. There is nothing to redirect the TUI to that it is not already
  talking to.
- **The one path that WAS an escalation is closed.** `koto tui` resolving the
  `koto-tui` binary out of the daemon-writable state dir would have turned
  state-dir write into code execution as the operator on the next attach. That
  fallback was removed in audit L10 (`cef445d`); today the binary comes from
  `PATH`, or from a checkout the operator is standing in.

So the finding describes the trust model rather than a violation of it: tier 2
holds the credential material, which is exactly what the "trust model addendum:
installed mode" section says ("The state dir … blast radius equals a clone's").
Narrowing it is real work with a real name — **give the TUI its own uid and its
own least-privilege role, and mount it only `ca.crt`, `client-tui.*` and
`token-tui`** — which CLAUDE.md already records as the open item under the
cs_tui addendum. Doing it as a reaction to this finding would be doing it for
the wrong reason: the payoff is containing a TUI dependency compromise (the
Charm tree is ~40 pinned modules), not containing a daemon that is already the
authority.

**Considered and rejected as security theater:** masking `creds/ca.key` from
the unit with `InaccessiblePaths=`. The daemon process genuinely never reads it
(only `koto pki` and the `koto setup` wizard do, both separate invocations
outside the unit), so it would apply cleanly — but a compromised daemon does
not need to mint a fresh identity when it can read the operator's, and it would
add a directive whose failure mode is a daemon that will not start.

## M133 — Per-session clear retains named-session notification markers — FIXED

`daemon/sessions.go` (`filterLogSession`), `daemon/logtail.go`, `daemon/groups.go`.

**Confirmed, and wrong in both directions.** A `[[notify]]` marker carries its
own session field and is appended by `tryFlushNotify` to the GROUP stream at
whatever line boundary comes next — out-of-band, with no `[[session]]` marker
around it. `filterLogSession` updated its deletion state from `[[session]]`
markers only, so it judged every notify line by the enclosing segment:

- **Clearing a NAMED session kept all of its notifications.** The group stream
  contains no `[[session]]` markers at all, so `cur` stays `""` for the whole
  file and nothing in it is ever attributed to a named session. `History` then
  re-parses the retained marker into a `notification` event carrying that
  session's title and message — the conversation the operator cleared is still
  quoting itself.
- **Clearing the DEFAULT session swept away every other session's.** Same
  cause, the other way round: every notify line read as `""`.

**Fix:** `notifyMarkerSession` (logparse.go) states the session a notify line
belongs to — the marker's own field, `""` for the 4-field pre-session shape —
and `filterLogSession` decides notify lines with it, before the segment rule.
No other line type is affected.

**Second half of the finding, also real and also fixed:** a marker still in
`notifyQueue` when the clear runs is appended by the next flush, i.e. lands
*after* the rewrite. `dropQueuedNotifies(g, session, all)` discards the
session's queued markers on a per-session clear and the whole queue on a
group-wide one, called before the transcript is rewritten/truncated in both
scopes. What that does NOT cover, stated rather than papered over: a flush that
has already taken its batch out of the queue can still append it — a window of
the few microseconds between the dequeue and the write, so what survives is a
notification delivered concurrently with the clear, not one the clear was asked
to erase.

Tests: `TestClearSessionFiltersNotificationsByTheirOwnSession` (both
directions, plus an alpha notification interleaved inside the default session's
segment) and `TestClearDropsQueuedNotifications` (both clear scopes), in
`daemon/audit_fixes_test.go`. Verified to fail with the notify branch removed.

## M134 — Stale autostart decisions can resurrect stopped groups — FIXED (destroy half was already closed)

`daemon/groups.go`, `autostartGroups`.

**Half confirmed.** `autostartGroups` reads `groups.json` and each group's
`autostart` key once, then boots the fleet **sequentially** — deliberately, so
a fleet does not contend for KVM and RAM all at once, but it means the last
group's `ensure` runs minutes after the decision to boot it was taken. Nothing
re-examined that decision.

- **Stop: real.** A stop leaves the group registered in `groups.json` on
  purpose, so the stale `ensure(g, false)` succeeded and booted the group back
  up after the stop had completed and reported success. The operator's stop
  silently undid itself — the same failure mode `stopGroupPrepare` already
  guards against for queued messages, in-flight turns and running goals, with
  the one producer it did not know about.
- **Destroy: already closed, by two fixes that predate this finding.** `destroy`
  holds `groupOpMu` across the `groups.json` delete (audit M89, this audit), and
  `ensure` with `create=false` refuses a name that is no longer registered. So
  the sweep cannot recreate a destroyed group's workspace or re-register its
  name; it gets `no such group`. Checked rather than assumed.

**Fix:** the sweep publishes its intended list (`autostartBegin`) and re-takes
each decision immediately before booting — **inside the `groupOpMu` critical
section the boot itself runs in**, via `ensureLocked`. Checking outside the lock
would only move the race to the gap between the check and `ensure`'s own
`Lock`. `stopGroupPrepare` revokes the claim, which puts the revocation *before*
the stop takes `groupOpMu`, so both interleavings end with the group down: a
sweep already inside its critical section finishes the boot and the stop powers
it off right after; a sweep that has not reached the group yet skips it and says
so in the log.

The pending set is bounded by the sweep's own list — `autostartCancel` only
forgets a name the sweep put there — and is emptied when the sweep ends, so
every call is inert outside it.

Test: `TestAutostartSweepYieldsToAnOperatorStop` in `daemon/audit_fixes_test.go`
drives the claim protocol through each interleaving, including the one-shot
property (no double boot) and inertness outside a sweep. Verified to fail with
the `autostartCancel` call removed from `stopGroupPrepare`.

## M135 — Slow request bodies exhaust proxy concurrency slots — FIXED

`daemon/proxy.go`, `readRequestBody`.

**Confirmed.** `proxyAcquire` takes the slot before the body is buffered — on
purpose, since the buffered body is the memory being bounded (M2) — and the
release is deferred to the handler's return. Nothing bounded the *time* that
read could take:

- `MaxBytesReader` caps the size, not the duration;
- `ReadHeaderTimeout` has already expired by the time the handler runs;
- `IdleTimeout` covers a keep-alive connection parked *between* requests, not
  an active body read.

So a guest could hold all 32 of its group's slots for as long as it liked by
opening POSTs and dribbling their bodies. Every other request for that group
then waits out `proxyInflightWait` (60 s) and answers 503 — and for a
`network=none` group the proxy is the *only* egress it has, so this is a total
outage of the group's own turns, self-inflicted by anything that can reach its
proxy socket. Coordinated across groups it also drains the 128-slot global pool.

**Fix:** `proxyStallReader`, the mirror image of the `proxyStallWriter` M2
already added on the write side, and for the same reason: the deadline is
**re-armed before every read**, so it bounds a peer that has stopped sending
rather than one sending a large body slowly. 120 s, which for a guest handing
a prompt across a vsock splice (kilobytes, at memory speed) is already far past
anything real. It clears the deadline on the way out, because the deadline
lives on the connection and keep-alive hands that to the next request — the
same trap `proxyStallWriter.clear` exists for. A stalled body now answers **408**
with a rate-limited warn line naming the group, rather than the generic 413.

Both providers go through `readRequestBody`, so the Anthropic and Venice legs
are covered by the one change.

Test: `TestProxyStalledRequestBodyReleasesTheSlot` in
`daemon/audit_fixes_test.go` drives a real `httptest` server with a body that
sends one byte and goes quiet, asserting the 408 and that the group's semaphore
is empty afterwards. Verified against the unfixed code, where it hangs until the
suite's own timeout kills it.

## M136 — Daemon creates sensitive state with permissive modes — FIXED

`daemon/statemodes.go` (new), plus sixteen creation sites across the daemon.

**Confirmed, and measured on the live install before changing anything:**

```
drwxr-x---  /var/lib/koto/                     ← the installer's 0750
drwxr-xr-x  /var/lib/koto/groups/
drwxr-xr-x  /var/lib/koto/groups/<g>/
-rw-r--r--  1 554374 554374  8.0G  groups/KEEB/workspace.img
-rw-r--r--  goals.json  schedules.json  groups.json  metrics.jsonl (170 MB)
```

`workspace.img` is the finding. It is the ext4 image behind the guest's
`/workspace`: every transcript, every `.claude` session file, the agent's
memory, anything it wrote down — mode 0644, owned by the per-VM subuid, one
per group. Any local uid that can traverse the state dir reads all of it
straight off a stopped VM, with no daemon, no credential and no VM involved.
The installed root's 0750 narrows "any uid" to "members of the root
directory's group"; a dev clone in a 0755 home does not narrow it at all.

**Two halves, because a creation-time fix alone fixes nothing that exists.**
`MkdirAll`, `WriteFile` and `OpenFile` do not repair the mode of a path that is
already there, so an upgraded install would have kept every 0644 image forever.

1. **Creation sites tightened** (16): `groups/` and `run/` to 0700;
   `goals.json`, `schedules.json`, `groups.json`, `metrics.jsonl`, `acl.json`
   (both writers), `clients.allow` to 0600; the workspace image's temp file,
   the per-VM console log, `fc.json` and the migration staging dir; the
   attachments directory and the attachments themselves; the per-VM socket
   directory; and `sessions.json`'s `.cs` MkdirAll, which was still 0755 while
   `ensure()` had been creating the same directory 0700 since M113.
2. **`hardenStatePaths()`**, run once at daemon start, walks the group tree and
   the top-level state files and clears group/other bits. It only ever CLEARS
   (`mode &^ 0o077`), never sets, so it cannot open anything up and cannot
   disturb owner access; symlinks are skipped rather than followed.

**What keeps this invisible to the two processes that use these files:** a
jailed VMM reaches its workspace image as the file's own owner (`fcjail`
chowns it to the per-VM uid) and its sockets through a bind mount into the
chroot, never by traversing the host path; the daemon is root in its own user
namespace over that uid range, so it keeps access to files it does not own.

**One deliberate exclusion:** the state ROOT itself. The installer already makes
it 0750, and in a dev clone it is the operator's git checkout — a daemon that
silently chmods the directory you are working in has overstepped what it was
asked to protect.

**Operator-visible consequence, flagged rather than buried:** after the next
daemon restart, `groups/<g>/workspace.img` becomes 0600 owned by the per-VM
subuid, so the operator's own uid can no longer read a stopped group's image
directly. The offline-reclaim and EROFS-recovery workflows already run under
`unshare`/`sudo` for the same ownership reason, so this changes the mode they
need rather than introducing the need.

**Not a finding, and left alone:** "`kotoHome()` trusts an environment-controlled
path or the current working directory". `KOTO_HOME` is set by the operator, in
the unit or their shell, and the working directory is the dev clone by design —
this is tier 1 choosing where its own state lives, which is the one decision the
trust model does not try to second-guess.

Tests: `TestHardenStatePathsClearsGroupAndOtherBits` (the repair, including that
it preserves the owner triad, skips nothing it should tighten, and never widens
an already-tight file) and `TestStateWritersCreateOwnerOnlyFiles` (the creation
side, since the repair only runs at startup), in `daemon/audit_fixes_test.go`.

## M137 — Active detached jobs can be removed from concurrency accounting — ALREADY FIXED (duplicate of M83)

`sidecar/cs-job`, the `rm` verb.

Same defect as M83 in this audit, described from the accounting angle rather
than the resource-leak one, and closed by the same commit (`071f8e6`). `rm` now
signals the recorded process group — TERM, up to 2 s of grace, then KILL — and
only then deletes the directory. `setsid` made the job its own group leader, so
the negative pid reaches its descendants too.

With that in place the accounting the finding is worried about holds:
`count_running` counts job directories whose `status` file reads `running`, and
a job `rm` has just terminated is neither running nor present. `clean` was
already correct — it skips any directory whose status is `running` — so the
`rm` path was the only way to drop a live job out of the count.

No further change. Verified against the current file rather than assumed.

## M138 — Mount failure unconditionally reformats the persistent workspace — FIXED

`fcguest/main.go`, `mountWorkspace`.

**Confirmed, and the worst-consequence finding in this batch.** The guest agent
ran `mkfs.ext4 -F` on *any* non-nil error from the first mount, with no
classification of the error and no look at what was on the device. `/dev/vdb`
is the group's persistent workspace: the conversation transcripts, the pinned
claude session ids, the agent's memory, its background jobs, its uploads and
whatever the user put there. So a dirty journal, a kernel without ext4, a wrong
mount flag, an `EBUSY`, or any inconsistency `e2fsck` would have fixed in a
second all read as "no filesystem here" — and were answered by destroying the
filesystem that was there, irreversibly, at boot, with no prompt and no backup.

**Fix — format only a device that is provably blank.** `workspaceIsBlank` reads
the first MiB and requires every byte to be zero. That is deliberately stricter
than a magic-number check: an ext4 image has its primary superblock at offset
1024 and its group descriptors right behind it, so the probe separates the two
easily, *and* it refuses to call "blank" a device whose superblock is damaged
but whose data is intact — precisely the case backup superblocks exist to
recover and the old code could not distinguish.

Everything else gets `e2fsck -p -f` and a second mount attempt. If that still
does not mount, the boot **fails**, loudly, naming the recovery: an unbootable
group whose data is intact is recoverable by the operator; a booted group whose
data was just erased is not.

The blank case is still reachable and still correct — the host formats a new
image before the VM ever sees it (`fcEnsureWorkspaceImg`, with `-d` to migrate
an old workspace in), so a blank device here means the host's own format did not
happen, and there is nothing to lose.

**Also fixed, the finding's second point:** the retry mount dropped
`MS_NOSUID|MS_NODEV`. Every attempt now carries the same flags, so a workspace
that had just been reformatted no longer came back without the L11 hardening.

Test: `TestWorkspaceIsBlankOnlyForAnUntouchedDevice` in
`fcguest/workspace_test.go` covers the zero-filled image, an ext4 superblock, a
damaged superblock over live data, a device too small to probe, and a missing
one.

**Carry-over for the operator:** this is guest-side code, so it reaches a group
only after `make rootfs` and a `/restart`. Same carry-over as the M46
claude-code pin — the rootfs has deliberately not been rebuilt in this session,
because `make rootfs` replaces the live `fcassets/rootfs.img` under running
groups.

## M139 — OAuth login follows an untrusted pre-existing `.claude` symlink — FIXED

`daemon/claude_login.go`, `authOAuthLogin`.

**Confirmed.** `claude auth login` writes to `$HOME/.claude/.credentials.json`,
and koto points HOME at the state dir — so whatever `<state>/.claude` resolves
to is where the OAuth bearer token lands, and `authOAuthPath` then reads it back
through the same link. The login created the symlink to `creds/` when the entry
was absent and otherwise **left whatever was there alone**.

That "leave it alone" had a real motive — a dev clone routinely has its own
project-local `.claude/` *directory* holding Claude Code's skills and settings,
and replacing it would destroy the operator's files — but it silently covered a
**symlink**, which is not the operator's files, it is a redirection of the
write. Anyone who can create that entry before the operator logs in chooses
where the token is written and where it can be read from. The interesting
"anyone" is the daemon: same uid, `ReadWritePaths` over the state dir.

**Fix:** `authClaudeDirCheck` establishes the boundary instead of inheriting it.
Three outcomes, two of which continue:

- absent → koto creates the `creds` symlink itself (what the installer leaves,
  and what this whole arrangement assumes);
- a real directory, or a symlink that **resolves** to `<state>/creds` → accepted,
  which covers the dev clone and every installed host. Judged by `EvalSymlinks`,
  not by the link text, so a relative link, an absolute one and a chain are all
  decided alike, and a dangling link fails with it;
- anything else — a symlink elsewhere, a regular file, a socket → **refused**,
  naming the target the token would have gone to and telling the operator to
  remove it.

`--status` reports the same judgement as a warning rather than enforcing it: the
doctor mutates nothing, and an operator whose next login is going to fail should
hear why while diagnosing. An absent link is not reported there — the login
creates it.

Checked against the live install (`/var/lib/koto/.claude -> creds`): accepted.

Test: `TestAuthClaudeDirRefusesARedirectedLink` in `daemon/audit_fixes_test.go`
covers all six shapes — absent, idempotent re-check, the clone's own directory
(asserting it is left untouched), a link outside the state dir (asserting the
message names the target), a dangling link, a regular file, and an absolute link
that lands on `creds/` anyway.

## M140 — Canceled turns cannot leave a full slot pool — FIXED

`daemon/queue.go` (`acquireSlot`), `daemon/send.go` (`sendNow`).

**Confirmed.** `sendNow` takes the turn's cancellation channel and then calls
`acquireSlot`, which loops on `slotCond.Wait()` with no way out.
`requestTurnCancel` closed the channel and woke nothing, and `sendNow` does not
look at the channel again until the slot is already in hand — so an interrupt
that landed while all ten of the group's slots were busy took effect **one slot
release later**, after acquiring a slot purely to hand it straight back at the
next check.

Each waiter is a resident goroutine, one per session, and nothing bounds how
many sessions a caller authorized to `Send` may open. So a group whose ten slots
are occupied by long turns accumulates blocked workers that cancellation cannot
clear — an authenticated, group-scoped availability problem, and one where the
operator's own remedy (interrupt everything) was the thing that did not work.

**Fix:** `acquireSlot(g, session, cancel)` now returns `(slotHold, bool)` and
re-examines `cancel` under `slotMu` before every wait; `slotWakeAll` broadcasts
from both cancellation entry points (`requestTurnCancel` and the identity-bound
`cancelTurn`). The broadcast takes `slotMu` rather than firing bare, because
`Cond.Wait` registers the waiter and only *then* releases the lock — a bare
`Broadcast` can land in the gap between a waiter's last check and its `Wait` and
be missed entirely. `nil` means "this acquisition cannot be cancelled", which is
what every non-turn caller passes.

`sendNow` discards the prompt and logs it when the wait is cancelled, matching
the two checks that already surround `ensure` and delivery.

Test: `TestCanceledTurnLeavesAFullSlotPool` in `daemon/audit_fixes_test.go`
fills all ten slots, parks a worker in the wait, cancels it through the same
`requestTurnCancel` the Interrupt RPC and `stopGroup` call, and asserts it
returns without a slot and without disturbing the pool. Verified to hang past
its own deadline with the wake removed. Also run under `-race`.

## M141 — Sanitizer accepts deceptive SGR attributes — FIXED

`daemon/sanitize.go` (`scanEsc` → `sgrApproved`), `tui/sgr.go`, `tui/vt_scrub.go`.

**Confirmed.** "Pure SGR" was the whole test: a CSI whose parameters were digits
and semicolons and whose final byte was `m` was copied through **verbatim**. Four
of those codes are not styling but deception, and every producer this sanitizer
exists for can set them — guest output, tool results, a peer's report, a
`cs-notify` title, model text:

| code | effect |
|---|---|
| **8** conceal | makes text invisible. A warning, a refusal, the `> `-quoted attribution around a peer report: gone, while the transcript still reads as complete. |
| **7** reverse | is koto's OWN vocabulary for "this is the UI, not content" — the status bar, the chip pair, the tree's cursor row — and in **mono mode it is the only signal those have**. Untrusted text painting itself reverse imitates them directly. |
| **5, 6** blink | manufacture urgency the renderer never asked for. |

**Fix:** `sgrApproved` filters the parameter list to an allowlist of the styling
half — 0/1/2/3/4/9, the decoration resets, 26, 53/55, 39/49/59, 30–37, 40–47,
90–97, 100–107, and the 38/48/58 extended forms with their arguments checked —
and drops the blink/reverse/conceal vocabulary **in both directions**: 25, 27
and 28 go too, because a lone "reverse off" inside a row koto is drawing
reversed escapes the highlight as effectively as a "reverse on" imitates it.

An allowlist rather than a denylist because the parameter space is open. A
malformed extended colour drops the **whole** sequence rather than resyncing,
since resyncing would reinterpret that form's arguments as codes in their own
right — `38;7` would otherwise leave a `7`. Styling in the same sequence as
deception survives (`ESC[1;8;31m` → `ESC[1;31m`), so a legitimately coloured
line is not collateral.

**The TUI mirrors it, with one deliberate split.** `scrubVT` — the **shell
pane** — keeps every SGR, reverse included: that pane is a terminal, framed and
bounded as one, showing the guest's own rendering, and `less`, `vim`, `fzf` and
`tmux` all reverse cells for legitimate reasons. Taking that away would break the
view to prevent a spoof the surrounding frame already contains. The new
`scrubVTStrict` applies the allowlist and is what the paths rendering untrusted
text as koto's own interface now use: job-tail chunks, error lines, decoded tool
arguments, shell error text. The TUI's version is expressed on the existing
`forEachSGRAttr` parser rather than re-parsing.

Tests: `TestSanitizeKeepsStylingAndDropsDeceptiveSGR` in
`daemon/audit_fixes_test.go` (23 cases plus a property check that no arrangement
of parameters gets conceal/blink/reverse through) and
`TestScrubVTStrictDropsDeceptiveSGR` in `tui/sgr_test.go`, which also pins that
the shell pane KEEPS its reverse video and that both scrubs still drop
everything that is not an SGR.

## M142 — Installer accepts a missing or incomplete artifact manifest — FIXED

`daemon/install.go`, `verifyArtifactsUI`.

**Confirmed, with one correction to the threat model.** Two of the manifest's
behaviours were deliberate and documented — an artifact with no entry is
skipped, and a manifest with no entries at all is not an error — but both were
**unconditional**, which made the only integrity check switchable off from
outside the code:

- delete `dist/artifacts.sha256` → a warning, then install;
- make it unreadable → `os.ReadFile` fails, and the error was swallowed by the
  "no manifest" branch, which is wrong on its own terms: a permission error or a
  short read on a file that *is there* says something is wrong with the tree;
- drop just the `koto` and `koto-tui` lines → the two artifacts that land
  root-owned on PATH go unchecked while the message still says "verified".

`dist/VERSION` is committed alongside the manifest and says which world the tree
is in — it is the same guard the Makefile's `fetch` target already uses. So the
exceptions are now scoped to "no release exists yet": a **released** tree refuses
a manifest that is missing, unreadable, or that fails to cover `koto` and
`koto-tui`, with a message naming `git checkout dist/artifacts.sha256` as the
fix. An unreadable manifest is refused in **both** worlds.

The non-reproducing artifacts keep their exemption unconditionally — `vmlinux`
and `rootfs.img` embed build timestamps and resolved package versions, so
`make build` legitimately produces bytes a published manifest cannot match, and
holding them to a published hash would break the build-from-source route the
project offers on purpose. Firecracker has its own pinned checksum in
`build-firecracker.sh`.

**The correction:** the finding says this is a bypass "when the installer
executable is trusted but an attacker can modify the staged artifact files or
manifest". Against an attacker with write access to the checkout the delta is
zero either way — they would edit the manifest to match their binaries rather
than delete it. What this actually closes is the case the manifest was built
for: the artifacts arrive over `KOTO_DIST_URL` while the checksums arrive over
git, and a tree that has *lost* its manifest through any means — a bad merge, a
partial checkout, a `make clean` that went too far, a tampering mirror serving a
truncated file — no longer installs unverified binaries while calling itself a
release.

Test: `TestReleasedTreeRefusesAnAbsentOrGuttedManifest` in
`daemon/audit_fixes_test.go` runs every combination of `dist/VERSION` present /
absent / `unreleased` against a full, partial, entry-less, missing and
unreadable manifest, and pins that a pre-release tree keeps every exception it
had. The existing `TestArtifactVerificationCoversEveryManifestEntry` (M116) still
passes unchanged.

## M144 — Per-session log clear can exhaust daemon memory — FIXED

`daemon/sessions.go`, `filterLogSession`.

**Confirmed.** The rewrite was `os.ReadFile` → `string(b)` → `strings.Split` →
`strings.Join` → `[]byte(content)`: four full-size allocations alive at once,
plus a string header per line all pinning the original. Per-file ceiling is
1 GiB and a per-session clear sweeps **all eleven** of a group's streams, so a
client authorized to operate a group could grow a permitted log and then call
`Clear` on it.

**Fix: streamed, in two passes, and allocation-free per line.**
`filterSessionScan` reads with `bufio.Reader.ReadSlice`, so the slices it hands
out point into the reader's own buffer and not even a line is allocated — only
the two marker shapes are converted to strings, matched by byte prefix first,
and both are short by construction. A line longer than the buffer arrives in
several chunks; only the first carries the decision and the rest follow it, so
an unbroken megabyte streams rather than being assembled.

**Two passes rather than one**, to keep the property that matters more than the
I/O: a stream with nothing to drop is **not rewritten**. The rename bumps the
inode, every live tailer then reopens at EOF, and bytes landing in that gap — a
`[[turn_end]]` among them — are lost to the live view, stalling a healthy turn
for the full wait window (the reason that check exists at all). A session's
segments live in a few of a group's streams; the rest must pass untouched,
especially the ones an active turn is writing this instant. Pass one only
decides, reading and discarding; pass two runs only when the answer is yes.

Measured by the test: clearing a **23.7 MB** log allocates **199 KB** — the read
and write buffers, and the marker lines. The old shape's peak was several times
the file.

Test: `TestFilterLogSessionIsStreamed` in `daemon/audit_fixes_test.go` pins the
allocation bound, byte-for-byte equivalence with the buffered implementation
across seven shapes (empty, nothing to drop, all-target, leading default
segment, clearing the default, no trailing newline, unterminated target tail),
that an untouched stream keeps its **inode**, and that a 300 KiB unbroken line
survives the chunked path intact.

### Aside — the `-race` suite (found while pinning M144)

Running the daemon suite under `-race` reported a data race on the **`turnFn`
test seam** itself. The seam's comment argued it was safe because tests use
unique group names, so a group's dedicated worker only reads `turnFn` after the
enqueue that follows the swap. That is true of the worker a test *creates* and
false of the ones it *inherits*: a worker outlives the test that made it
(`sessionIdleMax`, `retireQueue`), so a previous test's worker draining its last
job reads the variable while the next test writes it. A second one sat in
`turnRec`, which the goal tests read without the lock its own writers take.

Both are fixed — `setTurnFn` behind an `RWMutex`, and a locked `all()` snapshot —
because a suite that cannot be run under `-race` masks every real race behind
the first reported one, which is a poor instrument to audit with.

**Residual, stated rather than left silent:** `-race` over the whole suite still
reports cross-test races on the daemon's path globals (`HERE`, `ROOT`,
`SOCK_DIR`, `GROUPS_FILE`), which `fcHarness` and friends re-point per test while
tailers and workers from earlier tests are still running. Fixing that means
either draining every background goroutine between tests or making the path
globals atomic, which is a structural change to the daemon's most basic state
and its own piece of work. None of it reproduces in isolation, and the suite is
green in its normal (non-`-race`) mode.

## M145 — Failed self-heal advances a session while the stalled guest worker may remain alive — FIXED

`daemon/send.go` (the stall branch), `daemon/queue.go`.

**Confirmed.** The queue's single-flight guarantee for a conversation rests on
`sendNow` not returning until the previous guest worker has stopped. The stall
branch says as much in its own comment — *"the guest side of this turn may still
be alive and writing"* — quarantines the slot for exactly that reason, and then
called `selfHeal(g, now)` **without looking at the result**.

`selfHeal` returns false in two ordinary situations: the circuit breaker is open
(`healMaxAttempts` restarts inside `healWindow`, which logs "leaving STALLED —
manual /restart needed") and the restart itself failed. In both, the VM is still
up and the wedged worker may still be running — and the turn retired anyway, so
the session's next queued prompt took a different, non-quarantined slot. Two
claude processes on one conversation id, sharing one workspace: interleaved
responses, interleaved tool calls, interleaved file writes.

The slot quarantine only protects the log **stream**. The conversation needed its
own fence.

**Fix:** a failed self-heal marks the conversation wedged (`sessionWedged`,
beside the slot quarantine and under the same lock, since it has the same
lifecycle). Both admission points refuse it: `enqueueSend`, so a caller hears
about it synchronously, and `sendWorkerTurn`, for whatever was already queued
when the fence went up. The refusal names `/restart` — the same remedy the
breaker's own log line names.

**It lifts by itself wherever something proves no writer survives:**
`releaseGroupQuarantine` (the VM exited or a restart replaced it — group-scoped,
and only that group's), and `notifyTurnDone` (the wedged worker finally wrote its
`[[turn_end]]`, so the group recovered without the restart). Per session, because
the wedged worker is: a group's other conversations are untouched.

Test: `TestWedgedSessionIsFencedWhenSelfHealFails` in
`daemon/audit_fixes_test.go` covers both admission points, the message naming
`/restart`, both lift paths, that a sibling conversation is untouched, and that
one group's quarantine release does not lift another group's fence. Verified to
fail with the admission checks removed.

## M146 — `pki init` follows attacker-controlled symlinks — FIXED

`daemon/pki.go`.

**Confirmed, and wider than the finding states.** `pkiInit` asked
`os.Stat(aclPath)` whether `acl.json` existed — which **follows** a symlink and
reports a dangling one as an error, i.e. as absent — and then wrote with
`os.WriteFile`, which follows the same link with `O_CREATE`. A link planted in
the creds directory before an administrator runs `koto pki init` therefore
redirects the seed.

The same stat-then-write shape guarded **`ca.key`**, which is the material this
entire PKI rests on: a dangling link there sent a freshly generated CA private
key to a path of somebody else's choosing. `clients.allow` was appended to with
`O_APPEND|O_CREATE` and no `O_NOFOLLOW`, so a link there writes an allowlist
entry into someone else's file. And `pkiReadKey`/`pkiReadCert` followed links on
the way *in*, which for `acl.json` is the worse direction — that is the file
`loadACL` reads, so accepting a link means taking the authorization policy from
wherever it points.

**Fix, three parts:**

- `pkiWriteFile` — `O_WRONLY|O_CREATE|O_TRUNC|O_NOFOLLOW`. Overwrite is still
  fine (a server cert is reissued in place); only the link is refused. Used by
  every PEM write, the token file and the `tokens.json` temp.
- `pkiCreateNew` — `O_WRONLY|O_CREATE|O_EXCL|O_NOFOLLOW`, and **`O_EXCL` is the
  absence check**, which is the point: the stat-then-write it replaces had both
  halves of the problem at once, the following stat and the window after it.
- `pkiNoSymlink` for the paths that are *read* as well as written (`ca.key`,
  `ca.crt`, and the `acl.json` seed). Needed because **`O_EXCL` outranks
  `O_NOFOLLOW`**: a symlink at the final component comes back `EEXIST`, not
  `ELOOP`, so it is indistinguishable from the ordinary "already seeded" case
  that must pass — the one the CA's reuse-never-regenerate property depends on.
  An `Lstat` in that branch tells them apart.

A refusal says "is a symlink" rather than surfacing a bare `ELOOP`, which on a
path the operator just typed reads as nonsense.

**Not changed:** `koto pki` accepting an arbitrary `-creds` directory. That is
the operator naming where their own credentials live, the same decision
`KOTO_HOME` is, and the trust model does not second-guess tier 1 about it. What
it must not do is write *outside* the directory it was given, which is what the
above fixes.

Test: `TestPKIRefusesToWriteCredentialsThroughASymlink` in
`daemon/audit_fixes_test.go` — four subtests: a dangling link at `acl.json`
(refused, and nothing created at the target), the same at `ca.key` (the CA
private key is not written through it), an append-through-a-link at
`clients.allow` (the victim file is byte-identical afterwards), and idempotence
on an ordinary tree, since the exclusive create must not break the property
`make pki-init` depends on — an existing CA is reused, never regenerated.

## M143 — Guest-controlled PTY backpressure can freeze the operator TUI — FIXED

`tui/shell_view.go`.

**Confirmed, and it is the same freeze the drain goroutine was introduced to fix,
one layer further down.** `shellSession.send` took `sendMu` and called
`stream.Send` under it. Two goroutines call it: the Update goroutine
(keystrokes, pastes, resizes) and the goroutine that drains `term.Read()` for
the emulator's own protocol responses — which exists precisely because a
blocking emulator pipe once froze the event loop on the first `/shell` attach.

`stream.Send` blocks when the chain below it stops draining: daemon → vsock →
fc-agent → the guest's pty master. The guest controls **both** ends of that — it
decides what the pty emits *and* whether anything reads the pty's input — so it
can emit terminal queries forever while reading nothing, wedge the drain
goroutine inside `Send`, and leave the Update goroutine blocked on the mutex
behind it. That is the whole Bubble Tea event loop: no redraw, no `ctrl+]` to
detach, no `/exit`, no way out but killing the TUI.

**Fix:** one writer goroutine owns `stream.Send`, fed by a bounded queue
(`shellOutQueue`, 256 messages — one per keystroke, paste or resize, so ordinary
use never approaches it). `enqueue` never blocks: a full queue means the guest
has stopped reading its pty entirely, so the bytes were not going to arrive
anyway, and they are dropped with a log line naming the group rather than paid
for with the operator's UI. The single writer is also what makes the mutex
unnecessary — grpc-go only forbids *concurrent* senders.

A session with no queue (no writer goroutine) sends inline. That is the
degenerate case of the same rule rather than an exception to it: with nothing to
hand the message to, there is nothing to be blocked by.

Test: `TestShellSendNeverBlocksOnAWedgedGuest` in `tui/shell_keys_test.go` wedges
a stub stream inside `Send`, then fires three queues' worth of keystrokes plus a
resize and asserts they all return — and that the queue stays bounded. Run under
`-race` as well.

## M147 — Credential writes follow attacker-controlled state-directory symlinks — FIXED

`daemon/claude_login.go`, `authConnect`.

**Confirmed, and it is M146's other half.** The credential flow trusted
`<state>/creds` as a filesystem object: `os.MkdirAll` is happy with an existing
**symlink** in the path, `os.Stat` follows one, and `os.WriteFile` follows a
pre-existing destination link. So every secret this flow produces — the OAuth
refresh token, an entered API key, and through the wizard a CA private key and a
bearer token — could be written into a directory of somebody else's choosing by
anyone able to write the state tree before the operator runs it.

The predicate that catches this already existed: `stateDirTrusted` (real
directory, owned by the invoking user, no group/other write), introduced at
audit L9 for exactly this reason — *"a pre-created one under /tmp or a symlink is
another user's directory wearing the name"*. It was just confined to
`seedStateDir`, i.e. to `koto install`, and never applied to the standalone
paths, which are the ones that actually write the secrets.

**Fix:** `authConnect` creates `creds/` and then checks what is *there*
(`Lstat` after `MkdirAll`, so an existing symlink is seen as a symlink), through
the same `stateDirTrusted`. The API key write moved to `pkiWriteFile`, so the
final component cannot be a link either (M146).

**Scope, deliberately:** the **creds child**, not the state dir. Checking the
child is enough — a symlink there is caught as a symlink, and swapping in a
directory of one's own is caught by the owner test, both regardless of what the
parent allows — and holding the state dir to the mode rule as well would refuse
an ordinary 0755 checkout on a shared dev box for no credential-bearing reason.

Checked against the live install (`/var/lib/koto/creds`, `drwxr-x---`, owned by
the operator): accepted.

Test: `TestCredentialFlowRefusesAnUntrustedCredsDir` in
`daemon/audit_fixes_test.go` drives `authConnect` against a symlinked creds dir
(refused, with the reason named), exercises the owner and mode rules on the
shared predicate, and asserts that a freshly created creds dir passes.

## M148 — Malformed provider configuration fails open to Venice — FIXED (the write half was already closed)

`daemon/proxy.go` (`groupProvider`), `sidecar/cs-subagent`.

**Confirmed: three readers of one field, two answers.** The proxy's
`groupProvider` and `cs-subagent` answered **"venice"** for every failure —
absent file, unparseable JSON, unrecognised value — while the guest turn path
(`fcguest/turn.go readGroupConfig`) answered **"claudesdk"** for exactly the same
state. So a claudesdk group whose `config.json` was momentarily unreadable had
its traffic relayed to a different third party than the operator selected —
system prompt and workspace-derived task material included, with the credential
for it injected by the proxy.

The Venice default was true when it was written ("this is a Venice-first
deployment", says the comment it carried). It has not been for a long time:
`defaultProvider` is `claudesdk`, and `ensureProviderConfig` **writes that into
any group whose config is missing or invalid** on every `ensure()` — so a group
whose config cannot be read is, by koto's own definition, a claudesdk group.
Answering otherwise was the fail-open.

**Fix:** one implementation. `groupProvider` is now a thin wrapper over
`groupProviderName` → `loadGroupConfig(g).provider()`, which already returns
`defaultProvider` for a nil/failed parse and for a value that is neither known
provider. `cs-subagent`'s `jget` fallback becomes `claudesdk`. All three readers
now agree, which was the actual defect.

**The finding's second half was already closed**: `configCmd` no longer rewrites
`config.json` with a truncating `os.WriteFile` — `updateGroupConfig` holds the
group's config lock across read-mutate-commit and commits by **rename**, so a
concurrent reader sees either the old document or the new one, never half of
either.

Tests: `TestProviderResolutionFailsClosed` in `daemon/audit_fixes_test.go` walks
eight config shapes (explicit venice, explicit claudesdk, no key, empty
document, truncated mid-write, non-JSON, unrecognised value, wrong type) plus a
missing file and the empty group name, asserting the result AND that the proxy's
reader and the canonical one never disagree. `cs-subagent`'s one-liner was smoke
tested directly against the same four shapes: `venice` only for an explicit
`venice`, `claudesdk` for a truncated document, an empty one and a missing file.

## M149 — Guest can forge notification session attribution — FIXED

`daemon/ctl.go`, the `notify` verb.

**Confirmed.** The verb ran the payload's session through `normalizeSession`,
which validates the **charset** and nothing else — and `goal-anything` passes
it. So a group could attribute a notification to a live goal's worker or judge
conversation, the two the design calls follow-only. The ordinary `send` path
refuses reserved names outright, and `job_done` folds them to the default
session (audit M42, with a warn line); `notify` had neither check.

It is worse than a misattribution. A goal session's leaf in the tree comes from
`goalLiveSessions`, which shows it exactly while the run is live and drops it
when the goal ends, and the operator cannot type into or open it — so a
notification parked there is **hidden**, not just misfiled. That is the
suppression the finding names.

**Fix:** the same treatment `job_done` already gets — fold to the default
session with a warn line naming the group and the attempted name. Fold rather
than drop, because the notification is still the operator's to see; ordinary
named sessions are untouched, since attribution within the group's own
conversations is exactly what the field is for.

Test: `TestCtlNotifyCannotClaimAReservedSession` in `daemon/audit_fixes_test.go`
drives the real `ctlDispatch` → `tryFlushNotify` → parser round trip, asserting
an ordinary session keeps its attribution and that four shapes of reserved name
land on the default session with their content intact. Verified to fail with the
check removed.

## M150 — Unbounded Claude stream parser state permits guest memory exhaustion — FIXED

`fcguest/turn.go`, `claudeParser`.

**Confirmed.** The parser retained three things without any bound: a
`strings.Builder` per open tool block (`input`), a `strings.Builder` per open
thinking block (`words`), and the two maps keyed by content-block index. A
provider can emit any number of valid stream events, for any number of indices,
with no `content_block_stop` ever arriving — and nothing else caught it: the
scanner's 16 MiB limit bounds one JSON **record**, not the accumulation across
records; the turn queue is bounded but a tool input is not framed until its stop
event; and the host's frame-size and transcript checks happen only *after* the
guest has allocated, serialized and transmitted the value. The watchdog is a
time limit, not a memory one.

**Fix — three bounds, one of which removes the need for a bound entirely:**

- **Thinking retains nothing.** The body is already streamed straight to the
  host as it arrives; the only thing the block needed at stop time was the word
  **count**, so the builder was accumulating a whole reasoning trace to call
  `strings.Fields` on it once. `thinkState` now counts incrementally — exact,
  because `Fields` splits on runs of Unicode space, which is the same as
  counting space→non-space transitions with the boundary state carried between
  chunks — and keeps two words of state whatever arrives.
- **Tool input is capped per block and per turn**: `claudeToolInputMax` (1 MiB)
  and `claudeToolInputBudget` (8 MiB across all open blocks). 1 MiB is
  deliberately far above what survives the trip — the daemon truncates a
  `[[tool]]` marker body to 64 KiB for display, which is the only use this value
  has — so nothing visible is lost, and a capped block is marked so the emitted
  frame says `…[truncated]`. A block's bytes return to the budget when it stops.
- **Open blocks are capped** at `claudeMaxOpenBlocks` (64), over both maps
  together, with a one-per-turn log line. A real stream has a handful.

Test: `TestClaudeParserStateIsBounded` in `fcguest/turn_test.go` — 4 MiB offered
into one block (capped, marked, and released on stop), 64 MiB offered across
128 blocks with no stop events (both the map and the aggregate hold), and a
thinking trace split mid-word and mid-space across chunks (count matches
`strings.Fields` exactly, and nothing is retained).

**Carry-over:** guest-side, so it reaches a group only after `make rootfs` and a
`/restart` — same as M138 and M46.

## M151 + M152 — Forced stop races the offline resize, and releases memory before the VMM dies — FIXED together

`daemon/fc.go`, `fcStop` and `fcSpawn`'s failure closure. The findings are two
faces of one thing — `fcStop` returning while the VMM may still be alive — and
M152 says so itself ("two instances of the same teardown/accounting flaw …
should be fixed through a common lifecycle path").

**What the old code did:** deleted the registry entry *first*, sent the agent a
shutdown, waited 5 s, `SIGKILL`ed, and then — with no wait at all — removed the
pidfile, the socket dir and the jail, and returned.

**M152, accounting.** `fcHostMemCommittedMiB` sums `fcVMs`. Deleting first made
a still-running VMM invisible to fleet admission for the whole shutdown window
(up to 8 s), so a concurrent spawn could commit past the cap — and past the
`vms/` cgroup's `memory.max`, which is where the kernel decides which VM to
OOM-kill. `fcSpawn`'s `fail` closure had the same shape: `fcHostMemRelease`
before the `SIGKILL`, and no wait after it.

**M151, corruption.** `fcGrowWorkspaceImg` runs `truncate` + `e2fsck -fy` +
`resize2fs` on the group's ext4 image. It is guarded — `fcSpawn` refuses when
`fcRunning(g)` — and `fcRunning` falls back to the **pidfile** when the registry
entry is gone. Which is precisely the file `fcStop` deleted after an unconfirmed
kill. So a VMM that survived the `SIGKILL` had its group read as stopped, and
the next `ensure()` ran a filesystem repair-and-resize on an image that process
still had open.

**Fix — one rule: do not return until the VMM is gone, and when it will not go,
leave the evidence in place.** `fcAwaitExit` waits up to `fcReapWait` (20 s,
identity-checked throughout so a recycled pid is not mistaken for it). Then:

- the registry entry is deleted only after the exit is confirmed, so the
  memory keeps counting while the process lives;
- the pidfile is removed only after the exit is confirmed. When the kill does
  not take, it stays — the group keeps reading as running, which blocks both
  the resize and a second boot onto the same image — and an **error**-level log
  line says so, naming the pid and suggesting D state;
- `fcSpawn`'s `fail` closure kills, reaps, and only then releases the
  reservation; an unreaped pid keeps its memory reserved.

20 s is generous on purpose: what it waits out is a VMM stuck in an
uninterruptible kernel path. `kvm_async_pf` under host memory pressure is the
observed one, and a D-state process does not die on `SIGKILL` until the fault it
is waiting on completes — which is the same condition the operator already
handles by hand with the rule "never boot a group whose old VMM is D-state".
This makes the daemon follow that rule on its own.

Tests: `TestFcStopKeepsTheEvidenceWhenTheKillDoesNotTake` builds a real process
whose `/proc/<pid>/comm` is `firecracker`, confirms the pidfile alone makes
`fcRunning` true, then drives both branches — the kill that does not take (the
pidfile and the registry entry both survive, the group still reads as running)
and the kill that does (both are cleaned up). `TestFcAwaitExitJudgesIdentity`
pins that a live pid which is *not* ours does not hold a stop open.

## M153 — Default fleet memory cap ignores the daemon's effective cgroup limit — FIXED

`daemon/fchostmem.go`, `fcHostMemInit`.

**Confirmed.** The derived cap was 90% of `/proc/meminfo`'s `MemTotal` and
nothing else. cgroup limits are **hierarchical** — a child may name a number
larger than its parent allows, and the kernel enforces the parent's — so a
daemon running under a tighter ancestor (a slice with `MemoryMax`, a container,
a delegated scope) got a `vms/` `memory.max` it could never honour, and
admission went on saying yes until the **ancestor** hit its limit. At that point
the kernel picks its victim from that whole subtree, daemon and proxy included
— which is the one outcome the `vms/`-vs-`main/` split exists to prevent, as the
file's own header says: *"an OOM-killed daemon is a fleet outage."*

**Fix:** take the **smaller** of the two bounds, because both are real —
`MemTotal` is what the machine has, the cgroup limit is what this service may
use of it. `fcCgroupMemLimitMiB` walks the daemon's own cgroup and every
ancestor up to the mount root and returns the tightest `memory.max` in force
(`max` and unreadable levels contribute nothing). The same
`fcHostMemDefaultPct` applies to whichever bound wins, and the startup log line
now names which one it was. `KOTO_HOST_MEM_MIB` still outranks both: it is the
operator's call.

Best effort throughout — an unreadable file, a missing controller, or a cgroup
path this process cannot see all mean "nothing known here" and leave the
`/proc/meminfo` default exactly as it was, so nothing regresses on a host with
no limits (which is what this machine reads today: no `memory.max` at the root,
`max` on `system.slice`).

Test: `TestFleetMemoryCapHonoursTheCgroupLimit` in `daemon/audit_fixes_test.go`
builds a fake three-level hierarchy and pins each case: an all-`max` tree is no
limit, the tightest wins wherever it sits, **a child naming more than its parent
allows loses to the parent** (the hierarchy property the finding turns on), a
missing `memory.max` is not a limit of zero, the resolved cap takes the smaller
of the two bounds, and `KOTO_HOST_MEM_MIB` overrules both.

## M154 — SubscribeGroup can retain streams after subscriber shutdown — PARTLY FIXED, with the limit stated

`daemon/events.go`, `daemon/groups.go`.

**Confirmed, in the part that is this side's to fix.** The handler's select
returns on `sub.done` and `ctx.Done()` — **between** sends. A `stream.Send`
already in flight blocks on HTTP/2 flow control for as long as the client keeps
the connection open without reading it, and closing a channel does not
interrupt that. So `shut()` — called by the overflow path and by `destroy()` —
left the subscriber **registered**, with its 256 queued frames held and `emit()`
still walking it on every event of that group.

**Fixed:** `dropSub` is `shut()` plus the cleanup that used to wait on the
handler noticing — deregister, then drain the queue. The overflow path uses it,
and `destroy()` drains each subscriber it deregisters. The channel is
deliberately **not closed**: the handler may still be selecting on it, and a
closed channel would hand it a nil event to `Send`.

**Not fixed, and why:** the handler goroutine and its stream. Returning from a
server handler with a `Send` in flight, and calling `Send` from a second
goroutine so the first can be abandoned, are both outside what grpc-go permits,
and the server cannot cancel a stream context it does not own. So a stuck stream
holds its own goroutine until the transport dies.

**What bounds that is already in place**: `streamAdmit` (audit M95, this same
audit) caps concurrent streams at 512 per identity and 2048 globally, across
every streaming RPC. The finding's "consume daemon goroutines … to degrade or
deny service" is therefore already bounded in count; what this fix removes is
the *memory* behind them — up to 256 events each, which for streamed chunk
frames is the part that actually adds up — and the correctness bug where
`destroy()` could not get rid of a subscriber.

**Considered and rejected:** a server-wide `MaxConnectionAge`. It would
eventually reclaim any stuck stream, and the protocol is built for reconnection
(`since_seq` resumes gaplessly). But it tears down every healthy long-lived
stream on a timer, for every client including the out-of-tree Android app, to
reclaim a goroutine whose count is already capped. That is a behaviour change to
every client for a bounded gain, and it should be an operator's decision rather
than a side effect of this finding.

Test: `TestShutSubscriberIsDeregisteredAndDrained` in
`daemon/audit_fixes_test.go` overflows a subscriber through the real `emit`
path, then asserts it is shut, deregistered, drained, its channel still open,
and that later events do not reach it. Verified to fail with `dropSub` reverted
to a bare `shut()`.

## M157 — Incomplete Unicode format filtering permits operator-facing text spoofing — FIXED

`daemon/sanitize.go` (`isBidiOrFormat`), `tui/vt_scrub.go` (`isHostileFormat`).

**Confirmed, and the finding's last sentence is the important one:** the daemon
and the TUI kept **separate** hand-maintained lists of fourteen runes each, so a
rune only one of them dropped still reached the operator's terminal through the
other sink. Both lists leaked the same way — a `switch` somebody has to remember
to extend, against a character set that keeps growing.

What got through: `U+00AD` SOFT HYPHEN, `U+034F` COMBINING GRAPHEME JOINER,
`U+180E` MONGOLIAN VOWEL SEPARATOR, the invisible operators `U+2061`–`U+2064`,
`U+2065`, the Hangul fillers (`U+115F`, `U+1160`, `U+3164`, `U+FFA0` — blank,
but not space, so no width check catches them), and the whole **TAG block**
`U+E0000`–`U+E007F`, which encodes arbitrary ASCII invisibly.

**Fix:** derive the set from Unicode's own tables instead of a list.

- `unicode.Cf` — every format character: the bidi overrides and isolates, the
  zero-width set, the BOM, the Arabic marks, the tag block.
- `unicode.Other_Default_Ignorable_Code_Point` — the invisible runes that are
  *not* `Cf`, which is precisely where the hand list leaked.
- `U+2028`/`U+2029` stay listed: they are `Zl`/`Zp`, not format characters, and
  a rendered row must stay one row.

**Two exemptions, both kept deliberately:** `ZWJ` (emoji grapheme clusters — the
existing carve-out) and `ZWNJ` (Persian and Indic word separation). Neither can
move the cursor, reorder anything or hide anything; ZWNJ's effect is to make
text look *more* separated, which is the wrong direction for a spoof. Variation
selectors are in neither table — they carry the `Variation_Selector` property —
so emoji presentation survives; checked against `U+FE00`, `U+FE0F` and
`U+E0100` rather than assumed.

**The cost, stated rather than hidden:** `Cf` also holds the Arabic number-sign
prefixes (`U+0600`–`U+0605`, `U+06DD`, `U+08E2`), so text using them loses a
rendering hint. That is the same trade the old list already made for ZWSP and
the bidi marks — invisible characters do not get to stay on the grounds that
some of them are innocent.

Tests: `TestSanitizeDropsEveryInvisibleRune` (daemon) and
`TestScrubVTDropsEveryInvisibleRune` (TUI) walk the same 24 runes — the fifteen
the old lists missed plus the nine they caught, so neither can regress — and
assert the exemptions, a real ZWJ family emoji, a presentation selector, and
that Arabic, Devanagari, Japanese and accented Latin pass through unaltered. The
TUI case covers both `scrubVT` and `scrubVTStrict`.

## M158 — Minute-by-minute cron evaluation enables scheduler denial of service — FIXED

`daemon/cron.go`, `parsedCron.next`.

**Confirmed, and measured.** `next` advanced one minute at a time across a
four-year horizon, so answering "never" meant visiting all ~2.1 million minutes
in it. `0 0 30 2 *` — February 30th — reaches that full scan, because the parser
accepts it: every field is individually in range, and only `next` failing to
find a match makes `addSched` reject the schedule. Which means the scan runs
*before* anything is persisted, so it does not consume the schedule quota and can
be repeated. `toggleSched` runs the same evaluator for every `enabled=true`
request including a redundant one, and the toggle and due-entry paths hold
`schedLock` while doing it, so they block every scheduler operation for the
duration.

Measured on this machine (`BenchmarkCronNextImpossible`, kept in the test file):

| | per call |
|---|---|
| minute walk | **23.8 ms** |
| skipping | **22.6 µs** |

**Fix:** advance by the largest unit that cannot match. A non-matching month
jumps to the first minute of the next one, a non-matching day to the next
midnight, a non-matching hour to the next hour, and only a non-matching minute
costs a minute. An impossible date then crosses months at day speed rather than
minute speed. The day rule is factored into `dateMatches` so the POSIX
AND/OR semantics are stated once.

Every jump is wrapped in a `step` guard that falls back to one minute if the
computed time is not strictly after the current one. The jumps are all forward,
including on both sides of a DST fold — but a loop that fails to advance is the
exact failure class being fixed here, so it is checked rather than trusted.

Test: `TestCronNextSkipsInsteadOfWalking` in `daemon/audit_fixes_test.go` asserts
the timing bound (5 ms, three orders of magnitude clear of both implementations),
that seven real expressions resolve to the right minute, and — the part that
matters most for a rewrite like this — **equivalence with an exhaustive minute
walk**: ten expressions from five different starting points, each crossing month,
day and hour boundaries, must return the same FIRST matching minute. Verified to
fail on the timing bound with the old body restored.

## M155 — TUI retains deleted group data across clear, destroy and same-name reuse — FIXED

`tui/model.go`.

**Confirmed.** The client-side deletion boundary covered the state with a
*visible* symptom and stopped there. When a group left the daemon's snapshot —
a destroy, from this client or any other — the prune loop dropped `activity`
(a stale entry pins the 80 ms spinner tick chain on forever), `busy`, and
`unread` (a phantom pink dot). Everything else keyed by the group's **name**
stayed: the transcript lines, the viewport cache, the resume cursor (`lastSeq`),
the subscription flag, the paging and job-linger state, and — the privacy part —
`promptHistory`, `histNav` and `histDraft`, so the prompts the operator typed
into that group were still one Up-arrow or ctrl+R away. Group names are
reusable, so a later group of the same name inherited all of it.

A group-wide `/clear` had the same gap from the other direction: the transcript
lines went and the prompts stayed.

**Fix:** two functions, because the two paths mean different things.

- `forgetGroup(g)` — everything keyed by the name, called from the prune loop
  when a group vanishes from the snapshot. The list is exhaustive rather than
  symptom-driven, and the composite-keyed maps are swept by prefix, which works
  uniformly because `turnKey`, `unreadKey`, `sessKey` and `jobKey` are all
  `"<group>\x00<rest>"`.
- `forgetGroupHistory(g)` — what the operator would call "this conversation":
  the recallable prompts, the history cursor, the unsent draft, and the three
  **content-keyed render caches** (`vpCache` holds the group's rendered
  viewport, `mdCache` the rendered markdown of its messages, `treeRowCache` rows
  built from its name). Dropping the lines without those leaves the text cached
  under a key a later same-named group can hit. Called by `forgetGroup` and by
  the group-wide `/clear`.

Tests: `TestDestroyedGroupIsForgottenCompletely` populates thirteen state maps
plus transcript, prompts, cursor and draft, makes the group disappear from a
`listMsg`, and asserts every one is gone while a sibling group's prompt history
is untouched; `TestGroupClearForgetsThePrompts` drives the real `clear` response
path. Both verified to fail with the wiring reverted.

## M156 — SubscribeLogs silently drops audit events under backpressure — FIXED

`daemon/events.go`, `logDeliver`.

**Confirmed.** The fan-out sent non-blocking into each subscriber's 256-entry
channel and took the `default` branch on a full one — no marker, no stream
termination, nothing. The daemon log is the record an operator checks *after*
the fact (a resource alert, an auth rejection, an error forwarded from
`logalert`), and unlike the group event stream it carries no sequence numbers to
notice a gap with, so at the one consumer that renders it "nothing was logged"
and "you did not receive what was logged" looked identical. The 200-line replay
ring only helps a reconnect that happens before the lines age out.

**Fix:** the subscriber is told. Each `logSub` counts its own dropped lines, and
the next line it *can* receive is preceded by a `warn` frame naming the count
and pointing at `koto ctl logs`, which re-reads the daemon's own log. The count
is never lost, only deferred: if the notice itself does not fit, the counter
keeps climbing and the notice goes out when the subscriber catches up.

The fan-out moved inside `logSubsLock` — which `logDeliver` already takes for
the ring append — because the counters are per subscriber and this is their only
writer. The sends stay non-blocking, so the critical section is bounded by the
subscriber count.

Not changed: the drop itself. Blocking the emitter on a slow subscriber would
let one client stall every log line in the daemon, and `emit`'s answer for the
group stream (shut the stream, resume with `since_seq`) needs sequence numbers
this stream does not have. The record itself is never lost either way — every
line is mirrored to stderr and to the ring before the fan-out.

Test: `TestLogSubscriberIsToldWhatItMissed` in `daemon/audit_fixes_test.go`
overflows a subscriber, asserts the exact count, drains it, and asserts the
notice arrives **before** the next content line and resets the counter — then
repeats the overflow to pin that a notice which cannot fit is deferred rather
than lost.

## M159 — Group name `host` aliases fleet-wide alert state — FIXED

`daemon/resources.go`.

**Confirmed.** `resAlertLevel` was keyed by an unqualified string: the fleet
filesystem check wrote the literal `"host"`, the per-group disk check wrote the
raw group name, and `validGroupName` accepts `host`. So a group of that name and
the whole host's filesystem were **one subject** — a group disk crossing raised
`resAlertLevel["host"]` to that level, and the next *host* filesystem crossing at
the same severity was then not an increase and notified nobody.

Which alert that suppresses matters: at 100% host filesystem every guest
remounts read-only and the entire fleet wedges. It is the one this project least
wants silenced, and the suppression needs no privilege beyond spawning a group
with a particular name.

**Fix:** namespace every subject — `hostfs:`, `disk:<g>`, `cpu:<g>`. `:` is
outside the group-name charset (`[A-Za-z0-9][A-Za-z0-9_-]{0,31}`), so no group
name can produce a subject in another namespace. The CPU subject already had a
prefix; the disk one was the bare name, which is what collided.
`resForgetAlert` on destroy now drops both namespaced subjects.

Test: `TestAlertSubjectsCannotAliasEachOther` in `daemon/audit_fixes_test.go`
fires a group named `host` at critical and asserts the host filesystem still
fires, that the CPU subject is a third thing again, and that the charset cannot
produce a colon. Verified to fail with the bare-name keying restored.

## M165 — OAuth login inherits a caller-controlled credential destination — FIXED

`daemon/claude_login.go`.

**Confirmed in the part that is real; the second half was already fixed.** The
login subprocess got `os.Environ()` with only `HOME` replaced, so the
destination of a **credential** write was decided by whatever the operator's
shell exports ahead of `HOME`. `CLAUDE_CONFIG_DIR` and `XDG_CONFIG_HOME` are
config-root overrides: an operator who has one set for their own use would have
had `koto claude-login` write the koto token into their personal config — the
precise arrangement the trust model rules out (*"creds/ is dedicated, not
~/.claude. Compromise of cs_host can only steal the koto token, not your
personal claude session"*).

**Fix:** koto decides. Those two are removed from the child's environment, along
with `CRED_PATH` — which is koto's own variable rather than the CLI's, so it does
not steer the write, but it steers where `authOAuthPath` then *looks*, and an
inherited one differing from the daemon's is exactly the "verified a different
file than was written" trap this command exists to close. Everything else passes
through, because the child needs a PATH and a terminal type to run at all.

And since `authOAuthPath` resolves through the **daemon's** `CRED_PATH`, a
disagreement between where the daemon reads and where this login writes is now
reported **before** the handover rather than discovered after the token is on
disk.

**Already fixed:** "preserving an arbitrary existing `state/.claude` entry allows
the OAuth client to follow a project directory or symlink" — that is M139,
closed earlier in this audit by `authClaudeDirCheck`, which runs immediately
above this code.

Test: `TestLoginChildEnvironmentIsDecidedByKoto` in `daemon/audit_fixes_test.go`
asserts the three variables are gone, HOME points at the state dir, unrelated
entries survive, and no key is duplicated.

## M164 — Untrusted job output is parsed as authenticated daemon framing — FIXED

`daemon/grpc_server.go`, `JobTail`.

**Confirmed.** Parsed mode runs each line of `/workspace/.cs/jobs/<id>/out`
through `logParser.feedLine` — the grammar written for a stream the **daemon**
authors. That file has no trusted writer: any `cs-job run` command writes to it,
and so does model-controlled sidecar output. The parser's nesting rule is a
marker-injection defense for the *transcript* and says nothing about provenance
here; sanitizing afterwards does not restore any.

So a job could emit `>>> ` and have it render as a prompt the operator typed,
`[[turn_end]]` to assert a turn boundary, `[[notify]]` to raise a notification
event on a path the live tailer's `notifyExpected` allowlist does not cover, or
`[[session]]` to stamp every later event of that tail with a conversation of its
choosing.

**Parsed mode is not the problem and stays**: a `cs-subagent` job writes its
output through `stream_filter.js`, so its `out` carries exactly this framing, and
the peek pane renders thinking and tool blocks from it. That is what the mode
exists for.

**Fix:** the display vocabulary passes; the assertions about the daemon's own
state do not. `jobTailEventAllowed` drops `prompt`, `turn_end`, `notification`
and `bg`, and the handler clears `Session` on every event it forwards —
`[[session]]` is parser *state* rather than an event, so an allowlist over event
names cannot reach it.

Test: `TestJobTailParsedVocabularyIsRestricted` in `daemon/audit_fixes_test.go`
checks both halves of the vocabulary, then drives the real grammar over a forged
job output carrying all four markers plus a legitimate `[[tool]]`, asserting
none of the forbidden events (and no forged attribution) survives while the real
tool frame does.

## M162 — Guest-controlled payloads, unbudgeted parsing and replay retention — FIXED (the parser half was already closed)

`daemon/fcturn.go`, `daemon/events.go`.

Three claims; one was already closed and two were real.

**Already closed:** *"while thinking or tool-output blocks are open, the parser
retains every line and later materializes the complete body"*. `logParser` has
carried `blockBodyMax` (1 MiB, with `appendBounded` marking the cut) since an
earlier finding in this audit; a block's body cannot exceed it.

**Real, fixed:** *"Tool.Name and TurnFrame.Err are incorporated into marker lines
without the truncation used for Tool.Input."* Both are guest-authored and both
went into the marker line whole — and `strings.Fields` makes the name one token,
so a whitespace-free megabyte *was* one token. `Name` is now capped at
`fcMarkerMaxName` (256 — tool names are `Bash` and `WebFetch`; the bound exists
so the field cannot be a payload) and `Err` at the same `fcMarkerMaxBody` as the
tool input beside it.

**Real, fixed:** *"parsed events are retained in each group's replay ring, which
evicts only after 1024 entries."* A count is not a bound on memory: with each
body capped at 1 MiB, 1024 entries is up to a **gigabyte per group**, and the
guest chooses the sizes. At the sink's 1 MiB/s that is reachable in about
seventeen minutes of sustained maximum-size output, retained until 1024 more
events push it out.

`eventRingBytesMax` (64 MiB) bounds the same ring by size — far above real
traffic, far below anything that threatens the daemon. Whichever bound bites
first wins, the eviction is the same one from the oldest end, and a client that
resumes past it gets the `gap` it already handles. The total is **carried**
rather than re-summed, because this runs on every streamed partial (twenty a
second per turn, ten turns per group), and it follows every removal — including
the mid-ring one where a partial is superseded, which would otherwise let the
total climb forever under streaming and silently over-trim the ring.

One deliberate exception: an event larger than the whole budget is still kept.
Dropping it would lose it silently rather than bound anything, so it evicts its
neighbours and stands alone.

Test: `TestEventRingIsBoundedByBytesAndCount` in `daemon/audit_fixes_test.go`
offers 200 MiB against the 64 MiB budget, asserts both bounds hold and that the
byte bound is what bit; checks the carried total against a fresh sum both after
plain appends and after partial supersession; and pins the oversized-event case.

## M163 — Workspace images have no fleet-wide host-disk admission control — FIXED

`daemon/fchostmem.go`, `daemon/fc.go`.

**Confirmed.** The memory half of `fchostmem.go` refuses a spawn that will not
fit beside the running VMs. The disk half did not exist:
`fcEnsureWorkspaceImg` created a sparse image and `Truncate`d it to its apparent
size, guest writes materialised real blocks afterwards, and
`fcGrowWorkspaceImg` extended an existing image without looking at the
filesystem at all. With up to 100 registered groups, images that survive a stop,
and a resource collector that only *watches* the number go up, a caller able to
spawn groups could fill the host's filesystem — at which point every guest
remounts read-only, the daemon cannot write its own state, VM startup and
shutdown fail, and unrelated host services fail with them.

**Fix:** `fcHostDiskAdmit`, called before creating an image and before growing
one — the two places the daemon is about to add to the problem. It refuses when
the filesystem holding the group tree is at or past `resCritPct` (90%), the same
threshold the resource alerts already fire at, so the operator has been told
twice before anything is refused. The message names the group, the operation,
the percentage, the free space and what to do.

**It is a FLOOR, not an accounting model, and the difference is the point.**
Images are sparse and grow by guest writes, so no reservation arithmetic can
predict consumption the way memory admission can. What a floor *can* do is stop
adding new consumers to a filesystem that is already nearly full, which is the
same friendly-failure principle `fcHostMemAdmit` states: a refusal that says why
beats ENOSPC choosing for us.

A statfs that fails means "unmeasurable", not "refuse" — a missing directory
must not become a fleet-wide spawn block.

**Operator note:** this host currently reads **80% full** on `/var`. Nothing
changes at 80%; at 90% new groups and disk grows start being refused, with the
message above, while everything already running keeps running.

Test: `TestWorkspaceImagesHaveHostDiskAdmission` in `daemon/audit_fixes_test.go`
covers admission on a real filesystem with room, the unmeasurable case, the
threshold arithmetic at five levels, and the refusal text.

## M161 — Detached worker descendants can hold stderr open and stall turn cleanup — FIXED

`fcguest/turn.go`, `runWorker`.

**Confirmed, and it was worse than the finding says — stdout too.** A turn's
completion was **EOF on the worker's pipes**. Those pipes are parent-owned and
every descendant inherits the write ends; `startTracked` puts the worker in its
own process group and the timeout and stall paths kill that group, but a
descendant that calls `setsid` leaves it — while still holding the pipes.

The scanner therefore never reached EOF. `runWorker` sat there and never emitted
its deferred `TurnEnd`, so the host waited out the full `turnWaitTimeout` (25
minutes), declared the group STALLED and self-healed it: one slot quarantined
per occurrence, three inside the breaker window and the group needs a manual
`/restart`. All from a tool call that backgrounded something, which agents do
routinely — `cs-job` itself uses `setsid` by design.

**Fix:** the turn's completion is now the **worker's own exit** (`<-ch`, the
reaper's status channel), not EOF. Both pumps run on their own goroutine; once
the worker is gone, they get `workerDrainGrace` (2 s) to finish the bytes
already in flight, and then the **read** ends are closed — which is what unblocks
a scanner parked on a descendant that will never write again, since `os.Pipe`
files are registered with the runtime poller and a concurrent `Close` makes the
pending `Read` return. A forced close is logged, naming which stream and why.

The grace window exists so the fix costs no real output: a worker's last lines
are already written when it exits, and draining them takes microseconds.

Test: `TestRunWorkerDoesNotWaitOnADetachedDescendant` in `fcguest/turn_test.go`
runs a worker that prints a line, backgrounds a 30-second child inheriting both
pipes, and exits — asserting `runWorker` returns (against a 20-second bound; it
did not before) **and** that the worker's own output still arrived.

**Carry-over:** guest-side, so it reaches a group after `make rootfs` and a
`/restart` — with M138 and M150.

## M160 — Job inspection performs unbounded guest-controlled reads — FIXED

`daemon/jobs.go`, `fcguest/main.go`.

**Confirmed on both sides.**

**Guest-side buffering.** `handleExec` accumulated the child's whole combined
output in a `bytes.Buffer` and only then framed it, so the channel's 16 MiB
maximum was enforced on the **host** — after the guest had read, allocated and
held every byte, and after a frame too large to send made the call fail outright
rather than return what it had. `execOutMax` (4 MiB) now bounds it in the guest,
through a `boundedBuf` that reports the **full** write length so a capped
buffer stops *storing* bytes rather than handing the child an I/O error on
stdout, and marks the output truncated so the reply says so. Every caller here
is one of the daemon's own control execs — a jobs listing, `/proc/meminfo`, a
`df`, an `rm` — all of which answer in kilobytes.

**Host-side scripts.** Each job's `status`, `rc`, `session` and `started` is a
file the **worker** writes (cs-job mints the directory; uid 1000 owns it), and
all four were read with a bare `cat`. Whatever was in them went into a TSV line
that comes back through the exec buffer, is parsed into per-group state, folded
into the state hash and republished in every state frame — and a metadata file
containing tabs or newlines could invent fields and split one job across several
lines. Only `cmd` was capped, at 200 bytes, which is the shape the rest now
follow: `head -c 64 | tr -d '\t\n'` with the existing defaults preserved.

`wc -c < "$d/out"` also **reads** the file to count it; `stat -c %s` asks the
inode.

Measured on a scratch tree before and after: a job with 100 KB of `status`,
50 KB of `session` (with an embedded newline and a forged `>>> ` line) and 100 KB
of `cmd` produced a **150,023-byte line carrying 5 fields** under the old script
and a **470-byte line carrying exactly 7** under the new one, with a
metadata-less job still defaulting to `unknown`/`0`.

Tests: `TestExecOutputBufferIsBounded` in `fcguest/turn_test.go` (the cap, the
full-length write contract, that post-cap writes grow nothing, that the head is
what is kept, and that `execOutMax` stays inside the channel maximum) and
`TestJobsTSVParseSurvivesHostileMetadata` in `daemon/audit_fixes_test.go` (the
bounded line parses, carries no framing, and no forged line becomes a job id).

## M166 — JobTail parsed output can exhaust TUI memory — FIXED

`tui/peek_render.go`, `tui/model.go`.

**Confirmed, and the comment that justified the old bound named its own
mistake.** `peekLineCap` bounded the parsed scrollback at 2048 **blocks**, on the
stated grounds that "the bodies inside tool_out/thought blocks are already
capped by the daemon's 64KB tail window". That holds for the *initial replay*
only: `JobTail` then **follows** the file, and every block after that is bounded
by the daemon's own `blockBodyMax`, which is 1 MiB. 2048 blocks at 1 MiB each is
**two gigabytes** in the operator's TUI, sized by whoever wrote the job.
`peekOpen` was capped the same way — 2048 *elements*, each a streamed partial of
arbitrary size — and `snapshotPeek` then stored the slices per job behind a cache
that counted jobs, not bytes: sixteen entries × the block cap.

**Fix:** byte budgets alongside the counts, carried rather than re-summed.

- `peekBytesMax` (4 MiB) on the parsed scrollback, trimmed from the oldest end.
  The newest block is always kept even when it alone exceeds the budget —
  dropping it would show the operator nothing at all.
- `peekOpenBytesMax` (1 MiB) on an open block's partials, which are redundant by
  construction: the authoritative body arrives in the `*_done` frame, and this
  buffer exists only to show progress while the block streams.
- `peekCacheBytesMax` (16 MiB) across the whole cache, evicting
  least-recently-saved first and never the entry just written.

Every reset path zeroes the counters, and a restore from the cache recounts them
(`recountPeekBytes`) since those slices did not come through `applyPeekEvent`.

Test: `TestPeekStateIsBoundedByBytes` in `tui/peek_test.go` offers 50 MiB of
blocks against the 4 MiB budget, asserts both bounds hold and that the byte one
is what bit, checks the carried total against a fresh count, exercises the open
block and its reset on close, and fills the cache past its byte budget.
Verified to fail on all three counts with the budgets removed.

---

# low

171 findings. Same method as the mediums: every one is checked against the
code, gets a verdict, and a verdict of *accepted* or *not a finding* carries
its reasoning.

**Numbering.** `L1`–`L12` were already used by the 2026-09-05/06 audits and are
referenced from code comments and CLAUDE.md, so this audit's low findings are
written `audit 2026-09-11 L<n>` in code and plain `L<n>` here, where the file's
own date disambiguates them.

Related findings are fixed and committed together where they share a site or a
cause — a commit per finding would put 171 near-identical entries in a log whose
job is to explain the design.

## L1 — Oversized control line permanently stops guest-to-host forwarding — FIXED

`fcguest/main.go`, `ctlForward`.

**Confirmed.** The forwarder read `/workspace/.cs/ctl` with a `bufio.Scanner`
capped at 1 MiB. A record over that makes `Scan()` return false with
`ErrTooLong`, and the loop exited without checking `sc.Err()` — so the function
returned and the FIFO's **only reader was gone**. Every later `notify`,
`job_done`, `report` and `goal_*` from that guest went nowhere until the VM was
restarted, and writers blocked on a pipe nobody was draining. The worker owns
the FIFO, so writing one long line was the whole attack.

**Fix:** `readCtlLine` reads with `ReadSlice` and handles the two cases the
scanner conflated — an oversized record is **discarded through its terminator**
(retaining none of it, which is the memory half of the same bug) and answered on
`ctl.out`, and the stream then resynchronises on the next line. A read error is
ridden out rather than fatal, since the FIFO is held `O_RDWR` by this process and
has no EOF to reach; `ctlReadErrMax` stops that becoming a spin on a broken
descriptor.

Test: `TestReadCtlLineResynchronisesAfterAnOversizedRecord` in
`fcguest/ctl_test.go` — a record before, an oversized one, and a record after,
plus a long-but-legal record arriving in buffer-sized pieces and an unterminated
final record.

## L3 — Special filesystem objects can pin a worker through synchronous file operations — FIXED

`sidecar/venice_stream.js`, the `file` tool.

**Confirmed and measured.** The only check was `statSync().isDirectory()`, so a
FIFO or a character device reached `readFileSync`/`writeFileSync` — synchronous
calls with no timeout that block the **Node event loop**, which is why the bash
tool's 30-second budget cannot help: a blocked loop cannot run the timer that
would enforce it. `/workspace/.cs/ctl` is a FIFO the worker can name. Measured
against the old code: `read` on a FIFO and on `/dev/zero` both hang indefinitely
(killed at 3 s). Recovery needed the guest watchdog (up to 20 min) or the
daemon's turn wait (~25 min), per group and repeatable.

**Fix:** `openRegular` judges the **descriptor**, not a prior stat of the path —
`O_NONBLOCK` is what makes the refusal possible rather than academic, since it
lets the open of a FIFO or device return so `fstat` can classify it. `O_NOFOLLOW`
is deliberately *not* set: the agent's own workspace is its own and a symlinked
file is an ordinary thing to edit; what this refuses is a file that is not a
file. `readCapped` also reads at most the cap + 1 byte, so the read cap bounds
the **allocation** and not just the answer — `readFileSync` used to read a whole
file and slice afterwards.

Smoke-tested directly: FIFO, `/dev/zero`, `/dev/null` and a directory refused by
name for `read`, `write` and `edit`; a regular file and a **symlink to one** read,
written and edited normally; every call returned immediately.

## L2 — Optimistic pending prompts allow terminal escape-sequence injection — FIXED

`tui/model.go`, `dispatchInput`.

**Confirmed.** The optimistic ⏳ row stores the submitted text verbatim and
`renderPendingLines` hands each segment to `lipgloss.Style.Render`, which styles
text without neutralising what is in it. `themeFrame` and `monoFrame` do not
remove general terminal controls either — `monoFrame` deliberately preserves OSC
and cursor control. Every other piece of chat on that screen reaches it through
the daemon's sanitizer; this row was the one that did not, so a pasted escape
sequence rendered raw.

**Fix:** the **local** copy is scrubbed (`scrubVTStrict`); what goes on the wire
is not, because the model should see what the operator typed. It also makes the
two agree: `popPending` matches this text against the daemon's `prompt` event,
which *is* sanitized, so a control-bearing prompt used to leave its row stranded
until `reconcilePending` swept it.

Test: `TestPendingPromptIsScrubbed` in `tui/pending_reconcile_test.go` — the
stored row, the rendered frame, and the pop-by-echo.

## L4 — Unprivileged installer replaces unreadable root-only environment file — FIXED

`daemon/install.go`.

**Confirmed, and it fires on every upgrade rather than in an edge case.**
`readEnvFile` collapsed *every* error to nil, so a `koto.env` that exists but
cannot be read looked exactly like one that is absent. The file is root-owned
0600 and `koto install` deliberately refuses to run as root — verified on this
host: `-rw------- root root /etc/koto/koto.env`, `cat` → Permission denied. So
on an upgrade every operator-set value (`ANTHROPIC_API_KEY`, a hand-set
`KOTO_CLAUDE_BIN`, `KOTO_HOST_MEM_MIB`, `KOTO_HOST_CPUS`) was re-rendered as a
commented-out blank and written over by sudo — while `sudoWriteIfChanged`'s own
"unchanged" comparison was blind for exactly the same reason, so it always
rewrote. That contradicts the line the file prints about itself: *"Re-running
`koto install` preserves the values set here."*

**Fix:** `sudoReadFile` distinguishes **absent** from **unreadable** and falls
back to `sudo cat` for the latter — the same privilege every write here already
uses. `renderEnvFile` refuses to rewrite when the existing file cannot be read,
rather than treating it as empty, and `sudoWriteIfChanged` compares through the
same helper so an unchanged file is actually detected as unchanged.

Test: `TestEnvFileReadDistinguishesAbsentFromUnreadable` in
`daemon/audit_fixes_test.go` covers absent, readable-and-parsed (including that
a commented-out key is not "set"), and present-but-unreadable.

## L5 — Group-wide clear reports success when transcript truncation fails — FIXED

`daemon/groups.go`, `clearCmd` → `clearGroupLogs`.

**Confirmed.** Each stream was stat'd with the error discarded, truncated with
`_ = os.WriteFile(...)`, and the function returned `OK: true` regardless. A
transcript that could not be truncated — read-only filesystem, permissions, a
full host disk, or a non-regular file left in its place — reported a successful
clear to the operator and to every client, with the conversation still on disk
and still replayed by the next `History`. The per-session path already returned
`filterLogSession`'s error; this is the same promise on the wider verb.

**Fix:** `clearGroupLogs` returns the first failure, naming the stream; a
missing stream is still fine (never written), and a non-regular file in a
stream's place is refused explicitly.

Test: `TestGroupClearReportsATruncationFailure` in `daemon/audit_fixes_test.go`.

## L6 — Out-of-range proxy ports collapse VM jail identities onto UID 30000 — FIXED

`daemon/fcjail.go`, `fcJailUID`.

**Confirmed.** The uid is `fcJailBaseUID + (port - PORT_BASE)`, and anything
outside the 30000–60000 band was **clamped to the base** — which is the one
outcome the function exists to prevent. Every group whose port fell outside
(after a `PROXY_PORT` change against existing `groups.json` state, a hand-edited
port, or eventual exhaustion) shared a single uid, and that uid owns the
workspace image, the vsock socket directory and the jailed VMM process itself.
The per-VM chroot and mount namespaces make it defence-in-depth rather than
immediate cross-VM access today — but "the isolation identity collapsed
silently" is not a state to boot into.

**Fix:** an out-of-range port is an error and the spawn is refused, with a
message naming the port, the computed uid, the band and `PORT_BASE`. The
unjailed path (`KOTO_FC_NOJAIL=1`) is unaffected, since it has no per-VM
identity to collapse.

Test: extended `TestJailUIDsAreDistinctAndInRange` in `daemon/userns_test.go` —
five out-of-range ports refused, and the refusal names `PORT_BASE`.
