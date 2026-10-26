# Operations

Day-2 reference: where logs go, how resource monitoring and alerts work,
upgrading with `koto update`, removing koto with `koto uninstall`, and the
error messages you are most likely to meet. Every claim names the source
file and symbol it was read from; grep for the symbol to check it.

Related: [configuration.md](configuration.md) (env vars incl. `KOTO_REPO`,
`KOTO_REPO_URL`, `KOTO_HOST_MEM_MIB`), [ctl-reference.md](ctl-reference.md)
(`logs`, `resources` verbs), [getting-started.md](getting-started.md)
(install, preflight errors), [groups-goals-schedules.md](groups-goals-schedules.md)
(queue and goal errors), [acl-and-auth.md](acl-and-auth.md) (denial errors).

## 1. Logs

There are three distinct log surfaces.

### 1.1 The daemon log

- Every daemon log line goes through one funnel, `logDeliver`
  (`daemon/events.go`), called by `emitLog` / `emitLogG` / `emitLogf` /
  `emitLogfG` / `emitLogfQuiet`.
- Each line is written to **stderr** as `[<level>] <subsystem>: <msg>`
  (the `fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", ...)` in `logDeliver`).
  Under the installed systemd unit that stderr lands in the journal; the
  wizard's final screen points at `journalctl -u koto -f` ("daemon logs") and
  the smoke-test failure at `journalctl -u koto -n 50` (`stepHandoff`,
  `daemon/setup_steps.go`).
- Messages are scrubbed with `sanitize` and then `flattenInline`, so one log
  record is always one line (`logDeliver`).
- The daemon keeps the last **200** lines in memory (`logRingMax`,
  `daemon/events.go`). A new `SubscribeLogs` stream (`koto ctl logs`, the TUI
  log pane) first receives that ring, then live lines (`SubscribeLogs`,
  `daemon/grpc_server.go`). Each subscriber has a 256-entry channel; a
  subscriber that falls behind gets a `warn` line from subsystem `log`:
  `log stream fell behind: %d line(s) were not delivered to this subscriber`
  (`logDeliver`).
- Scoped (non-admin) identities only see lines attributed to a group they
  can see; unattributed lines (group `""`) are withheld (`logVisible`,
  `daemon/grpc_server.go`). See [acl-and-auth.md](acl-and-auth.md).

### 1.2 Error lines become operator notifications

(`daemon/logalert.go`)

- Every `error`-level line emitted through `emitLogG` is also forwarded as a
  **high**-severity notification to the `main` group (`forwardLogAlert`,
  delivered by `notifyDeliver(ctlMainGroup, "high", ...)`). `warn` and below
  stay log-only.
- Title format: `<LEVEL> <subsystem>` plus ` [<group>]` when attributed,
  e.g. `ERROR fc [dev]` (`logAlertTitle`).
- Rate limits: a token bucket per (subsystem, group) of burst 5, refilling
  one per minute (`logAlertBurst`, `logAlertRefill`), and a fleet-wide bucket
  of burst 20, one per 10s (`logAlertGlobalBurst`, `logAlertGlobalRefill`).
  Suppressed lines are still in the log; only the banner is dropped.
- `emitLogfQuiet` skips forwarding; the resource alerts use it because they
  queue their own notification (`resNotifyOperator`).
- Each group's undelivered notification markers are capped at 32
  (`notifyQueueMax`, `daemon/logtail.go`). The queue is in memory, so a
  daemon restart drops undelivered markers (comment above `notifyQueueMax`).

### 1.3 Per-group transcripts

(`daemon/logtail.go`)

- Each group has a group stream at `<workspace>/.cs/log` (`groupLogPath`)
  and one stream per concurrency slot at `<workspace>/.cs/log.<n>`
  (`slotLogPath`); `logPaths` lists the group stream plus `groupSlots` slot
  streams. `vol(g)` is the group's workspace mount on the host.
