译自 ../operations.md；以英文版为准。

# 运维

Day-2 参考：日志去向、资源监控与告警的工作方式、用 `koto update` 升级、
用 `koto uninstall` 移除 koto，以及你最可能遇到的错误信息。每条论断都
标注了其出处的源文件和符号；grep 该符号即可核实。

相关文档：[configuration.md](configuration.md)（环境变量，含 `KOTO_REPO`、
`KOTO_REPO_URL`、`KOTO_HOST_MEM_MIB`）、[ctl-reference.md](ctl-reference.md)
（`logs`、`resources` 动词）、[getting-started.md](getting-started.md)
（安装、preflight 预检错误）、[groups-goals-schedules.md](groups-goals-schedules.md)
（队列与 goal 的错误）、[acl-and-auth.md](acl-and-auth.md)（拒绝类错误）。

## 1. 日志

日志有三个彼此独立的呈现面。

### 1.1 守护进程日志

- 守护进程的每一条日志都经过同一个漏斗 `logDeliver`
  （`daemon/events.go`），由 `emitLog` / `emitLogG` / `emitLogf` /
  `emitLogfG` / `emitLogfQuiet` 调用。
- 每行以 `[<level>] <subsystem>: <msg>` 的形式写到 **stderr**
  （`logDeliver` 中的 `fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", ...)`）。
  在已安装的 systemd 单元下，这些 stderr 进入 journal；安装向导的最后
  一屏指向 `journalctl -u koto -f`（"daemon logs"），冒烟测试失败时则
  指向 `journalctl -u koto -n 50`（`stepHandoff`，
  `daemon/setup_steps.go`）。
- 消息先经 `sanitize` 清洗再经 `flattenInline` 压平，因此一条日志记录
  永远是一行（`logDeliver`）。
- 守护进程在内存中保留最近 **200** 行（`logRingMax`，
  `daemon/events.go`）。新建的 `SubscribeLogs` 流（`koto ctl logs`、TUI
  日志面板）先收到这个环形缓冲，再收到实时行（`SubscribeLogs`，
  `daemon/grpc_server.go`）。每个订阅者有一个 256 条的通道；跟不上的
  订阅者会收到来自子系统 `log` 的一条 `warn`：
  `log stream fell behind: %d line(s) were not delivered to this subscriber`
  （`logDeliver`）。
- 受限（非管理员）身份只能看到归属于其可见 group 的日志行；未归属的行
  （group 为 `""`）被扣留（`logVisible`，
  `daemon/grpc_server.go`）。见 [acl-and-auth.md](acl-and-auth.md)。

### 1.2 error 级日志会变成操作者通知

（`daemon/logalert.go`）

- 经 `emitLogG` 发出的每一条 `error` 级日志还会作为 **high** 严重级的
  通知转发给 `main` group（`forwardLogAlert`，由
  `notifyDeliver(ctlMainGroup, "high", ...)` 投递）。`warn` 及以下只进
  日志。
- 标题格式：`<LEVEL> <subsystem>`，有归属时再加 ` [<group>]`，
  例如 `ERROR fc [dev]`（`logAlertTitle`）。
- 速率限制：每个（subsystem, group）组合一个 token bucket（令牌桶），
  突发 5，每分钟补充一个（`logAlertBurst`、`logAlertRefill`）；另有一个
  机群级桶，突发 20，每 10 秒补充一个（`logAlertGlobalBurst`、
  `logAlertGlobalRefill`）。被抑制的行仍在日志中；丢掉的只是横幅。
- `emitLogfQuiet` 跳过转发；资源告警使用它，因为它们自己排队发送通知
  （`resNotifyOperator`）。
- 每个 group 未投递的通知标记上限为 32
  （`notifyQueueMax`，`daemon/logtail.go`）。该队列在内存中，因此守护
  进程重启会丢掉未投递的标记（`notifyQueueMax` 上方的注释）。

### 1.3 每 group 的转写记录

（`daemon/logtail.go`）

- 每个 group 有一条 group 流，位于 `<workspace>/.cs/log`
  （`groupLogPath`），以及每个并发 slot 一条流，位于
  `<workspace>/.cs/log.<n>`（`slotLogPath`）；`logPaths` 列出 group 流
  加上 `groupSlots` 条 slot 流。`vol(g)` 是该 group 的 workspace 在宿主
  机上的挂载点。
