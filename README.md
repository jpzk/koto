# koto

Minimal isolated orchestrator for long-lived `claude` agents. Every group
(agent) runs in its own **Firecracker microVM** with **no network by default**;
a credential-injecting proxy is the only path to the LLM API, and agents never
see a real credential. Host side is a Go daemon; the UI is a separate,
network-isolated TUI container speaking gRPC over mTLS.

## Architecture

```
you ──▶ cs_tui (Go/BubbleTea, --network=none, sock-only)
             │ gRPC over mTLS + bearer token (:8443)
             ▼
        cs_host ─ daemon (Go) + credential-injecting proxy
             │        │
             │        └── creds/ (OAuth token / Venice key — proxy memory only)
             │
             │ per-group: jailed Firecracker VMM, vsock-only IPC
             ▼
   ┌─ microVM "main" ──┐  ┌─ microVM <g> ──┐   guest kernel behind KVM,
   │ claude -p loop    │  │ claude -p loop │   own /workspace (ext4 image),
   │ + rootless podman │  │ ...            │   no NIC unless network=wan/lan/full
   └───────────────────┘  └────────────────┘
```

- **Daemon** (`daemon/`; entry in `daemon.go`, VM runtime in `fc.go`, one
  topic per file — see CLAUDE.md → Layout): boots/supervises microVMs, serializes one
  `claude -p --continue` turn per inbound message, tails each group's log and
  fans events out to subscribers, runs the cron scheduler and the ctl verb
  plane.
- **Proxy** (`proxy.go`): one listener port per group (attribution), injects
  the real credential per request, records per-request token metrics. Guests
  authenticate with a sentinel (`ANTHROPIC_API_KEY=proxied`).
- **Guest** (`fcguest/`, `sidecar/`): a PID-1 agent bridges everything over a
  single vsock device; `entrypoint.sh` runs the message loop as uid 1000.
- **Orchestration is verb-based**: no shared filesystem anywhere. `main`
  drives peers via ctl verbs (`spawn`/`send`/`stop`/`list`/`sched_*`/
  `config_set`/`skill_write`/`tail`); non-main groups get only self-scheduling
  verbs, force-scoped to themselves.

## Data flow

**Inbound turn**: client → gRPC `Send` → attachments materialized into the
group's workspace (queue carries plain text only) → per-group send queue →
one `claude -p` invocation in the guest → conversation state persists in the
guest's `workspace.img`.

