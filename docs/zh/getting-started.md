本文档为 ../getting-started.md 的翻译；以英文版为准。

# 入门指南

本页介绍如何把 koto 从零带到一个运行中的 daemon：主机需要什么、三个阶段
（获取、集成、配置）、决定能否安装的平台检查及其确切提示信息，以及向导
每一步做什么。

每条陈述都附有来源引用（文件加函数、常量或 Makefile target），方便你
grep。当代码注释或 usage 文本与代码不一致时，本页以代码为准并予以说明。

另见：[configuration.md](configuration.md)（koto.env 键、`KOTO_*` 环境
变量、group 配置）、[acl-and-auth.md](acl-and-auth.md)（向导签发的 PKI
和令牌）、[ctl-reference.md](ctl-reference.md)（`koto ctl`）。

## 1. 两种安装方式

### 1.1 管道安装器

```sh
curl -fsSL https://kotovm.com/install.sh | sh
curl -fsSL https://kotovm.com/install.sh | sh -s -- --version 1.0.0
```

`install.sh` 按顺序执行四个阶段（`install.sh` 的头部注释）：

1. fetch：把发布 tag 克隆到临时目录，然后把经过签名验证的产物下载到该目录（等同于 `make fetch`）。
2. install：`koto install`（等同于 `make install`）。
3. setup：`koto setup`（等同于 `make wizard`）。
4. tui：`koto tui`。

在 `curl | sh` 下，stdin 是管道，因此阶段 2 到 4 从 `/dev/tty` 读取。没有
终端时，脚本在阶段 2 之后停止并打印剩余命令（`install.sh` 的头部注释）。

`install.sh` 的标志（`install.sh` `usage()`）：

- `--version X`：要安装的发布版本。默认 `latest`，来自 `VERSION="${KOTO_VERSION:-latest}"`。
- `--no-attach`：在 `koto setup` 之后停止，不打开 TUI。`koto update` 用的就是它。
- `--yes`：接受 `koto install` 和 `koto setup` 的默认值，包括升级时停止 daemon。
- `--download-only`、`--dest DIR`、`--manifest FILE`：只做获取和验证。`--dest` 默认为当前目录。`--manifest` 额外对照一份并非来自该发布的 sha256sum(1) 清单做校验。
- `--next TEXT`：`--download-only` 运行结束后建议执行的命令。
- `-h`、`--help`。

`install.sh` 会拒绝的情况（每一条都是 `install.sh` 中的一个 `die`）：

- 非 Linux/x86_64 的主机：``koto is Linux/x86_64 only (this is <os>/<arch>)``。
- 缺少工具：``<tool> is required but not installed``。所需工具为 `curl gpg zstd sha256sum`，若未指定 `--download-only` 还需要 `git`。
- 空的密钥指纹：``no release key fingerprint — refusing to accept a signature from whatever key turns up``。

验证顺序（`install.sh` 头部注释，"WHAT THIS VERIFIES" 一节）：

1. 按指纹获取签名密钥。
2. 验证 `SHA256SUMS` 上的分离签名，并要求 `VALIDSIG` 行指向该指纹。
3. 从已签名的 `SHA256SUMS` 中取得资产文件名。
4. 解压前校验每个资产，并把所有文件暂存在目标位置旁边，直到整套文件全部通过。

固定（pinned）的指纹是 `A6E69ED6BC4779F3281721484CC1AFDE15B64EA3`
（`install.sh` `KEY_FPR`；Makefile `RELEASE_KEY_FPR`）。`install.sh` 读取的
环境变量覆盖项列于 [configuration.md](configuration.md) §2.5。

### 1.2 从克隆仓库开始，三个阶段

Makefile 文件顶部的注释（"GETTING koto RUNNING IS THREE STAGES"）定义了这些阶段：

1. 获取（ACQUIRE）五个产物，两条路线任选其一：`make fetch` 或 `make build`。
2. 集成（INTEGRATE）到系统中：`make install`。
3. 配置（CONFIGURE）已安装的系统：`make wizard`。

```sh
make fetch && make install && make wizard   # download — minutes
make build && make install && make wizard   # build it yourself — ~40 min
```

