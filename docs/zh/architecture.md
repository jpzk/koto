译自 ../architecture.md；以英文版为准。

# 架构

本文介绍 koto 各个组件如何协同工作：从操作者的 gRPC 客户端，一直到 microVM（微型虚拟机）
内部的 agent 进程。每一条论断都标注了其出处的文件（通常还包括函数或常量名），
你可以用 `grep` 来核实。总体示意图在仓库 `README.md` 的
`## Architecture` 一节。本页是那张示意图经代码核实后的版本。

相关文档：[getting-started.md](getting-started.md)（安装方法）、
[configuration.md](configuration.md)（各项配置旋钮）、
[acl-and-auth.md](acl-and-auth.md)（gRPC 信任边界）、
[firecracker-vsock.md](../firecracker-vsock.md)（最初的 vsock/jailer
设计笔记）。

## 1. 层级概览

1. **第 1 层：宿主机用户。** gRPC 客户端（`koto tui`、`koto ctl`）以及
   状态目录下 `creds/` 中的凭据存储。见
   [acl-and-auth.md](acl-and-auth.md)。
2. **第 2 层：koto 守护进程。** 一个 Go 进程（`koto daemon`，即
   `daemon/daemon.go` 中的 `daemonMain`）。它以安装用户的身份、在 systemd
   单元下以 rootless（无 root 权限）方式运行，内部承载 gRPC 服务器、按组隔离的
   LLM 代理、gVisor 网络网关、cron、goal 机制和 ctl 控制面。
3. **第 3 层：每个 group 一个被监禁（jailed）的 Firecracker VMM。** 每个 VMM
   经由 `koto fcjail`（`daemon/fcjail.go`）启动，引导一个 microVM，其 PID 1
   是 `fc-agent`（`fcguest/main.go`）。

每一层都假设其内侧的那一层已经被攻破。这就是 README 中"纵深防御
（defense in depth）"的表述。下面各节描述每一层在代码中的实际行为。

## 2. 第 2 层：守护进程

### 2.1 启动顺序

顺序即 `daemon/daemon.go` 中的 `daemonMain`：

1. `usernsEnsure()`（`daemon/userns.go`）将守护进程重新 exec 到一个全新的
   user namespace（用户命名空间）中。`newuidmap`/`newgidmap` 依据
   `/etc/subuid` 和 `/etc/subgid` 安装映射。这一步是无条件的。失败时守护
   进程打印 ``koto: run `koto userns-check` to diagnose`` 并以 1 退出。
   原因见 `userns.go` 头部注释：fcjail 必须为每个 VMM 写入一个双条目
   `uid_map`，且 `fcJailFixupPerms` 必须把文件 chown 给该 VM 专属的 uid。
   对于处在 userns 之外的非特权进程来说，这两件事都做不到。
2. `initPaths()` 从 `HERE`（`kotoHome()`）推导状态路径：
   - `groups/`（`ROOT`）
   - `groups.json`、`schedules.json`、`goals.json`
   - `run/`（`SOCK_DIR`）
   - `metrics.jsonl`

   `PORT_BASE` 默认为 `8787`，可由 `PROXY_PORT` 覆盖。
3. 依次运行 `hardenStatePaths()` 和 `fcClearStalePids()`，然后是
   `fcHostMemInit()` 与 `fcCgroupInit()`（见 §4.3），再是
   `postureStartup()`（`daemon/posture.go`）。posture 这一步会把每一项
   *未*生效的宿主机级限制记入日志。
4. `proxyStart(bind)` 启动进程内 LLM 代理（§3）。
5. SIGTERM/SIGINT 在第一个 VM 启动*之前*就已注册（`daemon/daemon.go` 中
   注释 "audit 2026-09-11 L146"）。这保证关停流程仍会执行 `fcStopAll`。
6. `spawnEnsure("main", true)` 拉起 `main` group。随后
   `go autostartGroups()` 在后台启动所有配置了 `autostart=yes` 的 group。
