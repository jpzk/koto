# koto

Minimal isolated orchestrator for long-lived `claude` agents. Every group
(agent) runs in its own **Firecracker microVM** — hardware-isolated by **KVM**,
so the guest kernel is the security boundary — with **no network by default**;
a credential-injecting proxy is the only path to the LLM API, and agents never
see a real credential. Host side is a Go daemon; the UI is a separate,
network-isolated TUI container speaking gRPC over mTLS.

## Features

- **Isolation** — one Firecracker microVM per agent on KVM; no network by
  default (the LLM proxy is the only egress, `wan`/`lan`/`full` opt-in and
  frame-filtered); credentials injected by the proxy, never seen by agents
- **Orchestration** — `main` spawns, sends, schedules and delegates to peers
  over a control plane, no shared files; many concurrent named sessions per
  agent; background jobs, cron and goals that call back into the session
  that started them
- **Operation** — a TUI with a shared tmux terminal into every conversation;
  per-group config (`network`, `size`, `root`, `autostart`, `provider`,
  `model`) applied by `/restart`; per-VM cgroups and IO budgets with
  disk/CPU/memory alerts; per-group token metrics and tok/s
- **Access** — mTLS gRPC with role-based ACL, one proto for TUI, `koto ctl`
  and Android

## Architecture

```
╔═ TIER 1 · host user (full authority) ═══════════════════════════════════════╗
║  gRPC clients: cs_tui (tier 2.5, koto-net) · koto ctl · Android             ║
║  creds/: OAuth token · PKI (ca, client-*, tokens) · acl.json                ║
╚═══════════════╤═══════════════════════════════════════════╤═════════════════╝
                │ gRPC :8443  (mTLS + bearer token)         │ read per request
                ▼                                           ▼ (proxy memory only)
╔═ TIER 2 · cs_host — daemon container (no podman socket) ════════════════════╗
║  ┌─ role ACL ─────────┐   ┌─ daemon (Go) ─────────────┐   ┌─ LLM proxy ───┐ ║
║  │ admin · operator   │──▶│ lifecycle · session queues│   │ per-group port│═╬═▶ LLM API
║  │ reader · agent     │   │ log tailer · replay ring  │   │ injects key   │ ║   (real credential)
║  │ verb × target      │   │ cron · goals · ctl verbs  │   └───────▲───────┘ ║
║  └────────────────────┘   └──────▲────────────┬───────┘   ┌───────┼───────┐ ║
║                                  │            │           │ gVisor gateway│┄╬┄▶ internet / LAN
║                                  │            │           │ wan/lan/full  │ ║   (filtered NAT)
║                                  │            │           └───────▲───────┘ ║
╚══════════════════════════════════╪════════════╪═══════════════════╪═════════╝
        vsock 9001 log ·  9002 ctl │            │ vsock 10000       │ vsock 9000 (sentinel key)
              9004 turn streams    │            │ agent RPC         ┆ vsock 9003 (L2 frames)
╔═ TIER 3 · one jailed Firecracker VMM per group ═════════════════════════════╗
║  fcjail: userns+mnt+pid+net+ipc+uts · per-VM chroot · unprivileged uid ·    ║
║          no_new_privs · seccomp                                             ║
║  ┌┄ KVM boundary · guest kernel ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┐  ║
║  ┆  microVM <g>: fc-agent (PID 1) → claude -p --bare loop as uid 1000    ┆  ║
║  ┆  /workspace = workspace.img · rootless podman · no NIC by default     ┆  ║
║  └┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┘  ║
╚═════════════════════════════════════════════════════════════════════════════╝
```

**Control flow** runs top-down: a client authenticates (mTLS + token, then the
role ACL) and calls a gRPC verb; the daemon turns it into agent RPCs over
vsock 10000. `main`'s agent orchestrates peers through the ctl plane on vsock
9002, authorized by *group identity* (non-main groups get only self-scoped
verbs). **Data flow** runs bottom-up: guest output on vsock 9001/9004 → daemon
tailer → seq-numbered replay ring → `SubscribeGroup`/`WatchState` streams to
every attached client. The LLM leg is the only egress a default group has
(vsock 9000 → proxy, which alone holds the credential); a `network=` profile
adds a filtered NIC via the gateway on vsock 9003.