`make setup` 是依次执行 `make build`、`make install` 和 `make wizard` 的简写（Makefile target `setup`）。

每个阶段都拒绝做上一阶段的工作，并指出完成该工作的命令。每个阶段都可以
安全地重复运行（Makefile 头部注释）。

## 2. 要求

### 2.1 平台

- 仅支持 linux/amd64。`checkPlatform`（`daemon/setup_checks.go`）对任何其他 `GOOS/GOARCH` 报错，补救提示为：``koto needs linux/amd64: Firecracker microVMs need KVM, and the guest kernel and rootfs are built for x86_64.``
- `/dev/kvm` 必须存在且对所有用户可读写（模式 `&0o006 == 0o006`）。见 `checkKVMAt` 和 §4.2。
- 非特权 user namespace（用户命名空间），以及你的用户的 subuid/subgid 区间。见 §4.3 和 §4.4。

README 列出了已验证可构建和运行的发行版：Fedora 44、Ubuntu 24.04 LTS、
Ubuntu 26.04 LTS 和 Arch（`README.md`，"From sources"）。本页未重新验证
该列表。

### 2.2 仅构建期需要（`make build` 路线）

Docker 或 podman。两者都存在时 Makefile 优先选 docker（Makefile
`CONTAINER ?= $(shell command -v docker … || command -v podman …)`；
`daemon/setup_checks.go` 的 `containerEngine` 顺序相同）。Go 二进制在
digest 固定的镜像 `GO_IMAGE` 内以 `CGO_ENABLED=0` 构建，所以主机不需要
Go 工具链（Makefile `GO_BUILD_RUN`）。两个引擎都不存在时，构建在编译前
就会失败（Makefile `need-container`）：

```
podman or docker is required to build koto (sudo dnf install podman / sudo apt install podman)
```

其他构建工具是 `git`、`make` 和 `curl`（`checkBuildTools`）。README 的
安装命令还为 rootless（免 root）podman 添加了 `passt`（Ubuntu 上还有
`uidmap`）：

```sh
sudo dnf install -y git make podman                  # Fedora 44+
sudo apt install -y git make podman passt uidmap     # Ubuntu 24.04+
sudo pacman -S --needed git make podman passt        # Arch
```

### 2.3 运行时（已安装的 daemon 会调用的外部命令）

以下来自 `daemon/setup_checks.go` 的 `checkRuntimeTools`，附 Fedora 和
Debian 的包名：

- `mkfs.ext4`、`e2fsck`、`resize2fs`：`e2fsprogs`。用于创建、检查和扩容每个 group 的 workspace 镜像。
- `tar`：`tar`。用于向 guest 投递 skills。
- `newuidmap`、`newgidmap`：Fedora 上是 `shadow-utils`，Debian/Ubuntu 上是 `uidmap`。用于映射被 jail 的 microVM 监控进程所使用的每 VM uid/gid。
- `claude`：仅警告。只有 OAuth（订阅）认证需要它，此时 proxy 会调用它刷新 token。其提示要求 `node >= 22` 和 `npm i -g @anthropic-ai/claude-code`。在 Debian 系主机上还会说明 Ubuntu 的 `nodejs` 包是 v18，并建议 `sudo snap install node --classic --channel=22` 或 nvm。

Podman 不是运行时依赖。koto 运行的任何东西都不是容器
（`checkRuntimeTools` 注释；Makefile 头部）。

## 3. 阶段 1：获取

### 3.1 五个产物

Makefile `ARTIFACTS` 和 `daemon/install.go` 的 `artifacts` 都列出了它们：

- `koto`
- `koto-tui`
- `fcassets/firecracker`
- `fcassets/vmlinux`
- `fcassets/rootfs.img`

两条路线产出的正是这五个文件。

### 3.2 `make fetch`

`make fetch` 以 `KOTO_BASE_URL="$(KOTO_DIST_URL)/$(DIST_VERSION)"` 运行
`install.sh --version $(DIST_VERSION) --dest . --manifest artifacts.sha256 --download-only --next "make install"`
（Makefile target `fetch`）。默认值为：

