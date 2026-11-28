> 2026-11-28: the 42-day age gate was removed by operator decision; sections referring to it are historical.

# Design: an update channel for base OCI images and inner harnesses

Status: DRAFT, for discussion. No runtime code changes in this PR.

Question from the operator: how do we best update (1) the base OCI images
and (2) the inner harnesses (claude, codex, pi) when a new upstream version
appears? Today both need a new koto release.

Line numbers below refer to `origin/develop` at e86da1c.

## 0. Summary

- **Base OCI images:** a signed pins manifest (option A1). It lists digests
  and publish dates, is signed offline with the release key, and the
  daemon fetches it only when the operator asks. The existing
  `koto image update` / `apply` / `rollback` verbs apply it. The age gate
  is checked by the daemon.
- **Harnesses:** split them out of `guest.img` into a separately versioned
  `harness.img`. It is listed in the same signed manifest and pinned per
  group (option B2 + B4). Rollback per group uses the same
  current/previous pin swap that images already have. `fc-agent` stays in
  `guest.img`, so the daemon/agent protocol still ships with the release.
- **Upstream of both:** an age-gated CI bump bot (A2/B1, "option e")
  produces the candidate pins. A human signs the manifest with the offline
  release key. That signature is the trust root. Nothing is ever applied
  automatically.

## 1. Current state

### 1.1 Base OCI images

- **The list of well-known images.** It lives in one place:
  `daemon/ociimage/known.go:41-46` (`knownImages`). The TUI and the setup
  wizard read it through the admin-only Image rpc, op `known`
  (`daemon/ociimage/known.go:3-7`, `daemon/imagecmd.go:149-153`,
  `daemon/imagecmd.go:622`).
- **fedora is pinned by digest. The others are moving tags.**
  `FedoraRef` (`daemon/ociimage/known.go:36`) is
  `docker.io/library/fedora@sha256:be9d65e2…`. debian, ubuntu and kali are
  `:latest` / `kali-rolling:latest` (`daemon/ociimage/known.go:43-45`). The
  tag is resolved to a digest once, at `add`/`update` time, and that digest
  is pinned in the catalog (`daemon/ociimage/known.go:9-12`). The comment
  also says *"koto has no image age gate (operator decision); the created
  date is shown, never enforced"* (`daemon/ociimage/known.go:11-12`). The
  output says the same thing: `created … (…, not enforced)`
  (`daemon/imagecmd.go:326`).
- **Image ops.** `daemon/imagecmd.go:135-215`. The ops are `add` (the ref
  is optional for a known name and comes from `KnownRef`,
  `daemon/imagecmd.go:137-148`), `update` (re-resolves the recorded source
  ref, or takes a new one, `daemon/imagecmd.go:154-162`,
  `daemon/imagecmd.go:233-239`), `ls`, `inspect`, `rm`, `gc`, `apply`,
  and `rollback`. `imageFetch` (`daemon/imagecmd.go:216-218`) pulls the
  image, verifies blob digests (`daemon/ociimage/registry.go:373`; the
  cache is re-hashed and never trusted blindly,
  `daemon/ociimage/registry.go:300-311`), builds layers, then writes the
  catalog under its lock (`daemon/imagecmd.go:288-310`).
- **The catalog.** `daemon/ociimage/catalog.go:41-64` holds name → digest,
  digest → entry (with `Created`, `Ref` as `repo@digest`), and
  `Sources` (the ref to re-resolve). `Default` names the base that
  no-image groups boot (`daemon/ociimage/catalog.go:58`).
- **Per-group pins and rollback already exist.** A group stores
  `image`, `image_digest`, `image_prev`, `image_prev_name` and `image_sys`
  (`daemon/config_image.go:26-30`). `imageApplyOne` writes the pin under
  the group op lock and restarts the group (`daemon/imagecmd.go:428-456`).
  `imageRollback` swaps the current and previous pins and restores the
  archived `sys.img` (`daemon/imagecmd.go:531-602`). The `sys.img`
  machinery is `PrepareSys` / `ArchiveSys`
  (`daemon/ociimage/sys.go:145`, `daemon/ociimage/sys.go:210`).