- **Daemon** (`daemon/`; entry in `daemon.go`, VM runtime in `fc.go`, one
  topic per file — see CLAUDE.md → Layout): boots/supervises microVMs, serializes one
  `claude -p --bare --resume <session>` turn per inbound message, tails each
  group's log and fans events out to subscribers, runs the cron scheduler,
  the goal loop (`goals.go`) and the ctl verb plane.
- **Proxy** (`proxy.go`): one listener port per group (attribution), injects
  the real credential per request, records per-request token metrics. Guests
  authenticate with a sentinel (`ANTHROPIC_API_KEY=proxied`).
- **Guest** (`fcguest/`, `sidecar/`): a PID-1 agent bridges everything over a
  single vsock device; `entrypoint.sh` runs the message loop as uid 1000.
- **Orchestration is verb-based**: no shared filesystem anywhere. `main`
  drives peers via ctl verbs (`spawn`/`send`/`stop`/`list`/`resources`/
  `sched_*`/`config_set`/`tail`); non-main groups get only self-targeted
  verbs (`sched_*`, `goal_*`, `notify`, `job_done`, and a solicited one-shot
  `report` back to main), force-scoped to themselves.

## Guest kernel

Every group boots the same `fcassets/vmlinux`, built by `fcguest/build-kernel.sh`
(containerized, no host toolchain). **No patches**: the source is a pristine
`amazonlinux/linux` clone at a pinned tag with its commit sha asserted before
the build; everything koto adds is `.config`.

**Amazon Linux tree, not vanilla.** It is the tree Firecracker builds its own
guest kernels from. A vanilla kernel cannot parse Firecracker's ACPI tables
and needs `acpi=off` — which removes the LAPIC timer, so **every idle microVM
busy-polls a full host CPU**. The amzn tree boots with ACPI and idles at ~0%.
See `docs/kernel-amzn-vs-vanilla.md`.

The base `.config` is Firecracker's own CI guest config plus, all built-in
(the guest has no module loader):

| enabled | why |
|---|---|
| `TUN` | the `network=wan\|lan\|full` gateway: the guest's TAP (`eth0`) talks L3 to the gVisor gateway over vsock; also what pasta needs for rootless podman |
| `FUSE_FS` | fuse-overlayfs, the storage driver for rootless podman in the guest |
| `NF_TABLES` + `NFT_*` / `NF_CONNTRACK` / `NF_NAT` | the NAT stack netavark needs for bridged podman networking |
| `IKCONFIG_PROC` | `zcat /proc/config.gz` in a guest shows what was actually built |

That is the whole delta: network egress and in-guest containers. Symbols
outside that list arrive via Kconfig `select` closure or as defaults
`olddefconfig` fills in for options Firecracker's older config never named
(e.g. the newer CPU mitigations) — harmless, but not chosen.

**Caveats:**

- The **shipped `vmlinux` predates the script's pin**: it was built against
  Firecracker v1.11.0's base config, the script now pins v1.16.1 (~312 config
  lines apart, notably `CONFIG_PCI` on). Nothing is broken — the guest boots
  `pci=off` on virtio-MMIO — but `make fc-kernel` produces a different kernel
  from the one running today.
- `TUN` is a capability, not just a device. A `network=wan` group can bring up
  its own overlay (WireGuard, tailscale) whose outer packets are ordinary
  public UDP, so the frame-layer egress filter cannot see inside it. Verified:
  a `network=wan` guest running `tailscaled` reached the host's tailnet via a
  DERP relay despite `100.64/10` being classed as LAN. The filter blocks the
  direct peer path, not the relayed one.

## Security

### Threat model

Untrusted tier-3 agents run `claude` on attacker-influenceable input (prompt
injection is assumed, not hypothetical). The design goal is that a fully
compromised agent gets **nothing**: no credentials, no network, no peer or
host filesystem, no control-plane authority beyond its own group.