- History reads at most the last 4 MiB of each stream (`historyTailCap`),
  returns at most 8 MiB per response (`historyBytesMax`), and clamps the page
  size to 5000 events (`historyLimitMax`). Scroll-back past the 4 MiB window
  ends with an empty page.
- A partial (unterminated) line is buffered up to 8 MiB (`tailMaxPartial`);
  beyond that the surviving tail is prefixed with `…[line truncated] `
  (`tailTruncMark`).

### 1.4 Proxy metrics

- The LLM proxy appends one JSON record per completed request to
  `<state>/metrics.jsonl` (`METRICS`, set in `daemon/daemon.go`;
  `metricsAppend`, `daemon/proxy.go`).
- It rotates at 256 MiB (`metricsMaxBytes`), keeping one previous generation
  as `metrics.jsonl.1`, so the worst case on disk is about twice the ceiling.
  A failed rotation logs at error: `metrics rotation failed (%v): %s is past
  its %d MiB ceiling and still growing`.

## 2. Resource monitoring

(`daemon/resources.go`, wire types in `daemon/wire/wire.go`)

### 2.1 Collection

- `resourcesLoop` samples once at startup and then every **30s**
  (`resSampleInterval`). The figures come from the host (`stat`, `statfs`,
  `/proc/<pid>/stat`), so a wedged or stopped guest still reports.
- Exception: guest memory and the guest's own disk usage are read from
  inside each **running** guest with a bounded exec of `resGuestProbe`
  (`stat -f ... /workspace; cat /proc/meminfo`). The exec has a 5s timeout
  (`resGuestExecTimeout`), runs at most 8 at a time (`resGuestExecPar`), and
  the whole sweep has a 20s deadline (`resSweepDeadline`). Stopped VMs are
  never booted for it. If a running guest misses a refresh, its last
  reading is kept for up to 3 sample intervals (`resGuestMaxAge`). A stopped
  VM's reading is dropped at once. Either way it then reads as unknown (0)
  (`resGuestRetain`).
- Per-group history: 120 samples (`resRingLen`), i.e. 1h at 30s.
- Snapshots are cached for 1s (`resSnapTTL`), so polling faster returns the
  same answer.
- Host filesystem figures are `statfs` on `ROOT` (`<state>/groups`), using
  `Bavail` (blocks free to non-root users) for the free count (`hostFSStats`).

### 2.2 Fields (`koto ctl resources`, in-guest `resources` verb)

Both planes are built from the same `resourcesSnapshot`
(`resourcesCtlResp`). JSON names from `wire.GroupResources` /
`wire.HostResources`:

- Per group: `group`, `running`, `alloc_bytes`, `declared_bytes`,
  `alloc_pct`, `growth_bytes_per_hour`, `growth_span_seconds`, `rss_bytes`,
  `cpu_pct`, `vcpus`, `mem_mib`, `guest_disk_total_bytes`,
  `guest_disk_avail_bytes`, `guest_disk_used_bytes`, `guest_disk_used_pct`,
  `guest_mem_total_bytes`, `guest_mem_avail_bytes`, `guest_mem_used_pct`,
  `mem_committed_mib`.
- Host: `fs_total_bytes`, `fs_free_bytes`, `fs_used_pct`,
  `alloc_total_bytes`, `provisioned_bytes`, `groups`, `running_groups`,
  `mem_cap_mib`, `mem_committed_mib`, `mem_host_total_mib`.

What they mean:

- `alloc_bytes` is the workspace image's allocated blocks (`st_blocks × 512`,
  `statAllocBytes`). The images are sparse and Firecracker's virtio-blk has
  no discard, so this number effectively **only grows**. It is the host's
  cost, not how full the guest is (header comment of `resources.go`).
- `guest_disk_used_pct` is how full the guest filesystem is, computed like
  `df`: used ÷ (used + avail), so ext4's reserved blocks count as neither
  (`resGuestDiskPct`). Use this one to tell if a group is about to fill up.