- **The default base for no-image groups.** These groups follow the
  catalog default name *dynamically* (`daemon/fcimage.go:151-161`). If
  `fedora` is missing, it is added from the compiled-in `FedoraRef`
  (`daemon/fedora_default.go:27-60`, specifically
  `daemon/fedora_default.go:49-50`).
- **The same fedora digest is the build base of the guest runtime.** It
  appears in `fcguest/Dockerfile.rootfs:16` (`FROM …@sha256:be9d65e2…`),
  `fcguest/Dockerfile.guest:21-24` (`ARG FEDORA=…`, `FROM ${FEDORA} AS tc`)
  and `fcguest/Dockerfile.guest:89` (`FROM ${FEDORA} AS tree`). It is
  recorded in the guest's PINS as `fedora_base=` (`fcguest/Dockerfile.guest:188`).

### 1.2 Inner harnesses

- **Harness pins are build ARGs in `fcguest/Dockerfile.guest:101-111`.**
  - `CLAUDE_CODE_VERSION`, `CLAUDE_CODE_SHA512`,
    `CLAUDE_CODE_LINUX_X64_SHA512` (lines 104-106)
  - `CODEX_VERSION`, `CODEX_SHA256`, `CODEX_CODE_MODE_HOST_SHA256`
    (lines 107-109)
  - `PI_VERSION`, `PI_LOCK_SHA256` (lines 110-111)

  The comment at `fcguest/Dockerfile.guest:101-103` says *"same pins as
  Dockerfile.rootfs; bump both together"* and records that the current
  pins are *"NOT age-gated, operator override 2026-11-16"*. The same pins
  are duplicated in `fcguest/Dockerfile.rootfs:50-52`,
  `fcguest/Dockerfile.rootfs:223-225` and `fcguest/Dockerfile.rootfs:271-272`.
- **Fetch and verify happen at build time.** Node is fetched by sha256
  (`fcguest/Dockerfile.guest:96-97`, `fcguest/Dockerfile.guest:118-119`),
  npm by sha512 (`fcguest/Dockerfile.guest:99-100`,
  `fcguest/Dockerfile.guest:123-124`), and claude's wrapper and native
  tarballs come from registry.npmjs.org (`fcguest/Dockerfile.guest:138-140`).
  pi is `npm ci` against a committed lockfile whose sha256 is pinned
  (`fcguest/Dockerfile.guest:162-172`). The PINS record is at
  `fcguest/Dockerfile.guest:185-193`.
- **The guest.img build.** `fcguest/build-guest.sh` writes a
  byte-reproducible ext4. Its `share/koto/MANIFEST` is PINS plus a sha256
  of every file (`fcguest/build-guest.sh:80-84`). `guest.img` holds
  fc-agent, node+npm, claude, codex, pi, tmux and /sidecar
  (`fcguest/Dockerfile.guest:1-5`).
- **Release assets.** `guest.img_<ver>_x86_64.zst`
  (`Makefile:205`). The version is read back out of the image's MANIFEST,
  and `make dist` refuses a guest.img stamped with a different version
  than the release (`Makefile:322-326`). `SHA256SUMS` is signed as
  `SHA256SUMS.asc` with `RELEASE_KEY_FPR=A6E69ED6BC4779F3281721484CC1AFDE15B64EA3`
  (`Makefile:252`, `Makefile:365-368`, `Makefile:459-461`). `install.sh`
  checks the fingerprint, the gpg exit status, the bad/expired/revoked
  status lines, and that `VALIDSIG` names the pinned fingerprint
  (`install.sh:73`, `install.sh:268-284`).
- **How the daemon picks guest.img.** It is exactly one file,
  `fcassets/guest.img` (`daemon/guest_source.go:13`,
  `daemon/guest_source.go:21-28`). Every image group attaches it read-only
  as vda (`daemon/fcimage.go:203-209`), and fc-agent bind-mounts it at
  `/opt/koto` (`fcguest/image.go:47`, `fcguest/image.go:313-315`). There
  is no per-group guest pin. The file is replaced only by `koto install`
  (`daemon/install.go:152`, `daemon/install.go:161`,
  `daemon/install.go:490-497`), which `koto update` runs through the
  embedded install.sh (`daemon/update.go:3-25`).
- **Supply-chain CI.** It already has a 6-week dependency-age gate, but
  only for go.mod (`.github/workflows/supplychain.yml:166-185`,
  `scripts/supplychain/dep-age.sh:1-17`).