- `KOTO_DIST_URL ?= https://github.com/jpzk/koto/releases/download`
- `DIST_VERSION ?= 1.0.5`
- `DIST_ARCH ?= x86_64`

发布资产以 zstd 压缩，命名为 `<basename>_<version>_<arch>.zst`，例如
`rootfs.img_fedora44_x86_64.zst`（Makefile 中 `DIST_ARCH` 上方的注释）。
仓库中提交的 `artifacts.sha256` 校验的是解压之后、按安装形态存在的产物
（Makefile 中 `MANIFEST` 上方的注释）。

如果 `DIST_VERSION` 是 `unreleased`，fetch 以退出码 1 结束并输出
``no koto release has been published yet, so there is nothing to fetch.``

### 3.3 `make build`

`make build` 按顺序运行这些子 make：`koto`、`koto-tui`、`firecracker`、
`kernel`、`rootfs`（Makefile target `build`）。一旦某个目标的输出比输入
新，它就是空操作。

- `koto`：`go build -trimpath -ldflags "-s -w -X main.kotoVersion=$(KOTO_VERSION)" -o koto ./daemon`。它会先把 `install.sh` 复制到 `daemon/installer/install.sh`，因为安装器被嵌入二进制以供 `koto update` 使用。`KOTO_VERSION` 来自 `git describe --tags --always --dirty`，回退为日期。`koto version` 会打印它（`daemon/main.go`，`kotoVersion`，默认 `"dev"`）。
- `firecracker`：`fcguest/build-firecracker.sh`。
- `kernel`：`fcguest/build-kernel.sh`。
- `rootfs`：`fcguest/build-rootfs.sh`。其输入包含 `fcguest/*.go`、`sidecar/*` 和 `protocol/`（Makefile `FCGUEST_SRC`），所以协议变更会触发 guest 镜像重建。

构建不可复现。从源码构建之所以可信，是因为是你自己构建的；它从不与发布
清单比对（Makefile 中 `dist` 上方的注释；`daemon/install.go` 中
`requireArtifacts` 的注释）。

## 4. 阶段 2：`make install` / `koto install`

`make install` 先运行 `require-artifacts`。五个文件中任何一个缺失时，它
打印 ``missing artifact(s): …`` 和 fetch/build 提示，然后运行
`./koto install`（Makefile targets `install`、`require-artifacts`）。

### 4.1 `koto install` 的标志

以下来自 `daemon/install.go` 的 `installMain` 和 `installUsage`：

- `-state DIR`：状态目录。默认 `/var/lib/koto`（`defaultStateDir`）。取值必须通过 `unitSafeValue`，否则输出 ``-state: …`` 并以退出码 2 结束。
- `-y`：接受默认值，从不提示。
- `-no-color`：禁用颜色。该标志会被解析，但没有列在 usage 文本中。

usage 文本说 install "starts the service"。代码只在升级时启动它。全新
安装运行 `systemctl enable koto` 并打印
``service enabled (not started — `koto setup` starts it)``（`runInstall`）。
向导签发 `server.crt` 之前，daemon 无法启动。

### 4.2 install 做什么，按顺序

以下步骤来自 `daemon/install.go` 的 `runInstall`：

