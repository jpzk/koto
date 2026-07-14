# Firecracker microVM groups — vsock IPC design

Status: **implemented and verified end-to-end** (2026-07-08): real Venice
turns with in-guest tool execution through the vsock proxy path, correct
per-group metrics attribution, no-NIC isolation confirmed from inside the
guest (lo only, curl fails, no DNS), graceful stop via guest reset, and
conversation history persisting across VM + daemon restarts. Opt-in per
group via config.json `"runtime": "firecracker"`; default remains podman,
both runtimes coexist.
Code: `fc.go` (daemon side), `fcguest/` (guest agent + rootfs build),
`fc_test.go` (wire-logic smoke tests against a fake FC endpoint).

## Why vsock at all

Under podman, all host↔group IPC is host files under a bind mount:
`.cs/in` (FIFO), `.cs/ctl`/`.cs/ctl.out`, `.cs/log`, plus
`ANTHROPIC_BASE_URL` pointing at the proxy over the bridge network.
A Firecracker microVM has **no shared filesystem** (no virtio-fs) and — by
design here — **no network device**. The only host↔guest channel is one
virtio-vsock device, backed on the host by a unix socket
(`run/fc/<g>.vsock`, Firecracker "hybrid vsock").

## Trust-model payoff

A vsock-only VM has *no NIC*: `curl` from the bash tool has nowhere to send,
peers and the LAN are unreachable, DNS doesn't exist in the guest. The
credential-injecting proxy stops being a *default* and becomes the **only**
egress, enforced by the absence of a route rather than by convention (this
closes the "sidecar egress is NOT restricted" hole in CLAUDE.md). And the
kernel boundary becomes hardware virtualization: a container escape becomes
a VM escape. This is the Nitro-Enclave shape: the host mediates every byte.

## Decision: no shared mutable filesystem (→ raw Firecracker, not Kata)

We audited every daemon/proxy read+write under `groups/<g>/` and confirmed
nothing requires a shared mutable filesystem, given main's raw `/peers` RW
mount is dropped (decided: yes). `main` keeps spawn/send/stop/sched via the
ctl plane, skill authoring via the SkillNew RPC, peer skill toggling via the
Config RPC; it loses direct reads/writes of peer workspace *files*.
Consequence: no Kata/virtio-fs needed. Skills are read-only+shared →
delivered as a tarball at VM init. Config/prompt/log are host-authoritative.

## Port map (single vsock device, demuxed by port — as built)

| Dir          | port  | purpose                                | replaces                  |
|--------------|------:|----------------------------------------|---------------------------|
| guest → host | 9000  | API egress (TCP-in-vsock → proxy port) | `ANTHROPIC_BASE_URL` bridge |
| guest → host | 9001  | log stream → **appended to host log**  | `.cs/log` bind mount      |
| guest → host | 9002  | ctl plane (JSON lines, replies inline) | `.cs/ctl` + `.cs/ctl.out` |
| guest → host | 9003  | L3 ethernet frames → gVisor gateway (**`network` ≠ `none`**) | a real NIC |
| host → guest | 10000 | agent RPC (init/msg/exec/exec_stream/shutdown) | `.cs/in` FIFO + `podman exec` |

Attribution comes from *which* `<g>.vsock_<port>` socket a connection lands
on, exactly like the per-group proxy TCP port does today.

Key insight that kept the diff small: **the host log file stays the single
source of truth.** The 9001 handler (`fcLogSink`) just appends guest bytes to
`groups/<g>/.cs/log` — so `tailLog`, History, `/clear` truncation, proxy
`logAppend`, and sendNow's `>>>` markers are all runtime-oblivious. No parser
refactor. Similarly the 9000 handler splices into the group's *existing*
proxy listener, so credential injection and metrics attribution are unchanged.

## Guest side (`fcguest/main.go`, PID 1)

The agent keeps `sidecar/entrypoint.sh` **byte-identical** across runtimes by
recreating its environment inside the VM:

- early boot: /proc /sys /dev tmpfs devpts mounts, hostname, **loopback up**
  (needed for the in-guest TCP bridge), mount `/dev/vdb` at `/workspace`
  (mkfs.ext4 fallback), chown to uid 1000.
