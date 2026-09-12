# Firecracker microVM groups — vsock IPC design

Status: **implemented and verified end-to-end** (2026-07-08): real Venice
turns with in-guest tool execution through the vsock proxy path, correct
per-group metrics attribution, no-NIC isolation confirmed from inside the
guest (lo only, curl fails, no DNS), graceful stop via guest reset, and
conversation history persisting across VM + daemon restarts. **Every group is
a microVM — this is the only runtime.** The podman group runtime has been
retired; there is no `runtime` config key and no way back to it.
Code: `fc.go` (daemon side), `fcguest/` (guest agent + rootfs build),
`fc_test.go` (wire-logic smoke tests against a fake FC endpoint).

## Why vsock at all

Under the retired podman runtime, all host↔group IPC WAS host files under a bind mount:
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
ctl plane and peer config via the Config RPC; it loses direct reads/writes
of peer workspace *files*. Consequence: no Kata/virtio-fs needed.
Config/prompt/log are host-authoritative. (The skills feature — a host-side
catalog pulled on demand over the ctl plane — has since been removed
entirely; per-group prompt.md + /runscript cover its use cases.)

## Port map (single vsock device, demuxed by port — as built)

| Dir          | port  | purpose                                | replaces                  |
|--------------|------:|----------------------------------------|---------------------------|
| guest → host | 9000  | API egress (vsock → the group's proxy unix socket, `run/proxy/p<port>.sock`) | `ANTHROPIC_BASE_URL` bridge |
| guest → host | 9001  | *(retired — no raw guest→host text channel remains)* | `.cs/log` bind mount |
| guest → host | 9004  | turn stream: one connection per turn, framed `TurnFrame` (open{slot}, text/think/tool/tool_out/err, turn_end) → rendered as `[[marker]]` text into host `.cs/log.<slot>` | `.cs/log` bind mount |
| guest → host | 9002  | ctl plane (framed protobuf `CtlRequest`/`CtlResponse`, replies inline)  | `.cs/ctl` + `.cs/ctl.out` |
| guest → host | 9003  | L3 ethernet frames → gVisor gateway (**`network` ≠ `none`**) | a real NIC |
| guest → host | 9004  | per-slot turn streams (one per concurrent turn) | — (new: 10 slots) |
| host → guest | 10000 | agent RPC (framed protobuf `AgentRequest`/`AgentResponse`; ops init/msg/exec/exec_stream/run_script/shell_attach/shutdown) | `.cs/in` FIFO + `podman exec` |

**9002 and 10000 speak protobuf (`protocol/guest.proto`), not JSON lines.**
Every message is one `uint32` big-endian length + one serialized message,
with a per-channel maximum enforced on the length *before* the payload is
allocated (`daemon/fcframe.go`, `fcguest/frame.go`): 1 MiB for a ctl request,
16 MiB for anything the guest sends on 10000, 64 MiB host→guest (attachments
ride in `MsgReq`). The verb/op set is a `oneof`, so an unknown verb is
unrepresentable and every field is typed at decode; binary payloads (job
output, notify/report bodies, exec output) are `bytes`, never base64-in-JSON.
The guest-facing interface is unchanged: the shell tools still write one JSON
line to `.cs/ctl` and read a flat JSON reply from `.cs/ctl.out` — `fc-agent`
(`fcguest/ctl.go`) converts the line with *strict* protojson (unknown field,
wrong type or unknown verb is refused in the guest with an `{"ok":false}`
reply and never framed) and flattens the typed `CtlResponse` back into the
documented `{"ok":true,"port":…}` shape. Host-side, `ctlDispatchPB`
(`daemon/ctlpb.go`) adapts the decoded request onto `ctlDispatch`'s verb
handlers. The turn stream (9004) is typed too: fc-agent runs the worker
itself (`fcguest/turn.go`) and sends `TurnFrame`s; `fcTurnSink`
(`daemon/fcturn.go`) renders the marker text host-side, escaping any text
line that would parse as a marker, so the guest cannot author a marker at all. Guest and daemon must move together
(`make rootfs` + `/restart` every group).

Attribution comes from *which* `<g>.vsock_<port>` socket a connection lands
on, exactly like the per-group proxy socket does (`run/proxy/p<port>.sock`,
unix — the loopback TCP listener it replaced was reachable by every local
uid, audit 2026-09-04 M1).

Key insight that kept the diff small: **the host log file stays the single
source of truth.** The turn-stream handler (`fcTurnSink`) renders the guest's
typed frames into `groups/<g>/.cs/log.<slot>` in the marker grammar — so
`tailLog`, History, `/clear` truncation, proxy `logAppend`, and sendNow's
`>>>` markers are all runtime-oblivious. No parser refactor. Similarly the 9000 handler splices into the group's *existing*
proxy listener, so credential injection and metrics attribution are unchanged.

## Guest side (`fcguest/main.go`, PID 1)

The agent runs each turn itself (`fcguest/turn.go` — what
`sidecar/entrypoint.sh` used to do in shell, retired with the typed turn
stream):

- early boot: /proc /sys /dev tmpfs devpts mounts, hostname, **loopback up**
  (needed for the in-guest TCP bridge), mount `/dev/vdb` at `/workspace`
  (mkfs.ext4 fallback), chown to uid 1000.
- `.cs/ctl` is the one **FIFO in the guest** (the shell tools' request line;
  replies append to `.cs/ctl.out`). There is no `in` or `log` FIFO any more.
- bridges: TCP `127.0.0.1:18888` → vsock 9000 (`ANTHROPIC_BASE_URL` points
  here); ctl FIFO line → vsock 9002 (strict protojson → framed `CtlRequest`)
  → framed `CtlResponse` → flattened JSON line → `ctl.out`.
- turns (vsock 9004, `fcguest/turn.go`): `msg` spawns a goroutine that dials
  9004, sends `TurnOpen{slot}`, runs the worker as uid 1000 — `claude -p
  --bare … --output-format stream-json` decoded in Go (what
  `stream_filter.js` did; it now serves only cs-subagent), or
  `venice_stream.js` in `KOTO_EVENTS` mode emitting JSON events — and sends
  `TurnFrame`s as they happen, then `TurnEnd` unconditionally (a
  1200s watchdog SIGKILLs the worker's process group and reports `[[err]]`).
  Per-session claude conversation pinning (`/workspace/.cs/sessions/<name>.id`,
  `--resume`; the one-shot `--continue` migration for pre-session workspaces),
  provider/model/effort from config.json, and `KOTO_SESSION` /
  `KOTO_SHELL_SESSION` in the worker env all moved here from the entrypoint.
- agent RPC (vsock 10000): `init` (published ports + env, kept for every
  worker; reconciles orphaned job dirs once), `msg` (writes the per-session
  system prompt + config.json + attachments into the guest workspace, then
  starts the turn above), `exec` (sh -c, 60s cap, rc+output — the `podman exec`
  analogue used by interrupt + /clear), `exec_stream` (raw streamed output,
  peer-close kills the child — used by background-job tailing), `run_script`
  (its OWN op, backing the admin-only RunScript RPC: framed streaming exec as
  the worker user node/uid 1000 with HOME=/workspace, rather than the agent's
  root), `shell_attach` (bidi tmux attach behind `/shell`), `shutdown`
  (sync + umount + poweroff — dirty-ext4 protection for workspace.img).
- PID-1 zombie reaping via a central wait4(-1) loop with a tracked-pid table
  (the catatonit role), children in their own process groups.

## Daemon side (`fc.go` + call sites in `groups.go`/`send.go`)

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
- turn lifecycle: `[[turn_end]]` arrives via vsock → host log → tailLog →
  `notifyTurnDone`, feeding sendNow's wait/stall/selfHeal logic
  (restart() = stopGroup + ensure).

## Where per-VM state lives (host layout)

```
groups/<g>/
  workspace.img       guest /workspace (ext4, virtio-block rw) — the ONLY
                      guest-writable persistent state. Single-writer: never
                      mount it host-side while the VM runs.
  .cs/log.<slot>      daemon-rendered transcript of the vsock 9004 turn stream
  .cs/config.json     host-authoritative (proxy + daemon read/write; guest
                      gets a copy pushed per turn)
  prompt.md           host-authoritative (composeSystemPrompt reads it)
run/fc/
  <g>.sock/v[_9000/1/2/3]  per-group dir: hybrid-vsock UDS + per-port listener
                      sockets. Own dir so the jailer can bind-mount exactly
                      this VM's sockets into its chroot (as /vsock).
  <g>.jail/            per-VM chroot root the jailer stages (bind targets for
                      firecracker/kernel/rootfs/workspace/dev/vsock + fc.json)
  <g>.cfg.json         unjailed only (KOTO_FC_NOJAIL=1); jailed config is
                      written into <g>.jail/fc.json instead
  <g>.pid / <g>.console.log
fcassets/             (gitignored) firecracker binary, vmlinux, rootfs.img
```

RAM/vCPU state is ephemeral (no snapshots in v1). Rootfs is read-only and
shared by all VMs.

## Build & run

```sh
make assets    # = firecracker + kernel + rootfs:
                  #   fetch firecracker (pinned v1.16.1), BUILD the guest
                  #   kernel (FC's CI vmlinux lacks CONFIG_TUN, so we compile
                  #   the Amazon Linux tree — see build-kernel.sh), and build
                  #   the golden rootfs (fedora + node + claude-code +
                  #   sidecar/ + fc-agent + rootless podman; no chrome)
make host-build   # once: host image includes e2fsprogs + tar
make host-run     # run-host.sh passes --device /dev/kvm when present
```

Rootfs rebuild (`make rootfs`) is required after editing
`sidecar/*.{sh,js}` or `fcguest/` — microVMs have no live bind mounts (the
one ergonomic regression vs podman's hot-reload mounts).

## Bugs found & fixed during bring-up (the log for posterity)

- `/workspace` must be baked into the (read-only) rootfs as a mount point.
  (Earlier versions also baked in `/skills` + a tmpfs for an init-time
  tarball; superseded first by an on-demand ctl-plane pull, then removed
  with the skills feature itself.)
- PID 1 starts with an empty environment — set PATH before any exec.
- FC has no ACPI: guest poweroff is a no-op; graceful exit is
  `reboot(RESTART)` + `reboot=k` (i8042 reset, FC catches it and exits).

## Known limitations / follow-ups

1. **Published ports** bind inside cs_host (reachable on koto-net as
   `cs_host_go:<port>`), not on the real host loopback — host publishing
   needs a `-p` on cs_host itself (podman can't add one live).
2. ~~No open-internet escape hatch yet.~~ **DONE** — the `network` profile
   (`none` default / `wan` / `lan` / `full`). The networked profiles attach a
   gVisor L3 gateway over vsock 9003 (real NIC: arbitrary TCP/UDP + DNS,
   egress-filtered at the frame layer by destination class); `none` stays
   NIC-less. See "Network egress profile" below.
   *L3-native inbound is still a follow-up (see that section).*
4. ~~**main on firecracker**: skill authoring via the rw /skills mount
   doesn't exist there.~~ Moot — the skills feature was removed entirely.
5. ~~**skills refresh** for a running FC group needs /restart.~~ Moot —
   the skills feature was removed entirely.

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
| `wan`   | ✅ | ❌ | ❌ |
| `lan`   | ❌ | ✅ | ✅ |
| `full`  | ✅ | ✅ | ✅ |

**`wan` is the safe general-purpose profile** (and where legacy `internet=full`
now maps — see below): the agent can `curl`/`git`/`npm` the public internet but
cannot reach the host LAN, so a prompt-injected agent can't scan or pivot into
your other machines. `lan` is for the rare group that must talk to a LAN device
but should not have public egress; `full` is both (the old `internet=full`
behavior, now explicit). The **tailnet (CGNAT 100.64/10) is classed as LAN**,
not WAN — tailnet peers are host-reachable infrastructure, same trust tier as
the LAN, so a `wan` group cannot reach them; tailnet access requires `lan` or
`full`.

(ICMP/ping is best-effort — it needs the gateway process to open a
raw/unprivileged ICMP socket on the host; TCP/UDP, i.e. all koto tooling,
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
- **Summarized flow log.** The frame filter also logs every new guest-initiated
  flow on the `egress` subsystem of the daemon log (`fcFlowLogger`, fcnet.go):
  `[<g>] flow TCP 192.168.127.2 -> 1.2.3.4:443` at info, blocked flows at warn
  (`BLOCKED flow … (network profile 'wan')`). "New flow" = a TCP SYN, or the
  first UDP/ICMP packet of a (proto, dst, port) tuple per minute — one line per
  HTTPS request/connection, not per packet; SYN retransmits and repeat traffic
  within the TTL are deduped. Guest↔gateway traffic (DNS to `.1`) is skipped.
  This restores a *who-talked-to-whom* audit trail for networked profiles
  (IP-level, not L7 — URLs/SNI are not visible at the frame layer). The **LLM
  leg is covered too**, on its own **`llm` subsystem** (`llmFlowLog`, proxy.go):
  each origin-form request the proxy forwards upstream logs
  `[<g>] flow POST api.anthropic.com/v1/messages` (or the Venice equivalent),
  deduped per (group, method, path) on the same TTL. Split from `egress` so the
  steady LLM heartbeat is filterable apart from general traffic — `egress` is
  the anomaly-hunting ground, `llm` the expected baseline. Watch either via the
  TUI log view (^L) or `koto ctl logs`.
- **Egress authority is at the frame layer.** gvisor-tap-vsock has no
  destination-filter hook (it `net.Dial`s the packet's destination directly),
  so `fcEgressConn` (fcnet.go) parses each guest frame, classifies its
  destination (`fcClassifyDst`), and applies the group's profile (`fcDstAllowed`).
  The classes:
  - **ctl** — loopback (`127/8`, `::1`), link-local (`169.254/16`, `fe80::/10`,
    incl. link-local multicast like mDNS `224.0.0.251`), and **cs_host's own
    interface IPs** (`fcSelfIPs`, where the daemon gRPC on
    `KOTO_BIND:KOTO_PORT` lives; the per-group proxies are unix sockets). **Dropped
    under every profile.** `Ec2MetadataAccess=false` also blocks metadata inside
    the netstack.
  - **gw** — the guest↔gateway subnet `192.168.127.0/24` (DNS at `.1`). **Always
    allowed** at the frame layer; carved out explicitly because it sits inside
    the `192.168/16` LAN range, so `wan` DNS would otherwise break. (At the L7
    proxy this carve-out does not apply — it's frame-layer-only.)
  - **lan** — RFC1918 (`10/8`, `172.16/12`, `192.168/16`), IPv6 ULA `fc00::/7`,
    non-link-local multicast, limited broadcast, CGNAT `100.64/10` (tailnet).
    Allowed for `lan`/`full`.
  - **wan** — everything else. Allowed for `wan`/`full`.
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
- **`lan` can't reach the koto host itself.** cs_host's own LAN IP is in
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
- **Kernel:** any networked profile (`wan`/`lan`/`full`) needs `CONFIG_TUN`, which FC's CI vmlinux lacks — so the
  guest kernel is built (`build-kernel.sh`, `make kernel`), not fetched.
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
    resolv.conf during build). fc-agent's `netUp` writes the target on
    every networked profile (`wan`/`lan`/`full`); `none` never calls `netUp`,
    so it is left dangling — no DNS, as intended.

## VM size profile (`size`: `small` | `medium` | `large` | `xlarge`)

Per-group config key selecting the machine shape — vCPU, RAM, and workspace
disk together, as one named preset (defined in `fcSizePresets`, `fc.go`):

| preset | vCPU | RAM      | workspace.img |
|--------|------|----------|---------------|
| small  | 2    | 1024 MiB | 8 GiB (default; absent `size` ⇒ small) |
| medium | 2    | 2048 MiB | 12 GiB        |
| large  | 4    | 4096 MiB | 16 GiB        |
| xlarge | 8    | 8192 MiB | 24 GiB        |

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

## Host resource limits (IO rate limiter · niceness · per-VM cgroups)

Motivation (TODO: "one firecracker vm can bring down the whole host"): the
`size` preset bounds what the *guest* sees (vCPUs, RAM), but before this
stack, nothing bounded what the VMM cost the *host* — a guest hammering
virtio-blk could saturate the host disk for the whole fleet, and a guest
spinning all its vCPUs competed with the daemon at equal priority. Four
layers, all defaults, no new user-facing knobs:

1. **Virtio-blk rate limiter** (`fcVMConfig`, `fc.go`) — Firecracker token
   buckets on **both** drives (the rootfs is read-only but `dd if=/dev/vda`
   still generates host reads), sized by the `size` preset:

   | preset | bandwidth | ops/s | one-time burst |
   |--------|-----------|-------|----------------|
   | small  | 100 MiB/s | 15000 | 256 MiB |
   | medium | 150 MiB/s | 22500 | 256 MiB |
   | large  | 200 MiB/s | 30000 | 256 MiB |
   | xlarge | 250 MiB/s | 37500 | 256 MiB |

   The burst keeps short legitimate spikes (git checkout, npm install unpack)
   snappy; only *sustained* IO is throttled. The ops bucket guards
   fsync/small-random-IO storms that saturate a disk far below its bandwidth
   ceiling — it is sized at 150 ops per MiB/s because the guest kernel splits
   sequential IO into ~8 KiB virtio requests (measured: at 2000 ops/s a
   dd bs=1M ran at 15 MB/s, ops-bound), so a much lower ops budget silently
   becomes the bandwidth cap. Raw `io_mbps` / `io_ops` config keys layer on the preset
   (clamped 10–4000 / 100–100000; same escape-hatch pattern as
   `vcpus`/`mem_mib`) for the rare legitimately IO-heavy group; `/config`
   shows the resolved values as a display-only `io` field. Applies on
   `/restart` (`fcResolveIO`). This is the **only** IO lever available:
   rootless delegation excludes the cgroup `io` controller, and `network`
   egress is a vsock channel, not a virtio device.
2. **`nice=10` on the VMM process** — the jail shim renices itself before the
   uid drop (`fcjailMain`; the unjailed path renices post-`Start`), so the
   daemon/proxy at nice 0 always preempt runaway VMs. CPU *capacity* is
   already bounded by `vcpu_count`; this fixes *priority*.
3. **Per-VM cgroups** (`fccgroup.go`) — probed once at startup; when cs_host
   has a writable cgroup tree (see below), every VM is placed at clone time
   (clone3 `CLONE_INTO_CGROUP`) into `<scope>/vms/<g>` with
   `cpu.weight = 50 × vcpus` — scaled by the size preset's vCPU count so under
   host CPU contention an xlarge (weight 400) gets 4× a small's (weight 100)
   share; per-VM weights compete only among siblings under `vms/`, while the
   daemon+proxy in `main/` are protected by the `main/`-vs-`vms/` split (100
   vs 100 → the control plane keeps half the host under full contention,
   whatever the VM weights sum to) — and `memory.high = mem_mib + 512 MiB` — a
   soft throttle, deliberately **never `memory.max`**: OOM-killing the VMM
   hard-kills the VM with a dirty ext4, and the guest's real ceiling is
   `mem_size_mib` anyway. Unavailable → one info log line and `cgroup=off`
   in the spawn log; nothing else changes. **Placement has a second mode**,
   because clone3 is not always callable: the installed unit sets
   `RestrictNamespaces=`, and systemd — unable to inspect the flags inside
   clone3's args struct — blocks the syscall outright with `ENOSYS`, expecting
   glibc to fall back to `clone()`. Go's `os/exec` does not fall back, so every
   `UseCgroupFD` spawn failed with `firecracker start: fork/exec: function not
   implemented` (measured 2026-09-12: an entire installed fleet unable to boot
   a single VM, while the same binary ran fine from a dev clone, which has no
   unit and so no seccomp filter). `fcClone3Available` now probes for it at
   startup — a `clone3(NULL, 0)` that the kernel rejects with `EINVAL` before
   it can fork, so `ENOSYS` means blocked — and when it is blocked the pid is
   written into `<leaf>/cgroup.procs` right after `Start` instead
   (`fcCgroupPlace`), the pre-clone3 way. The cost is the move-after-start race
   the clone-time path exists to avoid, which is small here: the caps are soft,
   and the window closes before the VMM has read its config, let alone
   allocated guest RAM. `koto userns-check` reports which mode a host gets. Enablement is
   `--cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw` in
   `host/run-host.sh` (the daemon evacuates its scope into a `main/` leaf to
   satisfy cgroup v2's no-internal-process rule, then enables `+cpu +memory`
   on the scope). The jailed VMM never sees the mount — the jail unshares
   `CLONE_NEWCGROUP`.
4. **Sustained-CPU alert** (`resources.go`) — subject `cpu:<g>` beside the
   host-fs and per-group-disk subjects: the 30s sweep's trailing 5-minute
   average, expressed as a percent of the group's *own* vCPU entitlement
   (size-independent thresholds), through the same 80/90 + hysteresis
   notify path. Alert-only; layers 1–3 do the enforcing.

Fleet-wide: `run-host.sh` passes `--cpus` to the cs_host container — a hard
ceiling on daemon + proxy + all VMs together. Default `nproc - 1` (min 1) so
the host stays responsive no matter what; override with `KOTO_HOST_CPUS=<n>`,
or `KOTO_HOST_CPUS=0` for unlimited.

Fleet-wide **memory** is capped too, but *not* via podman `--memory` — that
would put the daemon and proxy in the same OOM pool as the VMs, and an
OOM-killed daemon is a fleet outage. The daemon enforces it one level down
(`fchostmem.go`), where only VMMs can ever be charged:

- **Admission** — `fcSpawn` refuses to boot a VM when the live VMs' `mem_mib`
  (+ the 512 MiB VMM margin each) plus the new one would exceed the cap. The
  error names the numbers and the way out (stop a VM, smaller `size`, raise
  the cap). This is the normal path; the cap should never actually be hit.
- **Backstop** — `fcCgroupInit` writes `memory.max = cap` and
  `memory.high = cap − 512 MiB` on the `vms/` parent cgroup. Overhead that
  admission can't see (VMM page cache from `workspace.img` IO, device-model
  growth) throttles the VMs first and, at worst, OOM-kills one VMM — the host
  and the daemon in `main/` stay up. Needs the same writable cgroup mount as
  the per-VM caps; without it only admission applies.

Default = 90 % of `MemTotal` (read from `/proc/meminfo` at daemon start),
so the kernel, podman, the daemon and the operator's shell keep the remaining
10 %. Override with `KOTO_HOST_MEM_MIB=<n>` (passed through by
`run-host.sh`), or `KOTO_HOST_MEM_MIB=0` for unlimited (no admission, no
parent limit). The daemon logs the resolved cap at start
(`fleet memory cap: … MiB`).

**The cap is observable, not just enforced.** `resourcesSnapshot` reports it
on both planes (`HostResources.mem_cap_mib` / `mem_committed_mib` /
`mem_host_total_mib`, and per group `mem_committed_mib` = `mem_mib` + margin
while running, 0 stopped — `koto ctl resources`, main's ctl-plane `resources`
verb). The TUI's fleet view (ctrl+k) renders it as a second rollup row:
`vm mem 11.5G/12G 94% · free 704M` plus a one-line memory map — a bar scaled
to the cap, one named segment per running VM sized by its committed share,
biggest first, dotted tail = what the next spawn can still get. Committed is
the admission figure on purpose: RSS is a high-water mark of touched pages
and says nothing about whether another VM fits.

The spawn log line records what was applied:
`microVM up pid=… vcpus=… mem=…MiB io=…MiB/s,…ops nice=10 cgroup=on|off`.

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
  (`fcguest/main.go`) then calls `enableRoot` once, before the entrypoint starts,
  so the first turn already has it. Applied on **`/restart`**.
- **Writable-persistent root (`sudo dnf install` works and survives restarts).**
  The root drive is the shared golden rootfs, attached read-only — it is never
  written. Instead `enableRoot` → `overlayRootDirs` mounts an **overlayfs** on
  each package-manager-owned directory (`/usr`, `/etc`, `/var`, `/opt`) with the
  upper/work layers on the per-group workspace ext4 under
  `/workspace/.rootovl/<dir>/{upper,work}` (root-owned `0700` at the top, so
  `node` can't tamper with the layer except through sudo). `sudo dnf install`,
  `sudo npm i -g`, config edits under `/etc` — all land in the upper layer and
  **persist across `/restart`** with the rest of the workspace image, while the
  golden rootfs stays pristine and shared by every VM. Needs
  `CONFIG_OVERLAY_FS` (already `=y` in the FC microvm CI kernel config our
  vmlinux builds from). Package installs need repo egress, so pair `root=yes`
  with `network=wan`/`full` in practice.
- **How the sudo grant is installed.** With the `/etc` overlay up, `enableRoot`
  simply writes `node ALL=(ALL) NOPASSWD: ALL` to `/etc/sudoers.d/node`
  (mode 0440, root-owned); sudo's baked `/etc/sudoers` already `@includedir`s
  that dir, and sudo's timestamp dir lives under `/run` (a tmpfs). If the
  overlay mounts fail (e.g. a stale pre-`CONFIG_OVERLAY_FS` vmlinux), it falls
  back to the original scheme — a small **tmpfs** on `/etc/sudoers.d` — so
  `root=yes` still means sudo even when it can't mean persistence. A partial
  overlay failure unwinds every already-mounted layer first, so the fallback
  never runs on a half-applied set. `sudo` itself ships in the golden rootfs
  unconditionally (`Dockerfile.rootfs`); only the grant is runtime-gated.
- **Caveats.** (1) Installs consume **workspace disk** — dnf-heavy groups may
  want a bigger `size` preset. (2) Upper-layer entries **shadow the golden
  rootfs**: after a `make rootfs` that upgrades a file the group also
  modified, the group keeps its upper copy. Reset by deleting the `.rootovl`
  tree while its overlays are NOT mounted (never rm a live upper layer): stop
  the group and wipe it from `workspace.img` host-side, or destroy/recreate the
  group. (3) Dirs outside the four overlays
  (`/root`, `/home`, `/boot`) stay read-only; Fedora packages virtually never
  write there at install time. (4) Flipping back to `root=no` leaves the
  `.rootovl` tree on disk but unmounted — its contents (including the persisted
  sudoers grant) become invisible until `root=yes` returns.

## Autostart profile (`autostart`: `yes` | `no`)

Per-group config key deciding **when** the group's microVM boots. Default `no`:
a group's VM comes up lazily, on the first thing that needs it (a send, a spawn,
a `/restart`, a schedule firing) — the daemon starts with only `main` running,
so a fleet of idle groups costs nothing. `yes` boots the group as soon as the
daemon does.

- **Why.** Some groups have to be up before anyone talks to them: a group
  publishing a port (`ports`) has nothing listening until its VM boots, and a
  group whose work is entirely scheduled would otherwise be down until its first
  cron fire. Autostart makes "the daemon is up" imply "this group is up".
- **Plumbing.** `groupAutostart(g)` (`groups.go`) reads config.json
  `"autostart"`; `autostartGroups()` runs once from `daemonMain` in a goroutine
  (a VM boot is seconds — the gRPC listener must not wait on it), sorted for a
  deterministic order and **sequential**, so N groups don't contend for KVM and
  RAM at once. `main` is skipped: `daemonMain` ensures it unconditionally,
  autostart or not.
- **Applies at daemon start, not `/restart`.** This is the one spawn-adjacent
  knob a `/restart <g>` does nothing for — the value is read only while the
  daemon comes up. Setting it takes effect on the next daemon start; to get the
  VM up right now, just send the group a message (or `/restart` it), which boots
  it the ordinary way.
- **No turn is enqueued by the boot.** Booting (autostart or otherwise) does
  not wake the agent — the VM just comes up and waits for its next message. An
  agent that needs to resurrect services after a restart does so on its next
  turn (`cs-job list` shows orphaned jobs).
- **Failures are logged, not fatal.** A group that fails to boot logs at `error`
  on its own group subsystem and the loop continues to the next one.

## Jailer (host-side isolation of the Firecracker process)

The KVM boundary protects the host *from the guest*. The **jailer** protects
the host from a compromise of the **Firecracker VMM process itself** (a bug in
its virtio/vsock device model exploited from inside the guest). Upstream ships
a `jailer` binary for exactly this, but it assumes real root — it `mknod`s
`/dev/kvm` inside the chroot and manages cgroups, and `mknod` of a device node
needs `CAP_MKNOD` in the **initial** user namespace, which a rootless
`cs_host` container does not have. So `fcjail.go` implements the same model
with primitives that work rootless (verified: real Firecracker v1.11 boots the
real kernel to `Hypervisor detected: KVM` inside the jail; re-verified on the
v1.16.1 upgrade, 2026-08-21 — full fleet boot, vsock exec, wan gateway).

**Mechanism.** `fcSpawn` re-execs the daemon binary as `koto fcjail <spec>`
with `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS` and a uid/gid map of
`{0→0, uid→uid}`. The child (`fcjailMain`) is mapped-root for setup, then in
its **private mount namespace**: bind-mounts only what FC needs into the
per-VM chroot (`<g>.jail/`) — the firecracker binary, kernel and rootfs
(read-only), this VM's `workspace.img` and vsock socket dir (`/vsock`), and
`/dev/kvm` + `/dev/urandom` — `chroot`s in, sets `PR_SET_NO_NEW_PRIVS`, drops
to the unprivileged per-VM uid, and execs Firecracker. FC's own seccomp filter
(never disabled) still applies on top.

**Per-VM uid.** `fcJailUID` = `30000 + (proxyPort − PORT_BASE)`. koto-host's
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
(FC creates its own `uds` listener there), the daemon-created `uds_<port>`
listener sockets, and `workspace.img` to the VM uid. The daemon keeps full
access as the container's mapped-root (`CAP_DAC_OVERRIDE` over its subuids), so
later resize/migration still works.

> This used to say `chmod 0666` on the listener sockets, and did that. A
> `connect(2)` needs write permission on the socket inode, and 0666 granted it
> to any local uid that could traverse the path — including on `v_9002`, the
> ctl plane, which is authorized purely by which socket the connection arrived
> on, so `main`'s socket was `main`'s full verb set. Audit M7 replaced it with
> the chown; this paragraph described the removed behavior until 2026-09-11.

**Opt-out.** `KOTO_FC_NOJAIL=1` runs FC unjailed as the daemon uid with the
absolute-path config at `<g>.cfg.json` (the pre-jailer behavior) — for
environments that can't create nested user namespaces, or for debugging.

**Resource caps.** Upstream's jailer also manages cgroups; our equivalent
lives in `fccgroup.go` — per-VM `cpu.weight`/`memory.high` applied at clone
time when cs_host has a writable cgroup tree, degrading to log-only when it
doesn't (see "Host resource limits" above). The jail contributes
`CLONE_NEWCGROUP` (the VMM can't see the host hierarchy) and the shim's
`nice=10`.

**Not (yet) covered.** The e2fsck/resize2fs host-side parse of the
guest-writable `workspace.img` (trust-model gap #2) still runs unjailed;
that's a separate follow-up.

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
