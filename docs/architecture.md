# Architecture

How koto's pieces fit together, from the operator's gRPC client down to the
agent process inside a microVM. Every claim names the file (and usually the
function or constant) it came from, so you can check it with `grep`. The
high-level diagram lives in the repository `README.md` under
`## Architecture`. This page is the code-verified version of that diagram.

Related: [getting-started.md](getting-started.md) (installing it),
[configuration.md](configuration.md) (the knobs),
[acl-and-auth.md](acl-and-auth.md) (the gRPC trust boundary),
[firecracker-vsock.md](firecracker-vsock.md) (the original vsock/jailer
design notes).

## 1. Tiers at a glance

1. **Tier 1, host user.** gRPC clients (`koto tui`, `koto ctl`) and the
   credential store under the state dir's `creds/`. See
   [acl-and-auth.md](acl-and-auth.md).
2. **Tier 2, the koto daemon.** One Go process (`koto daemon`,
   `daemonMain` in `daemon/daemon.go`). It runs rootless as the installing
   user under a systemd unit and holds the gRPC server, the per-group LLM
   proxy, the gVisor network gateway, cron, goals and the ctl plane.
3. **Tier 3, one jailed Firecracker VMM per group.** Each VMM is started
   through `koto fcjail` (`daemon/fcjail.go`) and boots a microVM whose PID 1
   is `fc-agent` (`fcguest/main.go`).

Each layer assumes the one inside it has already been compromised. This is
the README's "defense in depth" statement. The sections below describe what
each layer actually does in code.

## 2. Tier 2: the daemon

### 2.1 Startup order

The order is `daemonMain` in `daemon/daemon.go`:

1. `usernsEnsure()` (`daemon/userns.go`) re-execs the daemon into a fresh
   user namespace. `newuidmap`/`newgidmap` install the mapping from
   `/etc/subuid` and `/etc/subgid`. This is unconditional. On failure the
   daemon prints ``koto: run `koto userns-check` to diagnose`` and exits 1.
   The reason, from the `userns.go` header: fcjail must write a two-entry
   `uid_map` per VMM, and `fcJailFixupPerms` must chown files to the per-VM
   uid. Neither is possible for an unprivileged process outside a userns.
2. `initPaths()` derives the state paths from `HERE` (`kotoHome()`):
   - `groups/` (`ROOT`)
   - `groups.json`, `schedules.json`, `goals.json`
   - `run/` (`SOCK_DIR`)
   - `metrics.jsonl`

   `PORT_BASE` defaults to `8787` and can be overridden by `PROXY_PORT`.
3. `hardenStatePaths()` and `fcClearStalePids()` run, then
   `fcHostMemInit()` and `fcCgroupInit()` (see §4.3), then
   `postureStartup()` (`daemon/posture.go`). The posture step logs every
   host-wide restriction that is *not* in effect.
4. `proxyStart(bind)` starts the in-process LLM proxy (§3).
5. SIGTERM/SIGINT are registered *before* the first VM boots (comment
   "audit 2026-09-11 L146" in `daemon/daemon.go`). This ensures a shutdown
   still runs `fcStopAll`.
6. `spawnEnsure("main", true)` brings up the `main` group. Then
   `go autostartGroups()` boots the groups configured with `autostart=yes`,
   in the background.
7. The gRPC server listens on TCP `KOTO_BIND:KOTO_PORT`. The defaults are
   `127.0.0.1` and `8443`. It uses mTLS (`serverTLSConfig`) plus the
   `authUnary`/`authStream` interceptors. Details are in
   [acl-and-auth.md](acl-and-auth.md).

No host-side ctl FIFOs exist. A group's ctl plane is served over vsock once
its VM is up. The comment above the gRPC listener in `daemon/daemon.go` says
so.

### 2.2 systemd sandbox

The unit text is rendered by `daemon/install.go`. It contains the following
directives:

- `Type=exec` and `ExecStart=/usr/local/bin/koto daemon`.
- `ProtectSystem=strict`.
- `ProtectHome=yes`, or `ProtectHome=tmpfs` plus read-only binds when the
  `claude` binary lives under your home directory (`installHomeScoping`).