- History 每条流最多读取末尾 4 MiB（`historyTailCap`），每次响应最多
  返回 8 MiB（`historyBytesMax`），分页大小钳制在 5000 个事件
  （`historyLimitMax`）。回滚超过 4 MiB 窗口时以空页结束。
- 不完整（未终止）的行最多缓冲 8 MiB（`tailMaxPartial`）；超出后幸存的
  尾部会被加上前缀 `…[line truncated] `（`tailTruncMark`）。

### 1.4 代理指标

- LLM 代理为每个完成的请求向 `<state>/metrics.jsonl` 追加一条 JSON 记录
  （`METRICS`，在 `daemon/daemon.go` 中设置；`metricsAppend`，
  `daemon/proxy.go`）。
- 它在 256 MiB 时轮转（`metricsMaxBytes`），保留上一代为
  `metrics.jsonl.1`，因此磁盘上的最坏情形约为上限的两倍。轮转失败记
  error 级日志：`metrics rotation failed (%v): %s is past
  its %d MiB ceiling and still growing`。

## 2. 资源监控

（`daemon/resources.go`，线上类型在 `daemon/wire/wire.go`）

### 2.1 采集

- `resourcesLoop` 启动时采样一次，之后每 **30s** 一次
  （`resSampleInterval`）。数字来自宿主机（`stat`、`statfs`、
  `/proc/<pid>/stat`），因此卡死（wedged）或已停止的 guest 仍有数据。
- 例外：guest 内存和 guest 自身的磁盘用量是在每个**运行中的** guest 内
  用一次有界 exec 读取的，即 `resGuestProbe`
  （`stat -f ... /workspace; cat /proc/meminfo`）。该 exec 有 5s 超时
  （`resGuestExecTimeout`），同时最多运行 8 个（`resGuestExecPar`），
  整轮扫描有 20s 期限（`resSweepDeadline`）。绝不会为此启动已停止的 VM。
  运行中的 guest 若错过一次刷新，其上次读数最多保留 3 个采样周期
  （`resGuestMaxAge`）。已停止 VM 的读数立即丢弃。两种情况下之后都读作
  未知（0）（`resGuestRetain`）。
- 每 group 历史：120 个样本（`resRingLen`），即 30s 间隔下的 1 小时。
- 快照缓存 1s（`resSnapTTL`），所以更快的轮询返回相同答案。
- 宿主机文件系统数字是对 `ROOT`（`<state>/groups`）的 `statfs`，空闲数
  采用 `Bavail`（非 root 用户可用的块数）（`hostFSStats`）。

### 2.2 字段（`koto ctl resources`，guest 内 `resources` 动词）

两个平面都构建自同一个 `resourcesSnapshot`
（`resourcesCtlResp`）。JSON 名来自 `wire.GroupResources` /
`wire.HostResources`：

- 每 group：`group`、`running`、`alloc_bytes`、`declared_bytes`、
  `alloc_pct`、`growth_bytes_per_hour`、`growth_span_seconds`、`rss_bytes`、
  `cpu_pct`、`vcpus`、`mem_mib`、`guest_disk_total_bytes`、
  `guest_disk_avail_bytes`、`guest_disk_used_bytes`、`guest_disk_used_pct`、
  `guest_mem_total_bytes`、`guest_mem_avail_bytes`、`guest_mem_used_pct`、
  `mem_committed_mib`。
- 宿主机：`fs_total_bytes`、`fs_free_bytes`、`fs_used_pct`、
  `alloc_total_bytes`、`provisioned_bytes`、`groups`、`running_groups`、
  `mem_cap_mib`、`mem_committed_mib`、`mem_host_total_mib`。

各字段的含义：

- `alloc_bytes` 是 workspace 镜像已分配的块（`st_blocks × 512`，
  `statAllocBytes`）。镜像是稀疏的，且 Firecracker 的 virtio-blk 没有
  discard，所以这个数字实际上**只增不减**。它是宿主机的成本，不是 guest
  的占用程度（`resources.go` 的头部注释）。
- `guest_disk_used_pct` 是 guest 文件系统的占用程度，按 `df` 的方式计算：
  used ÷ (used + avail)，因此 ext4 的保留块两边都不计
  （`resGuestDiskPct`）。要判断一个 group 是否快满，用这个。