## 2. Why each one needs a koto release today

### 2.1 OCI base images

Mechanically, `koto image update fedora <ref@digest>` already moves the
catalog without a release (`daemon/imagecmd.go:154-162`), and no-image
groups follow it (`daemon/fcimage.go:151-161`). It is still
release-bound in practice:

1. **The vetted default digest is a compile-time constant.** `FedoraRef`
   is in `daemon/ociimage/known.go:36`, and fresh installs use it
   (`daemon/fedora_default.go:49-50`). koto has no signed way to say
   "this newer digest is the vetted one" other than shipping a new binary.
2. **The fedora digest is also guest.img's build base.** It appears at
   `fcguest/Dockerfile.guest:22` and `fcguest/Dockerfile.rootfs:16`, and
   guest.img's MANIFEST records it (`fcguest/Dockerfile.guest:188`). The
   bind-mounted runtime in `/opt/koto` is built and tested against that
   userland. Moving the base without the guest breaks the
   "same Fedora userland" claim (`daemon/ociimage/known.go:30-35`), and
   moving the guest requires a release (see 2.2).
3. **The non-fedora names have no age gate.** They resolve `:latest` *at
   update time* (`daemon/ociimage/known.go:9-12`, `daemon/ociimage/known.go:43-45`).
   The only way to update them without a release is to take whatever the
   registry serves today, with no signature, no 6-week gate, and no
   record of who vetted it. That fails operator policy, so in practice
   the safe path is "wait for a release that changes the pin".

### 2.2 Harnesses

1. **Pins are Dockerfile ARGs** (`fcguest/Dockerfile.guest:104-111`), baked
   into one ext4 that also contains fc-agent.
2. **guest.img version = koto version.** This is enforced at
   `Makefile:322-326`. The only signed carrier for its bytes is the
   release's `SHA256SUMS(.asc)` (`Makefile:365-368`).
3. **One host-wide file, replaced only by `koto install`/`koto update`**
   (`daemon/guest_source.go:13`, `daemon/install.go:490-497`). There is no
   per-group harness pin, so there is no canary and no per-group rollback.
   A harness bump moves every group at once on the next boot.
4. **The bytes are coupled.** fc-agent (which speaks the daemon's vsock
   protocol) and the harnesses share an image, so a harness bump cannot
   ship without shipping an agent build and vice versa.

## 3. Options

Each option below gives its mechanism, trust chain, rollback, blast radius
and cost.

### Axis A: base OCI images

**A1. A signed pins manifest (out of band).** Recommended.

- Mechanism: a JSON document `pins.json` plus a detached `pins.json.asc`,
  published as assets of a dedicated GitHub release tag
  (`pins-<YYYYMMDD>-<n>`). It lists every catalog image as `name`,
  `ref@digest`, `published` (the upstream push time) and `vetted` (CI
  check time). The operator runs `koto pins fetch`; the daemon downloads
  the manifest, verifies it, and stages it. `koto pins show` diffs it
  against the catalog. `koto image update <name> --pins` feeds the
  pinned digest into the existing `imageFetch`.
- Trust chain: release key (offline, human) → signature over `pins.json`
  → digest → registry blob digests, checked by the existing client
  (`daemon/ociimage/registry.go:373`). The registry is untrusted beyond
  availability.
- Rollback: the existing per-group `koto image rollback`
  (`daemon/imagecmd.go:531`). The catalog keeps the previous digest until
  `gc`.
- Blast radius: catalog names only. Groups move only on `image apply`,
  except no-image groups, which follow the default; see open question 4.
- Cost: medium. It needs a manifest format, gpg verify code in the
  daemon, two verbs, and a signing step per channel publish.

**A2. Status quo plus an age-gated bump bot (option e).**

- Mechanism: a scheduled CI job resolves each known tag. When a digest is
  at least 42 days old, it opens a PR that bumps `known.go`,
  `Dockerfile.guest` and `Dockerfile.rootfs` together. A patch release
  then ships it.
- Trust chain: unchanged; it is the release's `SHA256SUMS.asc`.
- Rollback: `koto update` to the previous release, which is host-wide.
- Blast radius: host-wide, and it moves the daemon too.
- Cost: low to build. A release per bump is cheap but not free: signing
  and roughly 2 GB of assets per release.