- `.cs/in`, `.cs/log`, `.cs/ctl` are **FIFOs in the guest** (consume-once
  transport; durable copies live host-side). entrypoint's `>>` appends work
  unchanged; the agent holds the FIFOs open O_RDWR so nothing blocks or EOFs.
- bridges: TCP `127.0.0.1:18888` → vsock 9000 (`ANTHROPIC_BASE_URL` points
  here); log FIFO → vsock 9001 (reconnect with carry buffer); ctl FIFO line →
  vsock 9002 → response line → `ctl.out`.
- agent RPC (vsock 10000): `init` (skills tar + published ports + env; starts
  entrypoint.sh as uid 1000 afterwards, restart-with-backoff), `msg` (writes
  system-prompt.md + config.json into the guest workspace, then the b64 line
  into the in FIFO), `exec` (sh -c, 60s cap, rc+output — the `podman exec`
  analogue used by interrupt + /clear), `exec_stream` (raw streamed output,
  peer-close kills the child — used by background-job tailing), `shutdown`
  (sync + umount + poweroff — dirty-ext4 protection for workspace.img).
- PID-1 zombie reaping via a central wait4(-1) loop with a tracked-pid table
  (the catatonit role), children in their own process groups.

## Daemon side (`fc.go` + call sites in `groups.go`/`send.go`)

- `groupRuntime(g)`: config.json `"runtime"`; anything but "firecracker" →
  podman. Branch points: `ensure()` (spawn), `sendNow()` (delivery),
  `stopGroup`, `listGroups` (via `groupRunning`), `interruptAgent`,
  `tailBackgroundTask`, `clearCmd`, `destroy` (fc droppings sweep).
- `fcSpawn`: preflight (/dev/kvm + assets), workspace.img create (sparse 8G +
  host mkfs.ext4), UDS listeners **before** boot, static `--no-api`
  `--config-file` FC launch (console → `run/fc/<g>.console.log`), init-op
  retry loop (30s) as the readiness barrier, then per-port TCP bridges.
- `fcStop`: agent shutdown op (graceful), 5s wait, SIGKILL fallback; pidfile
  + comm check guards pid reuse.
- vcpus/mem/disk per group: config.json `size` preset (default `small` =
  2 vCPU / 1024 MiB / 8 GiB). See "VM size profile" below. Raw `vcpus` /
  `mem_mib` keys still override the preset (legacy escape hatch).
- turn lifecycle is **shared**: `[[turn_end]]` arrives via vsock → host log →
  tailLog → `notifyTurnDone`, so sendNow's wait/stall/selfHeal logic is the
  same code path for both runtimes (restart() = stopGroup + ensure is already
  runtime-agnostic).

## Where per-VM state lives (host layout)

```
groups/<g>/
  workspace.img       guest /workspace (ext4, virtio-block rw) — the ONLY
                      guest-writable persistent state. Single-writer: never
                      mount it host-side while the VM runs.
  .cs/log             daemon-owned mirror of the vsock 9001 stream
  .cs/config.json     host-authoritative (proxy + daemon read/write; guest
                      gets a copy pushed per turn)
  prompt.md           host-authoritative (composeSystemPrompt reads it)
run/fc/
  <g>.sock/v[_9000/1/2/3]  per-group dir: hybrid-vsock UDS + per-port listener
                      sockets. Own dir so the jailer can bind-mount exactly
                      this VM's sockets into its chroot (as /vsock).
  <g>.jail/            per-VM chroot root the jailer stages (bind targets for
                      firecracker/kernel/rootfs/workspace/dev/vsock + fc.json)
  <g>.cfg.json         unjailed only (CLAWSON_FC_NOJAIL=1); jailed config is
                      written into <g>.jail/fc.json instead
  <g>.pid / <g>.console.log
fcassets/             (gitignored) firecracker binary, vmlinux, rootfs.img
```

RAM/vCPU state is ephemeral (no snapshots in v1). Rootfs is read-only and
shared by all VMs. Skills are pushed as a tarball at init, not mounted.

## Build & run

```sh
make fc-assets    # fetch firecracker (pinned v1.11.0) + CI kernel (6.1)
                  #   + build golden rootfs (fedora + node + claude-code +
                  #     sidecar/ + fc-agent; no chrome, no podman)
make host-build   # once: host image now includes e2fsprogs + tar
make host-run     # run-host.sh passes --device /dev/kvm when present
# then per group:  /config runtime=firecracker  +  /restart <g>
```