7. gRPC 服务器监听 TCP `KOTO_BIND:KOTO_PORT`。默认值为
   `127.0.0.1` 和 `8443`。它使用 mTLS（`serverTLSConfig`）加上
   `authUnary`/`authStream` 拦截器。细节见
   [acl-and-auth.md](acl-and-auth.md)。

宿主机侧不存在任何 ctl FIFO。一个 group 的 ctl 控制面在其 VM 启动后通过
vsock 提供。`daemon/daemon.go` 中 gRPC 监听器上方的注释正是这样说的。

### 2.2 systemd 沙箱

单元文件文本由 `daemon/install.go` 渲染。它包含以下指令：

- `Type=exec` 以及 `ExecStart=/usr/local/bin/koto daemon`。
- `ProtectSystem=strict`。
- `ProtectHome=yes`；当 `claude` 二进制位于你的家目录下时，则为
  `ProtectHome=tmpfs` 加只读绑定挂载（`installHomeScoping`）。
- `DevicePolicy=closed` 以及 `DeviceAllow=/dev/kvm rw`。
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`。
- `RestrictNamespaces=user mnt pid net ipc uts cgroup`。
- `LockPersonality=yes` 以及 `RestrictRealtime=yes`。
- `NoNewPrivileges=no`。必须保持 `no`：否则 setuid 的 `newuidmap` 会被
  忽略，`installCapBounding` 中的注释对此有解释。
- `CapabilityBoundingSet=…`，由 `installCapBounding` 构建。
- `CPUQuota`，仅当 `KOTO_HOST_CPUS` 要求时才设置（`installCPUQuota`）。
- `WantedBy=multi-user.target`。

## 3. LLM 代理（凭据注入）

代理运行在守护进程的进程内部，并不是单独的二进制
（`daemon/proxy.go` 中的 `proxyStart`、`proxyListen`）。

- **每个 group 一个 unix socket，而非 TCP。** 每个 group 的监听器是
  `run/proxy/p<port>.sock`（`proxySockDir`、`proxySockPath`），位于一个
  仅属主可访问（`0700`）的目录中。`proxySockDir` 上方的注释给出了理由
  （审计 M1）：若用 loopback TCP 端口，宿主机上任意本地 uid 都能获得带
  凭据的 LLM 访问。group 的端口号（从 `PORT_BASE` 分配，
  `daemon/groups.go` 的 `allocPort`，持久化在 `groups.json`）仍然存在，
  用来标识 group 并为 socket 文件命名。
- **一对一不变式。** 若调用 `proxyListen` 时端口已绑定到另一个 group，
  会失败并报
  `port %d already bound for group %q (requested for %q)`。
- **guest（客户机）如何访问它。** guest 没有通往宿主机的 TCP 路径。
  guest 中的 `ANTHROPIC_BASE_URL` 是 `http://127.0.0.1:18888`
  （`daemon/fc.go` 的 `fcGuestProxyTCP`）。fc-agent 在该地址监听，并把
  每个连接转发到宿主机 vsock 端口 9000。守护进程再把该连接接驳
  （splice）到这个 group 的代理 socket（`fcSpliceToProxy`）。
- **上游。** 常量为 `upstream = "https://api.anthropic.com"` 和
  `veniceUpstream = "https://api.venice.ai"`。group 的 provider 在每次
  请求时从 `config.json` 读取（`groupProvider`）。
- **凭据。** 每次请求时读取（`currentAPIKey`、`readCreds`）。Venice 的
  密钥位于凭据文件旁边的 `venice.key`（`veniceKeyPath`）。guest 永远看
  不到两者中的任何一个。
- **限额。** 带缓冲的请求体上限为 `proxyMaxBody = 64 << 20`
  （64 MiB）。`http.Server` 使用 30 s 的 `ReadHeaderTimeout` 和 90 s 的
  `IdleTimeout`。`proxyListen` 的注释解释了它们的重要性：及时回收连接，
  VM 的 vsock 连接表才不会被填满。