- `growth_bytes_per_hour` 在一个尾随 10 分钟窗口上测量
  （`resGrowthWindow`）。在样本历史覆盖完整窗口之前（例如守护进程刚重启
  后），它读作 **0 且 `growth_span_seconds` 为 0**，并且永不为负
  （`resGrowth`）。
- `provisioned_bytes` 是所有 group 的 `size` 预设之和。它超过
  `fs_total_bytes` 是正常的（`HostResources.ProvisionedBytes` 上的
  注释）。
- `mem_cap_mib` 为 0 表示没有机群内存上限（`wire.HostResources` 中的
  注释）。

### 2.3 阈值告警

（`resCheckThresholds`、`resNotifyOperator`）

- 阈值是硬编码的：80% 警告（严重级 `normal`），90% 危急（严重级
  `high`）（`resWarnPct`、`resCritPct`、`resAlertSeverity`）。
- 告警只在级别**上升**时触发。当数值降到触发阈值以下 5 个百分点后，
  该级别重新武装（`resClearMargin`、`resArmedLevel`）。
- 三个对象：
  - 宿主机文件系统（`resSubjectHostFS`）。标题 `Host disk N% full`。
  - 每个运行中 group 的 **guest** 文件系统（`resSubjectDisk`）。标题
    `Group <g> disk N% full`。guest 磁盘大小未知（已停止或不可达）的
    group 不触发任何告警。
  - 每个 group 在最近 10 个样本上平均的 CPU（`resCPUAvgWindow`，
    约 5 分钟），以其自身 vCPU 数为分母的百分比（`resSubjectCPU`）。
    标题 `Group <g> CPU N% sustained`。
- 投递：一条来自子系统 `resources` 的日志（级别 1 为 `warn`，级别 2 为
  `error`，经 `emitLogfQuiet` 发出），加上发给 `main` 的一条通知。若
  main 的通知队列已满，守护进程记日志 `notification backlog
  full, alert dropped: <title>`。

## 3. 升级：`koto update`

（`daemon/update.go`）

```
usage: koto update [--check] [--version X] [-y]
```

标志（`updateMain`）：

- `--check`：只报告。已是最新时退出码 0，存在更新时退出码 **3**
  （`updateExitAvailable`）。
- `--version X`：安装版本 X 而非最新版。只有这样显式点名版本时才允许
  降级。
- `-y`：接受各项提示，包括停止守护进程的那个提示。它以 `--yes` 传给
  安装器。

步骤：

1. **已安装版本。** 若 `/usr/local/bin/koto` 存在
   （`installedKotoBinPath`），采用其 `version` 输出。否则采用当前运行
   二进制自身的 `kotoVersion`（`updateInstalledVersion`）。
2. **最新发行版。** 一次 `HEAD <repo>/releases/latest` 请求；tag 从重定向
   的 `Location` 头读取，而不跟随重定向（`updateLatestRelease`）。仓库是
   `https://github.com/<KOTO_REPO>`（默认 `jpzk/koto`），或设置了
   `KOTO_REPO_URL` 时取其值（`updateRepoURL`）。整个命令有 30s 的
   context，HTTP 客户端在没有 context 时设 20s 超时。
3. **决策**（`updateDecide`；版本按 `MAJOR.MINOR.PATCH` 比较，由
   `parseSemver` 执行，它忽略前导 `v` 及任何 `-`/`+` 后缀）。
   会打印的消息：
   - `koto %s is available (installed: %s)`：有更新版本，执行更新。
   - `up to date: koto %s is the latest release`
   - `up to date: development build %s is based on the latest release %s`：
     形如 `1.0.0-6-gd5150df-dirty` 的构建按其基础发行版计。
   - `release %s replaces development build %s (named explicitly)`
   - `%s is a downgrade from %s (named explicitly)`
   - `installed %s is newer than the latest release %s; nothing to do`
   - `installed %s is not a release build; %s is available`
   - `%q is not a release version`：不更新。
4. **安装**（`updateRun`）。拒绝以 root 运行（`run as your normal
   user, not root — koto installs as you and uses sudo where it must`）。
   把**内嵌于二进制**的 `install.sh`（`embeddedInstaller`，
   `//go:embed installer/install.sh`）写到临时文件，在你的终端上运行
   `sh <file> --version <target> --no-attach [--yes]`。该安装器校验
   发行版，并运行新发行版的 `koto install` 和 `koto setup`（文件头部
   注释）。安装会停止守护进程，连带其中每一个运行中的 microVM。
   失败时：`koto update: the installer failed: <err>`。

