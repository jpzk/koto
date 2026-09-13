# koto

**koto is a microVM-based and hardware-isolated agent sandbox harness.** It runs under Linux with KVM capability and CPU with hardware virtualization (Intel VT or AMD-V). koto uses the well-established and reputable Firecracker VM solution developed and used by AWS. Currently it's LLM backend is Anthropic's Claude CLI. 

Every group/sandbox has 1 linux kernel, 1 overlay workspace, 1 agent runtime and 1 shared terminal. The group can be configured in different sizes with different vCPUs, memory and storage. Other configurations include LLM model, network access, root sudo. The daemon speaks a gRPC protocol and can be commanded by `koto ctl` or `koto tui`. 

koto also has ACL with roles and permissions on system configuration and groups. koto is not a batteries included personal agent but an open source, self-host able secure foundation to compartmentalize different prototypes. Developer UX is so important and therefore `koto tui` was built, it's leveraging established paradigms and tries to break new ground. 

## Quick start 

### From signed builds

```sh
git clone https://github.com/jpzk/koto && cd koto
make fetch      # 1. fetch signed artifacts and verify checksum 
make install    # 2. install to /var/lib/koto and a systemd user-run unit (asks for sudo, prints each command)
make wizard     # 3. mint the TLS identities, connect your Anthropic credentials, start the daemon
koto tui        # attach the TUI: /new <name> spawns your first agent, /exit detaches
```


### From sources

It is encouraged to **run your own AI security review to verify** on the repository and to **build all the components from scratch**. If you feel like becoming a steward, we're looking for more release signers. Building and running verified on Fedora 44, Ubuntu 24.04 LTS, Ubuntu 26.04 LTS and Arch.

```sh
# Fedora 44+
sudo dnf install -y git make podman
# Ubuntu 24.04+
sudo apt install -y git make podman passt uidmap
# Arch (rolling)
sudo pacman -S --needed git make podman passt

git clone https://github.com/jpzk/koto && cd koto
make build      # 1. build koto, koto-tui, firecracker, the guest kernel + rootfs (20-40 min cold)
make install    # 2. install to /var/lib/koto and a systemd user-run unit (asks for sudo, prints each command)
make wizard     # 3. mint the TLS identities, connect your Anthropic credentials, start the daemon
koto tui        # attach the TUI: /new <name> spawns your first agent, /exit detaches
```



Every stage is safe to re-run, and `koto setup --check` reports the health of
an install without changing anything. `make fetch` replaces `make build` once
releases exist.

## Architecture

```
╔═ TIER 1 · host user (full authority) ═══════════════════════════════════════╗
║  gRPC clients: koto tui · koto ctl (host binaries)                          ║
║  /var/lib/koto/creds: OAuth token · API keys · PKI · tokens · acl.json      ║
╚═══════════════╤═══════════════════════════════════════════╤═════════════════╝
                │ gRPC :8443  (mTLS + bearer token)         │ read per request
                ▼                                           ▼ (proxy memory only)
╔═ TIER 2 · koto daemon — systemd service, rootless as the host user ═════════╗
║  own userns · ProtectSystem=strict · ProtectHome · DevicePolicy · CPUQuota  ║
║  ┌─ role ACL ─────────┐   ┌─ daemon (Go) ─────────────┐   ┌─ LLM proxy ───┐ ║
║  │ admin (built in)   │──▶│ lifecycle · session queues│   │ unix socket   │═╬═▶ LLM API
║  │ agent (seeded)     │   │ log tailer · replay ring  │   │ per group,    │ ║   (real credential)
║  │ verb × target      │   │ cron · goals · ctl verbs  │   │ injects key   │ ║
║  └────────────────────┘   └──────▲────────────┬───────┘   └───────▲───────┘ ║
║                                  │            │           ┌───────┼───────┐ ║
║                                  │            │           │ gVisor gateway│┄╬┄▶ internet / LAN
║                                  │            │           │ wan/lan/full  │ ║   (filtered NAT)
║                                  │            │           └───────▲───────┘ ║
╚══════════════════════════════════╪════════════╪═══════════════════╪═════════╝
                    vsock 9002 ctl │            │ vsock 10000       │ vsock 9000 (sentinel key)
           vsock 9004 turn streams │            ▼ agent RPC         ┆ vsock 9003 (L2 frames)
╔═ TIER 3 · one jailed Firecracker VMM per group ═════════════════════════════╗
║  fcjail: userns+mnt+pid+net+ipc+uts · per-VM chroot · unprivileged uid ·    ║
║          no_new_privs · seccomp · per-VM cgroup                             ║
║  ┌┄ KVM boundary · guest kernel ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┐  ║
║  ┆  microVM <g>: fc-agent (PID 1) runs each turn: claude -p, uid 1000    ┆  ║
║  ┆  /workspace = workspace.img · rootless podman · no NIC by default     ┆  ║
║  └┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┘  ║
╚═════════════════════════════════════════════════════════════════════════════╝
```