- `growth_bytes_per_hour` is measured over a trailing 10-minute window
  (`resGrowthWindow`). It reads **0 with `growth_span_seconds` 0** until the
  sample history covers a full window (for example, right after a daemon
  restart), and it never goes below zero (`resGrowth`).
- `provisioned_bytes` is the sum of every group's `size` preset. It is
  normal for it to exceed `fs_total_bytes` (comment on
  `HostResources.ProvisionedBytes`).
- `mem_cap_mib` 0 means no fleet memory cap (comment in `wire.HostResources`).

### 2.3 Threshold alerts

(`resCheckThresholds`, `resNotifyOperator`)

- Thresholds are hard-coded: warn at 80% (severity `normal`), critical at
  90% (severity `high`) (`resWarnPct`, `resCritPct`, `resAlertSeverity`).
- An alert fires only when the level **rises**. A level re-arms once the
  value falls 5 points below the threshold that fired it (`resClearMargin`,
  `resArmedLevel`).
- Three subjects:
  - host filesystem (`resSubjectHostFS`). Title `Host disk N% full`.
  - each running group's **guest** filesystem (`resSubjectDisk`). Title
    `Group <g> disk N% full`. A group whose guest disk size is unknown
    (stopped or unreachable) raises nothing.
  - each group's CPU averaged over the last 10 samples (`resCPUAvgWindow`,
    ≈5 min), as a percentage of its own vCPUs (`resSubjectCPU`). Title
    `Group <g> CPU N% sustained`.
- Delivery: a log line from subsystem `resources` (`warn` for level 1,
  `error` for level 2, sent through `emitLogfQuiet`) plus a notification to `main`. If
  main's notification queue is full, the daemon logs `notification backlog
  full, alert dropped: <title>`.

## 3. Upgrading: `koto update`

(`daemon/update.go`)

```
usage: koto update [--check] [--version X] [-y]
```

Flags (`updateMain`):

- `--check`: report only. Exits 0 when up to date and **3** when an update
  exists (`updateExitAvailable`).
- `--version X`: install release X instead of the latest. A downgrade is
  allowed only when you name the version this way.
- `-y`: accept the prompts, including the one to stop the daemon. It is
  passed to the installer as `--yes`.

Steps:

1. **Installed version.** If `/usr/local/bin/koto` exists
   (`installedKotoBinPath`), its `version` output is used. Otherwise the
   running binary's own `kotoVersion` is used (`updateInstalledVersion`).
2. **Latest release.** A `HEAD <repo>/releases/latest` request; the tag is
   read from the redirect's `Location` header without following it
   (`updateLatestRelease`). The repo is `https://github.com/<KOTO_REPO>`
   (default `jpzk/koto`), or `KOTO_REPO_URL` when set (`updateRepoURL`). The
   whole command has a 30s context, and the HTTP client gets a 20s timeout
   when it has none.
3. **Decision** (`updateDecide`; versions are compared as `MAJOR.MINOR.PATCH`
   by `parseSemver`, which ignores a leading `v` and any `-`/`+` suffix).
   Messages printed:
   - `koto %s is available (installed: %s)`: newer, so it updates.
   - `up to date: koto %s is the latest release`
   - `up to date: development build %s is based on the latest release %s`:
     a build like `1.0.0-6-gd5150df-dirty` counts as its base release.
   - `release %s replaces development build %s (named explicitly)`
   - `%s is a downgrade from %s (named explicitly)`
   - `installed %s is newer than the latest release %s; nothing to do`
   - `installed %s is not a release build; %s is available`
   - `%q is not a release version`: does not update.
4. **Install** (`updateRun`). Refuses to run as root (`run as your normal
   user, not root — koto installs as you and uses sudo where it must`).
   Writes the `install.sh` **embedded in the binary** (`embeddedInstaller`,
   `//go:embed installer/install.sh`) to a temp file and runs `sh <file>
   --version <target> --no-attach [--yes]` on your terminal. That installer
   verifies the release and runs the new release's `koto install` and
   `koto setup` (file header comment). The install stops the daemon, and with
   it every running microVM.
   On failure: `koto update: the installer failed: <err>`.