**A3. Operator-local pinning with a local age gate (no channel).**

- Mechanism: `koto image add|update` refuses tags and accepts only
  `ref@sha256:`. It refuses an image whose config `created` (or the
  registry push time) is less than 42 days old, unless the operator
  passes `--allow-young <reason>`. The operator picks digests themselves.
- Trust chain: the operator's own judgement plus the registry. There is
  no koto signature.
- Rollback: the existing per-group rollback.
- Blast radius: per name / per group.
- Cost: low. Weaknesses: `created` is author-controlled and can be faked,
  and every operator repeats the vetting work.

**A4. Re-publish vetted bases in a koto-owned registry, signed there.**

- Mechanism: CI copies vetted digests to `ghcr.io/jpzk/koto-base/*` and
  signs them with an OCI signature scheme.
- Trust chain: the ghcr account plus a signing tool.
- Rollback: same as A1.
- Blast radius: same as A1.
- Cost: high. It adds new third-party tooling (sigstore/cosign or
  notation), which violates "no new deps", and it adds a registry
  account to the trusted base. Rejected.

### Axis B: inner harnesses

**B1. Status quo plus an age-gated bump bot (option e).**

- Mechanism: CI watches npm (`time[<version>]`) and GitHub releases. For
  a version at least 42 days old, it opens a PR that bumps the
  `Dockerfile.guest` and `Dockerfile.rootfs` ARGs, with the hashes taken
  from the registry's `dist.integrity` and the release checksum file.
  CI builds guest.img, and the bump ships in a patch release.
- Trust chain: unchanged.
- Rollback: host-wide, by downgrading koto.
- Blast radius: every group, plus the daemon.
- Cost: low, and the bot is reusable by B2.

**B2. A separate, signed `harness.img`.** Recommended.

- Mechanism: a new `fcguest/Dockerfile.harness` holds only the harness
  stages (node, npm, claude, codex, pi). It is moved out of
  `fcguest/Dockerfile.guest:89-193`; fc-agent, tmux, /sidecar and the CA bundle
  stay in guest.img. `build-harness.sh` reuses the reproducible
  ext4/MANIFEST step (`fcguest/build-guest.sh:76-84`). The output is
  `harness.img_<setver>_x86_64.zst`, where `setver` is a date-serial
  "harness set" version independent of koto's. It is listed by sha256 in
  `pins.json` (same channel as A1), and its MANIFEST carries the
  per-harness versions and hashes. The daemon stores it
  content-addressed (`fcassets/harness/<sha256>.img`, mode 0444).
  Firecracker attaches it read-only as a second drive, and fc-agent
  mounts it at `/opt/koto/harness` and puts its `bin` first in the
  harness PATH. With no harness pinned, the group uses the harnesses still
  in guest.img for one release (compatibility, like the RootfsTools
  fallback at `daemon/guest_source.go:24-26`).
- Trust chain: release key → `pins.json` → harness.img sha256 → its
  internal MANIFEST → upstream sha512/sha256 pins, which were checked at
  build time.
- Rollback: per group, via `harness_prev` (section 4.3).
- Blast radius: exactly the groups the operator applies it to.
- Cost: medium to high. It needs a guest mount change, a daemon drive,
  config keys, a build split, and a compatibility contract between
  fc-agent and the harness CLIs (flags and env that fc-agent uses).

**B3. Install harnesses into sys.img at build time from pinned artifacts.**

- Mechanism: on a fresh `sys.img` (`PrepareSys`), the daemon (or
  fc-agent at first boot) installs npm tarballs whose sha512 comes from
  the signed manifest.
- Trust chain: manifest → sha512 → npm tarball.
- Rollback: through the `sys.img` archive (`daemon/ociimage/sys.go:42`).
- Blast radius: per group.
- Cost: low to build, but the result is worse. sys.img is
  **guest-writable** (and root-writable with root=yes), so the agent can
  modify the harness that drives it. The install needs egress to npm on
  every fresh sys.img. pi's `npm ci` runs lifecycle scripts inside the
  guest. Verification happens once, then the bytes stay mutable, which is
  a TOCTOU by construction. Rejected.

**B4. Per-group harness version pin with a canary group.**