**Defense in depth.** No single boundary is trusted to hold on its own. An
agent runs behind the **KVM boundary** with its own guest kernel, no network
interface by default, and no credentials — the proxy injects them host-side,
per request. Should the guest escape into the Firecracker VMM, it lands in
**fcjail**: fresh user, mount, PID, network, IPC and UTS namespaces, an empty
per-VM chroot, a distinct unprivileged uid, `no_new_privs`, Firecracker's own
**seccomp** filter, and a per-VM cgroup that weights its CPU and bounds its
memory. The daemon around it holds no root either: it bootstraps its own
**user namespace** through `newuidmap`/`newgidmap` from `/etc/subuid`, and
runs inside a **systemd sandbox** — `ProtectSystem=strict`, `ProtectHome`,
`DevicePolicy=closed` with only `/dev/kvm` added, and seccomp-enforced
restrictions on address families, namespaces, realtime scheduling and
personality, plus a capability bounding set that leaves only what
`newuidmap` needs. Each layer assumes the one inside it has already failed.

## Guest kernel & Root FS

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

### Root FS

Every group also boots the same `fcassets/rootfs.img`: one **golden ext4
image attached read-only** as the root drive of every microVM. It is built by
`fcguest/build-rootfs.sh` from `fcguest/Dockerfile.rootfs`, and the image is
never run as a container — its filesystem is exported and written straight
into ext4 with `mkfs.ext4 -d` (populated at mkfs time; no loop mount, no root
on the host). The export/mkfs stage runs inside a digest-pinned `alpine`
container so the tar's root-owned entries keep their ownership; the image is
sized to its contents plus 20% and 256 MiB, 2 GiB at minimum.

What is in it:

| | |
|---|---|
| base | `fedora` 44, pinned by digest, 27 dnf packages with weak deps off |
| agent runtime | `nodejs` + `npm` + `@anthropic-ai/claude-code` 2.1.268 (pinned), `python3`, `git`, `ripgrep`, `jq`, `curl`, `tmux` |
| PID 1 | `/usr/local/bin/fc-agent`, the static Go guest agent selected by `init=` on the kernel command line; it runs every turn, bridges vsock, and mounts `/workspace` |
| worker scripts | `/sidecar/` — `cs-job`, `cs-notify`, `cs-subagent`, `venice_stream.js` (baked in: a microVM has no bind mounts) |
| user | `node`, uid 1000 — everything runs as it, because `claude --dangerously-skip-permissions` refuses root |
| in-guest containers | rootless `podman` 5 with `crun`, `conmon`, `fuse-overlayfs` storage and `passt`/pasta networking over `/dev/net/tun`; subuid range and `containers.conf` preconfigured, graphroot under `/workspace` |
| mount points | `/workspace` and `/skills` pre-created (the root drive is read-only, so the agent cannot create them at boot); `/etc/resolv.conf` is a symlink into tmpfs |

**Read-only and shared** is the point. Nothing per group lives on it: a
group's state is its own `workspace.img` (ext4, read-write, the guest's
`/workspace`, `$HOME` included). The `root=yes` profile does not write to the
golden image either — `fc-agent` mounts a persistent **overlayfs** on `/usr`,
`/etc`, `/var` and `/opt` with the upper layer on the workspace disk, so
`sudo dnf install` persists across `/restart` while every other VM keeps the
pristine image. Upper-layer entries shadow a later rootfs rebuild until the
overlay is reset.