The installer is embedded rather than downloaded because a downloaded one
would bring its pinned signing-key fingerprint over the same channel as the
release it checks (header comment of `update.go`). The signature checks
themselves live in `install.sh` and are described in
[getting-started.md](getting-started.md).

Upgrading from 1.7.1 or earlier: run the installer once,
`curl -fsSL https://kotovm.com/install.sh | sh`. Installers before 1.7.2
cannot read the signed `# koto-version:` line in `SHA256SUMS`, so
`koto update` from those versions fails. From 1.7.2 on, `koto update` works.

## 4. Removing koto: `koto uninstall`

(`daemon/uninstall.go`)

```
usage: koto uninstall [flags]
```

Flags (`uninstallMain`):

- `-state DIR`: state directory (default `/var/lib/koto`, `defaultStateDir`
  in `daemon/install.go`).
- `--purge`: also delete the state directory and `/etc/koto/koto.env`.
- `-n`: dry run. Each privileged command is printed as `would: sudo …` and
  nothing changes (`uninstallOpts.step`).
- `-y`: accept defaults. **Combined with `--purge` it deletes without a
  prompt.**
- `-no-color`: accepted by the code but not listed in the usage text.

Positional arguments are rejected: `unexpected argument %q — this command
takes flags only`. Running as root is refused: `run as your normal user, not
root (sudo is used only for the system files)` (`runUninstall`).

Order (`runUninstall`):

1. If none of the unit, `/usr/local/bin/koto`, `/usr/local/bin/koto-tui` or
   `/etc/koto/koto.env` exist, it prints `nothing installed — …` and stops,
   unless `--purge` was given.
2. **Stop first.** If the unit file `/etc/systemd/system/koto.service`
   (`unitPath`) exists **or** `systemctl is-active koto` says `active`
   (`systemctlIsActive`), it runs `systemctl stop koto`, `disable`, removes
   the unit, `daemon-reload` and `reset-failed`. A failed stop aborts:
   `stop koto: … (fix, or `sudo systemctl kill koto`, then re-run)`. The unit
   sets `TimeoutStopSec=25` (`daemon/install.go`).
3. **Live-daemon check.** It scans `/proc` for a `koto daemon` process whose
   state root (`KOTO_HOME` from its environment, otherwise its cwd) is the
   same directory (`runningKotoDaemon`, `procKotoHome`). If one is still
   running, it refuses with `a koto daemon is still running against …`. With
   `-n` it only warns.
4. Removes `/usr/local/bin/koto` and `/usr/local/bin/koto-tui`.
5. Without `--purge`, it lists what it kept (the env file, which may hold
   `ANTHROPIC_API_KEY`, and the state dir with its group count and `du -sh`
   size) and stops. A later `koto install` picks them up.