- `DevicePolicy=closed` and `DeviceAllow=/dev/kvm rw`.
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`.
- `RestrictNamespaces=user mnt pid net ipc uts cgroup`.
- `LockPersonality=yes` and `RestrictRealtime=yes`.
- `NoNewPrivileges=no`. It must stay `no`: the setuid `newuidmap` would
  otherwise be ignored, as the comment in `installCapBounding` explains.
- `CapabilityBoundingSet=…`, built by `installCapBounding`.
- `CPUQuota`, only when `KOTO_HOST_CPUS` asks for it (`installCPUQuota`).
- `WantedBy=multi-user.target`.

## 3. The LLM proxy (credential injection)

The proxy runs inside the daemon process. It is not a separate binary
(`proxyStart`, `proxyListen` in `daemon/proxy.go`).

- **One unix socket per group, not TCP.** Each group's listener is
  `run/proxy/p<port>.sock` (`proxySockDir`, `proxySockPath`) in an
  owner-only (`0700`) directory. The comment above `proxySockDir` gives the
  reason (audit M1): a loopback TCP port would let any local uid get
  credentialed LLM access. The group's port number (allocated from
  `PORT_BASE`, `allocPort` in `daemon/groups.go`, persisted in
  `groups.json`) still exists. It identifies the group and names the socket
  file.
- **One-to-one invariant.** If you call `proxyListen` with a port that is
  already bound to another group, it fails with
  `port %d already bound for group %q (requested for %q)`.
- **How the guest reaches it.** The guest has no TCP path to the host.
  `ANTHROPIC_BASE_URL` in the guest is `http://127.0.0.1:18888`
  (`fcGuestProxyTCP` in `daemon/fc.go`). fc-agent listens there and forwards
  each connection to host vsock port 9000. The daemon then splices that
  connection into the group's proxy socket (`fcSpliceToProxy`).
- **Upstreams.** The constants are `upstream = "https://api.anthropic.com"`
  and `veniceUpstream = "https://api.venice.ai"`. The group's provider is
  read from `config.json` on every request (`groupProvider`).
- **Credentials.** They are read per request (`currentAPIKey`, `readCreds`).
  The Venice key lives at `venice.key` next to the credentials file
  (`veniceKeyPath`). The guest never sees either.
- **Limits.** A buffered request body is capped at `proxyMaxBody = 64 << 20`
  (64 MiB). `http.Server` uses `ReadHeaderTimeout` 30 s and `IdleTimeout`
  90 s. The `proxyListen` comment explains why these matter: they reap
  connections so the VM's vsock connection table does not fill up.
- **L7 egress gate.** `egressTargetAllowed` in `daemon/proxy.go` applies the
  group's network policy to proxy CONNECT/absolute-form targets.
  `proxySetBootNetwork` records the policy at VM spawn. Per the `fcnet.go`
  header, this gate shares `fcClassifyDst` with the L3 filter (§5).

## 4. Tier 3: the jailed VMM

### 4.1 fcjail

From `daemon/fcjail.go`:

- **How it starts.** The daemon re-execs itself as `koto fcjail <spec>` with
  `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS`. The child
  (`fcjailMain`) does the following, in order:
  1. Bind-mounts only what Firecracker needs into a per-VM chroot: the
     binary, the kernel, the rootfs, this VM's workspace image, `/dev/kvm`,
     `/dev/urandom`, and the VM's vsock socket dir.
  2. Chroots into it.
  3. Sets `PR_SET_NO_NEW_PRIVS`.
  4. Drops to the per-VM uid.
  5. Execs Firecracker.

  Firecracker's built-in seccomp filter stays on, because koto never passes
  `--no-seccomp`.
- **Per-VM uid band.** It starts at `fcJailBaseUID = 30000` and ends at
  `fcJailMaxUID = 60000`. If `/etc/subuid` grants less, the end is clamped,
  and the clamped value is passed to the re-exec'd stages in
  `KOTO_JAIL_MAX_UID` (`jailMaxEnv`).
- **Deriving the uid.** `fcJailUID` computes
  `uid = fcJailBaseUID + (proxyPort − PORT_BASE)`. Because each group has a
  distinct proxy port, each group gets a distinct uid. An out-of-band result
  is an error, not a fallback ("audit 2026-09-11 L6").