- **L7 出口闸门。** `daemon/proxy.go` 中的 `egressTargetAllowed` 把
  group 的网络策略应用到代理的 CONNECT/绝对形式目标上。
  `proxySetBootNetwork` 在 VM 启动时记录该策略。按 `fcnet.go` 头部注释，
  这个闸门与 L3 过滤器（§5）共享 `fcClassifyDst`。

## 4. 第 3 层：被监禁的 VMM

### 4.1 fcjail

出自 `daemon/fcjail.go`：

- **启动方式。** 守护进程以
  `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS` 将自身重新 exec 为
  `koto fcjail <spec>`。子进程（`fcjailMain`）按顺序做以下事情：
  1. 只把 Firecracker 需要的东西绑定挂载进每个 VM 专属的 chroot：二进制、
     内核、rootfs、该 VM 的 workspace 镜像、`/dev/kvm`、
     `/dev/urandom`，以及该 VM 的 vsock socket 目录。
  2. chroot 进入其中。
  3. 设置 `PR_SET_NO_NEW_PRIVS`。
  4. 降权到该 VM 专属的 uid。
  5. exec Firecracker。

  Firecracker 内建的 seccomp 过滤器保持开启，因为 koto 从不传递
  `--no-seccomp`。
- **每 VM 的 uid 区间。** 起于 `fcJailBaseUID = 30000`，止于
  `fcJailMaxUID = 60000`。若 `/etc/subuid` 授予的范围更小，上限会被收紧
  （clamp），收紧后的值通过 `KOTO_JAIL_MAX_UID`（`jailMaxEnv`）传给重新
  exec 的后续阶段。
- **uid 的推导。** `fcJailUID` 计算
  `uid = fcJailBaseUID + (proxyPort − PORT_BASE)`。因为每个 group 的代理
  端口各不相同，所以每个 group 得到不同的 uid。越界的结果是报错，而不是
  回退（"audit 2026-09-11 L6"）。
- **路径。** chroot 是 `run/fc/<g>.jail`（`fcJailDir`）。socket 位于
  `run/fc/<g>.sock/`（`fcSockDir`）。运行目录 `run/fc` 为 `0700`
  （`fcEnsureRunDir`）。

### 4.2 VM

出自 `daemon/fc.go`：

- **资产。** 二进制、内核和 rootfs 分别是 `HERE` 下的
  `fcassets/firecracker`、`fcassets/vmlinux` 和 `fcassets/rootfs.img`
  （`fcBinPath`、`fcKernelPath`、`fcRootfsPath`）。
- **持久磁盘。** guest 的 `/workspace` 是
  `groups/<g>/workspace.img`（`fcWorkspaceImg`），是每个 group 一个的
  ext4 镜像，通过 virtio-blk 挂接。VM 运行时绝不要在宿主机上挂载它。
- **规格预设**（`fcSizePresets`）。每条为 vCPU / 内存 / 磁盘 /
  IO MiB/s / IO ops/s：
  - `small`（默认）：2 / 1024 MiB / 8 GiB / 100 / 15000
  - `medium`：2 / 2048 MiB / 12 GiB / 150 / 22500
  - `large`：4 / 4096 MiB / 16 GiB / 200 / 30000
  - `xlarge`：8 / 8192 MiB / 24 GiB / 250 / 37500

  每个 VM 还获得一次性的 IO 突发额度 `fcIOBurstBytes = 256 << 20`。
  VMM 以 nice 值 `fcVMNice = 10` 运行。
- **guest 内的工作进程。** 以 uid/gid 1000 运行
  （`fcWorkerUID`/`fcWorkerGID`，即 `node` 用户）。
- **vsock 设备。** 配置为 `"guest_cid": 3`
  （`fcVMConfig`）。guest 以 CID 2 拨号宿主机（`fcguest/main.go` 中的
  `hostCID`）。