```
tier 1   host user            full authority (by definition)
  │  boundary: dedicated creds dir; no ~/.claude mount; no podman socket in cs_host
tier 2   cs_host              daemon + proxy; vetted code, pinned deps
  │      blast radius: koto OAuth token + workspaces — no path to host podman
  │  boundary: KVM + jailer; vsock-only IPC; no shared FS; verb authorization
tier 3   microVM groups       untrusted; own kernel, no NIC, sentinel creds
tier 2.5 cs_tui               gRPC client on koto-net; scratch image; mounts
                              creds/ ro, scripts/ + prompts/ ro, run/tui rw.
                              NOT isolated: the `tui` clientid holds the admin
                              role, and every per-group proxy port is reachable
                              on koto-net (BIND=0.0.0.0 in host/Dockerfile)
```

**Security architecture — the load-bearing decisions:**

- **KVM is the boundary, not namespaces.** An escape from a group is a VM
  escape against Firecracker's minimal device model (virtio blk/vsock/net),
  not a shared-kernel container escape. This is why in-guest root
  (`root=yes`) and in-guest rootless podman are safe to offer.
- **The VMM process itself is jailed** (`fcjail.go`): re-exec'd into fresh
  user/mount/pid/net/ipc/uts namespaces, per-VM chroot with only what FC
  needs, distinct unprivileged uid, `no_new_privs`, FC seccomp on. A virtio
  device-model bug lands as nobody-in-an-empty-chroot, not as cs_host.
- **Credentials never enter a guest.** The proxy holds the real OAuth token
  in process memory and injects per request; guests get a sentinel
  and a `ANTHROPIC_BASE_URL` pointing at vsock 9000. A compromised guest can
  *use* the proxy, not steal from it.
- **Egress is a per-group profile.** `network=none` (default): no NIC, no
  route, no DNS — the LLM leg via the proxy is the only egress, enforced by
  absence of hardware, not policy. `network=wan|lan|full`: a real L3 NIC via a
  userspace gVisor gateway over vsock 9003, egress-filtered at the frame layer
  by destination class — `wan` = public internet only, `lan` = host LAN only,
  `full` = both. The guest can never reach loopback/link-local/cs_host itself (the
  control plane stays unreachable); general HTTPS then bypasses proxy audit —
  that's the documented tradeoff. **Performance caveat:** the gateway is a
  userspace netstack running *inside the daemon process* — every packet of a
  networked guest costs daemon CPU (~a core around 1 Gbps, per busy guest).
  The VMMs run niced and cgrouped so the daemon always preempts them, but the
  gateway is daemon-side, so this is the one path where guest load is *not*
  contained by that budget: a guest saturating its NIC (large `git clone`,
  `podman pull`, bulk ingest) competes directly with the proxy, event streams
  and gRPC for daemon cycles. Fine for API-scale traffic; if the fleet feels
  laggy while a networked group downloads, this is why. `network=none` groups
  are unaffected (no NIC at all).
- **No shared mutable filesystem.** Workspaces are per-group ext4 images;
  there is no `/peers`, no bind mounts into guests. Cross-group interaction
  exists only as authorized ctl verbs, checked against source-group identity
  daemon-side (`ctl.go`); non-main callers have targets force-overwritten to
  self.
- **Control-plane transport is mTLS + bearer** (private CA, client-cert
  fingerprint allowlist, per-RPC token — `auth.go`). No anonymous endpoint.
- **The UI is below the daemon in privilege.** `cs_tui` is a static Go binary
  on `scratch` joined to `koto-net` with the client PKI material mounted
  read-only: a compromised TUI dependency yields a gRPC client the daemon
  still authorizes per verb (role ACL). It has no shell and no ca-certs.
  Two caveats, both on the release todo: `creds/` is mounted **whole** (ro),
  so the TUI can read the OAuth token and the private CA key;
  and the credential-injecting proxy does **not** listen on loopback —
  `host/Dockerfile` sets `BIND=0.0.0.0`, so every per-group proxy port is
  reachable from anything on `koto-net`, `cs_tui` included.
- **No Docker-out-of-Docker.** cs_host holds no podman socket (removed with
  the whisper container, its last user); a tier-2 compromise cannot spawn
  containers or mount host paths. Podman exists only *inside* guests,
  rootless, behind KVM.

### Supply chain

No generated SBOM artifact is checked in; the surface is small enough to
state outright. Everything Go-side is pinned via `go.sum` under a 6-week
dependency-lag rule (each pin's release date is verified against
proxy.golang.org before adoption — see CLAUDE.md → Conventions).