6. With `--purge` (`uninstallPurge`):
   - The state dir is checked by `purgeRefusal`. It refuses (`refusing to
     delete <dir>: …`) when the path is not absolute, is the filesystem root,
     is your home directory, or contains `.git` or `go.work` ("that is a koto
     clone"). It also refuses when the path has none of `.koto-version`,
     `groups`, `creds`, `fcassets`, `groups.json` (`no koto state found
     there …`).
   - It prompts `Delete it?` (default no), unless `-y`.
   - Its (device, inode) is recorded before the prompt and checked again
     before deleting (`dirIdentity`). If they differ, it refuses with `… is
     not the directory that was checked (it was replaced while the
     confirmation was open) — refusing to delete it`.
   - The state dir is removed **without sudo** (`os.RemoveAll`); it is owned
     by the user.
   - Removes `/etc/koto/koto.env` (sudo), then
     `rmdir --ignore-fail-on-non-empty /etc/koto`.

Re-running finishes an interrupted uninstall, because each item is detected
and skipped when it is already gone (header comment).

Development-checkout equivalents in the `Makefile`: `make clean` removes
build output, `metrics.jsonl`, `proxy.log` and `run`. `make clean-groups`
**permanently deletes** `groups groups.json schedules.json goals.json` after
asking you to type `yes` (`FORCE=1` skips the prompt). `make clean-creds`
removes `creds`.

## 5. Troubleshooting

Each entry gives the message exactly as it appears in the code, where it
comes from, and what to do.

### Install / setup

- `daemon did not answer within 45s: … check `systemctl status koto` and
  `journalctl -u koto -n 50``: the wizard's smoke step (`daemon/setup_steps.go`).
  Read the journal.
- `no usable /dev/kvm on this host — groups will not boot until that is
  fixed`: a warning from the same step (`checkKVM`). The preflight checks
  and their messages are listed in [getting-started.md](getting-started.md).

### Credentials and the LLM proxy (`daemon/proxy.go`)

- `no credentials: run `koto claude-login` (or set ANTHROPIC_API_KEY)`:
  no Anthropic credential could be found.
- `oauth token refresh failed — turns will 401 until it works (check `koto
  claude-login --status`): …` and `… but the token on disk is still expired —
  the CLI did not rotate it; run `koto claude-login``: OAuth refresh
  problems. These are error lines, so they also appear as banners in `main`.
- `the venice credential is not available on this host; ask the operator`
  (HTTP 503): a group with the venice provider, but no key was found
  (`no venice key: write to …`). Fix: `koto venice-login`.
- `the minimax credential is not available on this host; ask the operator`
  (HTTP 503, `daemon/proxy_minimax.go`): a minimax group, but
  `creds/minimax.key` is missing or empty. Fix: `koto minimax-login`. A key starting `sk-api-` is billed
  as `minimax/apikey`; any other key is an M Plan subscription
  (`minimax/subscription`), whose 5h and weekly windows the daemon reads from
  `token_plan/remains` at most once a minute after a minimax call.
  `KOTO_MINIMAX_REGION=cn` in koto.env switches to the mainland endpoints.
- `koto proxy: <METHOD> <path> is outside the endpoint allowlist for
  provider <p> (the proxy relays inference and model metadata only)` (HTTP
  403, `llmRefuse`): the guest called an API path the proxy does not relay.
- `koto proxy: too many concurrent requests for this group` (HTTP 503): over
  32 requests in flight for the group, or 128 overall, after waiting 60s
  (`proxyMaxInflightPerGroup`, `proxyMaxInflightGlobal`, `proxyInflightWait`).
- `koto proxy: request-body memory budget exhausted; retry shortly` (503),
  `koto proxy: request body stalled` (408) and `request body too large or
  unreadable` (413): problems reading the request body.

### Egress (`daemon/proxy.go`)

- `egress denied: this group's network profile is 'none' (a raised profile
  applies on /restart)` (403): set `network` (see
  [configuration.md](configuration.md)), then restart the group.
- `egress blocked: target not permitted under this group's network profile`
  (403): the target is outside what the group's profile allows.
- `koto proxy: too many concurrent egress connections for this group` (503).

### Booting groups (`daemon/fchostmem.go`)

- `group %s needs %d MiB (mem %d + %d VMM margin) but the fleet cap is %d
  MiB with %d MiB already committed to running VMs — stop a VM, pick a
  smaller size, or raise KOTO_HOST_MEM_MIB`: the fleet memory cap is full.
- `refusing to boot %s: the host's memory capacity could not be determined …`:
  the cap could not be computed.

### Turns

- `this conversation's previous turn is wedged and the group could not
  self-heal — /restart the group …` (`errSessionWedged`,
  `daemon/queue.go`). Restart the group. Other queue limits are covered in
  [groups-goals-schedules.md](groups-goals-schedules.md).

### Uninstall

- `a koto daemon is still running against …`: stop it with
  `sudo systemctl stop koto` or `kill <pid>` as the message says, then re-run.
- `refusing to delete …: that is a koto clone, not an installed state dir`:
  `-state` points at a source checkout. Point it at the real state dir.
