---
name: release-test
description: Clean-machine release test — provision a throwaway VM (Fedora, Ubuntu or Arch Linux) and take koto through all three stages (build from source → install → wizard), asserting the invariants at each boundary. Use before tagging a release, or after changing the Makefile stages, koto install, or the setup wizard.
---

# Clean-machine release test

One part of the release test: proves a **fresh host with nothing on it** can go
from `git clone` to a running daemon. It is the only way to catch the class of
bug that only exists on a machine that has never run koto — which is where the
install bugs have historically come from (`8d1e33c`).

Do this in a throwaway VM, never on the dev host: the install stage writes
root-owned system state (`/var/lib/koto`, `/etc/koto/koto.env`, a systemd unit)
and the build stage is a ~40-minute kernel compile.

**Three distro targets, and they are not interchangeable.** The install bugs
this test exists to catch have all been distro-shaped — where `/dev/kvm`'s mode
comes from, how `newuidmap` carries its privilege, whether unprivileged user
namespaces are gated, whether `/etc/subuid` is populated at all. Fedora alone
proves almost nothing about the other two, which is exactly how a daemon that
could not start on Ubuntu shipped (see the Ubuntu variant below).

| target | status | section |
|---|---|---|
| Fedora 44 | verified, the default path below | §1 |
| Ubuntu 24.04 LTS | verified 2026-09-12 on 24.04.5 | §1 → Ubuntu variant |
| Arch Linux | verified 2026-09-12 (kernel 7.2.4, systemd 261.3) | §1 → Arch variant |

## The three stages under test

```
make build      acquire  — all five artifacts, from source
make install    integrate — state dir, binaries on PATH, koto.env, unit
make wizard     configure — PKI, credentials, first start, smoke
```

Each stage must refuse to do the previous one's work and name the command that
does. That refusal is part of what you are testing, not incidental.

## 1. Provision

Hand-rolled QEMU with user-mode networking. Do **not** use `assemble/fed.sh`
here: it needs a host bridge + tap created by a root script that also rewrites
nftables, which is too much to impose on the dev host for a test.

**Check host free space FIRST — this run needs ~20 G and failure is ugly:**

```sh
df -h ~          # need ~20 G free; the qcow2 grows to ~15 G during the build
```

A qcow2 is sparse, so a 30 G disk starts near zero and grows silently as the
build writes. If the host fills, the guest's writes start failing and it dies
in a way that looks like a network or hang bug — sshd stops answering, no OOM,
no panic, and you will diagnose everything except the disk. Check `df` before
blaming anything else when a guest goes unresponsive on a healthy host.

```sh
VMDIR=~/.cache/koto-vmtest          # NOT /tmp — that is tmpfs, a VM disk there eats RAM
mkdir -p $VMDIR && cd $VMDIR
curl -fL -o base.qcow2 "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2"
qemu-img create -f qcow2 -F qcow2 -b base.qcow2 disk.qcow2 30G
ssh-keygen -t ed25519 -N '' -f $VMDIR/id_vm

cat > user-data <<EOF
#cloud-config
hostname: kototest
users:
  - name: fedora
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: wheel,kvm
    shell: /bin/bash
    ssh_authorized_keys: [ $(cat $VMDIR/id_vm.pub) ]
ssh_pwauth: false
runcmd: [ [ systemctl, disable, --now, firewalld ] ]
EOF
printf 'instance-id: kototest\nlocal-hostname: kototest\n' > meta-data
cloud-localds seed.iso user-data meta-data

qemu-system-x86_64 -enable-kvm -cpu host -smp 4 -m 4096 \
  -drive file=$VMDIR/disk.qcow2,if=virtio,format=qcow2 \
  -drive file=$VMDIR/seed.iso,if=virtio,format=raw,readonly=on \
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:2222-:22 \
  -display none -serial file:$VMDIR/serial.log -pidfile $VMDIR/qemu.pid &
```

`-cpu host` plus nested virt on the outer host (`/sys/module/kvm_*/parameters/nested`
= `Y`) is what gives the guest a usable `/dev/kvm`. Without it the KVM preflight
degrades to the "continue without KVM?" path and you are no longer testing the
real thing. Check it in the guest before starting: `ls -l /dev/kvm` should be
mode `0666`.

SSH: `ssh -i $VMDIR/id_vm -p 2222 fedora@127.0.0.1`
(add `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR`).

### Ubuntu variant — verified 2026-09-12 on 24.04.5 LTS

Same QEMU invocation; three things change. Swap the image and the login user:

```sh
IMG_URL=https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
# cloud-init user: ubuntu (groups: sudo), not fedora (groups: wheel,kvm)
```

**Do not pre-fix `/dev/kvm` or the userns sysctl in cloud-init.** Both being
wrong on a stock Ubuntu image is what §4's two distro checks exist to catch;
fixing them at provision time silently deletes the test. Confirm the
preconditions before installing — a stock 24.04 gives exactly this:

```sh
ls -l /dev/kvm    # crw-rw---- root kvm  → 0660, NOT world-accessible
cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns    # 1 → restricted
```