**Go modules** (direct deps; indirect counts approximate):

| module | direct deps | indirect |
|--------|-------------|----------|
| `koto` (daemon) | `containers/gvisor-tap-vsock` v0.8.8 (`network=wan/lan/full` gateway; pulls the gvisor netstack), `golang.org/x/sys`, `grpc` v1.80.0, `protobuf` v1.36.11, local `koto-protocol` | 19 |
| `protocol/` (proto + generated pb) | `grpc` v1.80.0, `protobuf` v1.36.11 | 4 |
| `tui/` | charmbracelet `bubbletea` / `bubbles` / `glamour` / `lipgloss` / `log` / `x/ansi` / `x/vt` + `muesli/termenv`, `grpc`, `protobuf` — one auditable upstream org for the whole UI stack | 38 |
| `fcguest/` (guest PID-1 agent) | `golang.org/x/sys` only | 0 |

**Pinned non-Go components:**

| component | pin | where |
|-----------|-----|-------|
| Firecracker VMM | v1.16.1 (static musl, GitHub release) | `fcguest/fetch-assets.sh` |
| guest kernel | `amazonlinux/linux` tag `microvm-kernel-6.1.176-43.358.amzn2023`, verified against a pinned commit sha | `fcguest/build-kernel.sh` |
| protoc plugins | `protoc-gen-go` v1.36.11, `protoc-gen-go-grpc` v1.6.1 | `Makefile` |

**Container images:** cs_host = `golang:1.24-alpine` + nodejs/npm/
e2fsprogs(+extra)/tar + claude-code; TUI runtime = `scratch` (one static
binary, no shell, no ca-certs); guest rootfs = `fedora:44` + 27 dnf packages
(podman, crun, conmon, fuse-overlayfs, passt, nodejs, python3, git, ripgrep,
tmux, sudo, …) + claude-code;
build-only = `ubuntu:24.04` (kernel) and `golang:1.24-alpine` (protoc,
fc-agent).

**Known-floating** (what a formal SBOM would flag): `@anthropic-ai/
claude-code` is installed unpinned by npm in both cs_host and the guest
rootfs — it resolves to latest on every image build; the dnf packages and
the base-image tags (`fedora:44`, `golang:1.24-alpine`, `ubuntu:24.04`)
float within their tags. The Go trees are fully locked; the OS-package and
claude-code layers are the accepted moving parts.

## Host requirements

Everything runs rootless as the host user. A dev clone touches nothing
outside it; `koto install` adds exactly three system files (the `koto`
binary, `/etc/koto/koto.env`, the systemd unit) and the service still runs
rootless, as you. What the host must provide:

- **Fedora, or Ubuntu 24.04+.** Both are supported and tested; the
  differences they need (`/dev/kvm` permissions, the AppArmor userns sysctl,
  the `uidmap` package) are checked by `koto setup` with the fix printed for
  your distro. 22.04 is out of scope: no `passt`, podman 3.4.
- **Linux x86_64 with KVM** — `/dev/kvm` present and user-accessible
  (VT-x/AMD-V, or nested virtualization when the host is itself a VM). This
  is the one hard requirement: every group boots as a Firecracker microVM.
  `run-host.sh` passes `--device /dev/kvm` through when present; without it
  the daemon runs but group boots fail a clear preflight (`fcPreflight`,
  `fc.go`).
- **Rootless podman** with pasta networking (the default on Fedora and on
  Ubuntu 24.04; slirp4netns is not used) and working subuid/subgid ranges —
  the rootfs build runs under `podman unshare`. Podman hosts cs_host, the
  TUI, and every containerized build (Go, protoc, the guest kernel). On
  Ubuntu the id-mapping helpers are the separate `uidmap` package and pasta
  is `passt`; both are Recommends of podman, so a default `apt install` has
  them and a `--no-install-recommends` one does not. Rootless podman does not
  work at all without `uidmap`, so `koto setup` probes for it directly.
- **Unprivileged user namespaces — nested.** Rootless podman puts cs_host
  in a userns; the VMM jailer (`fcjail.go`) then clones a *second* userns
  from inside that container. So the kernel must allow not just
  unprivileged userns creation but creation from within an existing one:
  `user.max_user_namespaces` > 0 and no seccomp/LSM policy blocking
  `clone(CLONE_NEWUSER)` inside containers. Fedora's defaults satisfy
  both; hardening like Ubuntu 24.04's
  `kernel.apparmor_restrict_unprivileged_userns` is the kind of setting
  that breaks it.
