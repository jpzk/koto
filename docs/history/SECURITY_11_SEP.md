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
