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

## Daemon side (`fc.go` + branches in daemon.go)

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
- vcpus/mem per group: config.json `vcpus` / `mem_mib` (defaults 2 / 2048).
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
  <g>.vsock[_9000/1/2]  hybrid-vsock UDS + per-port listener sockets
  <g>.cfg.json / <g>.pid / <g>.console.log
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
2. ~~No open-internet escape hatch yet.~~ **DONE** — the `internet` profile
   (`none` default / `full`) forwards general egress *through the proxy*, no
   NIC added. See "Internet egress profile" below.
3. **`pip` (podman-in-podman) and Chrome groups** stay on the podman runtime
   (not in the minimal rootfs).
4. **main on firecracker**: works protocol-wise (ctl over vsock), but skill
   *authoring* via the rw /skills mount doesn't exist there — main keeps
   using the SkillNew RPC path, or stays on podman.
5. **skills refresh** for a running FC group needs /restart (tar is pushed at
   init) — same rule as the ports feature.
6. **Migrating an existing podman group** doesn't move its workspace files
   into workspace.img; fresh workspace (or copy offline while stopped).

## Internet egress profile (`internet`: `none` | `full`)

Per-group config key controlling general outbound. Default `none` — a
microVM group's only egress is the LLM upstream via the proxy. `full` grants
arbitrary outbound **forwarded through the same proxy**, still with no NIC in
the guest:

- **Reuses the existing channel.** No new vsock port. The guest's `bash`/
  `curl`/`git`/`npm` get `HTTP_PROXY`/`HTTPS_PROXY=http://127.0.0.1:18888`
  (the same in-guest bridge → vsock 9000 → the group's proxy port) plus
  `NO_PROXY=127.0.0.1,localhost` so the LLM client's own base URL stays
  direct. The proxy recognizes `CONNECT` / absolute-form requests (LLM
  clients only ever use origin-form) as egress and forwards them.
- **Enforced server-side.** `serveEgress` (proxy.go) checks
  `groupInternet(g)` on every request, so a compromised guest that sets its
  own `HTTP_PROXY` gets a **403** unless the operator granted `full`. The
  guest env is only the client-side enabler; the proxy gate is the authority.
  Verified: a `none` group forcing the proxy → `403 CONNECT tunnel failed`.
- **Auditable.** Every forward is logged `egress[<group>] CONNECT <host:port>`
  (or the method for plain HTTP); denials log `egress[<group>] DENIED …`.
- **Self-target guard.** `egressTargetAllowed` blocks loopback, `cs_host_go`,
  link-local, and the daemon's gRPC port so a `full` guest can't turn the
  forwarder back on the control plane. It does NOT do full SSRF/IP filtering:
  a `full` group can reach the host LAN, same as a podman group with a NIC —
  inherent to "full internet".
- **Applies on `/restart`** (the guest env is set at spawn), but lowering to
  `none` denies egress **live** on the next request (the gate reads config
  per-request). Set via ctl `config_set internet=full` or by editing
  config.json.
- **podman groups**: the profile is a firecracker feature. A podman group has
  a real NIC and full internet regardless; `internet=none` is not enforced
  there (would need `--internal` networking).
