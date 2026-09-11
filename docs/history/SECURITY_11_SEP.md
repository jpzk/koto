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

**Residual, stated rather than fixed:** the claude path has the same exposure by
a different route — `runClaude` passes the composed system prompt as
`--append-system-prompt <text>` on ARGV, which any process of the same uid
reads from `/proc/<pid>/cmdline` for the turn's duration. (The message body
goes over stdin and is not exposed.) Closing it needs the CLI to accept the
prompt from a file or stdin; that is an upstream capability question, not a
change koto can make on its own, so it is recorded here rather than guessed at.

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