- **Paths.** The chroot is `run/fc/<g>.jail` (`fcJailDir`). The sockets are
  in `run/fc/<g>.sock/` (`fcSockDir`). The run dir `run/fc` is `0700`
  (`fcEnsureRunDir`).

### 4.2 The VM

From `daemon/fc.go`:

- **Assets.** The binary, kernel and rootfs are `fcassets/firecracker`,
  `fcassets/vmlinux` and `fcassets/rootfs.img` under `HERE` (`fcBinPath`,
  `fcKernelPath`, `fcRootfsPath`).
- **Persistent disk.** The guest's `/workspace` is
  `groups/<g>/workspace.img` (`fcWorkspaceImg`). It is a per-group ext4
  image attached over virtio-blk. Never mount it on the host while the VM
  runs.
- **Size presets** (`fcSizePresets`). Each entry is vCPU / RAM / disk /
  IO MiB/s / IO ops/s:
  - `small` (the default): 2 / 1024 MiB / 8 GiB / 100 / 15000
  - `medium`: 2 / 2048 MiB / 12 GiB / 150 / 22500
  - `large`: 4 / 4096 MiB / 16 GiB / 200 / 30000
  - `xlarge`: 8 / 8192 MiB / 24 GiB / 250 / 37500

  Every VM also gets a one-time IO burst of `fcIOBurstBytes = 256 << 20`.
  The VMM runs at nice `fcVMNice = 10`.
- **Guest worker.** It runs as uid/gid 1000 (`fcWorkerUID`/`fcWorkerGID`,
  the `node` user).
- **vsock device.** It is configured with `"guest_cid": 3`
  (`fcVMConfig`). The guest dials the host at CID 2 (`hostCID` in
  `fcguest/main.go`).

### 4.3 Resource limits around the VMM

- **Per-VM cgroup** (`daemon/fccgroup.go`). Each VM runs in `vms/<group>`.
  `cpu.weight` scales with vCPU count (`fcCgroupWeightPerVCPU = 50`).
  `memory.high` is set to `mem_mib + fcCgroupMemMarginMiB` (512). It is
  deliberately never `memory.max`. If no writable cgroup tree is available,
  the daemon logs one line, runs VMs without cgroups, and spawn logs
  `cgroup=off`.
- **Fleet memory cap** (`daemon/fchostmem.go`). `vms/` gets `memory.max`.
  `fcHostMemAdmit` refuses to boot a VM that would not fit. The cap comes
  from `KOTO_HOST_MEM_MIB`, where `0` means unlimited. If it is unset, the
  cap defaults to 90% of MemTotal (`fcHostMemDefaultPct = 90`).
- **Connection cap.** A group may have at most `fcMaxConnsPerGroup = 64`
  guest→host vsock connections open across all its listeners. Further
  connections are refused with
  `refusing guest vsock connection: %d already open (per-group cap)`
  (`fcAcceptLoop`).

## 5. Networking

- **`network=none`, the default.** The VM has no NIC. Its only egress is the
  L7 proxy over vsock 9000 (`fc.go` header, `fcnet.go` header).
- **`network=wan|lan|full`.** `fcSpawn` attaches a userspace gVisor gateway
  (`containers/gvisor-tap-vsock`, `fcNetGateway` in `daemon/fcnet.go`) and
  opens the 9003 listener only when `groupNetwork(g) != fcNetNone`. The
  policy is captured at spawn, so changing it needs a restart of the group.
- **Topology** (constants in `daemon/fcnet.go`):
  - guest `192.168.127.2` (`fcNetGuestIP`)
  - gateway `192.168.127.1` (`fcNetGatewayIP`), which also serves DNS
  - subnet `192.168.127.0/24`, MTU 1500

  Ethernet frames are Qemu-framed, with a 4-byte big-endian length prefix
  per frame, on vsock 9003.
- **Frame filter.** `fcEgressConn`, `fcClassifyDst` and `fcDstAllowed` run
  before the netstack sees a packet. There are four destination classes:
  - `ctl`: loopback, link-local and the host's own interface IPs. Always
    dropped.
  - `gw`: `192.168.127.0/24`. Always allowed.
  - `lan`: RFC1918, IPv6 ULA, multicast, limited broadcast and CGNAT
    `100.64/10`. Allowed for `lan` and `full`.
  - `wan`: everything else. Allowed for `wan` and `full`.

  The netstack also blocks `169.254.169.254` (`Ec2MetadataAccess=false`).