- **e2fsprogs** (`mkfs.ext4`) for the golden-rootfs build. Workspace image
  creation and growth at runtime use the copy baked into the cs_host image.
- **Baseline CLI tools**: `make`, `curl`, `tar`, `git`. (`openssl` and `jq`
  are needed only by the legacy `make pki-*` targets and `make metrics` —
  `koto setup` mints the PKI itself with Go's stdlib.)
- **Build-time network and resources**: `make fc-assets` downloads the
  pinned Firecracker release, clones the Amazon Linux kernel tree, and
  compiles the guest kernel inside an Ubuntu container (a few GiB of disk
  under `.kernelcache/`, minutes of CPU). At runtime each group reserves
  1–8 GiB RAM and an 8–24 GiB workspace image per its `size` preset.

Deliberate **non**-requirements: no root (all rootless), no `vhost_vsock`
module (Firecracker's hybrid vsock is unix-socket-backed), no host
`/dev/net/tun` (the `network` gateway is userspace gVisor inside
cs_host; `CONFIG_TUN` is a *guest* kernel option), no Go/Node/protoc
toolchain on the host (all builds are containerized), and no SELinux
tuning (`--security-opt label=disable` is set on every podman run).

### How it compares

| | koto | NanoClaw | OpenClaw | herdr | Claude Code |
|---|---|---|---|---|---|
| Agent boundary | Firecracker microVM on **KVM** (own kernel) | Docker container (shared kernel) | host process; sandboxing optional | host process (owns terminals) | host process; optional sandbox |
| Network | **none by default**; per-group `wan`/`lan`/`full`, frame-filtered | container network | host network | host network | host network |
| Credentials | proxy-injected, never in the guest | proxy-injected (OneCLI vault) | `.env` on host | host | host keychain |
| Containers inside the agent | rootless podman on the guest's own kernel; a container escape is still inside the VM | not by default (would need the host's docker socket in the container) | host docker, full authority | host docker, full authority | host docker, full authority |
| Multi-agent | `main` orchestrates peers over a verb control plane; no shared FS | agent groups per channel | limited | agents spawn panes, prompt each other via socket API | subagents / agent teams in-process |
| Interface | TUI, `koto ctl`, Android (one gRPC proto) | WhatsApp/Telegram/Slack/… | messaging channels, web UI, CLI, TUI | terminal multiplexer | terminal |
| Scheduling / jobs | cron, goals, background jobs with callbacks | recurring jobs | — | — | — |
| Providers | Anthropic (OAuth) | Anthropic (Agent SDK) | many, incl. local | any CLI agent | Anthropic |

koto is for running **untrusted, long-lived agents** where a compromised agent
must not reach your network, your credentials, or its siblings — the
messaging-channel breadth of NanoClaw/OpenClaw and the terminal ergonomics of
herdr are not its focus. If you want an assistant on WhatsApp, use those; if you
want a fleet of agents behind a hardware boundary, this is it.

## Data flow

**Inbound turn**: client → gRPC `Send` → attachments materialized into the
group's workspace (queue carries plain text only) → the SESSION's send queue
(one worker per conversation; up to `groupSlots`=10 turns run concurrently per
group) → one `claude -p` invocation in the guest → conversation state persists in the
guest's `workspace.img`.

**Outbound stream**: guest stream filter → `/workspace/.cs/log` → vsock 9001 →
daemon log tailer → parsed frames with per-group monotonic `seq` → in-memory
ring (1024/group) → `SubscribeGroup` streams (gapless resume via `since_seq`;
explicit `gap` event when the ring can't cover).

**Vsock port map** (single device, demuxed by port):

| dir | port | purpose |
|-----|------|---------|
| guest→host | 9000 | LLM API egress → per-group proxy port (credential injection) |
| guest→host | 9001 | group log stream (host file is the source of truth) |
| guest→host | 9002 | ctl plane (JSON lines) |
| guest→host | 9003 | L3 ethernet frames → gVisor gateway (`network` ≠ `none`) |
| guest→host | 9004 | per-slot turn streams (one per concurrent turn) |
| host→guest | 10000 | agent RPC (init / msg / exec / exec_stream / run_script / shell_attach / shutdown) |

## Config profiles (per group, `groups/<g>/.cs/config.json`)

| key | values | applies |
|-----|--------|---------|
| `model` | model id (default `claude-sonnet-5`) | next message |
| `network` | `none` (default) \| `wan` \| `lan` \| `full` | `/restart` |
| `size` | `small` (default) \| `medium` \| `large` \| `xlarge` | `/restart` |
| `root` | `no` (default) \| `yes` | `/restart` |
| `autostart` | `no` (default) \| `yes` — boot with the daemon | daemon start |
| `ports` | e.g. `[8080]` — vsock↔TCP bridge into `koto-net` | `/restart` |

## Install

```sh
# Fedora
sudo dnf install -y git make podman
# Ubuntu 24.04+ (passt and uidmap are Recommends of podman, so a default
# apt install already pulls them in; name them for --no-install-recommends)
sudo apt install -y git make podman passt uidmap

git clone <repo> && cd koto
make setup
```

That is the whole prerequisite list — no Go, Node, protoc or openssl toolchain
on the host, all of it is containerized. They are named explicitly because a
stock cloud image of either distro ships podman but not git or make.

**On Ubuntu, also make `/dev/kvm` world-accessible**, which Fedora does by
default and Ubuntu does not:

```sh
echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules
sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm
```

Adding yourself to the `kvm` group is *not* sufficient: the process that opens
the device is the jailed microVM monitor, which runs as an unprivileged id
with no supplementary groups, so it can only be reached by the world bits.
`koto setup` checks for this and prints the same commands, along with the
`kernel.apparmor_restrict_unprivileged_userns=0` sysctl Ubuntu 23.10+ needs
for the monitor's jail. Ubuntu 22.04 is not supported — it has no `passt` and
ships podman 3.4.

`make setup` builds the `koto` binary in a container (so the host needs only
git, make and podman) and runs the interactive wizard: it checks every host
requirement and tells you how to fix what's missing, builds the images and
the Firecracker guest kernel and rootfs, mints the private CA and the TUI's
client identity, connects your Anthropic credentials, installs koto as a
systemd service, and smoke-tests the result.

It is safe to re-run — every step detects whether it is already done, so an
interrupted install resumes where it stopped. `koto setup --check` reports
the health of an install without changing anything, and never modifies it.

**What it asks you**, all in the first couple of minutes:

- whether to continue if a host requirement is unmet (it prints the fix for
  each one — package names, sysctl lines — and refuses only on the ones that
  genuinely block);
- whether you will reach this daemon from another machine, so it can put
  extra names or addresses in the server certificate (you can reissue later,
  so "no" is a safe default);
- how to authenticate: a Claude subscription, which hands the terminal to
  `claude auth login` for a browser flow, or an Anthropic API key, typed
  with the echo off;
- confirmation before the guest kernel build, which is the long one.

**How long**: a few minutes of downloads and image builds, then the guest
kernel — 20-40 minutes on a cold machine, a few minutes if a source cache is
already there. It says which case you're in before starting, and it is safe
to walk away or interrupt.

**Sudo** is asked for only at the install step, near the end, for three
files: the `koto` binary, `/etc/koto/koto.env` and the systemd unit. Every
privileged command is printed before it runs. The service itself runs
rootless, as you.

**If a step fails**, the wizard says which one and stops rather than
continuing on a broken foundation. Fix the cause and run `make setup` again —
finished steps are skipped. To retry or force one step on its own, use
`koto setup --only <id>` (`koto setup --list` names them).

Once installed:

```sh
koto tui                    # attach the TUI (/exit detaches, daemon keeps running)
koto ctl list               # the agent-facing CLI
systemctl status koto       # the service
journalctl -u koto -f       # daemon logs
sudo systemctl restart koto # after editing /etc/koto/koto.env
```

State lives in `/var/lib/koto` (groups, credentials, guest assets — same
layout as a clone) and service configuration in `/etc/koto/koto.env`. The
clone is only a source checkout after this; `koto install` from a newer one
upgrades in place, preserving state and any config you have edited.

To add another client — a phone, a second laptop:

```sh
koto pki client -creds /var/lib/koto/creds <name>   # cert, key and token
```

If that client reaches the daemon over the network, the server certificate
needs a name or address it can verify. Adding one later is safe — the CA is
untouched, so every client you have already issued keeps working:

```sh
koto pki server -creds /var/lib/koto/creds \
  -san DNS:koto-daemon,DNS:localhost,IP:127.0.0.1,IP:192.168.1.20
sudo systemctl restart koto
```

Reaching it from off-box also means widening the bind: set `KOTO_BIND` in
`/etc/koto/koto.env` to an address other than `127.0.0.1` and restart. mTLS
and the bearer token are what gate access — but only issue certificates to
devices you control.

To uninstall: `sudo systemctl disable --now koto`, then remove
`/etc/systemd/system/koto.service`, `/etc/koto`, `/usr/local/bin/koto` and
`/var/lib/koto` (the last needs `podman unshare rm -rf /var/lib/koto/run`
first — those sockets are owned by subuids, not by you).

## Build & run (development)

Working on koto itself? Run it straight from the clone — no install, and
nothing containerized at runtime:

```sh
make koto          # build the daemon binary (compiles in a container, so no host Go)
make koto-tui      # build the TUI binary, likewise
make fc-assets     # firecracker binary + guest kernel + golden rootfs (required)
./koto pki init && ./koto pki client tui     # private CA + the TUI's identity
make login         # one-time subscription OAuth into ./creds — OR export
                   # ANTHROPIC_API_KEY before host-run
make host-run      # run the daemon in the foreground from this directory
make tui           # attach the TUI (/exit detaches; daemon keeps running)
make stop          # SIGTERM the dev daemon (stops its microVMs cleanly)
```

**Podman is a build-time dependency only.** It compiles the Go binaries (so
your host needs no Go toolchain) and builds the guest kernel and rootfs.
Nothing koto runs is a container: the daemon is a systemd service on the host,
the TUI is a plain binary, and each agent group is a Firecracker microVM.

A dev clone and an installed service can coexist — different state directories,
and the dev daemon binds whatever `KOTO_PORT`/`PROXY_PORT` you give it. The
daemon resolves all state from `KOTO_HOME`, falling back to the working
directory, which is what makes both modes the same code path.

The daemon puts itself in a user namespace at startup (`daemon/userns.go`)
using `newuidmap`/`newgidmap` against your `/etc/subuid` allocation. That is
what lets the jailer give each microVM monitor its own unprivileged uid — the
one thing the podman container used to supply. It needs no root; the helpers
carry `cap_setuid`/`cap_setgid`. `koto userns-check` verifies it in isolation.

Guest-side code (`sidecar/`, `fcguest/`) is baked into the rootfs: rebuild
with `make fc-rootfs` + `/restart <g>`.

## Credentials and Anthropic's terms

koto runs the **unmodified** `claude` binary and never hands a real credential
to a guest: the proxy injects one of two things on the host side
(`daemon/proxy.go` `authHeaders`):

1. **An API key** — `export ANTHROPIC_API_KEY=sk-ant-…` before `make host-run`.
   **Recommended.** Anthropic's guidance is that developers building agent
   systems on Claude Code / the Agent SDK use API-key authentication; `claude
   -p --bare` (what every group runs) is documented as an API-key mode; and
   API traffic falls under the Commercial Terms, so business use is fine.
   Billing is per token to the key owner.
2. **A Claude subscription login** — `make login` runs `claude auth login`
   (Anthropic's own flow) and the proxy forwards the resulting OAuth token,
   refreshing it via `claude` itself. **This works, but read the fine print
   before relying on it:**
   - OAuth is "intended exclusively for purchasers of … subscription plans
     and designed to support ordinary use of Claude Code"; advertised Pro/Max
     limits "assume ordinary, individual usage". A fleet of scheduled,
     autonomous agents is a stretch of "ordinary individual usage", and
     Anthropic "reserves the right to take measures to enforce these
     restrictions … without prior notice".
   - The Consumer Terms (Free/Pro/Max) allow **personal, non-commercial use
     only**. Commercial work belongs on an API key or a Team/Enterprise plan.
   - Anthropic prohibits developers from collecting, storing, or
     intermediating Claude.ai credentials **on behalf of other users**. koto
     is a single-operator tool: the credential is yours, the agents are
     yours. **Do not run koto as a shared or hosted service on a subscription
     login** — that is exactly the pattern the clause forbids.

Sources: [Claude Code legal & compliance](https://code.claude.com/docs/en/legal-and-compliance)
(authentication and credential use, running Claude Code in agent
infrastructure), [Consumer Terms](https://www.anthropic.com/legal/consumer-terms),
[Commercial Terms](https://www.anthropic.com/legal/commercial-terms),
[Usage Policy](https://www.anthropic.com/legal/aup). This is a summary, not
legal advice; the linked documents govern.

## AI disclosure

koto was built predominantly with AI models. Nearly all of the code, the
design docs, and this README were written by Claude (Anthropic's models, via
Claude Code) working from prompts, reviews, and corrections by a single human
maintainer. The human decided *what* to build and what the trust model must
guarantee, read and pushed back on the output, and ran it; the models wrote
most of the lines. Commit messages record the design rationale in the same
way — many were drafted by the model and edited by the maintainer.

What that means for you as a reader or user:

- **Review it like code from a fast, confident contractor you don't know.**
  The isolation claims (KVM boundary, no-NIC default, jailed VMM, credential
  proxy) were verified by running them, and the security-relevant parts have
  been audited more than once (`docs/history/` holds the dated audits), but
  no part of this repository has been reviewed line-by-line by a second
  human. Treat it as unaudited from a third-party standpoint.
- **Prose may overstate.** Model-written docs tend toward completeness and
  certainty; where the README or `CLAUDE.md` and the code disagree, the code
  is right and a doc fix is welcome.
- **Dependencies were still chosen deliberately.** The ≥6-week pin rule and
  the small dependency tree (see [SBOM](#sbom-supply-chain)) were human
  constraints, not model defaults.
- **Licensing.** AI-generated code has an unsettled copyright status in some
  jurisdictions; if that matters to you, factor it in.

If you contribute with AI assistance, that is fine — say so in the commit
message or PR so the provenance stays visible.

## Why Firecracker

The agent boundary had to be something an untrusted process cannot argue with.
Containers share the host kernel, so isolation rests on namespaces, cgroups and
seccomp, and a single kernel LPE collapses the whole stack. A microVM moves the
boundary to hardware virtualization: the guest runs its own kernel behind
KVM, and an escape means defeating the VMM's device model rather than the
Linux syscall surface.

Firecracker specifically, over a general-purpose VMM like QEMU, for the reason
that also makes it fast: it implements almost nothing. Its device model is
virtio-block, virtio-net and vsock, with no BIOS, no PCI enumeration, no USB,
no emulated graphics or audio — the parts of a full VMM where device-model
CVEs have historically lived. Less emulation is less to get wrong, and it
boots in ~125ms, which is what makes one VM per agent group practical rather
than a thought experiment.

That reasoning is not koto's invention — it is the mainstream advice for
isolating untrusted workloads:

> Zudem rät Dinaburg, für die Virtualisierung auf minimalistische Lösungen
> umzusteigen, die eine geringere Angriffsfläche bieten. Als Beispiel nennt er
> das von AWS entwickelte Firecracker.
>
> *"Dinaburg further advises moving virtualization to minimalist solutions that
> present a smaller attack surface. As an example he names Firecracker,
> developed by AWS."*
>
> — TODO: add source

The trade koto accepts for it is the absence of a shared filesystem: a group
gets no bind mounts, so every host↔guest channel is vsock, guest-side code
ships baked into the rootfs image (`make fc-rootfs`), and orchestration
between groups is verb-based rather than file-based. That constraint is what
also closed the egress hole — with no NIC by default, the credential-injecting
proxy is the only way out. See `docs/firecracker-vsock.md`.

## Further reading

- `docs/firecracker-vsock.md` — authoritative microVM runtime design
  (vsock IPC, jailer, egress gateway, size/root profiles, guest podman).
- `CLAUDE.md` — full operational detail, protocol reference, conventions.
- `git log --oneline` — every non-trivial fix is one commit; the messages
  are the design-rationale log.