- Mechanism: this is orthogonal to the carrier, and only B2 (or B3) can
  carry it. Group config gets `harness`, `harness_digest`, `harness_prev`
  and `harness_prev_name`, mirroring `daemon/config_image.go:26-30`.
  `koto harness apply <set> --canary <group>` marks the set as
  canary-only until the operator runs `koto harness promote <set>`, which
  unlocks `--all`.
- Trust chain: same as the carrier.
- Rollback: per group.
- Blast radius: one group until promoted.
- Cost: low on top of B2.

**B5. Host-side assembly of harness.img from upstream tarballs.**

- Mechanism: the daemon downloads the manifest-pinned npm tarballs itself
  and assembles the ext4 locally with `mke2fs -d`.
- Trust chain: manifest → upstream hashes, so no koto-built binary image
  is needed.
- Rollback: same as B2.
- Blast radius: same as B2.
- Cost: high. It needs node/npm on the host for pi's lockfile install,
  and it moves the build toolchain into the daemon's runtime. Rejected in
  favour of CI-built, reproducible B2. Operators can still rebuild B2
  locally and compare hashes, because build-guest.sh is byte-reproducible.

## 4. Recommendation

### 4.1 One channel, two consumers

A single signed `pins.json` covers both axes:

```json
{
  "schema": 1,
  "channel": "stable",
  "serial": 17,
  "issued": "2026-11-17T00:00:00Z",
  "expires": "2026-12-17T00:00:00Z",
  "min_koto": "1.13.0",
  "images": [
    {"name": "fedora", "ref": "docker.io/library/fedora@sha256:…",
     "published": "2026-09-20T…Z", "vetted": "2026-11-16T…Z"}
  ],
  "harness_sets": [
    {"set": "2026.11.17-1", "sha256": "…", "size": 312000000,
     "url": "https://github.com/jpzk/koto/releases/download/pins-20261117-1/harness.img_2026.11.17-1_x86_64.zst",
     "zst_sha256": "…",
     "components": [
       {"name": "claude-code", "version": "2.1.300", "sha512": "…", "published": "2026-10-01T…Z"},
       {"name": "codex", "version": "0.160.0", "sha256": "…", "published": "2026-09-30T…Z"},
       {"name": "pi", "version": "1.4.0", "lock_sha256": "…", "published": "2026-09-28T…Z"}
     ],
     "agent_abi": 1}
  ]
}
```

- **Producer:** the bump bot (A2/B1) proposes candidates as a PR against
  a `pins/` directory (as data, not Go constants). CI enforces the age
  gate and, for harness sets, builds `harness.img` reproducibly. After
  merge, a human signs `pins.json` offline with the release key, the same
  way `SHA256SUMS` is signed today (`Makefile:459-461`), and uploads it to
  a `pins-*` release.
- **Consumer:** daemon verbs that are admin-only, CLI/TUI only, and
  **not** on the guest ctl plane:
  - `koto pins fetch [--file <path>]`: download (or air-gap import),
    verify, then stage under `state/pins/staged/`.
  - `koto pins show`: diff of the staged manifest against the catalog and
    each group, with per-artifact age and override status.
  - `koto image update <name> --pins`: an existing verb with a new
    source.
  - `koto harness fetch <set>`, `koto harness apply <set> <group..>|--canary <g>|--all`,
    `koto harness promote <set>` and `koto harness rollback <group>`.
  - TUI: a "Pins" view that drives the same rpc.

### 4.2 Axis A, base images: A1 (+ A2 as the producer)

`knownImages` (`daemon/ociimage/known.go:41-46`) keeps names and labels,
but refs become digest-only. The compiled-in `FedoraRef` stays as the
**offline floor** for a fresh install with no channel. With a verified
manifest staged, the manifest's digests are what `add`/`update --pins` use.
Moving tags (`:latest`) are dropped from the known list. `koto image
update <name>` with no ref keeps working for operator-added names. When
the ref is a known name, it refuses tags and asks for `--pins` or an
explicit `@sha256:`.

Coupling to guest.img (2.1 point 2): the manifest's `fedora` entry carries
the `fedora_base` that the current guest.img and harness sets were tested
against. `pins show` warns when they differ. Each harness set has a
`base_tested` list, which the bot fills from the CI boot test.