1. 检测升级（`installed()`：`/etc/systemd/system/koto.service` 和 `/etc/koto/koto.env` 同时存在）并打印 ``existing installation detected — upgrading in place``。
2. 拒绝在克隆仓库之外运行：``run this from a koto clone (no Makefile/daemon here)``。也拒绝缺失的产物（`requireArtifacts`）。
3. 拒绝 root：``run as your normal user, not root — the daemon runs as you (sudo is used only for the system files)``。
4. 运行主机要求检查（`preflightGate`，§5）。
5. 停止运行中的 daemon（`installStopDaemon`）。
6. 初始化状态目录（`seedStateDir`）。目录不存在时用 `sudo install -d -o <uid> -g <gid> -m 0750` 创建。已存在的目录必须通过 `stateDirTrusted`：不能是符号链接、必须是目录、必须归你所有、不能有 group/other 写位。失败信息是 ``<dir>: <reason> — remove it or choose another -state``。随后 install 创建 `groups/`、`creds/`、`fcassets/`、`prompts/` 和 `run/`（模式 0750），链接 `.claude -> creds`，并从克隆仓库迁移 `groups/`、`groups.json`、`schedules.json`、`goals.json` 和 `metrics.jsonl`。它从不覆盖已有文件（`migrateState`）。
7. 安装二进制：`sudo install -m 0755 <self> /usr/local/bin/koto`，以及把 `koto-tui` 装到 `/usr/local/bin/koto-tui`。`koto-tui` 缺失时只警告。
8. 如果 AppArmor 限制 userns 且运维者接受了提议（或已存在 koto profile），则安装或刷新 AppArmor profile（`installAppArmorProfile`）。
9. 解析 `claude` CLI（`installClaudeBin`）并写入 `/etc/koto/koto.env`（`writeEnvFile`；键见 [configuration.md](configuration.md)）。
10. 渲染 `/etc/systemd/system/koto.service`（`renderUnit`，`ExecStart=/usr/local/bin/koto daemon`）。unit 有变化时运行 `systemctl daemon-reload`。
11. 升级时运行 `systemctl start koto`。全新安装时运行 `systemctl enable koto`。
12. 从 `git describe` 写入 `<state>/.koto-version`，回退为 `kotoVersion`。

每个特权操作都是一次单独的 `sudo` exec，并在运行前回显（`sudoRun`；
`daemon/install.go` 头部注释）。

## 5. 平台检查（preflight）

`daemon/setup_checks.go` 的 `runPreflight` 按此顺序运行这些检查：平台、
`/dev/kvm`、嵌套 userns、subuid/subgid、构建期工具、运行时工具、磁盘
空间、网络。`koto install` 和 `koto setup --check` 都会运行它们。

构建期一组（容器引擎加 `git`、`make`、`curl`）经过 `advisory()` 处理，
把失败降级为警告，因为 fetch 路线不需要这些工具。

### 5.1 门禁如何决定（`preflightGate`）

- 任何硬失败都会阻止安装。存在一个或多个失败时报告 ``<n> unmet requirement(s) — see the remediation above``。若硬失败与 `/dev/kvm` 同时失败，报告 ``<n> unmet requirement(s), all shown above — fix all of them (<m> blocking, plus /dev/kvm)``。
- 仅 `/dev/kvm` 失败是软失败。提示 ``Continue without KVM?`` 默认否。回答"是"会装出一个 API 能应答、但任何 group 都无法启动的 daemon（`preflightResult.noKVM`）。回答"否"则中止（`errSetupAborted`，文本 `aborted`）。
- 当 userns 为 ``restricted by AppArmor`` 且 `apparmor_parser` 可用时，install 会提议 ``Install an AppArmor profile for koto?``（默认是）。拒绝则失败：``nested user namespaces are unavailable — see the remediation above, or re-run and accept the AppArmor profile``。

### 5.2 `/dev/kvm`（`checkKVMAt`）

- 缺失：详情 ``not present``。补救是在 BIOS/UEFI 中启用虚拟化，或当 koto 本身运行在 VM 中时启用嵌套虚拟化。
- 模式缺少全局 rw：详情 ``mode NNNN — not world-accessible``。加入 kvm 组没有用。被 jail 的 Firecracker VMM 以每 VM 的 uid 运行，附属组集合为空（`kvmRemediation`；见 `daemon/fcjail.go`）。首先打印的补救是一条 udev 规则：

  ```sh
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm
  ```

  替代方案是覆盖 jail uid 区段的按 uid POSIX ACL，该区段由你的 `/etc/subuid` 起点经 `fcJailHostUID(start, fcJailBaseUID)` 计算至 `fcJailBaseUID+ctlMaxSpawn`。补救文本警告了 ACL 的两个问题：systemd-logind 会重写它，而本检查读取的是模式位，因此会一直失败。
- 设备能打开但失败：详情 ``present but not usable: <err>``。补救是 ``sudo usermod -aG kvm $USER``。
- 正常：``usable (mode NNNN)``。

### 5.3 嵌套 user namespace（`checkUserns`）

