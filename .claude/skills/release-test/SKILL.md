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

## 2. Ship the source — and nothing else

```sh
rsync -a -e "$SSH" \
  --exclude '.git' --exclude '.gocache' --exclude '.gomodcache' --exclude '.build' \
  --exclude '.kernelcache' --exclude 'fcassets' --exclude '/koto' --exclude '/koto-tui' \
  --exclude 'creds' --exclude 'groups' --exclude 'run' \
  --exclude 'groups.json' --exclude 'goals.json' --exclude 'schedules.json' \
  --exclude 'metrics.jsonl' \
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
being tested.

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

```sh
sudo dnf install -y nodejs npm tmux
sudo npm i -g @anthropic-ai/claude-code      # preflight's `! claude not found`
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

**The API key shadows OAuth completely.** `authHeaders()` short-circuits on
`apiKey != ""` and never consults the OAuth credential, so a placeholder key
left from section 5 means every request 401s and it reads as a bad token.
Delete it first, and confirm with `koto setup --check` reporting auth as
`OAuth credentials present`, not `API key stored`.

Assert, once login succeeds:

- the credential is at `/var/lib/koto/creds/.credentials.json`, mode `0600`
- **`~/.claude` does not exist for the operator.** This is the whole point of
  the `HOME=<state>` + `<state>/.claude → creds` design: koto's token must
  never land in the operator's personal profile. Cheap to check, and the thing
  that would silently regress.
- the shape is `{"claudeAiOauth":{"accessToken",…}}` — observed alongside
  `expiresAt`, `refreshToken`, `refreshTokenExpiresAt`, `scopes`,
  `subscriptionType`; the proxy reads only the first two.

No daemon restart is needed after OAuth: `readCreds()` re-reads the file on
every request. Only the API-key path is cached at startup (`proxyInitPaths`),
so that one does need a restart — an asymmetry worth remembering when a
credential change appears not to take.

`expiresAt` came back ~7 hours out. Past that the proxy kicks `refresh()`,
which shells out to `claude -p ok` — which is why the CLI has to be installed
in the VM, not just for the login itself.

> An unattended variant using `claude setup-token` is plausible but UNVERIFIED:
> it is not known whether that token satisfies the `Bearer` +
> `anthropic-beta: oauth-2025-04-20` shape the proxy sends. Use the flow above.

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

### Frame integrity: `make tui-walk`

Before hand-rolling any of the above, run `make tui-walk` (tools/tuiwalk/walk.py).
It drives the TUI under a real VT emulator (pyte) and fails on any wrapped row
or scrolled frame — the glitch class a char-per-cell model cannot see. Two legs,
`--term xterm,vt100`; the vt100 leg additionally fails on any 8-bit byte or SGR
color parameter, which is the end-to-end proof that mono mode (`tui/mono.go`)
leaves nothing a VT100 cannot render. Non-destructive by construction.

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
  `rsync --sparse` if you ever do copy it in, and give the VM ≥30 G.
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