### 4.3 Axis B, harnesses: B2 + B4 (+ B1 as the producer)

- fc-agent stays in guest.img and ships with koto releases, because it is
  daemon protocol and must match the daemon.
- harness.img is a separate, content-addressed, read-only drive, pinned
  per group with `harness_prev` rollback. An apply is: write the pin under
  the group op lock (same pattern as `daemon/imagecmd.go:428-456`), then
  restart if the group is running. Rollback swaps current and previous,
  as `imageRollback` does (`daemon/imagecmd.go:531-602`). Old sets stay on
  disk while any group's current *or previous* pin names them (same rule
  as `ociimage.GroupDigests`, `daemon/ociimage/sys.go:275`). `koto harness
  gc` removes the rest.
- `agent_abi` in the manifest is an integer contract (CLI flags and env
  fc-agent relies on). The daemon refuses to apply a set whose
  `agent_abi` the running fc-agent does not support. This is the only
  thing that still forces a koto release, and only when the contract
  itself changes.

### 4.4 Hard requirements and how each is met

1. **Never auto-update to latest.** No timer, no background fetch, no
   apply on boot. `pins fetch` only stages. Every move of a catalog name
   or a group pin is an explicit verb. Moving tags are removed from the
   known list, and a known name refuses a tag ref. A newer staged manifest
   changes nothing until applied.
2. **Every artifact pinned by digest/hash.** Images are `ref@sha256:`,
   with blobs verified by the existing client
   (`daemon/ociimage/registry.go:373`). harness.img has a sha256 for the
   image and for its `.zst`. Its MANIFEST records sha512/sha256 per
   upstream artifact, carried over from the current build pins. The
   manifest itself is covered by its signature.
3. **Nothing newer than 6 weeks unless the operator overrides per
   artifact.** `published` is attested in the signed manifest; CI derives
   it from npm `time[<version>]`, GitHub `published_at`, or the registry
   push time, not from the author-controlled image `created`. The daemon
   **re-checks** `now − published ≥ 42d` at apply time; this is cheap and
   catches a mis-signed manifest. A younger artifact needs
   `--allow-young <artifact>@<version> --reason "<text>"` on the apply
   command line. The override is per artifact (one flag per artifact, no
   wildcard), recorded in the group config next to the pin, and audited.
   A signed manifest may mark an entry `override_suggested` with a reason
   (as `fcguest/Dockerfile.guest:103` does today), but it still needs the local
   flag. Note: this changes the stance in `daemon/ociimage/known.go:11-12`
   ("no image age gate"); see open question 1.
4. **Fail-closed signature verification with the same OpenPGP key.** The
   daemon verifies `pins.json.asc` the same way install.sh does
   (`install.sh:268-284`): it runs the host's `gpg --batch --status-fd`
   against a koto-owned `GNUPGHOME`, requires exit status 0 and `GOODSIG`
   and `VALIDSIG A6E69ED6BC4779F3281721484CC1AFDE15B64EA3`, and rejects
   `EXPKEYSIG|REVKEYSIG|EXPSIG|BADSIG|ERRSIG|NO_PUBKEY`. The fingerprint
   is a constant in the root-owned binary, not in the state dir
   (reasoning from `daemon/update.go:11-20`). A missing gpg, missing
   signature, parse error, expired manifest, or non-increasing `serial`
   all mean nothing is staged. There is no "warn and continue" path. gpg
   is already a host requirement of install/update, so this adds no new
   dependency. (A stdlib `crypto/ed25519` alternative is in open
   question 2.)
5. **Operator-initiated apply through a ctl/TUI verb, never silent.** The
   apply verbs are admin-only rpc ops (like the Image rpc,
   `daemon/ociimage/known.go:6`), exposed in the CLI (`koto pins|harness …`,
   `koto ctl`) and the TUI, and not on the guest ctl allowlist. A guest
   agent cannot apply, fetch or override. Every apply prints from → to
   and restarts only the named groups.
6. **Per-group rollback to the previous pin.** Images keep the existing
   `koto image rollback <group>` (`daemon/imagecmd.go:531`). Harnesses get
   `koto harness rollback <group>`, using `harness_prev` with the same
   swap semantics. Previous artifacts are kept until no current or
   previous pin names them. Rollback is local and needs no network or
   manifest.