- `/proc/sys/user/max_user_namespaces` 为 `0`：失败 ``user.max_user_namespaces = 0``。补救是 ``sudo sysctl -w user.max_user_namespaces=15000``，持久化到 `/etc/sysctl.d/`。
- AppArmor 限制非特权 userns（`apparmorUsernsRestricted`）。此时检查会 exec 已安装的二进制（`apparmorProfileWorks(installedKotoBin)`）。成功则结果为 ok：``granted to koto by AppArmor profile (host-wide restriction still on)``。否则失败：``restricted by AppArmor``。回退补救（`usernsRemediation`）是主机全局的 `kernel.apparmor_restrict_unprivileged_userns=0`，持久化到 `/etc/sysctl.d/60-koto.conf`。
- 文件不可读：警告 ``unknown``。
- 其他情况 ok：``allowed (<n>)``。

### 5.4 subuid/subgid（`checkSubuid`）

`/etc/subuid` 和 `/etc/subgid` 都需要有包含你的用户名或 uid 的一行。缺少
其中之一时，检查以 ``no range for <user>`` 失败，补救为：

```sh
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>
podman system migrate
```

### 5.5 容器引擎（`checkContainerEngine`，advisory）

- 两个引擎都没有：``neither found``。
- `<engine> info --format json` 失败：`` `<engine> info` failed: … ``。
- 只有 podman：podman 非 rootless 时警告（``running as root``），pasta 缺失时警告（``not found``，安装 `passt`）。Docker 没有进一步的检查。

### 5.6 磁盘和网络

- `checkDisk` 检查当前工作目录，剩余空间低于 20 GiB 时警告。
- `checkNetwork` 发送 `HEAD https://github.com`，超时 5 秒，只警告。产物已就位时离线安装也能工作。

## 6. 阶段 3：`make wizard` / `koto setup`

`make wizard` 检查 `./koto` 是否可执行。不可执行时打印
``./koto is missing — acquire the artifacts first:`` 并以退出码 1 结束。
否则运行 `./koto setup`（Makefile target `wizard`）。

### 6.1 标志

来自 `daemon/setup.go` 的 `setupMain` 和 `setupUsage`：

- `--check`：医生模式。运行每个 preflight 探测和每个步骤的 `detect`，不做任何更改。就绪时退出码 0，否则 1（`setupCheck`）。它会打印 ``koto is fully set up``、``<n> host requirement(s) unmet`` 或 ``<n> step(s) pending — run `koto setup` to finish``。
- `-y`：接受默认值，从不提示。需要真实输入的步骤会失败。
- `--only ID`：只运行一个步骤。该步骤的 `detect` 被跳过，所以步骤一定会执行。未知 ID 给出 ``no such step "<id>" (see `koto setup --list`)``，退出码 2。usage 文本中的示例 `--only rootfs` 指向一个已不存在的步骤。
- `--from ID`：从该步骤开始。
- `--no-color`：纯文本输出。设置了 `NO_COLOR`、或 `TERM` 为 `dumb` 或为空时同样关闭样式（`colorOK`，`daemon/setup_ui.go`）。
- `--list`：按顺序打印步骤 id。

没有 TTY 且未传 `-y` 或 `--check` 时，setup 以退出码 2 结束：``setup needs
a terminal (or pass -y / --check)``。

状态目录是 `KOTO_HOME`，未设置时为 `/var/lib/koto`（`setupCtx.stateDir`）。
凭据存入 `<state>/creds`。

### 6.2 步骤

步骤按顺序为（`daemon/setup_steps.go` 的 `setupSteps`）：

