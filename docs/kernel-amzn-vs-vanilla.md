# Guest kernel: build from Amazon Linux source, not kernel.org vanilla

**Decision argument for the Firecracker microVM guest kernel.**

> **Status: IMPLEMENTED** (2026-07-08). `build-kernel.sh` now clones
> `github.com/amazonlinux/linux` at a pinned `microvm-kernel-*.amzn2023` tag,
> and `fc.go` boots with **no `acpi=off`**. Verified: fresh idle guest ~3% CPU
> (was ~100%), `LOC` timer incrementing, L3 + rootless podman working. This doc
> is retained as the rationale.

## TL;DR

> Build the guest `vmlinux` from the **Amazon Linux 2023 kernel tree**, not
> kernel.org vanilla.
>
> Firecracker's guest configs and prebuilt kernels are Amazon-Linux-tree, not
> vanilla. A vanilla kernel **cannot parse Firecracker's ACPI tables** and dies
> at boot; the `acpi=off` workaround gets it booting but leaves the guest with
> **no local APIC and no LAPIC timer**, so every idle microVM **busy-polls a
> full host CPU (~100%)**. An amzn-tree kernel boots *with* ACPI and idles at
> ~0% — the normal Firecracker behavior. There is no boot-arg fix for the
> vanilla path.

## Why we build a kernel at all

The microVM's L3 networking and in-guest rootless podman need `CONFIG_TUN`
(and `FUSE_FS`/`NF_TABLES`). Firecracker's downloadable CI `vmlinux` ships
`# CONFIG_TUN is not set`, and the only current prebuilt-with-TUN (Kata) is
locked in a ~1.4 GB tarball. So the kernel has to be built. The only question
is **from which source tree**.

## The evidence

### 1. Firecracker's kernels are Amazon-Linux-tree, not vanilla

- The guest config Firecracker ships and tests against is literally named and
  headed **`6.1.x…amzn2023`** (`resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config`).
- Firecracker's **prebuilt** `vmlinux-6.1.102` (from their S3) carries
  **`CONFIG_SYSGENID=y`** and **`CONFIG_UNIX_SCM=y`** — `SYSGENID` is an
  **out-of-tree Amazon driver that does not exist in kernel.org**. A vanilla
  build with the same config drops it (the symbol is unknown), proving the
  prebuilt was built from the amzn tree.

### 2. Vanilla + Firecracker's config does not boot

Building kernel.org **6.1.102** and **6.1.177** with Firecracker's exact config
(base + `ci.config`), across multiple toolchains (Ubuntu 22.04/gcc 11, Ubuntu
24.04/gcc 13, Alpine/gcc 13), **all fail identically** during ACPI table load:

```
ACPI Error: AE_BAD_PARAMETER, During Region initialization (20220331/tbxfload-52)
ACPI: Unable to load the System Description Tables
virtio_blk: probe of virtio0 failed with error -22
Kernel panic - not syncing: VFS: Unable to mount root fs on unknown-block(0,0)
```

Firecracker's **own** prebuilt 6.1.102 boots the *same* rootfs on the *same*
host with no ACPI error. Config, version, and compiler were all matched to
FC's — only the **source tree** differs. Conclusion: vanilla ACPICA chokes on
a Firecracker DSDT OperationRegion that the amzn tree handles.

### 3. The `acpi=off` workaround boots — but cripples idle

To ship *something*, we boot vanilla FC's **pre-ACPI** way: `acpi=off` plus
`CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES` (devices from the injected
`virtio_mmio.device=` cmdline, legacy 8259 interrupts). It reaches init. But:

```
[ ] No local APIC present
[ ] APIC: disable apic facility → switched to apic NOOP
[ ] APIC: Keep in PIC mode(8259)
```

With no ACPI (no MADT) **and** `# CONFIG_X86_MPPARSE is not set` (no MP-table
parsing), the kernel cannot enumerate the local APIC. So:

- **`LOC` (local-timer interrupts) stays 0** — there is **no LAPIC timer**,
  only the PIT (`IRQ0`, XT-PIC, ~90/s). Not an interrupt storm.