7. **Audit event emitted.** Each verb emits an event through the audit
   spool proposed in draft PR #125 (`docs/audit-events.md` there;
   hash-chained JSONL). The events are `pins.fetch` (serial, outcome,
   reason on refusal), `pins.verify`, `image.update`, `harness.apply`,
   `harness.rollback`, `harness.promote` and `pins.override`. The
   `args` field carries artifact, version, digest, from → to, group and
   reason. Refusals are audited too. If #125 has not landed, phase 3
   depends on it, and it does not invent a second log.
8. **No new third-party deps.** Go stdlib (`encoding/json`, `crypto/sha256`,
   `os/exec` for gpg), the existing zstd and ext4 handling, bash for CI
   scripts, and the GitHub API via curl in CI. No cosign, TUF library,
   or OCI artifact tooling.

### 4.5 Threat model

- **Compromised upstream** (docker.io, npm, GitHub release of a harness).
  A swapped tarball or tag cannot change a pinned hash. A *malicious new
  version* published upstream is held back by the 6-week gate (most
  malicious npm releases are found and pulled within days), and CI
  verification runs before a human signs. Residual risk: a slow-burn
  backdoor older than 6 weeks. The age gate makes this less likely but
  does not prevent it.
- **Compromised update channel** (GitHub release assets, the ghcr/GitHub
  account, a network MITM). An attacker without the offline key cannot
  produce a valid `pins.json.asc`, so verification fails and nothing is
  staged. The account can still *withhold* or *replay* manifests, which
  the next two items cover. harness.img bytes from any URL are checked
  against the signed sha256 before they are stored.
- **Downgrade / rollback attack** (replaying an old, validly signed
  manifest to push known-vulnerable pins). `serial` is monotonic. The
  daemon persists the highest verified serial and refuses `serial ≤
  stored`. Within a manifest, applying an artifact older than the group's
  current pin needs `--allow-downgrade`, and it is audited. The local
  `rollback` verb is explicit, operator-only, and uses already-verified
  local bytes, so it is not reachable by the channel.
- **Freeze attack** (withholding new manifests so the host never learns
  of a fix). `expires` (proposed: 30 days) means `pins show` flags the
  staged manifest as stale, and `fetch` refuses an expired one. Running
  groups keep their pins: expiry blocks *new applies from that manifest*
  and never stops existing groups, which matches the fail-closed
  direction. The operator sees "channel stale since …" in the TUI. A
  freeze becomes visible instead of silent.
- **TOCTOU between verify and mount.**
  - Images: the existing layer path re-verifies cached blobs.
  - harness.img: it is verified by streaming sha256 while it is written
    to `<sha>.img.part`, then fsync, rename and `chmod 0444` under a
    daemon-owned 0700 directory. The name *is* the hash. At boot, the
    daemon opens the file and checks size plus a stored
    `(dev, ino, ctime, size)` record taken at verify time. Any mismatch
    means a full re-hash before attach, or refusal. Firecracker attaches
    the drive `is_read_only`, and fc-agent mounts it `ro,nodev,nosuid`.
    The attacker who can rewrite the file between check and open already
    has the daemon's uid, which is out of scope, the same as guest.img
    today. dm-verity would close it fully; see open question 6.
- **Compromised signer workstation / key.** This is out of scope for the
  channel and the same risk as releases today. Mitigations: key
  revocation is honoured (`REVKEYSIG` is refused), and the signing
  ceremony has a human review the PR diff plus the CI evidence.
- **Malicious guest.** The verbs are not reachable from the guest ctl
  plane, and harness.img is read-only, so a guest cannot persist a
  modified harness across a restart, unlike B3.

## 5. Phased plan (PR-sized)

1. **P1: Bump bot (no runtime change).**
   - Files: `scripts/supplychain/bump-harness.sh` and
     `scripts/supplychain/bump-base.sh` (bash + curl + jq on the CI
     runner), plus `.github/workflows/bump.yml` (weekly). It opens a PR
     that edits `fcguest/Dockerfile.guest`, `fcguest/Dockerfile.rootfs`
     and `daemon/ociimage/known.go`.
   - Tests: fixture JSON for npm, GitHub and registry responses; check
     that a version younger than 42 days is never proposed; check hash
     extraction.
   - Value: releases become cheap even if later phases slip.