Rootfs rebuild (`make fc-rootfs`) is required after editing
`sidecar/*.{sh,js}` or `fcguest/` — microVMs have no live bind mounts (the
one ergonomic regression vs podman's hot-reload mounts).

## Bugs found & fixed during bring-up (the log for posterity)

- `/workspace` + `/skills` must be baked into the (read-only) rootfs as
  mount points; `/skills` needs a tmpfs for the init tarball.
- PID 1 starts with an empty environment — set PATH before any exec.
- FC has no ACPI: guest poweroff is a no-op; graceful exit is
  `reboot(RESTART)` + `reboot=k` (i8042 reset, FC catches it and exits).
- FIFO buffers die with their last fd: the agent must hold `.cs/in` open
  O_RDWR for its lifetime, or a msg racing entrypoint's `exec 3<>` (the
  spawn-triggered-by-send timing) is silently dropped.

## Known limitations / follow-ups

1. **Published ports** bind inside cs_host (reachable on clawson-net as
   `cs_host_go:<port>`), not on the real host loopback — host publishing
   needs a `-p` on cs_host itself (podman can't add one live).
2. ~~No open-internet escape hatch yet.~~ **DONE** — the `network` profile
   (`none` default / `wan` / `lan` / `full`). The networked profiles attach a
   gVisor L3 gateway over vsock 9003 (real NIC: arbitrary TCP/UDP + DNS,
   egress-filtered at the frame layer by destination class); `none` stays
   NIC-less. See "Network egress profile" below.
   *L3-native inbound is still a follow-up (see that section).*
3. **`pip` (podman-in-podman) and Chrome groups** stay on the podman runtime
   (not in the minimal rootfs).
4. **main on firecracker**: works protocol-wise (ctl over vsock), but skill
   *authoring* via the rw /skills mount doesn't exist there — main keeps
   using the SkillNew RPC path, or stays on podman.
5. **skills refresh** for a running FC group needs /restart (tar is pushed at
   init) — same rule as the ports feature.
6. **Migrating an existing podman group** doesn't move its workspace files
   into workspace.img; fresh workspace (or copy offline while stopped).

## Network egress profile (`network`: `none` | `wan` | `lan` | `full`)

Per-group config key controlling general outbound. Default `none` — a
microVM group has **no NIC and no route**; its only egress is the LLM upstream
via the proxy (vsock 9000). The three networked profiles attach a userspace
**gVisor L3 gateway** (`containers/gvisor-tap-vsock`) over a new vsock port,
giving the guest a real interface — outbound TCP and UDP on any port with NAT,
plus DNS — and differ only in *which destinations the egress filter passes*:

| profile | public internet (WAN) | host LAN | tailnet (CGNAT 100.64/10) |
|---------|:---------------------:|:--------:|:-------------------------:|
| `none`  | — no NIC at all —                                        |||
| `wan`   | ✅ | ❌ | ✅ |
| `lan`   | ❌ | ✅ | ❌ |
| `full`  | ✅ | ✅ | ✅ |

**`wan` is the safe general-purpose profile** (and where legacy `internet=full`
now maps — see below): the agent can `curl`/`git`/`npm` the public internet but
cannot reach the host LAN, so a prompt-injected agent can't scan or pivot into
your other machines. `lan` is for the rare group that must talk to a LAN device
but should not have public egress; `full` is both (the old `internet=full`
behavior, now explicit). The **tailnet (CGNAT 100.64/10) is classed as WAN**,
not LAN — it's treated as intentionally-shared infrastructure, so a `wan` group
can reach tailnet peers.

(ICMP/ping is best-effort — it needs the gateway process to open a
raw/unprivileged ICMP socket on the host; TCP/UDP, i.e. all clawson tooling,
need no special privilege.) Verified live: TCP to arbitrary ports (`:22` SSH
banner, `:53`), UDP (NTP `:123`), and HTTPS all work; ICMP echo did not in the
standalone test. See `fcnet.go` (host) and `fcguest/net.go` (guest).

**Legacy `internet` key.** Pre-rename configs (and old TUI/Android clients)
carry `internet` = `none` | `full`. On read, `internet=full` maps to **`wan`**
— the secure reading of "full internet" (public egress, no LAN); `internet=none`
→ `none`. On write (`/config internet=…`), the daemon stores `network` and
deletes the old key, migrating configs forward. An explicit `network` key
always wins over a legacy `internet` key.

- **Real L3, not an HTTP proxy.** On any networked profile, `fcSpawn` builds a
  per-group `virtualnetwork` (gateway `192.168.127.1`, guest `192.168.127.2/24`)
  and opens a guest→host listener on vsock **9003**. fc-agent, told `net="l3"`
  at init, creates a TAP (`eth0`), self-assigns the address/route, points
  `/etc/resolv.conf` at the gateway, and pumps ethernet frames to 9003. Framing
  is the Qemu protocol (4-byte big-endian length prefix per frame), matching
  the host's `AcceptQemu`. The guest-side setup is identical for `wan`/`lan`/
  `full` — the profile only changes the host-side frame filter.
- **LLM traffic still rides the proxy.** `ANTHROPIC_BASE_URL` stays
  `http://127.0.0.1:18888` → vsock 9000 → the credential-injecting proxy, so
  key injection and per-group metrics are unchanged. `NO_PROXY=127.0.0.1`
  keeps that base URL direct. **`HTTP_PROXY` is NOT set** on a networked
  profile — general `curl`/`git`/`npm` go out over the NIC, NAT'd by the gateway.
  - **Tradeoff:** general HTTPS is no longer L7-audited by the proxy (only the
    LLM leg is). This is the cost of real L3 vs the old proxy-only egress.
- **Egress authority is at the frame layer.** gvisor-tap-vsock has no
  destination-filter hook (it `net.Dial`s the packet's destination directly),
  so `fcEgressConn` (fcnet.go) parses each guest frame, classifies its
  destination (`fcClassifyDst`), and applies the group's profile (`fcDstAllowed`).
  The classes:
  - **ctl** — loopback (`127/8`, `::1`), link-local (`169.254/16`, `fe80::/10`,
    incl. link-local multicast like mDNS `224.0.0.251`), and **cs_host's own
    interface IPs** (`fcSelfIPs`, where the daemon gRPC on
    `CLAWSON_BIND:CLAWSON_PORT` and every group's proxy port live). **Dropped
    under every profile.** `Ec2MetadataAccess=false` also blocks metadata inside
    the netstack.
  - **gw** — the guest↔gateway subnet `192.168.127.0/24` (DNS at `.1`). **Always
    allowed** at the frame layer; carved out explicitly because it sits inside
    the `192.168/16` LAN range, so `wan` DNS would otherwise break. (At the L7
    proxy this carve-out does not apply — it's frame-layer-only.)
  - **lan** — RFC1918 (`10/8`, `172.16/12`, `192.168/16`), IPv6 ULA `fc00::/7`,
    non-link-local multicast, limited broadcast. Allowed for `lan`/`full`.
  - **wan** — everything else, incl. CGNAT `100.64/10` (tailnet). Allowed for
    `wan`/`full`.
  - **802.1Q/802.1ad VLAN-tagged frames are dropped** — a tag would shift the
    IP header past the parser's fixed offsets and hide the destination. ARP
    stays allowed (the gateway link needs it).
  This mirrors, at L3, what `egressTargetAllowed` (proxy.go) does for the L7
  proxy path — both share `fcClassifyDst`.
- **DNS caveat.** The gateway resolves guest DNS queries on the **host** (via
  the host resolver, from the daemon process) — outside the frame filter. So a
  `lan` guest can still *resolve* public names (and a determined agent could
  exfiltrate bits via query names), and a `wan` guest's lookups of LAN names hit
  the host resolver. **Actual traffic can't bypass the filter** — data frames
  carry the resolved destination IP, which is classified at connect time. Per-
  profile DNS zones are a possible follow-up.
- **`lan` can't reach the clawson host itself.** cs_host's own LAN IP is in
  `fcSelfIPs` (ctl, unconditional), so `lan` reaches *other* LAN devices but not
  services on the host running the daemon. Correct per the control-plane rule —
  don't debug it as a bug.
- **`none` keeps the invariant.** A `none` group never opens the 9003 listener
  and never gets `net="l3"`, so no TAP and no route exist — "no NIC = no
  egress" is enforced by the absence of a route, unchanged.
- **Applies on `/restart`** (the gateway is attached at spawn and the guest
  env/`net` flag are set then). Lowering to `none` and restarting tears the
  gateway down.
- **Inbound** still rides the published-port vsock path (`portBridge`),
  delivered to the guest's loopback services — independent of L3. L3-native
  inbound (the guest accepting on `192.168.127.2` via gateway forwards) is a
  follow-up.
- **Kernel:** `full` needs `CONFIG_TUN`, which FC's CI vmlinux lacks — so the
  guest kernel is built (`build-kernel.sh`, `make fc-kernel`), not fetched.
  - **Source = the Amazon Linux tree, like FC itself.** `build-kernel.sh` does
    what FC's `resources/rebuild.sh` does: `git clone github.com/amazonlinux/linux`
    at a pinned `microvm-kernel-*.amzn2023` tag, apply FC's guest config, and add
    `CONFIG_TUN` + `FUSE_FS`/`NF_TABLES` (podman) + `IKCONFIG`. **NOT** kernel.org
    vanilla: vanilla can't load FC's ACPI tables (`AE_BAD_PARAMETER` during
    region init → panic), and the `acpi=off` workaround that gets it booting
    leaves the guest with **no local APIC / no LAPIC timer**, so every idle
    microVM busy-polls a full host CPU (~100%). The amzn tree boots **with
    ACPI**, so the LAPIC timer works and idle is ~0% — and `fc.go` boots with no
    `acpi=off`. Verified: fresh idle guest ~3% CPU, `LOC` incrementing. See
    `kernel-amzn-vs-vanilla.md`.
  - **DNS/resolv.conf.** The rootfs is read-only, so `/etc/resolv.conf` is a
    symlink to `/run/resolv.conf` (a tmpfs), set up in `build-rootfs.sh`'s
    staging tree (a Dockerfile `RUN` can't, since podman bind-mounts
    resolv.conf during build). fc-agent's `netUp` writes the target on `full`;
    `none` leaves it dangling — no DNS, as intended.
- **podman groups**: the profile is a firecracker feature. A podman group has
  a real NIC and full internet regardless; `network=none` is not enforced
  there (would need `--internal` networking).

## VM size profile (`size`: `small` | `medium` | `large`)

Per-group config key selecting the machine shape — vCPU, RAM, and workspace
disk together, as one named preset (defined in `fcSizePresets`, `fc.go`):

| preset | vCPU | RAM      | workspace.img |
|--------|------|----------|---------------|
| small  | 2    | 1024 MiB | 8 GiB (default; absent `size` ⇒ small) |
| medium | 2    | 2048 MiB | 12 GiB        |
| large  | 4    | 4096 MiB | 16 GiB        |

- Set at spawn (`/new <g> [provider] [model] size=large`) or on an existing
  group (`/config size=large`), then **applies on `/restart`** — `fcResolveSize`
  is read by `fcMachineCfg` (machine-config) and `fcWorkspaceDiskBytes` (disk)
  at the next `fcSpawn`.
- **Disk grows, never shrinks.** `fcEnsureWorkspaceImg` grows an existing
  `workspace.img` offline on the host (the VM is stopped during `ensure()`):
  `truncate` the sparse backing file → `e2fsck -fy` → `resize2fs`. Shrinking is
  never attempted (protects workspace data), so `large`→`small` drops RAM/vCPU
  on the next boot but leaves the disk at its grown size. `resize2fs` comes from
  Alpine's `e2fsprogs-extra` (`host/Dockerfile`).
- **Legacy override.** Raw `vcpus` / `mem_mib` config keys still layer on top of
  the resolved preset (clamped 1–32 and 128–65536 MiB), so any hand-tuned group
  keeps working. There is no raw disk-size override — disk follows the preset.

## Root / sudo profile (`root`: `yes` | `no`)

Per-group config key granting the guest's `node` user (uid 1000, the account
the entrypoint + `claude` + all bash run as) **passwordless sudo**. Default
`no` — `node` has no path to root, matching the podman-sidecar posture.

- **Why it's safe to grant.** The microVM's KVM boundary is the security
  boundary (see *Trust-model payoff*). Root *inside* the guest is still contained
  by Firecracker's minimal device model — a root-in-guest is a VM escape away
  from the host, exactly like non-root-in-guest. So unlike host-side sudo, this
  does **not** widen the host blast radius. It's a per-group ergonomics knob, not
  a trust decision.
- **Plumbing.** `groupRoot(g)` (`fc.go`) reads config.json `"root"` at spawn and
  sets `init.root=true` on the guest init RPC. `fc-agent`'s `handleInit`
  (`fcguest/main.go`) then calls `enableSudo` once, before the entrypoint starts,
  so the first turn already has it. Applied on **`/restart`**.
- **How the grant is installed (read-only root workaround).** The root drive is
  attached read-only, so `/etc/sudoers.d` can't be written directly. `enableSudo`
  overlays a small **tmpfs** on `/etc/sudoers.d` and drops
  `node ALL=(ALL) NOPASSWD: ALL` (mode 0440, root-owned) there; sudo's baked
  `/etc/sudoers` already `@includedir`s that dir, and sudo's timestamp dir lives
  under `/run` (also a tmpfs). `sudo` itself ships in the golden rootfs
  unconditionally (`Dockerfile.rootfs`); only the grant is runtime-gated.
- **`sudo dnf install` won't persist.** The root drive is read-only, so sudo is
  for running privileged commands against the writable workspace/tmpfs, network
  and mount config, reading root-owned files — not installing packages. Bake new
  packages into the rootfs (`make fc-rootfs`) instead.

## Jailer (host-side isolation of the Firecracker process)

The KVM boundary protects the host *from the guest*. The **jailer** protects
the host from a compromise of the **Firecracker VMM process itself** (a bug in
its virtio/vsock device model exploited from inside the guest). Upstream ships
a `jailer` binary for exactly this, but it assumes real root — it `mknod`s
`/dev/kvm` inside the chroot and manages cgroups, and `mknod` of a device node
needs `CAP_MKNOD` in the **initial** user namespace, which a rootless
`cs_host` container does not have. So `fcjail.go` implements the same model
with primitives that work rootless (verified: real Firecracker v1.11 boots the
real kernel to `Hypervisor detected: KVM` inside the jail).

**Mechanism.** `fcSpawn` re-execs the daemon binary as `clawson fcjail <spec>`
with `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS` and a uid/gid map of
`{0→0, uid→uid}`. The child (`fcjailMain`) is mapped-root for setup, then in
its **private mount namespace**: bind-mounts only what FC needs into the
per-VM chroot (`<g>.jail/`) — the firecracker binary, kernel and rootfs
(read-only), this VM's `workspace.img` and vsock socket dir (`/vsock`), and
`/dev/kvm` + `/dev/urandom` — `chroot`s in, sets `PR_SET_NO_NEW_PRIVS`, drops
to the unprivileged per-VM uid, and execs Firecracker. FC's own seccomp filter
(never disabled) still applies on top.

**Per-VM uid.** `fcJailUID` = `30000 + (proxyPort − PORT_BASE)`. clawson-host's
rootless userns maps container uids `1..65536` to unprivileged host subuids, all
distinct from the daemon (container uid 0 → host uid 1000). The uid is stable
per group (proxy port is persisted), so the workspace image's ownership stays
consistent across reboots. Distinct groups get distinct uids — VMs can't touch
each other's files, and none share the daemon's uid.

**What a VMM escape lands in**, versus the pre-jailer state (VMM ran as the
daemon uid, in the daemon's namespaces, with the creds mount + all group
workspaces + the daemon's own control-plane authority reachable). Note the
podman DooD socket — historically the worst thing reachable here — has since
been removed outright (its last user, whisper STT, is gone), so it no longer
factors in either the jailed or unjailed case:

| Axis        | Jailed VMM |
|-------------|-----------|
| uid         | distinct unprivileged subuid — can't ptrace/signal the daemon, doesn't own `creds/` or the workspaces |
| filesystem  | empty chroot — no host FS, no `creds/`, no other group's workspace, no project dir |
| network     | own netns — no route anywhere, no TCP path to the daemon's gRPC control plane |
| pid/ipc/uts | own namespaces |
| privilege   | no capabilities (setuid from 0 drops them) + `no_new_privs` + FC seccomp |

This closes **trust-model gap #1** (a VM escape no longer lands on host
authority). It does **not** change what a compromised *guest* can do through
its sanctioned channels (proxy egress, `main`'s peer-orchestration verbs) — the
jailer is strictly about containing the host-side VMM process.

**Permissions plumbing.** FC runs as the dropped uid but reaches its sockets
and workspace through bind mounts, so `fcJailFixupPerms` chowns the socket dir
(FC creates its own `uds` listener there) and `workspace.img` to the VM uid,
and `chmod 0666`s the daemon-created `uds_<port>` listener sockets so the
cross-uid connect is permitted (scoped to this group's own dir). The daemon
keeps full access as the container's mapped-root (`CAP_DAC_OVERRIDE` over its
subuids), so later resize/migration still works.

**Opt-out.** `CLAWSON_FC_NOJAIL=1` runs FC unjailed as the daemon uid with the
absolute-path config at `<g>.cfg.json` (the pre-jailer behavior) — for
environments that can't create nested user namespaces, or for debugging.

**Not (yet) covered.** cgroup resource caps — upstream's jailer sets them, but
rootless cgroup-v2 delegation is unreliable and the machine-config already
bounds vCPU + RAM. The e2fsck/resize2fs host-side parse of the guest-writable
`workspace.img` (trust-model gap #2) also still runs unjailed; that's a
separate follow-up.

## Containers (rootless podman in the guest)

The golden rootfs ships **rootless podman** (5.x) so the agent can run
containers *inside* the microVM. This replaces the old podman-in-podman "pip"
path we removed: there, the sidecar needed `SYS_ADMIN` on the **host**
container, collapsing tier-3→tier-1 isolation on any runc/crun escape. Here
podman runs on the guest's **own kernel behind KVM** — a container escape is a
guest-VM escape, not a host escape. Rootless, as `node` (uid 1000).

What makes it work (all verified live — pull over L3 + a container reaching the
internet through pasta):

- **Kernel** (`build-kernel.sh`): FC's config already had `USER_NS`,
  `OVERLAY_FS`, cgroup v2, `BRIDGE`/`VETH`, iptables NAT, `SECCOMP`; we added
  `FUSE_FS` (fuse-overlayfs) and — built-in, since the guest has no module
  loader — the **full nftables NAT stack** netavark needs for *bridged*
  networking (`NF_TABLES` + `NF_TABLES_INET` + `NFT_NAT`/`NFT_MASQ`/`NFT_CT`/
  `NFT_FIB_INET`/`NFT_COMPAT`). `TUN` was already there for L3 (rootless pasta
  reuses it). Verified: `podman network create` + a container on the bridge
  reaches the internet via netavark's nft masquerade.
- **Rootfs** (`Dockerfile.rootfs`): `podman crun conmon containers-common
  fuse-overlayfs passt slirp4netns shadow-utils`; `/etc/subuid`+`subgid` for
  node; `storage.conf` (overlay+fuse-overlayfs); `containers.conf`
  (`cgroup_manager=cgroupfs`, `events_logger=file`, pasta networking — no
  systemd in the guest); and `newuidmap`/`newgidmap` marked **setuid** (the
  `security.capability` xattr doesn't survive the export-tar → `mkfs -d`).
- **Guest agent** (`earlyInit`): `mknod /dev/fuse`; `/dev/net/tun` is `0666`
  (both the L3 TAP and pasta open it); mount `cgroup2`; make `/` rshared;
  `/run/user/1000` (0700 node) under a 0755 `/run/user`; `XDG_RUNTIME_DIR` +
  `TMPDIR=/workspace/.tmp` in the worker env (podman stages image blobs in
  `TMPDIR`; `/var/tmp` is on the read-only rootfs).

Practical notes: **pulling images needs egress**, so containers are effectively
a networked-profile feature (pull rides the L3 gateway; needs `wan` or `full`
to reach a public registry — `none` has no NIC; `lan` only reaches a LAN
registry). Container images live under `$HOME=/workspace` (the writable ext4), so
image-heavy groups may want a larger `workspace.img` and more `mem_mib`.
