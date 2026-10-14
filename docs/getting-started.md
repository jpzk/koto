# Getting started

This page covers getting koto from nothing to a running daemon: what the host
needs, the three stages (acquire, integrate, configure), the platform checks
that gate installation and their exact messages, and what each wizard step
does.

Every claim has a source reference (file plus function, const or Makefile
target) so you can grep it. Where code comments or usage text disagree with
the code, this page follows the code and says so.

See also: [configuration.md](configuration.md) (koto.env keys, `KOTO_*` env
vars, group config), [acl-and-auth.md](acl-and-auth.md) (the PKI and tokens
the wizard mints), [ctl-reference.md](ctl-reference.md) (`koto ctl`).

## 1. Two ways in

### 1.1 The piped installer

```sh
curl -fsSL https://kotovm.com/install.sh | sh
curl -fsSL https://kotovm.com/install.sh | sh -s -- --version 1.0.0
```

`install.sh` runs four stages in order (header comment of `install.sh`):

1. fetch: clones the release tag into a temp dir, then downloads and verifies the signed artifacts into it (the same thing as `make fetch`).
2. install: `koto install` (the same thing as `make install`).
3. setup: `koto setup` (the same thing as `make wizard`).
4. tui: `koto tui`.

Under `curl | sh`, stdin is the pipe, so stages 2 to 4 read `/dev/tty`. When
there is no terminal, the script stops after stage 2 and prints the remaining
commands (header comment of `install.sh`).

`install.sh` flags (`install.sh` `usage()`):

- `--version X`: the release to install. Default `latest`, from `VERSION="${KOTO_VERSION:-latest}"`.
- `--no-attach`: stop after `koto setup` instead of opening the TUI. This is what `koto update` runs.
- `--yes`: accept the defaults of `koto install` and `koto setup`, including the daemon stop on an upgrade.
- `--download-only`, `--dest DIR`, `--manifest FILE`: fetch and verify only. `--dest` defaults to the current dir. `--manifest` adds a check against a sha256sum(1) list that did not come from the release.
- `--next TEXT`: the command to suggest once a `--download-only` run finishes.
- `-h`, `--help`.

What `install.sh` refuses (each is a `die` in `install.sh`):

- A host that is not Linux/x86_64: ``koto is Linux/x86_64 only (this is <os>/<arch>)``.
- A missing tool: ``<tool> is required but not installed``. The tools are `curl gpg zstd sha256sum`, plus `git` unless `--download-only` was given.
- An empty key fingerprint: ``no release key fingerprint — refusing to accept a signature from whatever key turns up``.

The verification order (header comment of `install.sh`, section "WHAT THIS VERIFIES"):

1. Fetch the signing key by fingerprint.
2. Verify the detached signature on `SHA256SUMS` and require the `VALIDSIG` line to name that fingerprint.
3. Take the asset names from the signed `SHA256SUMS`.
4. Check every asset before decompressing it, and stage everything beside the destination until the whole set has passed.

The pinned fingerprint is `A6E69ED6BC4779F3281721484CC1AFDE15B64EA3`
(`install.sh` `KEY_FPR`; Makefile `RELEASE_KEY_FPR`). The env overrides
`install.sh` reads are listed in
[configuration.md](configuration.md) §2.5.

### 1.2 From a clone, in three stages

The Makefile's top-of-file comment ("GETTING koto RUNNING IS THREE STAGES") defines the stages:

1. ACQUIRE the five artifacts, by one of two routes: `make fetch` or `make build`.
2. INTEGRATE them into the system: `make install`.
3. CONFIGURE the installed system: `make wizard`.

```sh
make fetch && make install && make wizard   # download — minutes
make build && make install && make wizard   # build it yourself — ~40 min
```

`make setup` is shorthand for `make build`, `make install` and `make wizard`, in that order (Makefile target `setup`).

Each stage refuses to do the previous stage's work and names the command that
does. Every stage is safe to re-run (Makefile header comment).

## 2. Requirements

### 2.1 Platform

