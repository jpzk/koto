# koto

**koto is a microVM-based agent sandbox harness which runs under Linux with KVM capability.**

Every claude-based group (agent) runs in its own **Firecracker microVM** — hardware-isolated by **KVM**,
so the guest kernel is the security boundary — with **no network by default**;
a credential-injecting proxy is the only path to the LLM API, and agents never
see a real credential. Host side is a Go daemon running as a rootless
systemd service; the TUI and `koto ctl` are plain host binaries speaking gRPC
over mTLS with the daemon. It's simply superior to OpenClaw, NanoClaw wrt security. 

## Quick start

Until the first release is published, build the artifacts from source. The
host needs Linux x86_64 with KVM (`/dev/kvm` readable by you), Fedora or
Ubuntu 24.04+, and git, make and podman or docker (just for building).  

```sh
# Fedora
sudo dnf install -y git make podman
# Ubuntu 24.04+
sudo apt install -y git make podman passt uidmap

git clone https://github.com/jpzk/koto && cd koto
make build      # 1. build koto, koto-tui, firecracker, the guest kernel + rootfs (20-40 min cold)
make install    # 2. install to /var/lib/koto and a systemd user-run unit (asks for sudo, prints each command)
make wizard     # 3. mint the TLS identities, connect your Anthropic credentials, start the daemon
koto tui        # attach the TUI: /new <name> spawns your first agent, /exit detaches
```

On Ubuntu, make `/dev/kvm` world-accessible first (see [Install](#install));
`koto setup` checks for it and prints the fix. Every stage is safe to re-run,
and `koto setup --check` reports the health of an install without changing
anything. The [Install](#install) section explains what each stage does and
the `make fetch` route that replaces `make build` once releases exist.

## Architecture

```
╔═ TIER 1 · host user (full authority) ═══════════════════════════════════════╗
║  gRPC clients: koto tui · koto ctl · Android — host processes, no container ║
║  /var/lib/koto/creds: OAuth token · PKI (ca, client-*, tokens) · acl.json   ║
╚═══════════════╤═══════════════════════════════════════════╤═════════════════╝
                │ gRPC :8443  (mTLS + bearer token)         │ read per request
                ▼                                           ▼ (proxy memory only)
╔═ TIER 2 · koto daemon — systemd service, rootless as the host user ═════════╗
║  own userns (newuidmap) · ProtectSystem=strict · ProtectHome=yes · CPUQuota ║
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
| `fcguest/` (guest PID-1 agent) | `golang.org/x/sys`, `protobuf` v1.36.11, local `koto-protocol` | 4 |

**Pinned non-Go components:**

| component | pin | where |
|-----------|-----|-------|
| Firecracker VMM | v1.16.1, **built from source** at a pinned commit inside upstream's own `fcuvm` build image (pinned by digest); `FC_PREBUILT=1` fetches the GitHub release instead and verifies it against a pinned SHA256 | `fcguest/build-firecracker.sh` |
| guest kernel | `amazonlinux/linux` tag `microvm-kernel-6.1.176-43.358.amzn2023`, verified against a pinned commit sha | `fcguest/build-kernel.sh` |
| protoc plugins | `protoc-gen-go` v1.36.11, `protoc-gen-go-grpc` v1.6.1 | `Makefile` |

**Container images:** nothing koto runs at runtime is a container — the
daemon, the TUI and `koto ctl` are one static host binary each. The only
image that ships is the guest rootfs: `fedora` (pinned by digest) + 27 dnf
packages (podman, crun, conmon, fuse-overlayfs, passt, nodejs, python3, git,
ripgrep, tmux, sudo, …) + claude-code. Build-only images: `golang` (the
daemon, TUI, fc-agent and protoc), the same `fedora` for the guest kernel,
`alpine` for the mkfs stage that writes the rootfs image, and Firecracker's
`fcuvm` for the VMM.

**Pinned**: every build container is pinned by DIGEST, not tag — the Go
toolchain, the alpine that writes the guest filesystem, the fedora that
both compiles the guest kernel and seeds the guest rootfs, and Firecracker's
own `fcuvm` build image. Source is pinned by commit: the guest
kernel (tag + verified commit) and Firecracker (tag + verified commit; the
prebuilt path verifies a SHA256 instead). The Go module trees are locked by
`go.sum`.

**Known-floating** (what a formal SBOM would still flag): `@anthropic-ai/
claude-code` is installed unpinned by npm into the guest rootfs — it resolves
to latest on every rootfs build — and the `dnf`/`apk` packages inside the
build and guest images float within their pinned base images. Those are the
accepted moving parts; everything above them is fixed.

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
   resulting OAuth token, refreshing it via `claude` itself. **This works, but read the fine print
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
- **Licensing.** The project is MIT-licensed by the maintainer. AI-generated
  code has an unsettled copyright status in some jurisdictions; if that
  matters to you, factor it in.

If you contribute with AI assistance, that is fine — say so in the commit
message or PR so the provenance stays visible.