安装器之所以内嵌而非下载，是因为下载的安装器会把它所固定（pinned）的
签名密钥指纹与它要校验的发行版经同一信道送达（`update.go` 的头部注释）。
签名校验本身位于 `install.sh` 中，描述见
[getting-started.md](getting-started.md)。

从 1.7.1 或更早版本升级：运行一次安装器，
`curl -fsSL https://kotovm.com/install.sh | sh`。1.7.2 之前的安装器无法读取
`SHA256SUMS` 中已签名的 `# koto-version:` 行，因此从这些版本执行
`koto update` 会失败。从 1.7.2 起，`koto update` 可以正常工作。

## 4. 移除 koto：`koto uninstall`

（`daemon/uninstall.go`）

```
usage: koto uninstall [flags]
```

标志（`uninstallMain`）：

- `-state DIR`：状态目录（默认 `/var/lib/koto`，`daemon/install.go` 中的
  `defaultStateDir`）。
- `--purge`：同时删除状态目录和 `/etc/koto/koto.env`。
- `-n`：试运行（dry run）。每条特权命令打印为 `would: sudo …`，不改变
  任何东西（`uninstallOpts.step`）。
- `-y`：接受默认值。**与 `--purge` 组合时将不加询问直接删除。**
- `-no-color`：代码接受但未列入用法文本。

位置参数被拒绝：`unexpected argument %q — this command
takes flags only`。以 root 运行被拒绝：`run as your normal user, not
root (sudo is used only for the system files)`（`runUninstall`）。

顺序（`runUninstall`）：

1. 若单元文件、`/usr/local/bin/koto`、`/usr/local/bin/koto-tui`、
   `/etc/koto/koto.env` 一个都不存在，它打印 `nothing installed — …` 并
   停止，除非给了 `--purge`。
2. **先停止。** 若单元文件 `/etc/systemd/system/koto.service`
   （`unitPath`）存在**或** `systemctl is-active koto` 返回 `active`
   （`systemctlIsActive`），则依次运行 `systemctl stop koto`、`disable`、
   删除单元文件、`daemon-reload` 和 `reset-failed`。停止失败则中止：
   `stop koto: … (fix, or `sudo systemctl kill koto`, then re-run)`。单元
   设有 `TimeoutStopSec=25`（`daemon/install.go`）。
3. **存活守护进程检查。** 它扫描 `/proc`，寻找状态根目录（取其环境中的
   `KOTO_HOME`，否则取其 cwd）与本目录相同的 `koto daemon` 进程
   （`runningKotoDaemon`、`procKotoHome`）。若仍有一个在运行，则拒绝并
   报 `a koto daemon is still running against …`。带 `-n` 时只警告。
4. 删除 `/usr/local/bin/koto` 和 `/usr/local/bin/koto-tui`。
5. 不带 `--purge` 时，它列出保留的内容（env 文件——其中可能有
   `ANTHROPIC_API_KEY`；状态目录及其 group 数量和 `du -sh` 大小）然后
   停止。之后的 `koto install` 会重新拾起它们。
6. 带 `--purge` 时（`uninstallPurge`）：
   - 状态目录由 `purgeRefusal` 检查。当路径不是绝对路径、是文件系统根、
     是你的家目录，或包含 `.git` / `go.work`（"that is a koto
     clone"）时拒绝（`refusing to delete <dir>: …`）。当路径不含
     `.koto-version`、`groups`、`creds`、`fcassets`、`groups.json` 中的
     任何一个时也拒绝（`no koto state found there …`）。
   - 它询问 `Delete it?`（默认否），除非给了 `-y`。
   - 其（device, inode）在提问前记录，删除前再次核对
     （`dirIdentity`）。若不一致，则拒绝并报 `… is
     not the directory that was checked (it was replaced while the
     confirmation was open) — refusing to delete it`。
   - 状态目录**不用 sudo** 删除（`os.RemoveAll`）；它归用户所有。
   - 删除 `/etc/koto/koto.env`（sudo），然后
     `rmdir --ignore-fail-on-non-empty /etc/koto`。
   
重复运行可完成一次被中断的卸载，因为每一项都会被检测到已不存在并跳过
（头部注释）。

