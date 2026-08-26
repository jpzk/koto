# koto

Minimal isolated orchestrator for long-lived `claude` agents. Every group
(agent) runs in its own **Firecracker microVM** with **no network by default**;
a credential-injecting proxy is the only path to the LLM API, and agents never
see a real credential. Host side is a Go daemon; the UI is a separate,
network-isolated TUI container speaking gRPC over mTLS.

## Architecture

```
you ──▶ cs_tui (Go/BubbleTea, scratch image, on koto-net; creds ro)
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
  so the TUI can read the OAuth token, the Venice key and the private CA key;
  and the credential-injecting proxy does **not** listen on loopback —
  `host/Dockerfile` sets `BIND=0.0.0.0`, so every per-group proxy port is
  reachable from anything on `koto-net`, `cs_tui` included.
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
| `koto` (daemon) | `containers/gvisor-tap-vsock` v0.8.8 (`network=wan/lan/full` gateway; pulls the gvisor netstack), `golang.org/x/sys`, `grpc` v1.80.0, `protobuf` v1.36.11, local `koto-protocol` | 19 |
| `protocol/` (proto + generated pb) | `grpc` v1.80.0, `protobuf` v1.36.11 | 4 |
| `tui/` | charmbracelet `bubbletea` / `bubbles` / `glamour` / `lipgloss` / `log` / `x/ansi` / `x/vt` + `muesli/termenv`, `grpc`, `protobuf` — one auditable upstream org for the whole UI stack | 38 |
| `fcguest/` (guest PID-1 agent) | `golang.org/x/sys` only | 0 |

**Pinned non-Go components:**

| component | pin | where |
|-----------|-----|-------|
| Firecracker VMM | v1.16.1 (static musl, GitHub release) | `fcguest/fetch-assets.sh` |
| guest kernel | `amazonlinux/linux` tag `microvm-kernel-6.1.170-31.327.amzn2023`, verified against a pinned commit sha | `fcguest/build-kernel.sh` |
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

## Config profiles (per group, `groups/<g>/.cs/config.json`)

| key | values | applies |
|-----|--------|---------|
| `provider` | `claudesdk` (default) \| `venice` | next message |
| `model` | provider model id (default `claude-sonnet-5` / `kimi-k2.5`) | next message |
| `network` | `none` (default) \| `wan` \| `lan` \| `full` | `/restart` |
| `size` | `small` (default) \| `medium` \| `large` \| `xlarge` | `/restart` |
| `root` | `no` (default) \| `yes` | `/restart` |
| `autostart` | `no` (default) \| `yes` — boot with the daemon | daemon start |
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
  1–8 GiB RAM and an 8–24 GiB workspace image per its `size` preset.

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
make pki-init && make pki-client NAME=tui   # private CA + the TUI's client cert
make login        # one-time subscription OAuth into ./creds/ — OR export
                  # ANTHROPIC_API_KEY before host-run (recommended, see below)
make host-run     # start daemon (+ main group)
make tui-build    # TUI image (first time, and after editing tui/*.go)
make tui          # attach the TUI (/exit detaches; daemon keeps running;
                  # Ctrl+C interrupts the agent's turn)
make stop         # tear down
```

Guest-side code (`sidecar/`, `fcguest/`) is baked into the rootfs: rebuild
with `make fc-rootfs` + `/restart <g>`. Host-side Go is live (`go run` in
cs_host) — edit and `make host-run`.

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
legal advice; the linked documents govern. The Venice provider is unaffected
by any of this (`creds/venice.key`, Venice's own terms).

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

## Further reading

- `docs/firecracker-vsock.md` — authoritative microVM runtime design
  (vsock IPC, jailer, egress gateway, size/root profiles, guest podman).
- `CLAUDE.md` — full operational detail, protocol reference, conventions.
- `git log --oneline` — every non-trivial fix is one commit; the messages
  are the design-rationale log.
