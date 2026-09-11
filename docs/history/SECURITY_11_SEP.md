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