开发检出（checkout）下的对应物在 `Makefile` 中：`make clean` 删除构建
输出、`metrics.jsonl`、`proxy.log` 和 `run`。`make clean-groups` 在要求
你输入 `yes` 后**永久删除** `groups groups.json schedules.json goals.json`
（`FORCE=1` 跳过询问）。`make clean-creds` 删除 `creds`。

## 5. 故障排查

每个条目给出代码中原样出现的消息、其出处，以及应对方法。

### 安装 / 设置

- `daemon did not answer within 45s: … check `systemctl status koto` and
  `journalctl -u koto -n 50``：安装向导的冒烟步骤
  （`daemon/setup_steps.go`）。去读 journal。
- `no usable /dev/kvm on this host — groups will not boot until that is
  fixed`：同一步骤的警告（`checkKVM`）。preflight 预检及其消息列在
  [getting-started.md](getting-started.md) 中。

### 凭据与 LLM 代理（`daemon/proxy.go`）

- `no credentials: run `koto claude-login` (or set ANTHROPIC_API_KEY)`：
  找不到任何 Anthropic 凭据。
- `oauth token refresh failed — turns will 401 until it works (check `koto
  claude-login --status`): …` 以及 `… but the token on disk is still expired —
  the CLI did not rotate it; run `koto claude-login``：OAuth 刷新问题。
  这些是 error 级日志，因此也会以横幅形式出现在 `main` 中。
- `the venice credential is not available on this host; ask the operator`
  （HTTP 503）：某个 group 用 venice provider，但找不到密钥
  （`no venice key: write to …`）。修复：`koto venice-login`。
- `the minimax credential is not available on this host; ask the operator`
  （HTTP 503，`daemon/proxy_minimax.go`）：某个 minimax group，但
  `creds/minimax.key` 缺失或为空。修复：`koto minimax-login`。以 `sk-api-` 开头的密钥按
  `minimax/apikey` 计费；其他任何密钥都是 M Plan 订阅
  （`minimax/subscription`），daemon 在 minimax 调用之后最多每分钟一次从
  `token_plan/remains` 读取其 5 小时和每周窗口。
  koto.env 中的 `KOTO_MINIMAX_REGION=cn` 会切换到中国大陆端点。
- `koto proxy: <METHOD> <path> is outside the endpoint allowlist for
  provider <p> (the proxy relays inference and model metadata only)`（HTTP
  403，`llmRefuse`）：guest 调用了代理不转发的 API 路径。
- `koto proxy: too many concurrent requests for this group`（HTTP 503）：
  该 group 在途请求超过 32，或全局超过 128，且等待 60s 后仍然如此
  （`proxyMaxInflightPerGroup`、`proxyMaxInflightGlobal`、`proxyInflightWait`）。
- `koto proxy: request-body memory budget exhausted; retry shortly`（503）、
  `koto proxy: request body stalled`（408）和 `request body too large or
  unreadable`（413）：读取请求体时的问题。

### 出口（`daemon/proxy.go`）

- `egress denied: this group's network profile is 'none' (a raised profile
  applies on /restart)`（403）：设置 `network`（见
  [configuration.md](configuration.md)），然后重启该 group。
- `egress blocked: target not permitted under this group's network profile`
  （403）：目标超出该 group 网络配置档所允许的范围。
- `koto proxy: too many concurrent egress connections for this group`（503）。

### 启动 group（`daemon/fchostmem.go`）

- `group %s needs %d MiB (mem %d + %d VMM margin) but the fleet cap is %d
  MiB with %d MiB already committed to running VMs — stop a VM, pick a
  smaller size, or raise KOTO_HOST_MEM_MIB`：机群内存上限已满。
- `refusing to boot %s: the host's memory capacity could not be determined …`：
  无法计算上限。

### Turn

- `this conversation's previous turn is wedged and the group could not
  self-heal — /restart the group …`（`errSessionWedged`，
  `daemon/queue.go`）。重启该 group。其他队列限制见
  [groups-goals-schedules.md](groups-goals-schedules.md)。

### 卸载

- `a koto daemon is still running against …`：按消息所说，用
  `sudo systemctl stop koto` 或 `kill <pid>` 停掉它，然后重新运行。
- `refusing to delete …: that is a koto clone, not an installed state dir`：
  `-state` 指向了一个源码检出。把它指向真正的状态目录。
