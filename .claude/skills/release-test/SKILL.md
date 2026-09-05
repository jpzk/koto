---
name: release-test
description: Clean-machine release test — provision a throwaway Fedora VM and take koto through all three stages (build from source → install → wizard), asserting the invariants at each boundary. Use before tagging a release, or after changing the Makefile stages, koto install, or the setup wizard.
---

# Clean-machine release test

One part of the release test: proves a **fresh host with nothing on it** can go
from `git clone` to a running daemon. It is the only way to catch the class of
bug that only exists on a machine that has never run koto — which is where the
install bugs have historically come from (`8d1e33c`).

Do this in a throwaway VM, never on the dev host: the install stage writes
root-owned system state (`/var/lib/koto`, `/etc/koto/koto.env`, a systemd unit)
and the build stage is a ~40-minute kernel compile.

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

### Ubuntu variant — verified 2026-09-03 on 24.04.4 LTS

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

Then §4's preflight fails both checks and prints Ubuntu-specific remediation.
**Applying exactly what it prints works verbatim, with no reboot** — verified;
`/dev/kvm` goes 0660 → 0666 and the sysctl 1 → 0, after which preflight reads
`✓ /dev/kvm usable (mode 0666)` and `✓ nested userns allowed`.

Note the failure count reads `1 unmet requirement(s)` while **two** `✗` marks
and two remediation blocks are shown. That is correct, not a bug:
`/dev/kvm` failures go to a separate soft bucket (`setup_checks.go`, the
"Continue without KVM?" path) and the count reports hard blockers only. A
non-empty hard list returns before the KVM prompt is ever reached.

Clean build time on 4 vCPU / 8 G was **~24 min** (fcassets ~18, Go binaries
~6), not the ~40 min the Fedora note estimates.

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
sudo dnf install -y git make          # cloud image has podman, not these
cd ~/koto && nohup setsid make build > ~/build.log 2>&1 < /dev/null &
```

~40 min cold. Assert on completion:

- all five artifacts exist: `koto`, `koto-tui`, `fcassets/{firecracker,vmlinux,rootfs.img}`
- the log ends with `built: …` then `next:  make install`
- re-running `make build` is a **no-op** (targets are keyed on real outputs, not
  `.build/` markers — a second run that rebuilds anything is a regression)

## 4. Stage 2 — install

```sh
cd ~/koto && make install < /dev/null
```

Assert:

- preflight is all green, `/dev/kvm usable (mode 0666)` among it
- every privileged action is echoed as a discrete `sudo` line before it runs
- it ends `service enabled (not started — koto setup starts it)` and `next:  make wizard`
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

```sh
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt install -y nodejs tmux              # node 22.x, not the distro's 18
sudo npm i -g @anthropic-ai/claude-code
```

On Fedora:

```sh
sudo dnf install -y nodejs npm tmux
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
KOTO_CLIENT=tui koto ctl spawn e2e     # spawn/destroy are admin verbs; the default 'agent' identity is least-privilege (audit M11)
sleep 20                                     # let the guest agent come up
koto ctl ask -timeout 120s e2e 'hi' >/dev/null   # warm-up turn, see below
koto ctl ask -timeout 120s e2e 'Reply with exactly one word: pong'
echo "exit=${PIPESTATUS[0]}"                 # NOT $? — that is the pipe's tail
KOTO_CLIENT=tui koto ctl destroy e2e
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
`koto ctl list` must show `e2etui`. Clean up with `KOTO_CLIENT=tui koto ctl destroy e2etui`.

### Frame integrity: `make tui-walk` — BROKEN, do not rely on it

**`make tui-walk` silently passes without running anything.** Verified
2026-09-03: `tui-walk` is listed in the Makefile's `.PHONY` line but **has no
rule**, in the working tree and in `HEAD` alike, so make reports
`Nothing to be done for 'tui-walk'` and **exits 0**. A green exit here is not
evidence of anything. Do not treat it as a gate until it is fixed.

Underneath, `tools/tuiwalk/walk.py` is also stale: it drives the TUI as a
**podman container** (`podman run --network koto-net … -e KOTO_ENDPOINT=cs_host_go:8443`,
`--image koto-tui`) and its docstring wants `make host-run` and `make
tui-build`. That is the container-era architecture `0d5848b` moved away from —
`koto tui` is a plain host binary now (`tui_cmd.go:60`). So the script cannot
work as written even with a rule restored; it needs porting to exec the host
binary in a pty, not a container.

What it was *meant* to do, and what is still worth having: drive the TUI under
a real VT emulator (pyte) and fail on any wrapped row or scrolled frame — the
glitch class a char-per-cell model cannot see. Two legs, `--term xterm,vt100`;
the vt100 leg additionally fails on any 8-bit byte or SGR color parameter,
which is the end-to-end proof that mono mode (`tui/mono.go`) leaves nothing a
VT100 cannot render.

Until it is ported, use the tmux route above and accept that the wrap/scroll
class is not covered — and say so in the release notes rather than implying it
was checked.

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

  ```sh
  for img in $(grep -rohE "(public\.ecr\.aws|docker\.io)/[a-zA-Z0-9._/-]+@sha256:[0-9a-f]{64}" Makefile */*.sh); do
    for a in 1 2 3 4 5 6; do timeout --signal=KILL 420 podman pull -q "$img" && break; sleep 5; done
  done
  ```

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