- **Caveat** (stated in the `fcnet.go` header). DNS is resolved on the host,
  outside the frame filter. A `lan` guest can therefore still resolve public
  names. Data frames are still filtered by destination IP.

Stale comment: the comment on `fcPortNet` in `daemon/fc.go` says 9003 is
attached "only for internet=full". The code in `fcSpawn` attaches it for
any network value other than `none`. This page follows the code.

## 6. vsock port map

The authoritative map is the header comment of `daemon/fc.go`, together with
the constants `fcPortProxy`, `fcPortLog`, `fcPortCtl`, `fcPortNet`,
`fcPortLogSlot` and `fcPortAgent`.

Firecracker's hybrid vsock works like this. For guest→host connections,
Firecracker connects to `<uds>_<port>` and the daemon listens there. For
host→guest connections, the daemon connects to `<uds>` and sends
`CONNECT <port>\n` (`fcHostDial`). `<uds>` is `run/fc/<g>.sock/v` (`fcUDS`).

Guest → host:

- **9000, API egress** (`fcPortProxy`). Raw TCP is spliced into the group's
  proxy socket (`fcSpliceToProxy`).
- **9001, retired** (`fcPortLog`). This was the raw log stream. `fcSpawn`
  opens no listener for it.
- **9002, ctl plane** (`fcPortCtl`). Framed protobuf `CtlRequest` /
  `CtlResponse` (`fcCtlConn`). In the guest, the shell interface is the
  `/workspace/.cs/ctl` file, and replies are appended to
  `/workspace/.cs/ctl.out` (`fcguest/ctl.go`).
- **9003, L3 ethernet frames** (`fcPortNet`). Only for networked groups
  (§5).
- **9004, turn streams** (`fcPortLogSlot`). One connection per turn.
  `TurnOpen{slot}` comes first, then the turn's events, then `TurnEnd`.
  `fcTurnSink` (`daemon/fcturn.go`) renders them into the host log
  `.cs/log.<slot>`. Marker-shaped text from the guest is escaped, so the
  guest cannot forge markers.

Host → guest:

- **10000, agent RPC** (`fcPortAgent`). One framed `AgentRequest` per
  connection. The `fc.go` header lists the ops `init`, `msg`,
  `exec`, `exec_stream` and `shutdown`, and it names `run_script` and
  `shell_attach` as streaming ops that reply with `AgentFrame`s.

Framing (`daemon/fcframe.go`, mirrored byte-for-byte in `fcguest/frame.go`)
is a `uint32` big-endian length followed by one protobuf message. The
maximum length is checked before allocation:

- `fcFrameMaxCtl = 1 << 20` (1 MiB) for 9002
- `fcFrameMaxGuest = 16 << 20` (16 MiB) for guest→host agent frames
- `fcFrameMaxHost = 64 << 20` (64 MiB) for host→guest requests

More detail is in [protocol.md](protocol.md) and
[firecracker-vsock.md](firecracker-vsock.md).

Stale comment: the header of `fcguest/main.go` still describes 9001 as a
live log FIFO and calls the agent RPC "JSON line". The daemon side
(`fc.go` header, `fcframe.go`) is protobuf-framed and has no 9001 listener.
Where the two disagree, trust `daemon/fc.go`.

## 7. Inside the guest

- **PID 1 is `fc-agent`** (`fcguest/main.go`). It handles early boot
  (mounts, loopback up, and the `/dev/vdb` workspace mount, which formats the
  device only when it is provably blank, per `mountWorkspace`). It also reaps
  zombies.
- **Turns.** The `msg` op runs one turn. The agent writes the files and
  feeds the in-guest `.cs/in` FIFO, which `entrypoint.sh` reads (`fc.go`
  header, "msg" op). The comments call it `sidecar/entrypoint.sh`, but no
  `entrypoint.sh` is tracked in this repository (`git ls-files`), so where it
  comes from is **unverified**.
- **Containers.** The rootfs ships rootless podman, which runs on the guest
  kernel behind KVM (`fcguest/Dockerfile.rootfs`).
- **Host-side files.** The host keeps the per-group files that it reads
  (log, `config.json`, `prompt.md`) under `groups/<g>/` as plain files
  (`fc.go` header).
