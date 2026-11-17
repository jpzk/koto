---
name: release
description: Cut a koto release — make dist (assets, SBOMs, SHA256SUMS), a DRAFT GitHub release with every asset and SBOM uploaded, operator signature verified and attached, and only then publish. Use whenever a release is drafted, signed, or published.
---

# Releasing koto

jpzk/koto uses GitHub **immutable releases**: once a release is published its
assets are frozen and the tag cannot be reused. Everything a release will ever
carry — the five `.zst` assets, the three CycloneDX SBOMs, `SHA256SUMS` and
`SHA256SUMS.asc` — must therefore be uploaded while it is still a **DRAFT**.
There is no post-publish upload step, for SBOMs or anything else.

Never publish without `SHA256SUMS.asc` attached and verified, even when asked
to "publish": treat that as "finish the draft" and ask for the signature.

## 1. Build and package — `make dist`

From a clean checkout of `release/<version>` with `dist/VERSION` set:

    make build && make dist

`make dist` packages `<name>_<version>_<arch>.zst` for koto, koto-tui,
firecracker, vmlinux and guest.img, then runs `make sbom`, which writes

    koto_<ver>_<arch>.cdx.json  koto-tui_<ver>_<arch>.cdx.json
    fc-agent_<ver>_<arch>.cdx.json

(CycloneDX 1.6 JSON, `cyclonedx-gomod app -std -licenses`, same CGO_ENABLED=0
GO_IMAGE as the build). Firecracker, vmlinux and guest.img's Alpine packages
have **no SBOM** — documented gap, see `scripts/supplychain/sbom.sh`.

`SHA256SUMS` then holds:

    # koto-version: <ver>
    <sha256>  koto_<ver>_<arch>.zst            (… one line per .zst asset)
    # sbom: <sha256>  koto_<ver>_<arch>.cdx.json  (… one per SBOM)

The SBOM hashes are `# sbom:` comment lines on purpose. The installer embedded
in every koto already in the field (`koto update`, and install.sh) takes asset
names from the non-comment lines and refuses anything that is not
`*_<arch>.zst`; a plain SBOM entry would break updating from every existing
release. Comment lines are skipped by that installer and by `sha256sum -c`,
but they are inside the signed file, so `SHA256SUMS.asc` covers the SBOMs too.
`make dist` fails unless exactly three `# sbom:` lines were written.

Check before going on:

    cd dist/<ver> && sha256sum -c SHA256SUMS \
      && sed -n 's/^# sbom: //p' SHA256SUMS | sha256sum -c -

## 2. Draft — `make release-draft`

Creates the DRAFT release targeting `release/<ver>` and uploads every `.zst`,
every `.cdx.json` and `SHA256SUMS`. It refuses if the three SBOMs are missing
or do not match `SHA256SUMS`, and refuses outright if the release is already
published. `FORCE=1` refreshes a draft's assets (`--clobber`).

Verify the draft: every uploaded asset's digest equals the local file's
(`gh release view <ver> --json assets`), including the three `.cdx.json`.

## 3. Sign — operator

The release key does not live on build hosts. Hand `dist/<ver>/SHA256SUMS`
to the operator; they return `SHA256SUMS.asc`
(`gpg --local-user <RELEASE_KEY_FPR> --armor --detach-sign`). Put it at
`dist/<ver>/SHA256SUMS.asc`.

## 4. Attach — `make release`

Verifies GOODSIG + VALIDSIG == `RELEASE_KEY_FPR` in a throwaway GNUPGHOME,
checks the `# koto-version:` line, checks the draft's `SHA256SUMS` is the
signed one byte-for-byte, then uploads `SHA256SUMS.asc`. Still a draft.

## 5. Publish — `make release-publish`

Commit `dist/artifacts.sha256`, tag `<ver>`, push the tag. `release-publish`
checks the tag carries this build's manifest, then takes the release out of
draft. After this nothing can be added — if an SBOM is missing now, the fix is
a new patch release.

After publishing, verify the public download: install.sh `--download-only`
against the release, and the SBOM check from step 1 on the downloaded files.