- Without a per-CPU clockevent, tickless idle (`NO_HZ_IDLE`) can't arm a
  wakeup, so the idle loop **busy-polls instead of halting**.
- Inside the guest it *looks* idle (`/proc/stat` idle jiffies climb, load
  `0.00`), but the vCPU never yields → **the host sees ~99.5% CPU per VM**.

Measured: a **fresh, idle guest** (no init, no workload, no daemon) sits at
**99.5% host CPU**. With N groups running, that's N pegged cores.

### 4. No boot-arg rescues the vanilla path

Tested, none reduce idle CPU (all stayed ~99–100%, `LOC` stayed 0):

| Boot arg(s)                 | Result |
|-----------------------------|--------|
| `idle=halt`                 | 99.6%  |
| `lapic`                     | 100%   |
| `lapic nohz=off`            | 99.8%  |
| `nohz=off highres=off`      | 99.5%  |

The LAPIC never comes up without ACPI/MP-table enumeration, and there's no
in-guest knob to synthesize it. (KVM host-side `halt_poll_ns` was not writable
to test, but the guest-side data — `LOC=0`, PIT-only, idle-jiffies climbing —
already explains the spin without invoking host halt-polling.)

## Why the Amazon Linux kernel fixes it

Firecracker microVMs are designed to idle at **~0% CPU** — that's a headline
property of the platform, and FC's own (amzn-tree) kernels deliver it. Building
from the **Amazon Linux 6.1 source** means:

- FC's **ACPI tables parse** → boot **without `acpi=off`**.
- The **local APIC and LAPIC timer** come up from the MADT → proper tickless
  idle → the vCPU **halts** when idle → **~0% host CPU per idle VM**.
- We keep our additions on top (`CONFIG_TUN`, `FUSE_FS`, `NF_TABLES`, `IKCONFIG`).
- We stop fighting a config that was never meant for the vanilla tree, and we
  can drop the `acpi=off` + `CMDLINE_DEVICES` workarounds entirely.

## Alternatives considered (and why they lose)

- **Vanilla + boot args** — doesn't work (section 4); dead end.
- **Vanilla + `CONFIG_X86_MPPARSE=y`** — the kernel could parse MP tables, but
  **Firecracker doesn't provide MP tables**, so there's still nothing to
  enumerate the LAPIC from.
- **Debug the vanilla ACPICA region-init failure** — open-ended kernel/ACPICA
  spelunking against a moving target; even if solved, we'd be maintaining a
  patch the amzn tree already carries.
- **Prebuilt FC CI kernel** — boots and idles fine, but **lacks `CONFIG_TUN`**;
  can't be reconfigured (it's a binary).
- **Kata prebuilt** (has TUN) — ~1.4 GB multi-arch tarball for one file; heavy
  and off-pinning-policy.

## Costs / tradeoffs of the amzn approach (honest)

- **Sourcing:** pull kernel source from Amazon Linux (SRPM / their kernel repo)
  instead of `cdn.kernel.org`. Slightly more involved to pin than a kernel.org
  tarball + sha256, but reproducible from a tagged amzn release.
- **Provenance:** we track Amazon's 6.1 tree (patches + their config) rather
  than pure upstream. This is exactly what Firecracker itself does, so it's the
  *supported* path, not an exotic one.
- **6-week dependency-lag rule:** pin a specific amzn kernel release ≥6 weeks
  old, same as any other pinned dependency.

## Recommendation

Switch `fcguest/build-kernel.sh` to build from the **Amazon Linux 2023 6.1
kernel source**, keep the `+CONFIG_TUN +FUSE_FS +NF_TABLES` overlay, and
**remove `acpi=off` + `CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES`** once verified.
Acceptance test: a fresh idle guest sits at **< a few %** host CPU (not ~100%),
and `/proc/interrupts` shows `LOC` (LAPIC timer) incrementing.

---

*Investigation artifacts: reproduced on this host with Firecracker v1.11.0,
kernel 6.1.102/6.1.177, on 2026-07-08. Idle-CPU spin reproduced on a fresh
`acpi=off` guest at 99.5%; FC's prebuilt 6.1.102 boots the identical rootfs
with ACPI and no spin.*