### 4.3 VMM 周边的资源限制

- **每 VM 的 cgroup**（`daemon/fccgroup.go`）。每个 VM 运行在
  `vms/<group>` 中。`cpu.weight` 随 vCPU 数量缩放
  （`fcCgroupWeightPerVCPU = 50`）。`memory.high` 被设为
  `mem_mib + fcCgroupMemMarginMiB`（512）。刻意不设 `memory.max`。若没有
  可写的 cgroup 树，守护进程记一行日志，让 VM 在无 cgroup 的情况下运行，
  spawn 日志中记 `cgroup=off`。
- **机群内存上限**（`daemon/fchostmem.go`）。`vms/` 上设置
  `memory.max`。`fcHostMemAdmit` 拒绝启动放不下的 VM。上限来自
  `KOTO_HOST_MEM_MIB`，其中 `0` 表示不设限。若未设置，上限默认为
  MemTotal 的 90%（`fcHostMemDefaultPct = 90`）。
- **连接上限。** 一个 group 在其所有监听器上合计最多可保持
  `fcMaxConnsPerGroup = 64` 条 guest→host 的 vsock 连接。超出的连接被
  拒绝，并报
  `refusing guest vsock connection: %d already open (per-group cap)`
  （`fcAcceptLoop`）。

## 5. 网络

- **`network=none`，默认值。** VM 没有网卡（NIC）。它唯一的出口是经
  vsock 9000 的 L7 代理（`fc.go` 头部、`fcnet.go` 头部）。
- **`network=wan|lan|full`。** `fcSpawn` 挂接一个用户态 gVisor 网关
  （`containers/gvisor-tap-vsock`，`daemon/fcnet.go` 的
  `fcNetGateway`），且仅当 `groupNetwork(g) != fcNetNone` 时才打开 9003
  监听器。策略在 spawn 时固化，因此修改策略需要重启该 group。
- **拓扑**（`daemon/fcnet.go` 中的常量）：
  - guest `192.168.127.2`（`fcNetGuestIP`）
  - 网关 `192.168.127.1`（`fcNetGatewayIP`），同时提供 DNS
  - 子网 `192.168.127.0/24`，MTU 1500

  以太网帧采用 Qemu 式封装：vsock 9003 上每帧带 4 字节大端序长度前缀。
- **帧过滤器。** `fcEgressConn`、`fcClassifyDst` 和 `fcDstAllowed` 在
  netstack 看到数据包之前先行执行。目的地分四类：
  - `ctl`：loopback、链路本地地址及宿主机自身接口 IP。一律丢弃。
  - `gw`：`192.168.127.0/24`。一律放行。
  - `lan`：RFC1918、IPv6 ULA、组播、受限广播以及 CGNAT
    `100.64/10`。对 `lan` 和 `full` 放行。
  - `wan`：其余一切。对 `wan` 和 `full` 放行。

  netstack 还会拦截 `169.254.169.254`（`Ec2MetadataAccess=false`）。
- **注意事项**（`fcnet.go` 头部注明）。DNS 在宿主机上解析，处于帧过滤器
  之外。因此一个 `lan` guest 仍能解析公网域名。但数据帧仍按目的 IP 过滤。

陈旧注释：`daemon/fc.go` 中 `fcPortNet` 上的注释说 9003 仅在
"internet=full" 时挂接。而 `fcSpawn` 的代码对任何非 `none` 的 network
取值都会挂接它。本页以代码为准。

## 6. vsock 端口映射

权威的端口映射在 `daemon/fc.go` 的头部注释，连同常量
`fcPortProxy`、`fcPortLog`、`fcPortCtl`、`fcPortNet`、
`fcPortLogSlot` 和 `fcPortAgent`。

