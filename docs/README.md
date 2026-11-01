# koto documentation

Reference docs for koto users (people talking to groups) and operators
(people installing and running the daemon). Every factual claim in these
docs carries a source reference (file plus function/const name) so it can
be grepped in this repository; where the code was ambiguous the docs say
"unverified" instead of guessing. If a doc and the code disagree, the code
wins — please fix the doc.

## Languages

- [简体中文](zh/README.md)
- [한국어](ko/README.md)
- [Русский](ru/README.md)
- [Español](es/README.md)

## Start here

- [getting-started.md](getting-started.md) — build and runtime
  requirements, `make` stages (fetch / build / install / wizard), the
  `install.sh` route, `koto install` preflight checks and the `koto setup`
  wizard steps.
- [architecture.md](architecture.md) — the tiers (host CLI, daemon, proxy,
  gVisor network gateway, fcjail, Firecracker guest), daemon startup order,
  VM size presets and the vsock port map.

## Reference

- [configuration.md](configuration.md) — every `/etc/koto/koto.env` key and
  `KOTO_*` environment variable read by the code, with defaults, plus the
  per-group config keys (model, provider, network, size, root, effort,
  ports, autostart, …) and their accepted values.
- [ctl-reference.md](ctl-reference.md) — every `koto ctl` verb and flag on
  the host, and the in-guest ctl plane (`/workspace/.cs/ctl`) verbs.
- [acl-and-auth.md](acl-and-auth.md) — PKI, client tokens, roles, the
  verb × target ACL and admin-only verbs.
- [images.md](images.md) — OCI images for groups: `koto image` commands,
  the `image=` group key, VM drive layout, layer limit, update/apply/rollback.
- [protocol.md](protocol.md) — the gRPC service in `koto.proto` (all RPCs
  and request fields) and the guest vsock framing per channel.

## Using groups

- [groups-goals-schedules.md](groups-goals-schedules.md) — chat sessions
  and queue limits, `cs-job` background jobs, goal autopilot, cron
  schedules, and the report/reply delegation window.

## Running it

- [operations.md](operations.md) — daemon logs and notifications, the
  resources sampler and alerts, upgrading with `koto update`, removing with
  `koto uninstall`, and troubleshooting by exact error message.

## Design notes (pre-existing)

- [firecracker-vsock.md](firecracker-vsock.md) — design notes for the
  Firecracker microVM groups and vsock IPC.
- [kernel-amzn-vs-vanilla.md](kernel-amzn-vs-vanilla.md) — why the guest
  kernel is built from Amazon Linux source rather than kernel.org vanilla.

## History (archived reports, not maintained)

Point-in-time analyses; they describe the code as it was on their date and
may be stale.

- [history/SECURITY_19_MAY.md](history/SECURITY_19_MAY.md) — backend
  security analysis, 2026-05-19.
- [history/ANALYSIS_31_MAY.md](history/ANALYSIS_31_MAY.md) — problem
  analysis, 2026-05-31.
- [history/SECURITY_11_SEP.md](history/SECURITY_11_SEP.md) — security
  assessment triage, 2026-09-11.
- [history/FUZZ_09_OCT.md](history/FUZZ_09_OCT.md) — fuzzing campaign,
  2026-10-09.