1. `installed`（已安装的系统）。检测 `installed()`。它从不安装任何东西。koto 未安装时以"先获取、再 `make install`"的提示失败。
2. `pki`（TLS 身份）。检测需要 `creds/` 下的所有这些文件：`ca.crt`、`ca.key`、`server.crt`、`client-tui.crt`、`token-tui`、`client-agent.crt`、`token-agent`。它会询问你是否会从另一台机器访问 daemon，若是，则接收额外的 SAN，例如 `IP:192.168.1.20,DNS:koto.lan`。默认 SAN 为 `DNS:koto-daemon`、`DNS:localhost` 和 `IP:127.0.0.1`（`defaultServerSANs`，`daemon/pki.go`）。它运行 `pkiInit`，然后签发 `tui` 和 `agent` 两个客户端，角色均为 `admin`。已存在的 CA 从不重新生成。见 [acl-and-auth.md](acl-and-auth.md)。
3. `auth`（Anthropic 凭据）。当 `creds/.credentials.json`（OAuth）或 `creds/anthropic-api-key` 存在、或设置了 `ANTHROPIC_API_KEY` 时即视为完成。它运行 `authConnect`，与 `koto claude-login` 相同的流程。在 `-y` 下会失败，因为两种方法都需要人参与。错误指向 ``koto claude-login --api-key-stdin < keyfile``。
4. `service`（启动 daemon）。运行 `sudo systemctl restart koto`。只要本次运行更改过凭据（`credsChanged`），它就报告未完成。向导注释给出的原因是 proxy 在启动时一次性解析凭据。该注释已过时：proxy 现在按请求读取凭据（`daemon/proxy.go` 的 `currentAPIKey` 和 `readCreds`），`claudeLoginUsage` 也是这么说的。因此对基于文件的凭据，重启无害但并非严格必要。来自环境变量的 `ANTHROPIC_API_KEY` 仍在进程生命周期内固定（`envAPIKey`）。
5. `smoke`（冒烟测试）。为身份 `tui` 构建一个指向 `127.0.0.1:8443` 的客户端并调用 `List` RPC。每 2 秒重试，最长 45 秒，然后失败：``daemon did not answer within 45s: … check `systemctl status koto` and `journalctl -u koto -n 50` ``。如果 `/dev/kvm` 不可用则警告 ``no usable /dev/kvm on this host — groups will not boot until that is fixed``。
6. `done`（就绪）。打印 ``koto is installed and running.`` 和后续命令：`koto tui`、`koto ctl list`、`systemctl status koto`、`journalctl -u koto -f`。它还打印状态目录和 `/etc/koto/koto.env`，以及如何用 `koto pki client -creds <state>/creds <name>` 添加客户端。

续跑靠检测，而不是状态文件。重新运行 `koto setup` 会跳过每个 `detect`
已满足的步骤（`daemon/setup.go` 头部）。被中断的运行打印
``interrupted — re-run `koto setup` to resume from here``。失败的步骤打印
``step "<id>" failed: …``。

## 7. 首次使用

```sh
koto tui            # /new <name> spawns your first agent, /exit detaches (README.md)
koto ctl list       # list groups; see ctl-reference.md
koto setup --check  # health of the install, changes nothing
```

当 group 的某个 turn 以 401 失败时，运行 `koto claude-login`。其标志为
`--status`、`--method oauth|api-key`、`--api-key-stdin` 和 `--no-verify`
（`claudeLoginUsage`，`daemon/claude_login.go`）。

对基于密钥的 provider，`koto venice-login` 写入 `creds/venice.key`，
`koto minimax-login` 写入 `creds/minimax.key`（均为 0600，位于 state 目录；
密钥从不进入 guest）。密钥来自 `--key-file F`、`--api-key-stdin` 或终端上的
隐藏输入。写入前先用一个需要认证的请求检查：Venice
`GET /api/v1/api_keys/rate_limits`（Bearer），MiniMax
`GET /anthropic/v1/models`（`x-api-key`）。2xx 则保存；401/403 则拒绝、以 1
退出，且现有密钥文件保持不变。provider 无法访问（网络错误、15s 超时、5xx）
时会带警告保存。`--no-verify` 跳过检查；`--status` 只报告是否已有密钥。
错误信息只含状态码或传输错误，从不含密钥（`providerKeyCheck`，
`daemon/provider_keycheck.go`）。setup 向导的 `auth` 步骤使用同一检查。

`daemon/main.go` 中分发的其他顶层子命令是 `daemon`、`fcjail`、`ctl`、
`setup`、`claude-login`、`openai-login`、`venice-login`、`minimax-login`、`uninstall`、`update`、`install`、`tui`、
`userns-check`、`pki` 和 `version`。未知子命令打印
``unknown subcommand: <name>`` 并以退出码 2 结束。