**No live reload.** Editing anything under `sidecar/` or `fcguest/` means
`make rootfs` (~30 s, containerized) and a `/restart <g>` of each group you
want on the new image — the deliberate cost of having no shared filesystem
between host and guest.

## SBOM (supply chain)

Everything is pinned; `go.sum` locks the module trees, build containers are
pinned by digest, and source checkouts by commit. Go pins follow a 6-week
dependency-lag rule, with one exception: a pin that closes a *reachable*
govulncheck finding is taken at once (the Go toolchain included — a
supported series' newest patch is the standard library's security fix).
The `go.mod` ledgers name every pin taken under it. `make secrets-scan`
and the `release`-branch govulncheck action are the checks behind both.

**Go modules** (direct):

| module | dependencies | indirect |
|---|---|---|
| `daemon/` | `containers/gvisor-tap-vsock` v0.8.9 · `golang.org/x/sys` v0.47.0 · `grpc` v1.82.1 · `protobuf` v1.36.11 · `koto-protocol` (local) | 19 |
| `tui/` | charmbracelet `bubbletea` v1.3.10 · `bubbles` v1.0.0 · `glamour` v1.0.0 · `lipgloss` v1.1.1-pre (2025-04-04) · `log` v1.0.0 · `x/ansi` v0.11.7 · `x/vt` (2026-04-30) · `muesli/termenv` v0.16.0 · `grpc` v1.82.1 · `protobuf` v1.36.11 · `koto-protocol` (local) | 37 |
| `fcguest/` | `golang.org/x/sys` v0.47.0 · `protobuf` v1.36.11 · `koto-protocol` (local) | 4 |
| `protocol/` | `grpc` v1.82.1 · `protobuf` v1.36.11 | 4 |

All four modules build with Go **1.26.8** (`toolchain` line, and the golang
image below).

**Source and binaries:**

| component | pin |
|---|---|
| Firecracker | v1.17.0, built from source at `95f868c8e345b1cc8faccd1a3c910b4989dc3f58`; `FC_PREBUILT=1` fetches the release, sha256 `06094a11…de558` |
| guest kernel | `amazonlinux/linux` `microvm-kernel-6.1.186-50.374.amzn2023` at `8a40ca92bfa9b706b76287942c89b13884928cb0`, no patches |
| claude-code | `@anthropic-ai/claude-code` 2.1.268 (npm, in the rootfs) |
| protoc plugins | `protoc-gen-go` v1.36.11 · `protoc-gen-go-grpc` v1.6.1 |

**Container images** (all by digest; only the rootfs ships, the rest are build-only):

| image | used for | digest |
|---|---|---|
| `docker.io/library/fedora` (44) | guest rootfs base, guest kernel build | `sha256:be9d65e2…babbd19` |
| `docker.io/library/golang` (1.26.8-alpine) | daemon, TUI, fc-agent, protoc | `sha256:6e5de3f5…0a623be` |
| `docker.io/library/alpine` | mkfs stage of the rootfs build | `sha256:7c8cb692…5eb2e6` |
| `public.ecr.aws/firecracker/fcuvm` (v93) | Firecracker source build | `sha256:36d81dd6…9702720` |
| `ghcr.io/betterleaks/betterleaks` | pre-commit hook, `make secrets-scan` | `sha256:06d60954…725a633a` |
| `docker.io/trufflesecurity/trufflehog` | `make secrets-scan` | `sha256:aa821cf4…6a52577` |

**Floating:** the versions of the 27 dnf packages (and their dependencies)
installed on top of the pinned fedora image — resolved from the Fedora
repositories at rootfs build time, which is also why `rootfs.img` does not
reproduce bit-for-bit.

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
2. **A Claude subscription login** — `koto claude-login` (or `make login`)
   runs `claude auth login` (Anthropic's own flow) and the proxy forwards the
   resulting OAuth token, refreshing it via `claude` itself — so the installed
   daemon needs a `claude` it can exec: `koto install` records the one on your
   PATH as `KOTO_CLAUDE_BIN` in `/etc/koto/koto.env` and, for a claude under
   your home, binds its directories read-only through the unit's `ProtectHome`.
   `koto claude-login --status` verifies that path from the daemon's side.
   **This works, but read the fine print before relying on it:**
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