2. **P2: Manifest format and signing tooling.**
   - Files: `docs/design/pins-schema.md`, `pins/stable.json` (data),
     `scripts/pins/build.sh` and `scripts/pins/verify.sh` (bash, the same
     gpg checks as `install.sh:268-284`), a `make pins-sign` target, and
     CI that recomputes every hash and age in the PR.
   - Tests: verify.sh against good, bad, wrong-fpr, expired, lower-serial
     and malformed fixtures.
3. **P3: Daemon fetch, verify and show (read-only).**
   - Files: `daemon/pins.go` (parse, serial store, expiry),
     `daemon/pins_verify.go` (gpg exec and status parsing),
     `daemon/pinscmd.go`, proto ops, CLI and TUI view, and audit events
     (needs #125).
   - Tests: a table test with a stub gpg on PATH (the existing pattern in
     `daemon/sec172_release_test.go`), serial rollback refused, expired
     manifest refused, and the audit record shape.
4. **P4: Images from pins.**
   - Files: `daemon/imagecmd.go` (`update --pins`, age re-check,
     `--allow-young`), `daemon/ociimage/known.go` (digest-only refs),
     `daemon/fedora_default.go` (prefer the staged manifest over
     `FedoraRef` when present).
   - Tests: `imagecmd_test` cases for tag refusal on known names, a young
     image refused or overridden, and the audit event.
5. **P5: harness.img carrier.**
   - Files: `fcguest/Dockerfile.harness`, `fcguest/build-harness.sh`,
     `fcguest/Dockerfile.guest` (harness stages removed after one
     compatibility release), `fcguest/image.go` (mount the second drive
     at `/opt/koto/harness` and set PATH), `daemon/fcimage.go` and
     `daemon/fc.go` (attach the drive), and `daemon/harness_store.go`
     (content-addressed store with a verify record).
   - Tests: reproducible build (same hash twice), a boot e2e with no
     harness (fallback), with set A, and with set B, plus a TOCTOU test
     that mutates the file after verify and expects a refused boot.
6. **P6: Per-group apply, rollback, canary and promote.**
   - Files: `daemon/config_harness.go`, `daemon/harnesscmd.go`, and the
     TUI.
   - Tests: mirror `imagecmd` apply/rollback tests (lock, concurrent
     destroy, restart only when running), `--all` refused before promote,
     and `agent_abi` mismatch refused.
7. **P7: Cleanup.** Drop the harness ARGs from
   `Dockerfile.rootfs`/`Dockerfile.guest`. Docs: `docs/images.md` and
   `docs/operations.md`.

## 6. Open questions for the operator

1. **Age gate for base images.** `daemon/ociimage/known.go:11-12` records "no image age
   gate (operator decision)". This design enforces 42 days for
   channel-delivered images too. Confirm the policy change. Also, which
   date counts for distro images (registry push time vs CI first-seen)?
2. **Signature scheme.** Should this reuse the OpenPGP release key by
   exec'ing gpg (recommended for phase 3: same key, same checks as
   install.sh), or use a dedicated stdlib `crypto/ed25519` channel key
   embedded in the binary and cross-signed once by the release key?
   ed25519 removes the host-gpg runtime dependency and the parsing of gpg
   status output, at the cost of a second key to guard.
3. **Where the channel lives.** `pins-*` releases on jpzk/koto, or a
   separate repo, so release-asset permissions can be scoped
   separately?
4. **No-image groups.** Today they follow the catalog default dynamically
   (`daemon/fcimage.go:151-161`). After `image update fedora --pins`,
   should they move on their next restart (today's behaviour), or should
   the update require an explicit `apply`, by pinning them at first boot?
5. **Expiry window and signing cadence.** Is 30 days right? A shorter
   window catches a freeze sooner, but each re-sign is a manual key
   ceremony.
6. **dm-verity for harness.img.** This needs the guest kernel option
   (vanilla vs amzn, see `docs/kernel-amzn-vs-vanilla.md`) and a root
   hash on the kernel cmdline. It fully closes TOCTOU. Worth it now or
   later?
7. **Canary mandatory?** Should `--all` always require a promoted set,
   or only when the set contains a component under its override?