`sudo apt install -y git make podman passt uidmap` (the README's line) is
**sufficient and complete** on a bare cloud image — verified, all five land on
`PATH`, podman 4.9.3 with pasta at `/usr/bin/pasta`. Nothing else is needed
for build or install.

Then §4's preflight fails **two** checks — `✗ /dev/kvm mode 0660` and
`✗ nested userns restricted by AppArmor` — and prints Ubuntu-specific
remediation for each. **Applying exactly what it prints works verbatim, with no
reboot** — re-verified 2026-09-12 against a VM reset to genuinely stock state;
`/dev/kvm` goes 0660 → 0666 via the udev rule and the sysctl 1 → 0, after which
preflight reads `✓ /dev/kvm usable (mode 0666)` and `✓ nested userns allowed`.

The two `!` lines (`claude not found`, `disk space`) are warnings, not failures,
and are not counted.

Ordering matters and will confuse you if you don't know it: a non-empty HARD
list returns *before* the "Continue without KVM?" prompt is reached. So on a
stock image the first `make install` exits 2 with no prompt, and only a second
run — after the userns sysctl is set — offers the KVM prompt. `/dev/kvm` is the
soft bucket; `nested userns` is the hard blocker.

The failure line reads `2 unmet requirement(s), all shown above — fix all of
them (1 blocking, plus /dev/kvm)`. **An older version of this skill said it
reads `1 unmet requirement(s)` and called that correct-not-a-bug.** It was
changed deliberately (see the comment in `preflightGate`, setup_checks.go):
counting only hard blockers printed `1` under two `✗` marks and two remediation
blocks, leaving it ambiguous which one to fix. Don't re-rationalize the old
behavior if you meet it in an old binary.

**Do not expect the ACL remediation.** Until 2026-09-12 the `/dev/kvm` hint led
with a per-uid POSIX ACL over the VMM band (audit M88). That was reversed after
measuring it here, and the udev rule leads now — the ACL survives only as a
documented alternative with its caveats. All three reasons are Ubuntu-specific
and all three are silent:

- `setfacl` is not installed on a stock cloud image (`acl` package absent).
- `/dev/kvm` is tagged `uaccess`, so **systemd-logind owns its ACL and rewrites
  it on session changes** — a manual grant disappears within minutes of an ssh
  login, and the next VM to boot dies with "Error creating KVM object:
  Permission denied … configured on the /dev/kvm file's ACL". The old text said
  "recreated at boot"; it is every login.
- `checkKVMAt` reads the mode bits, which an ACL never sets, so even a working
  grant reads as `✗` and blocks the install.

The `nested userns` hint was reversed the same day and for the same shape of
reason (audit L9): the AppArmor profile it led with genuinely works, but
`checkUserns` reads the sysctl and cannot observe a profile, so the preflight
kept refusing a correctly-configured host. The sysctl leads now.

**`make install` succeeding is NOT the same as the daemon running — assert both
on Ubuntu.** The distro difference that caused this is permanent: Fedora ships
`newuidmap` with file capabilities (`cap_setuid=ep`, mode 0755), Ubuntu ships it
**setuid-root** (mode 4755, no caps). Under a setuid-root helper the unit's
`CapabilityBoundingSet` is the helper's entire permitted set rather than a
filter over a small one, so M13's tight `CAP_SETUID CAP_SETGID` starved it: the
install completed, the unit was enabled, and the daemon crash-looped every 5s
with `newuidmap: open of uid_map failed: Permission denied` while no group could
ever boot. Fixed 2026-09-12 by rendering the set from the host, but check it
every run — this is exactly the class of breakage that hides behind a clean
install:

```sh
systemctl is-active koto          # must be `active`, not `activating`
journalctl -u koto -n 20 | grep -i newuidmap    # must be empty
ps -eo uid,args | grep firecracker              # VMM uid = subuid base + 29999
```

Note that last number: the per-VM jail uid is a **namespace** id and the uid_map
puts ns id 1 on the subuid base, so the host uid is `base + 30000 - 1`. On
Ubuntu's default base of 100000 that is **129999**, not 130000.

Clean build time on 4 vCPU / 8 G was **~24 min** (fcassets ~18, Go binaries
~6), not the ~40 min the Fedora note estimates.

### Arch variant — verified 2026-09-12 (kernel 7.2.4, systemd 261.3)

This section used to hold predictions derived from the code. It has been
rewritten with what actually happened, as it instructed. **Two of the four
predictions were wrong**, and they were wrong in the direction that matters:
Arch turned out to need LESS remediation than guessed, while breaking something
no prediction covered. Worth remembering the next time a section like this is
written from reading rather than running.

Same QEMU invocation; swap the image and the login user:

```sh
# Official cloud image. Verify it — upstream publishes .SHA256 and .sig.
IMG_URL=https://geo.mirror.pkgbuild.com/images/latest/Arch-Linux-x86_64-cloudimg.qcow2
curl -fsSL -o img.sha256 "$IMG_URL.SHA256" && sha256sum -c img.sha256
# cloud-init works; define the user explicitly rather than relying on the
# image default — `arch`, groups `wheel`. The base image is 533 MB / 2 GiB
# virtual, so the overlay must supply the room (30 G, as elsewhere).
```

**Dependencies — `rsync` is NOT in base, and §2 fails without it.** The source
transfer dies with `bash: line 1: rsync: command not found` before anything
koto-related runs. Nor are git, make, podman or passt:

```sh
sudo pacman -Syu --noconfirm --needed git make podman passt tmux rsync
```

**pacman needs a resume-capable transfer over QEMU's SLIRP, or it will not
complete.** The first attempt failed outright — `Recv failure: Connection reset
by peer`, `OpenSSL ... unexpected eof while reading`, `Operation too slow. Less
than 1 bytes/sec` — across a 306 MiB upgrade, and pacman has no retry of its
own. This is the same user-mode-NAT flow-state drop documented under Gotchas for
`podman pull`, not an Arch or mirror problem. Fix it before installing anything:

```sh
sudo sed -i "/^\[options\]/a XferCommand = /usr/bin/curl -L -C - -f -o %o %u --retry 20 --retry-delay 3 --retry-all-errors --connect-timeout 20 --speed-time 30 --speed-limit 1000" /etc/pacman.conf
```

With that in place the same transaction completed with zero errors.

**Reboot after the upgrade.** `-Syu` pulls systemd and the kernel on a rolling
distro (261.3 and 7.2.4 here). This test is partly ABOUT systemd directive
behaviour, so measuring against a systemd that is installed but not running
makes the result meaningless.

What the predictions got right and wrong:

- **`/etc/subuid` — WRONG.** Predicted unpopulated; it is populated,
  `arch:100000:65536`, same base as Ubuntu. So the VMM host uid is **129999**,
  not Fedora's 554287. No `usermod --add-subuids` needed.
- **`/dev/kvm` — WRONG.** Predicted 0660 from systemd's own default rule; it
  ships **0666**, like Fedora. No udev remediation needed, and the preflight
  passes it outright.
- **nested userns — right.** No AppArmor, `max_user_namespaces=15388`,
  `✓ nested userns allowed`.
- **`newuidmap` — file capabilities** (`cap_setuid=ep`, mode 0755), i.e.
  Fedora-shaped, not Ubuntu's setuid-root. `installCapBounding` therefore
  renders the tight `CapabilityBoundingSet=CAP_SETUID CAP_SETGID`. Third data
  point for that branch, and it confirms Ubuntu is the outlier.

Net: **Arch's preflight is all green out of the box**, like Fedora and unlike
Ubuntu. Nothing in §4 needs remediation.

**The thing no prediction covered, and the reason this run was worth doing:
`~/.local/bin` is not on Arch's default login PATH**, and the claude native
installer modifies no shell rc file. Arch's default is
`/usr/local/sbin:/usr/local/bin:/usr/bin:/usr/bin/site_perl:...` — no
`$HOME/.local/bin`, where Fedora supplies it from `/etc/profile.d` and Ubuntu's
`~/.profile` adds it when the directory exists. So a correctly installed claude
was invisible to koto, and `koto install` reported `! claude not found`,
recorded no `KOTO_CLAUDE_BIN` and rendered `ProtectHome=yes` with no bind —
breaking OAuth refresh on a machine where the operator had done everything
right. Fixed 2026-09-12 (claudeBinResolve falls back to the native installer's
location; install, preflight, login and the daemon probe now share one
resolver). **Assert it rather than assuming**, since this is the distro that
proves the fallback works:

```sh
grep -E "^ProtectHome|^BindReadOnlyPaths" /etc/systemd/system/koto.service
#   ProtectHome=tmpfs
#   BindReadOnlyPaths=/home/arch/.local/bin /home/arch/.local/share/claude/versions
sudo grep ^KOTO_CLAUDE_BIN /etc/koto/koto.env
koto claude-login --status      # token refresh via … (KOTO_CLAUDE_BIN …)
```

**clone3 is BLOCKED here** (`clone3 is blocked (seccomp — systemd
RestrictNamespaces=); placing VMs in their cgroup after fork instead`), on
systemd 261 — siding with Fedora against Ubuntu 24.04, which allowed it. So the
post-fork placement fallback is the common case across distros and clone-time
placement the exception; `cgroup=on` either way.

Timing on 4 vCPU / 4 G: pre-pull ~18 min (the 4.63 GB fcuvm image, at ~29
MB/min over SLIRP — the slow leg by far), `make build` 25m48s.

## 2. Ship the source — and nothing else

```sh
rsync -a -e "$SSH" \
  --filter=':- .gitignore' \
  --exclude '.git' --exclude 'creds' --exclude 'groups' --exclude 'run' --exclude 'dist' \
  --exclude 'fcassets' --exclude 'groups.json' --exclude 'goals.json' \
  --exclude 'schedules.json' --exclude 'metrics.jsonl' \
  ~/koto/ fedora@127.0.0.1:~/koto/
```

**The last four excludes are not tidiness, they are the whole point.**
`seedStateDir`'s `migrateState` MOVES `groups.json`/`goals.json` out of the
clone and into the state dir, and `resumeGoalDrivers` then re-drives any goal
still marked `running` at the next daemon start. Copy the dev host's copies in
and the test VM will boot microVMs for your real groups and start making real
API calls. Verified: it does exactly that.

Exclude `creds` too, obviously — the CA key, client tokens and any
`anthropic-api-key`/`.credentials.json` live there. Afterwards confirm the
blast radius is clean: the VM's `ca.crt` fingerprint must DIFFER from the dev
host's, and `~/koto/creds` must not exist in the guest.

Ship no binaries and no `fcassets/` — stage 1 has to build them, or it is not
being tested. **`--filter=':- .gitignore'` is what enforces that**, and it
replaced a hand-enumerated exclude list that had drifted: that list named
`/koto` and `/koto-tui` at the repo root but missed `tui/koto-tui`,
`koto-fcagent`, `fcguest/koto-fcagent`, `fcguest/fc-agent` and `.cargocache/`
(~70 M of prebuilt binaries and a Rust build cache). Every one of those is a
gitignored build output, so honouring `.gitignore` excludes them by
construction and keeps doing so as new outputs are added. Verify rather than
trust — the source-only transfer is ~285 files / ~2.7 M:

```sh
rsync -a --dry-run --out-format='%l %n' … | sort -rn | head
```

Anything multi-megabyte in that list is a build output that should not be going
over.

## 3. Stage 1 — build from source

```sh
sudo dnf install -y git make          # Fedora: cloud image has podman, not these
# Ubuntu: sudo apt install -y git make podman passt uidmap
# Arch:   sudo pacman -S --needed --noconfirm git make curl tar e2fsprogs shadow podman passt
cd ~/koto && nohup setsid make build > ~/build.log 2>&1 < /dev/null &
```

~40 min cold. Assert on completion:

- all five artifacts exist: `koto`, `koto-tui`, `fcassets/{firecracker,vmlinux,rootfs.img}`
- the log ends with `built: …` then `next:  make install`
- re-running `make build` is a **no-op** (targets are keyed on real outputs, not
  `.build/` markers — a second run that rebuilds anything is a regression)

## 4. Stage 2 — install

**Install claude FIRST — the realistic state is that it is already there.**
Anyone installing koto is already a Claude Code user, so on a real host claude
predates koto rather than following it, and `koto install` resolves it once
from the installing shell's PATH and renders the unit's filesystem namespace
around what it finds. Install it afterwards and you get a materially different
unit that nothing will correct on its own. Do this before the command below
(details and assertions in §6):

```sh
curl -fsSL https://claude.ai/install.sh | bash     # → ~/.local/bin/claude
export PATH="$HOME/.local/bin:$PATH"
```

Testing the absent-claude path is also worth doing — it is a real first-run
state and the preflight warns `! claude not found` rather than failing, since
API-key auth never execs it. But it is the SECOND case to cover, not the
default, and if you only run one, run this one.

```sh
cd ~/koto && make install < /dev/null
```

Assert:

- preflight is all green, `/dev/kvm usable (mode 0666)` among it
- every privileged action is echoed as a discrete `sudo` line before it runs
- it ends `service enabled (not started — koto setup starts it)` and `next:  make wizard`
- with claude already installed under `$HOME`, the unit renders
  `ProtectHome=tmpfs` plus `BindReadOnlyPaths=` of BOTH the PATH entry's
  directory and the symlink target's directory, and `koto.env` records
  `KOTO_CLAUDE_BIN`. Plain `ProtectHome=yes` here means claude was NOT found
  at install time (or lives outside `$HOME`) — check which before continuing,
  because the rest of the run will then be exercising the weaker setup
- **`systemctl is-enabled koto` = `enabled` and `is-active` = `inactive`** —
  enabled-but-stopped is the designed state, because the daemon cannot come up
  before the wizard mints `server.crt`
- **`/var/lib/koto/creds/` is EMPTY.** Install must mint no PKI. If a CA appears
  here, the install-time minting has come back and it will silently swallow the
  wizard's SAN answer (`pkiInit` never regenerates an existing cert).

Also worth asserting: with `make` and `git` absent, install still succeeds. The
build-time tool checks are advisory precisely so the `make fetch` route works on
a host with no toolchain.

## 5. Stage 3 — wizard

Needs a real terminal (the secret prompt reads the tty), so `ssh -tt`:

```sh
printf 'y\nDNS:koto.test,IP:10.0.2.15\n2\nsk-ant-PLACEHOLDER-not-a-real-key\n' \
  | ssh -tt … 'cd ~/koto && make wizard'
```

Answers in order: extra SANs? → `y`; the SANs; auth method → `2` (API key);
the key. **Use a placeholder key, never a real credential** — the daemon only
needs it to exist for this test; nothing here exercises a real LLM call.

Assert:

- six steps, `installed → pki → auth → service → smoke → done`
- **the SANs you typed are in the certificate** — this is the regression guard:
  `openssl x509 -in /var/lib/koto/creds/server.crt -noout -ext subjectAltName`
  must contain `DNS:koto.test` and `IP Address:10.0.2.15`
- smoke reports `daemon answered`
- `koto setup --check` exits 0 with every step ✓

Then re-run `make wizard`: every step must detect as already done and skip.
Resumability is by detection, not a state file, so this is cheap to verify and
the thing most likely to rot.

## 6. Live LLM leg — the part that proves it actually works

Everything above ends at "the daemon answers its own API". This leg proves a
group can reach Claude **over the real Anthropic endpoint** — no mock, no
stub, no replay. `upstream` is a hardcoded const in `proxy.go:22`
(`https://api.anthropic.com`) with no env override, so there is nothing that
could silently redirect this leg to a fake; that is a property of the code, not
of the test setup.

It is deliberately last, because it is the only part that is **not hermetic**:
it needs `api.anthropic.com` reachable, a valid credential, and account quota.
A failure here is therefore not automatically a koto failure — check the three
of those before opening a bug.

### Getting a working credential into the VM — verified flow

The wizard's OAuth branch shells out to `claude auth login`. On a headless VM
that CLI falls back to printing an authorization URL and waiting for a
paste-back code, so it is drivable with a human in the loop. Hold the wizard in
a tmux session so you can read it and type into it across steps:

**On Ubuntu, install node 22 first — `apt install nodejs` gives v18.**
claude-code declares `"node": ">=22.0.0"`, but npm installs it anyway with
only an `EBADENGINE` warning and `claude --version` works, so nothing looks
wrong until the proxy shells out to refresh a token. Verified trap on 24.04:

**Install claude BEFORE `make install`, using the NATIVE installer, because
that is the configuration real operators run.** `koto install` resolves claude
once, at install time, and renders the unit's filesystem namespace from what it
finds — so installing claude afterwards produces a *different and weaker* setup
that no reinstall happens automatically to correct.

The native installer puts claude under `$HOME` (`~/.local/bin/claude`, pointing
into `~/.local/share/claude/versions/<v>`), which `ProtectHome=yes` hides. That
is the case `installHomeScoping` exists for, and the one to test:

```sh
curl -fsSL https://claude.ai/install.sh | bash     # → ~/.local/bin/claude
export PATH="$HOME/.local/bin:$PATH"               # koto install reads THIS shell's PATH
# ...then run `make install`, and assert the unit picked it up:
grep -E "^ProtectHome|^BindReadOnlyPaths" /etc/systemd/system/koto.service
#   ProtectHome=tmpfs
#   BindReadOnlyPaths=/home/<u>/.local/bin /home/<u>/.local/share/claude/versions
koto claude-login --status        # must say: token refresh via … (KOTO_CLAUDE_BIN …)
```

Both directories must be bound — the PATH entry's dir *and* the symlink
target's dir — and they must be DIRECTORIES, so a native-installer update that
writes a new `versions/<v>` and re-points the link keeps working without a
reinstall. Verified as the production configuration on the dev host 2026-09-12.

**The npm -g variant (`sudo npm i -g @anthropic-ai/claude-code`) is the
EXCEPTION, not the default.** It lands claude in `/usr/local/bin`, which
`ProtectHome` does not hide, so the unit stays on plain `ProtectHome=yes` and
the whole bind path goes untested. Both 2026-09-12 Ubuntu runs used it and
therefore did **not** exercise the normal configuration — don't repeat that. If
you do test this variant, note that installing it *after* `make install`
records no `KOTO_CLAUDE_BIN` at all and the daemon falls back to resolving a
bare `claude` against the unit's PATH; that happens to work from
`/usr/local/bin` and would not from anywhere else.

Two traps that are not about location:

- **`apt install nodejs` gives v18.** claude-code declares `"node": ">=22.0.0"`
  but npm installs anyway with only an `EBADENGINE` warning and
  `claude --version` works, so nothing looks wrong until the proxy shells out
  to refresh a token. Get node 22 first (tarball from nodejs.org into
  `/usr/local`, or `snap install node --classic --channel=22`, or nvm).
- **Do not use `curl … | sudo -E bash -` for the NodeSource repo**, which an
  older version of this skill printed. koto's own preflight deliberately
  refuses to print that one-liner, on the grounds that it makes a third
  party's HTTPS content a root shell; a release test instructing the opposite
  of the thing under test is worse than no instruction.

On Fedora (and Arch, which also ships a current node):

```sh
sudo dnf install -y nodejs npm tmux          # Fedora; Arch: sudo pacman -S nodejs npm tmux
sudo npm i -g @anthropic-ai/claude-code      # preflight's `! claude not found`
# npm -g lands claude in /usr/local/bin, on the unit's PATH. A claude from the
# NATIVE installer (~/.local/bin) is what `koto install` must record as
# KOTO_CLAUDE_BIN and bind through ProtectHome — worth one leg of the test:
# `koto claude-login --status` must print `✓ token refresh via …` either way.
rm -f /var/lib/koto/creds/anthropic-api-key  # or it shadows OAuth, see below
tmux new-session -d -s auth -x 200 -y 50 "cd ~/koto && koto setup --only auth; sleep 3600"
tmux capture-pane -t auth -p            # read the prompt
tmux send-keys -t auth "1" Enter        # 1 = Claude subscription (OAuth)
tmux capture-pane -t auth -p -J | grep -o "https://claude.com/cai/oauth/authorize[^ ]*"
```

`-J` joins wrapped lines — without it the URL comes back broken across the
pane width and is useless. Hand that URL to the operator, then feed back what
they get:

```sh
tmux send-keys -t auth "<code>#<state>" Enter
tmux capture-pane -t auth -p -J | tail -3      # expect: Login successful. / ✓ auth done
```

**The API key shadows OAuth completely** — `authHeaders()` short-circuits on a
non-empty key and never consults the OAuth credential. That much is by design.
What was NOT: the key used to be read once by `proxyInitPaths()` **at daemon
startup**, including from `<state>/creds/anthropic-api-key`, so section 5's
placeholder stayed cached in the running process after you `rm`'d the file.
Verified 2026-09-03: every request kept going out as
`x-api-key: sk-ant-PLACEHOLDER-not-a-real-key` and 401'd, with a healthy OAuth
token sitting unread on disk.

**FIXED**: `currentAPIKey()` now reads the file per request, the same way
`readCreds()` already re-read OAuth, so adding or removing the key takes effect
with no restart (verified in both directions). On a binary predating that fix
the order is: delete the key file, **then `sudo systemctl restart koto`**. Note
`koto setup --check` reads the disk, not the daemon's cached value, so on an
old binary it reports `OAuth credentials present` while the daemon is still
sending the stale key — it cannot be used to detect this.

**The 401 it produced also lied about the cause** — `credential expired; run
`make login`` while the credential had 7.9 h left, `user:inference` in scopes
and `subscriptionType: max`, so re-running the login could not have helped.
That message now names the credential actually sent (`upstream rejected the
API key …`). On an older binary treat "expired" as unreliable and check
`expiresAt` yourself.

### Assert a real completion

`defaultProvider` is `claudesdk` (`groups.go:269`), so a plainly-spawned group
goes to Anthropic with no extra config:

```sh
koto ctl spawn e2e
sleep 20                                     # let the guest agent come up
koto ctl ask -timeout 120s e2e 'hi' >/dev/null   # warm-up turn, see below
koto ctl ask -timeout 120s e2e 'Reply with exactly one word: pong'
echo "exit=${PIPESTATUS[0]}"                 # NOT $? — that is the pipe's tail
koto ctl destroy e2e
```

Assert the reply contains `pong`. A warm turn answers in ~2.5s.

**The first `ask` on a freshly spawned group can hang until its timeout even
though the turn completed.** Observed: the reply was in the log
(`[[turn_end]]` and all) and `koto ctl history` showed the `done` event with
the right text, while the live `ask` sat there and died with
`DeadlineExceeded … RST_STREAM CANCEL`. The group's `.cs/log` rotates to
`log.0` right around first turn, and the live subscriber attaching at that
moment misses the events. Do a throwaway warm-up turn, or retry once, before
treating a hang as a failure — and check `koto ctl history` before believing
`ask`.

Then prove the request really left for Anthropic. `llmFlowLog` logs the actual
upstream URL it is about to call, so this is evidence rather than assumption:

```sh
journalctl -u koto | grep "llm:.*flow POST api.anthropic.com/v1/messages"
```

There must be no `no credentials` line, and no `retry N/M ... upstream 401`.
A 401 here means the credential, not the endpoint — see the precedence trap
above.

## 7. TUI leg — a group created through the real UI

`koto ctl` above proves the daemon and the LLM path. This proves the TUI, which
is what a user actually touches. `koto tui` is a plain host binary now — it
reads the four cert paths the launcher hands it (`tui_cmd.go:60`), so there is
no container, no mount, and no `--detach-keys` chord to work around.

Drive it under **tmux** rather than a hand-rolled pty — tmux owns the window
size, so the `TIOCSWINSZ` dance below becomes unnecessary, and `capture-pane`
gives you a rendered frame instead of a raw escape stream you have to model
yourself:

```sh
tmux new-session -d -s tui -x 140 -y 40 "koto tui"
sleep 12 && tmux capture-pane -t tui -p      # first frame: banner + group tree
tmux send-keys -t tui "/new e2etui" Enter
sleep 25 && tmux capture-pane -t tui -p
koto ctl list                                 # the daemon must agree it exists
```

If you do hand-roll a pty instead, **`TIOCSWINSZ` is mandatory** — without it
the TUI exits immediately with "terminal too small" and it reads as a crash:

```python
m, s = pty.openpty()
fcntl.ioctl(s, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 140, 0, 0))
p = subprocess.Popen(["koto", "tui"], stdin=s, stdout=s, stderr=s,
                     env={**os.environ, "TERM": "xterm-256color"})
```

`/new <group>` creates a group, `/sw <group>` switches to one (`palette.go:94`).

Render the capture through a minimal ANSI screen model — apply `ESC[r;cH`,
`ESC[K`, `ESC[2J`, drop SGR — then assert against the final 140x40 dump. **Count
occurrences, do not just grep**: a duplicated render is a real bug class here
and a bare `grep -q` sails straight past it.

Then confirm the group the TUI made is real, from outside the TUI:
`koto ctl list` must show `e2etui`. Clean up with `koto ctl destroy e2etui`.

### Frame integrity: no end-to-end gate — say so, do not imply otherwise

**`make tui-walk` and `tools/tuiwalk/` were REMOVED on 2026-09-06.** Do not
look for them; do not report the wrap/scroll class as checked. The tool drove
the TUI as a podman container on `koto-net`, so it died with the container
runtime and had been dead code behind a deliberately failing target — while
still carrying an unpinned `grpcurl:latest` with all of `creds/` mounted and
the admin token on argv (audit L10). It was deleted rather than ported.

What covers frame integrity now, and what to claim in a release note:

- **The Go tests, per component** — `wrap_test.go`, `width_test.go`,
  `treewidth_test.go`, `responsive_test.go`, `mono_test.go`, `fuzz_test.go`.
  `go test ./...` in `tui/` is a real gate and can be reported as one.
- **The tmux route above** — the real TUI, real frames, asserted against a
  140x40 dump. Report exactly what it asserted.
- **Nothing covers composition**: a frame every Go width table calls exact
  that the TERMINAL still wraps. That is a live class — 2026-08-29, a raw TAB
  (width 0 by every table, advances to the next tab stop) wrapped a row,
  made the frame one line too tall and scrolled the screen; fixed by
  `expandTabs`, commit `5f68d03`.

If a "rows get added / layout breaks" report arrives, the technique that found
it is the one worth repeating rather than a target to run: drive the
`koto-tui` binary in a pty, feed the bytes to a `pyte.Screen` subclass, and
count `linefeed()` from inside `draw()` (auto-wrap) and `index()` at the
bottom margin (scroll), paging history at several sizes. Two pyte quirks: fold
`\x1b\x1b` to `\x1b` before feeding (it renders `ESC ESC [0m` as literal
text where xterm/kitty restart the escape), and either answer OSC 11 / DSR
queries or ignore them entirely — a late reply lands in the message bar as
typed text. Walk read-only, on a scratch group.

### Synthesizing turns without spending tokens

**Never use this for the LLM leg.** Section 6 must hit the real endpoint or it
proves nothing; this is only for asserting how the TUI *renders* a turn, where
a real completion would be a slow, non-deterministic way to produce a frame.

Subscribing to a group starts a log tail at `groups/<g>/.cs/log`; append framed
lines to synthesize a turn:

```sh
printf '[ts:1751900000123]\n>>> the prompt\nthe reply\n[[turn_end]]\n' >> groups/<g>/.cs/log
```

That yields `prompt`/`done`/`turn_end` events with fresh seq numbers. **Never
inject into a real group's log** — it lands in persisted history. Use a scratch
group when list membership matters.

## Gotchas

- **The kernel clone fails transiently.** `fatal: expected flush after ref
  listing` from `git clone amazonlinux/linux` is a transport flake, not a koto
  bug — it cloned fine on immediate retry. Retry once before investigating.

- **A stalled image pull looks exactly like a slow build, and never times
  out.** Observed: `podman pull` of the fcuvm image hung for 33 minutes with
  five ESTABLISHED TLS sockets to ECR, all `Recv-Q`/`Send-Q` at 0, podman
  parked in `futex_wait_queue` with no child processes and **no container in
  `podman ps`** — while a fresh `curl` to the same registry answered in 0.4 s.
  QEMU's SLIRP user-mode NAT drops the flow state on a long transfer; the
  guest still sees ESTABLISHED, the peer's data never arrives, no RST is ever
  generated, and podman's registry client has no read deadline. Not a koto
  bug. Pre-pull every pinned image with a hard per-attempt deadline before
  running `make build`, so a stall is killed and retried instead of hanging:

  **Use a PROGRESS-based watchdog, not a fixed deadline.** This snippet used
  to wrap each attempt in `timeout --signal=KILL 420`. Do not go back to that:
  the fcuvm image is multi-GB and SLIRP moves ~70 MB/min, so a perfectly
  healthy pull legitimately runs well past seven minutes and the deadline
  simply killed it, over and over, looking exactly like the stall it was meant
  to catch (burned ~40 min on 2026-09-12 before the loop was identified as the
  culprit rather than the network). Elapsed time cannot separate "stalled" from
  "slow"; bytes landing on disk can.

  ```sh
  for img in $(grep -rohE "(public\.ecr\.aws|docker\.io)/[a-zA-Z0-9._/-]+@sha256:[0-9a-f]{64}" Makefile */*.sh); do
    for a in 1 2 3; do
      podman pull -q "$img" & pid=$!
      last=$(df --output=used / | tail -1); quiet=0
      while kill -0 $pid 2>/dev/null; do
        sleep 30
        now=$(df --output=used / | tail -1)
        if [ "$now" -gt "$last" ]; then quiet=0; else quiet=$((quiet + 1)); fi
        last=$now
        [ $quiet -ge 10 ] && { kill -9 $pid; break; }   # 5 min with zero growth
      done
      wait $pid && break
    done
  done
  ```

  **Measure growth with `df`, never `du`.** Rootless podman's layer directories
  are owned by mapped subuids, so `du` on `~/.local/share/containers` hits
  permission-denied on nearly everything and reports a constant figure —
  verified: 62 MB of real growth over 40 s read as 0. A watchdog built on `du`
  therefore declares a false stall and kills the healthy pull, which is the
  identical bug one layer down.

- **Log silence is NOT a stall signal — check CPU.** The kernel build buffers
  its output, so `build.log` can go 10+ minutes without a write while the
  machine is flat out. A watcher keyed on log mtime alone fires a false
  positive here. The discriminator is CPU: a real compile shows `cc1`
  processes at ~96% and loadavg at the vCPU count; the SLIRP hang above showed
  podman at 2% with no children. Require log silence **and** no compiler
  processes **and** loadavg < 1 before calling it stalled.
- **Daemon shutdown orphaned every microVM — FIXED, verify it stayed fixed.**
  `daemonMain`'s signal handler called `srv.Stop()` before `fcStopAll()`, which
  released `srv.Serve()` in the main goroutine; main returning ended the
  PROCESS and killed the handler mid-shutdown. Measured before the fix: the
  daemon was gone in <1s, the log stopped at `fc: shutdown: stopping N
  microVM(s)` with no `[g] stopped`, firecracker survived the whole stop and
  beyond, `run/fc/<g>.{pid,sock,jail}` were left behind, and the next start
  logged `cleared N stale pidfile(s)`. `systemctl stop` took the full
  `TimeoutStopSec=25` and reported failure — systemd waiting on an orphan.
  **The severity is not the 25s: it is that guests never got the sync+umount
  window `fcStop` exists to give them, so every stop risked a dirty workspace
  ext4.** Raising `TimeoutStopSec` would have masked it. The fix makes
  `Serve`'s return wait on a `shutdownDone` channel. Regression check:

  ```sh
  time sudo systemctl stop koto     # ~0.4s, not 25s
  journalctl -u koto -n 5 -o cat    # must show `[g] stopped`
  pgrep -c firecracker              # must be 0
  ```

- **`systemctl reset-failed koto` after any teardown.** systemd keeps a unit's
  `failed` state under that name even after you delete and re-create the unit
  file, so the next fresh install reads `failed` instead of `inactive` and you
  will chase a bug that is not there.
- **A partial install poisons the next one.** `installed()` is
  `unit && koto.env`; if both exist the run takes the *upgrade* branch and the
  fresh-install path is silently not tested. Tear down completely: stop,
  disable, `rm` the unit + `/etc/koto` + `/var/lib/koto` + both
  `/usr/local/bin` binaries, `daemon-reload`, `reset-failed`.
- **Sparse rootfs.** `rootfs.img` is 2 G apparent / ~770 M real. Use
  `rsync --sparse` if you ever do copy it in, and give the VM ≥30 G — and make
  sure the *host* can actually back those 30 G, see the `df` warning in §1.
- **Give the VM ≥8 G RAM if you want more than two groups.** The fleet cap is
  90% of host RAM and each group reserves 1536 MiB (1024 mem + 512 VMM margin),
  so on a 4 G VM the third `spawn` fails with "needs 1536 MiB … but the fleet
  cap is 3511 MiB". That is the memory governor working correctly, not a bug —
  but it will stop a TUI-created group from booting if `main` and a test group
  are already up. Stop one first, or size the VM for it.
- **A completed test VM holds a REAL refresh token.** After section 6 the disk
  contains a live credential for the operator's account. Destroy the VM when
  done — do not leave the image lying around, and never snapshot or share it.

## Teardown

```sh
kill $(cat $VMDIR/qemu.pid)
rm -rf $VMDIR          # MANDATORY after section 6 — the disk holds a real
                       # OAuth refresh token for the operator's account
```

Revoke from the account side too if the run was interrupted before teardown.

## What this does NOT cover

- `make fetch` — the download route. Needs a published release on kotovm.com;
  until `dist/VERSION` is real, `make fetch` correctly refuses. Test it against
  a `file://` `KOTO_DIST_URL` once artifacts are published.
- **The wizard's interactive `claude auth login` handoff.** Section 6 injects a
  long-lived token instead, which exercises the proxy's OAuth path but not the
  browser flow itself. What is covered there is that the auth step reaches
  `claude auth login` and hands over the tty; that the login itself succeeds is
  a manual check.
- The API-key branch of the auth step, if you run section 6 with an OAuth token
  (and vice versa). Both branches store to `<state>/creds`; only one gets
  exercised per run.