Firecracker 的 hybrid vsock 工作方式如下。对于 guest→host 的连接，
Firecracker 连接到 `<uds>_<port>`，守护进程在那里监听。对于 host→guest
的连接，守护进程连接到 `<uds>` 并发送
`CONNECT <port>\n`（`fcHostDial`）。`<uds>` 是 `run/fc/<g>.sock/v`
（`fcUDS`）。

guest → host：

- **9000，API 出口**（`fcPortProxy`）。原始 TCP 被接驳到该 group 的
  代理 socket（`fcSpliceToProxy`）。
- **9001，已退役**（`fcPortLog`）。这曾是原始日志流。`fcSpawn` 不再为它
  打开监听器。
- **9002，ctl 控制面**（`fcPortCtl`）。帧式 protobuf `CtlRequest` /
  `CtlResponse`（`fcCtlConn`）。在 guest 内，shell 接口是
  `/workspace/.cs/ctl` 文件，回复追加到
  `/workspace/.cs/ctl.out`（`fcguest/ctl.go`）。
- **9003，L3 以太网帧**（`fcPortNet`）。仅用于联网的 group（§5）。
- **9004，turn 流**（`fcPortLogSlot`）。每个 turn 一条连接。先是
  `TurnOpen{slot}`，然后是该 turn 的事件，最后是 `TurnEnd`。
  `fcTurnSink`（`daemon/fcturn.go`）把它们渲染进宿主机日志
  `.cs/log.<slot>`。来自 guest 的形似标记（marker）的文本会被转义，
  因此 guest 无法伪造标记。

host → guest：

- **10000，agent RPC**（`fcPortAgent`）。每条连接一个帧式
  `AgentRequest`。`fc.go` 头部列出了操作 `init`、`msg`、
  `exec`、`exec_stream` 和 `shutdown`，并指出 `run_script` 和
  `shell_attach` 是以 `AgentFrame` 回复的流式操作。

帧格式（`daemon/fcframe.go`，与 `fcguest/frame.go` 逐字节一致）为一个
`uint32` 大端序长度，后跟一条 protobuf 消息。长度上限在分配内存之前就
检查：

- `fcFrameMaxCtl = 1 << 20`（1 MiB），用于 9002
- `fcFrameMaxGuest = 16 << 20`（16 MiB），用于 guest→host 的 agent 帧
- `fcFrameMaxHost = 64 << 20`（64 MiB），用于 host→guest 的请求

更多细节见 [protocol.md](protocol.md) 和
[firecracker-vsock.md](../firecracker-vsock.md)。

陈旧注释：`fcguest/main.go` 的头部仍把 9001 描述为在用的日志 FIFO，并把
agent RPC 称作 "JSON line"。守护进程一侧（`fc.go` 头部、`fcframe.go`）
是 protobuf 帧式的，且没有 9001 监听器。两者相左时，以 `daemon/fc.go`
为准。

## 7. guest 内部

- **PID 1 是 `fc-agent`**（`fcguest/main.go`）。它处理早期引导（各类挂载、
  启用 loopback，以及 `/dev/vdb` 的 workspace 挂载——仅当设备可证明为
  空白时才格式化，见 `mountWorkspace`）。它还负责收割僵尸进程。
- **Turn。** `msg` 操作运行一个 turn。agent 写入相关文件并向 guest 内的
  `.cs/in` FIFO 投喂，由 `entrypoint.sh` 读取（`fc.go` 头部的 "msg"
  操作）。注释称其为 `sidecar/entrypoint.sh`，但本仓库中并没有跟踪任何
  `entrypoint.sh`（`git ls-files`），因此其来源**未经核实**。
- **容器。** rootfs 自带 rootless podman，运行在 KVM 之后的 guest 内核上
  （`fcguest/Dockerfile.rootfs`）。
- **宿主机侧文件。** 宿主机把它需要读取的每 group 文件（日志、
  `config.json`、`prompt.md`）以普通文件形式保存在 `groups/<g>/` 下
  （`fc.go` 头部）。