- linux/amd64 only. `checkPlatform` (`daemon/setup_checks.go`) fails any other `GOOS/GOARCH` with this remedy: ``koto needs linux/amd64: Firecracker microVMs need KVM, and the guest kernel and rootfs are built for x86_64.``
- `/dev/kvm` must be present and world read/write (mode `&0o006 == 0o006`). See `checkKVMAt` and §4.2.
- Unprivileged user namespaces, and a subuid/subgid range for your user. See §4.3 and §4.4.

The README lists the distros where building and running have been verified:
Fedora 44, Ubuntu 24.04 LTS, Ubuntu 26.04 LTS and Arch (`README.md`, "From
sources"). This page has not re-verified that list.

### 2.2 Build-time only (route `make build`)

Docker or podman. When both are present the Makefile prefers docker (Makefile
`CONTAINER ?= $(shell command -v docker … || command -v podman …)`; the same
order is in `containerEngine` in `daemon/setup_checks.go`). The Go binaries
build inside the digest-pinned image `GO_IMAGE` with `CGO_ENABLED=0`, so the
host needs no Go toolchain (Makefile `GO_BUILD_RUN`). If neither engine is
present, the build fails before compiling (Makefile `need-container`):

```
podman or docker is required to build koto (sudo dnf install podman / sudo apt install podman)
```

The other build tools are `git`, `make` and `curl` (`checkBuildTools`). The
README install lines also add `passt` (and `uidmap` on Ubuntu) for rootless
podman:

```sh
sudo dnf install -y git make podman                  # Fedora 44+
sudo apt install -y git make podman passt uidmap     # Ubuntu 24.04+
sudo pacman -S --needed git make podman passt        # Arch
```

### 2.3 Runtime (what the installed daemon shells out to)

These come from `checkRuntimeTools` in `daemon/setup_checks.go`, with Fedora and Debian package names:

- `mkfs.ext4`, `e2fsck`, `resize2fs`: `e2fsprogs`. They create, check and grow each group's workspace image.
- `tar`: `tar`. It delivers skills into a guest.
- `newuidmap`, `newgidmap`: `shadow-utils` on Fedora, `uidmap` on Debian/Ubuntu. They map the per-VM uid/gid that the jailed microVM monitor runs as.
- `claude`: warn-only. It is needed only for OAuth (subscription) auth, where the proxy shells out to refresh the token. Its hint asks for `node >= 22` and `npm i -g @anthropic-ai/claude-code`. On Debian-family hosts it also notes that Ubuntu's `nodejs` package is v18, and suggests `sudo snap install node --classic --channel=22` or nvm.

Podman is not a runtime dependency. Nothing koto runs is a container
(`checkRuntimeTools` comment; Makefile header).

## 3. Stage 1: acquire

### 3.1 The five artifacts

Makefile `ARTIFACTS` and `daemon/install.go` `artifacts` both list them:

- `koto`
- `koto-tui`
- `fcassets/firecracker`
- `fcassets/vmlinux`
- `fcassets/rootfs.img`

Both routes produce exactly these.

### 3.2 `make fetch`

`make fetch` runs `install.sh --version $(DIST_VERSION) --dest . --manifest artifacts.sha256 --download-only --next "make install"` with `KOTO_BASE_URL="$(KOTO_DIST_URL)/$(DIST_VERSION)"` (Makefile target `fetch`). The defaults are:

- `KOTO_DIST_URL ?= https://github.com/jpzk/koto/releases/download`
- `DIST_VERSION ?= 1.0.5`
- `DIST_ARCH ?= x86_64`

Release assets are zstd-compressed and named `<basename>_<version>_<arch>.zst`, for example `rootfs.img_fedora44_x86_64.zst` (Makefile comment above `DIST_ARCH`). The committed `artifacts.sha256` checksums the artifacts as they are installed, after decompression (Makefile comment above `MANIFEST`).

If `DIST_VERSION` is `unreleased`, fetch exits 1 with
``no koto release has been published yet, so there is nothing to fetch.``

### 3.3 `make build`

`make build` runs these sub-makes in order: `koto`, `koto-tui`, `firecracker`, `kernel`, `rootfs` (Makefile target `build`). Each one is a no-op once its output is newer than its inputs.

- `koto`: `go build -trimpath -ldflags "-s -w -X main.kotoVersion=$(KOTO_VERSION)" -o koto ./daemon`. It first copies `install.sh` to `daemon/installer/install.sh`, because the installer is embedded for `koto update`. `KOTO_VERSION` is `git describe --tags --always --dirty`, falling back to the date. `koto version` prints it (`daemon/main.go`, `kotoVersion`, default `"dev"`).
- `firecracker`: `fcguest/build-firecracker.sh`.
- `kernel`: `fcguest/build-kernel.sh`.
- `rootfs`: `fcguest/build-rootfs.sh`. Its inputs include `fcguest/*.go`, `sidecar/*` and `protocol/` (Makefile `FCGUEST_SRC`), so a protocol change rebuilds the guest image.

The builds are not reproducible. A from-source build is trusted because you
built it, and is never checked against the release manifest (Makefile comment
above `dist`; `requireArtifacts` comment in `daemon/install.go`).

## 4. Stage 2: `make install` / `koto install`

`make install` first runs `require-artifacts`. If any of the five files is
missing it prints ``missing artifact(s): …`` and the fetch/build hint, then
runs `./koto install` (Makefile targets `install`, `require-artifacts`).

### 4.1 `koto install` flags

These are from `installMain` and `installUsage` in `daemon/install.go`:

- `-state DIR`: the state directory. Default `/var/lib/koto` (`defaultStateDir`). The value must pass `unitSafeValue`, otherwise you get ``-state: …`` and exit 2.
- `-y`: accept defaults and never prompt.
- `-no-color`: disable color. This flag is parsed but not listed in the usage text.

The usage text says install "starts the service". The code starts it only on
an upgrade. A fresh install runs `systemctl enable koto` and prints
``service enabled (not started — `koto setup` starts it)`` (`runInstall`).
The daemon cannot come up until the wizard has minted `server.crt`.

### 4.2 What install does, in order

These steps are from `runInstall` in `daemon/install.go`:

1. It detects an upgrade (`installed()`: both `/etc/systemd/system/koto.service` and `/etc/koto/koto.env` exist) and prints ``existing installation detected — upgrading in place``.
2. It refuses to run outside a clone: ``run this from a koto clone (no Makefile/daemon here)``. It also refuses missing artifacts (`requireArtifacts`).
3. It refuses root: ``run as your normal user, not root — the daemon runs as you (sudo is used only for the system files)``.
4. It runs the host requirement checks (`preflightGate`, §5).
5. It stops a running daemon (`installStopDaemon`).
6. It seeds the state dir (`seedStateDir`). It creates the directory with `sudo install -d -o <uid> -g <gid> -m 0750` when it is absent. An existing directory must pass `stateDirTrusted`: it must not be a symlink, must be a directory, must be owned by you and must have no group/other write bit. Failure reads ``<dir>: <reason> — remove it or choose another -state``. Install then creates `groups/`, `creds/`, `fcassets/`, `prompts/` and `run/` (mode 0750), links `.claude -> creds`, and migrates `groups/`, `groups.json`, `schedules.json`, `goals.json` and `metrics.jsonl` from the clone. It never overwrites (`migrateState`).
7. It installs the binaries: `sudo install -m 0755 <self> /usr/local/bin/koto`, and `koto-tui` to `/usr/local/bin/koto-tui`. When `koto-tui` is missing it only warns.
8. If AppArmor restricts userns and the operator accepted the offer (or a koto profile already exists), it installs or refreshes the AppArmor profile (`installAppArmorProfile`).
9. It resolves the `claude` CLI (`installClaudeBin`) and writes `/etc/koto/koto.env` (`writeEnvFile`; keys in [configuration.md](configuration.md)).
10. It renders `/etc/systemd/system/koto.service` (`renderUnit`, `ExecStart=/usr/local/bin/koto daemon`). If the unit changed, it runs `systemctl daemon-reload`.
11. On an upgrade it runs `systemctl start koto`. On a fresh install it runs `systemctl enable koto`.
12. It writes `<state>/.koto-version` from `git describe`, falling back to `kotoVersion`.

Every privileged action is a separate `sudo` exec that is echoed before it
runs (`sudoRun`; header comment of `daemon/install.go`).

## 5. Platform checks (preflight)

`runPreflight` in `daemon/setup_checks.go` runs these checks in this order:
platform, `/dev/kvm`, nested userns, subuid/subgid, build-time tools,
runtime tools, disk space, network. Both `koto install` and `koto setup
--check` run them.

The build-time group (container engine plus `git`, `make`, `curl`) goes
through `advisory()`, which turns its failures into warnings, because the
fetch route does not need those tools.

### 5.1 How the gate decides (`preflightGate`)

- Any hard failure stops install. With one or more failures it reports ``<n> unmet requirement(s) — see the remediation above``. If a hard failure and `/dev/kvm` both fail, it reports ``<n> unmet requirement(s), all shown above — fix all of them (<m> blocking, plus /dev/kvm)``.
- `/dev/kvm` failing on its own is soft. The prompt ``Continue without KVM?`` defaults to no. Answering yes installs a daemon whose API answers but where no group can boot (`preflightResult.noKVM`). Answering no aborts (`errSetupAborted`, text `aborted`).
- When userns is ``restricted by AppArmor`` and `apparmor_parser` is available, install offers ``Install an AppArmor profile for koto?`` (default yes). Declining fails with ``nested user namespaces are unavailable — see the remediation above, or re-run and accept the AppArmor profile``.

### 5.2 `/dev/kvm` (`checkKVMAt`)

- Missing: detail ``not present``. The remedy is to enable virtualization in BIOS/UEFI, or nested virtualization when koto itself runs in a VM.
- Mode lacks world rw: detail ``mode NNNN — not world-accessible``. Membership in the kvm group does not help. The jailed Firecracker VMM runs as a per-VM uid with an empty supplementary group set (`kvmRemediation`; see `daemon/fcjail.go`). The first remedy printed is a udev rule:

  ```sh
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm
  ```

  The alternative is a per-uid POSIX ACL over the jail uid band, computed from your `/etc/subuid` start via `fcJailHostUID(start, fcJailBaseUID)` up to `fcJailBaseUID+ctlMaxSpawn`. The remedy text warns about two problems with the ACL: systemd-logind rewrites it, and this check reads the mode bits, so it keeps failing.
- The device opens but fails: detail ``present but not usable: <err>``. The remedy is ``sudo usermod -aG kvm $USER``.
- OK: ``usable (mode NNNN)``.

### 5.3 Nested user namespaces (`checkUserns`)

- `/proc/sys/user/max_user_namespaces` is `0`: fail ``user.max_user_namespaces = 0``. The remedy is ``sudo sysctl -w user.max_user_namespaces=15000``, persisted in `/etc/sysctl.d/`.
- AppArmor restricts unprivileged userns (`apparmorUsernsRestricted`). The check then execs the installed binary (`apparmorProfileWorks(installedKotoBin)`). If that succeeds the result is ok, ``granted to koto by AppArmor profile (host-wide restriction still on)``. If not, it fails with ``restricted by AppArmor``. The fallback remedy (`usernsRemediation`) is the host-wide `kernel.apparmor_restrict_unprivileged_userns=0`, persisted to `/etc/sysctl.d/60-koto.conf`.
- Unreadable file: warn ``unknown``.
- Otherwise ok: ``allowed (<n>)``.

### 5.4 subuid/subgid (`checkSubuid`)

Both `/etc/subuid` and `/etc/subgid` need a line for your user name or uid.
When one is missing, the check fails with ``no range for <user>`` and this
remedy:

```sh
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>
podman system migrate
```

### 5.5 Container engine (`checkContainerEngine`, advisory)

- Neither engine: ``neither found``.
- `<engine> info --format json` fails: `` `<engine> info` failed: … ``.
- podman only: it warns if podman is not rootless (``running as root``) and if pasta is missing (``not found``, install `passt`). Docker gets no further checks.

### 5.6 Disk and network

- `checkDisk` checks the current working directory and warns under 20 GiB free.
- `checkNetwork` sends a `HEAD https://github.com` with a 5 s timeout and only warns. Install works offline if the artifacts are already present.

## 6. Stage 3: `make wizard` / `koto setup`

`make wizard` checks that `./koto` is executable. If not, it prints
``./koto is missing — acquire the artifacts first:`` and exits 1. Otherwise
it runs `./koto setup` (Makefile target `wizard`).

### 6.1 Flags

From `setupMain` and `setupUsage` in `daemon/setup.go`:

- `--check`: doctor mode. Runs every preflight probe and every step's `detect`, and changes nothing. Exit 0 when ready, 1 otherwise (`setupCheck`). It prints ``koto is fully set up``, ``<n> host requirement(s) unmet`` or ``<n> step(s) pending — run `koto setup` to finish``.
- `-y`: accept defaults and never prompt. Steps that need real input fail.
- `--only ID`: run one step. Its `detect` is skipped, so the step always runs. An unknown ID gives ``no such step "<id>" (see `koto setup --list`)``, exit 2. The usage text's example `--only rootfs` names a step that no longer exists.
- `--from ID`: start at this step.
- `--provider P`: the default provider for new groups (`claudesdk`, `venice` or `openai`, case-insensitive). The `provider` step then persists it without prompting. An unknown value exits 2 with ``unknown --provider "<p>" (want claudesdk, venice or openai)``. `koto setup --only provider --provider P` asks nothing and so also runs without a TTY.
- `--providers L`: the providers to set up, comma-separated (e.g. `claudesdk,openai`, case-insensitive, duplicates dropped). The `provider` step then asks nothing; the `auth` step connects each one. The default is `--provider` when given, else the first one listed. A `--provider` that is not in the list exits 2 with ``--provider <p> is not in --providers <list>: the default must be one of the providers being set up``; an unknown name exits 2 with ``unknown provider "<p>" in --providers (want claudesdk, venice or openai)``.
- `--no-color`: plain output. Styling is also off when `NO_COLOR` is set or `TERM` is `dumb` or empty (`colorOK`, `daemon/setup_ui.go`).
- `--list`: print the step ids in order.

Without a TTY and without `-y`, `--check` or `--only provider` with `--provider P` or `--providers L`, setup exits 2 with ``setup needs
a terminal (or pass -y / --check)``.

The state dir is `KOTO_HOME`, or `/var/lib/koto` when that is unset
(`setupCtx.stateDir`). Credentials go to `<state>/creds`.

### 6.2 Steps

The steps, in order (`setupSteps` in `daemon/setup_steps.go`):

1. `installed` (Installed system). Detects `installed()`. It never installs anything. When koto is not installed it fails with the "acquire, then `make install`" hint.
2. `pki` (TLS identities). Detection needs all of these under `creds/`: `ca.crt`, `ca.key`, `server.crt`, `client-tui.crt`, `token-tui`, `client-agent.crt`, `token-agent`. It asks whether you will reach the daemon from another machine and, if so, takes extra SANs such as `IP:192.168.1.20,DNS:koto.lan`. The default SANs are `DNS:koto-daemon`, `DNS:localhost` and `IP:127.0.0.1` (`defaultServerSANs`, `daemon/pki.go`). It runs `pkiInit`, then mints the `tui` and `agent` clients, both with role `admin`. An existing CA is never regenerated. See [acl-and-auth.md](acl-and-auth.md).
3. `provider` (Providers). Two questions. First a multi-select over `knownProviders` (`claudesdk`, `venice`, `openai`, each with a one-line description; a provider that already has a credential is marked `✓ connected`): answer with numbers separated by commas or spaces (`1,3`), provider names, or `all`. Enter keeps the preselected set: the current default plus every provider already connected. Anything else is explained and asked again (`parseMultiChoice`, `daemon/setup_ui.go`). Then, only when more than one was selected, which of THOSE new groups use by default, the current default preselected when it is among them. With exactly one selected it is the default and the second question is not asked. Only the default is persisted, as `KOTO_DEFAULT_PROVIDER=<p>` in `/etc/koto/koto.env` (`setupPersistProvider`, `daemon/setup_provider.go`); every other line of the file is kept, and no other setting records the selection (it is derivable from which credentials exist). Done when koto.env names a valid provider (and it matches `--provider`, or the first of `--providers`, if given). With `--provider` or `--providers` it does not prompt; under `-y` it keeps the current set and default (`claudesdk` when none). Advisory: `--check` reports it pending as a warning, not a failure, because an install that never chose runs on `claudesdk` exactly as before.
4. `auth` (Provider credentials). Acts on every provider selected above, in display order (when the provider step did not run: `--providers`, else `--provider` alone, else the default plus every provider already connected). A provider that already has a local credential is reported `already connected, login skipped`; each other one gets its own "Connect … now?" and ends `connected`, `skipped` (with the command to connect it later) or `failed` (with the error; the next provider is still offered). A summary then lists each selected provider with its state and marks the default. When the default ends without a credential it says loudly that every turn of new groups will fail until fixed; a non-default one gets a softer warning. Only the default decides whether the step passes: it passes when the default has a credential or its login was explicitly skipped, and fails when the default's login failed. The step is done (skipped by a re-run) when every selected provider has a credential, detected locally: claudesdk `creds/.credentials.json`, `creds/anthropic-api-key` or `ANTHROPIC_API_KEY`; venice a non-empty `creds/venice.key`; openai `creds/openai-auth.json` or a non-empty `creds/openai-api-key`. The logins run in-process: claudesdk `authConnect` (same as `koto claude-login`), venice a hidden key prompt written to `creds/venice.key` (0600), openai the device-code or API-key flow of `koto openai-login`. Under `-y` nothing is logged in: providers already connected pass, each missing one is reported with its non-interactive alternative (e.g. ``koto openai-login --api-key-stdin < keyfile``), and the step fails only when the missing one is the default.
5. `service` (Start the daemon). Runs `sudo systemctl restart koto`. It reports not-done whenever this run changed credentials or the default provider (`restartOwed`); the daemon reads `KOTO_DEFAULT_PROVIDER` only at start. The wizard comment gives the reason as the proxy resolving credentials once, at startup. That comment is stale: the proxy now reads credentials per request (`currentAPIKey` and `readCreds` in `daemon/proxy.go`), and `claudeLoginUsage` says the same. For file-based credentials the restart is therefore harmless but not strictly required. `ANTHROPIC_API_KEY` from the environment is still fixed for the life of the process (`envAPIKey`).
6. `smoke` (Smoke test). Builds a client for identity `tui` against `127.0.0.1:8443` and calls the `List` RPC. It retries every 2 s for up to 45 s, then fails with ``daemon did not answer within 45s: … check `systemctl status koto` and `journalctl -u koto -n 50` ``. If `/dev/kvm` is unusable it warns ``no usable /dev/kvm on this host — groups will not boot until that is fixed``. It then checks the default provider's credential locally (no LLM request) and warns, with the command to fix it, when there is none.
7. `done` (Ready). Prints ``koto is installed and running.`` and next commands: `koto tui`, `koto ctl list`, `systemctl status koto`, `journalctl -u koto -f`. It also prints the state dir and `/etc/koto/koto.env`, and how to add a client with `koto pki client -creds <state>/creds <name>`.

Resuming works by detection, not by a state file. Re-running `koto setup`
skips every step whose `detect` is satisfied (`daemon/setup.go` header). An
interrupted run prints ``interrupted — re-run `koto setup` to resume from
here``. A failed step prints ``step "<id>" failed: …``.

## 7. First use

```sh
koto tui            # /new <name> spawns your first agent, /exit detaches (README.md)
koto ctl list       # list groups; see ctl-reference.md
koto setup --check  # health of the install, changes nothing
```

When a group turn fails with 401, run `koto claude-login`. Its flags are
`--status`, `--method oauth|api-key`, `--api-key-stdin` and `--no-verify`
(`claudeLoginUsage`, `daemon/claude_login.go`).

The other top-level subcommands dispatched in `daemon/main.go` are `daemon`,
`fcjail`, `ctl`, `setup`, `claude-login`, `uninstall`, `update`, `install`,
`tui`, `userns-check`, `pki` and `version`. An unknown subcommand prints
``unknown subcommand: <name>`` and exits 2.