**Outbound stream**: guest stream filter → `/workspace/.cs/log` → vsock 9001 →
daemon log tailer → parsed frames with per-group monotonic `seq` → in-memory
ring (1024/group) → `SubscribeGroup` streams (gapless resume via `since_seq`;
explicit `gap` event when the ring can't cover).

**Vsock port map** (single device, demuxed by port; guest→host only):

| port | purpose |
|------|---------|
| 9000 | LLM API egress → per-group proxy port (credential injection) |
| 9001 | log stream (host file is the source of truth) |
| 9002 | ctl plane (JSON lines) |
| 9003 | L3 ethernet frames → gVisor gateway (`network` ≠ `none`) |

## Threat model

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
tier 2.5 cs_tui               sock-only gRPC relay; --network=none, scratch image
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
- **Credentials never enter a guest.** The proxy holds the real OAuth token /
  Venice key in process memory and injects per request; guests get a sentinel
  and a `ANTHROPIC_BASE_URL` pointing at vsock 9000. A compromised guest can
  *use* the proxy, not steal from it.
- **Egress is a per-group profile.** `network=none` (default): no NIC, no
  route, no DNS — the LLM leg via the proxy is the only egress, enforced by
  absence of hardware, not policy. `network=wan|lan|full`: a real L3 NIC via a
  userspace gVisor gateway over vsock 9003, egress-filtered at the frame layer
  by destination class — `wan` = public internet only, `lan` = host LAN only,
  `full` = both. The guest can never reach loopback/link-local/cs_host itself (the
  control plane stays unreachable); general HTTPS then bypasses proxy audit —
  that's the documented tradeoff.
- **No shared mutable filesystem.** Workspaces are per-group ext4 images;
  there is no `/peers`, no bind mounts into guests. Cross-group interaction
  exists only as authorized ctl verbs, checked against source-group identity
  daemon-side (`ctl.go`); non-main callers have targets force-overwritten to
  self.
- **Control-plane transport is mTLS + bearer** (private CA, client-cert
  fingerprint allowlist, per-RPC token — `auth.go`). No anonymous endpoint.
- **The UI is below the daemon in privilege.** `cs_tui` is a static Go binary
  on `scratch` with `--network=none` and a single socket mount: a compromised
  TUI dependency yields a command relay the daemon still authorizes, not
  workspace or network access.
- **No Docker-out-of-Docker.** cs_host holds no podman socket (removed with
  the whisper container, its last user); a tier-2 compromise cannot spawn
  containers or mount host paths. Podman exists only *inside* guests,
  rootless, behind KVM.

## SBOM (supply chain)

No generated SBOM artifact is checked in; the surface is small enough to
state outright. Everything Go-side is pinned via `go.sum` under a 6-week
dependency-lag rule (each pin's release date is verified against
proxy.golang.org before adoption — see CLAUDE.md → Conventions).

**Go modules** (direct deps; indirect counts approximate):

| module | direct deps | indirect |
|--------|-------------|----------|
| `koto` (daemon) | `containers/gvisor-tap-vsock` v0.8.8 (`network=wan/lan/full` gateway; pulls the gvisor netstack), `grpc` v1.80.0, `protobuf` v1.36.11, local `koto-protocol` | ~21 |
| `protocol/` (proto + generated pb) | `grpc` v1.80.0, `protobuf` v1.36.11 | 4 |
| `tui/` | charmbracelet `bubbletea` / `bubbles` / `glamour` / `lipgloss` / `log` + `muesli/termenv`, `grpc`, `protobuf` — one auditable upstream org for the whole UI stack | ~35 |
| `fcguest/` (guest PID-1 agent) | `golang.org/x/sys` only | 0 |

**Pinned non-Go components:**

| component | pin | where |
|-----------|-----|-------|
| Firecracker VMM | v1.11.0 (static musl, GitHub release) | `fcguest/fetch-assets.sh` |
| guest kernel | `amazonlinux/linux` tag `microvm-kernel-6.1.170-31.327.amzn2023`, verified against a pinned commit sha | `fcguest/build-kernel.sh` |
| protoc plugins | `protoc-gen-go` v1.36.11, `protoc-gen-go-grpc` v1.6.1 | `Makefile` |

**Container images:** cs_host = `golang:1.24-alpine` + nodejs/npm/
e2fsprogs/tar; TUI runtime = `scratch` (one static binary, no shell, no
ca-certs); guest rootfs = `fedora:44` + ~25 dnf packages (podman, crun,
conmon, fuse-overlayfs, passt, nodejs, python3, git, ripgrep, sudo, …);
build-only = `ubuntu:24.04` (kernel) and `golang:1.24-alpine` (protoc,
fc-agent).

**Known-floating** (what a formal SBOM would flag): `@anthropic-ai/
claude-code` is installed unpinned by npm in both cs_host and the guest
rootfs — it resolves to latest on every image build; the dnf packages and
the base-image tags (`fedora:44`, `golang:1.24-alpine`, `ubuntu:24.04`)
float within their tags. The Go trees are fully locked; the OS-package and
claude-code layers are the accepted moving parts.

## Config profiles (per group, `groups/<g>/.cs/config.json`)

| key | values | applies |
|-----|--------|---------|
| `provider` | `venice` (default) \| `claudesdk` | next message |
| `network` | `none` (default) \| `wan` \| `lan` \| `full` | `/restart` |
| `size` | `small` (default) \| `medium` \| `large` | `/restart` |
| `root` | `no` (default) \| `yes` | `/restart` |
| `ports` | e.g. `[8080]` — vsock↔TCP bridge into `koto-net` | `/restart` |

## Host requirements

Everything runs rootless as the host user; nothing is installed on or
configured into the host system itself. What the host must provide:

- **Linux x86_64 with KVM** — `/dev/kvm` present and user-accessible
  (VT-x/AMD-V, or nested virtualization when the host is itself a VM). This
  is the one hard requirement: every group boots as a Firecracker microVM.
  `run-host.sh` passes `--device /dev/kvm` through when present; without it
  the daemon runs but group boots fail a clear preflight (`fcPreflight`,
  `fc.go`).
- **Rootless podman** with pasta networking (the Fedora default;
  slirp4netns is not used) and working subuid/subgid ranges — the rootfs
  build runs under `podman unshare`. Podman hosts cs_host, the TUI, and
  every containerized build (Go, protoc, the guest kernel).
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
- **Baseline CLI tools**: `make`, `curl`, `tar`, `git`, plus `openssl` for
  the PKI targets and `jq` for `pki-client` / `metrics`.
- **Build-time network and resources**: `make fc-assets` downloads the
  pinned Firecracker release, clones the Amazon Linux kernel tree, and
  compiles the guest kernel inside an Ubuntu container (a few GiB of disk
  under `.kernelcache/`, minutes of CPU). At runtime each group reserves
  1–4 GiB RAM and an 8–16 GiB workspace image per its `size` preset.

Deliberate **non**-requirements: no root (all rootless), no `vhost_vsock`
module (Firecracker's hybrid vsock is unix-socket-backed), no host
`/dev/net/tun` (the `network` gateway is userspace gVisor inside
cs_host; `CONFIG_TUN` is a *guest* kernel option), no Go/Node/protoc
toolchain on the host (all builds are containerized), and no SELinux
tuning (`--security-opt label=disable` is set on every podman run).

## Build & run

```sh
make host-build   # cs_host image
make fc-assets    # firecracker binary + kernel + golden rootfs (required)
make login        # one-time OAuth into ./creds/
make host-run     # start daemon (+ main group)
make tui          # attach the TUI (Ctrl+C detaches; daemon keeps running)
make stop         # tear down
```

Guest-side code (`sidecar/`, `fcguest/`) is baked into the rootfs: rebuild
with `make fc-rootfs` + `/restart <g>`. Host-side Go is live (`go run` in
cs_host) — edit and `make host-run`.

## Further reading

- `docs/firecracker-vsock.md` — authoritative microVM runtime design
  (vsock IPC, jailer, egress gateway, size/root profiles, guest podman).
- `CLAUDE.md` — full operational detail, protocol reference, conventions.
- `git log --oneline` — every non-trivial fix is one commit; the messages
  are the design-rationale log.
