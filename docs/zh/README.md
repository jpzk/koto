本文档为 ../README.md 的翻译；以英文版为准。

# koto 文档

面向 koto 用户（与 group 对话的人）和运维者（安装并运行 daemon 守护进程的人）
的参考文档。文档中的每一条事实陈述都附有来源引用（文件加函数/常量名），可以
在本仓库中直接 grep 验证；在代码含义不明确的地方，文档会直接标注
"unverified" 而不是臆测。若文档与代码不一致，以代码为准——请修正文档。

## 语言

- [English](../README.md)
- [한국어](../ko/README.md)
- [Русский](../ru/README.md)
- [Español](../es/README.md)

## 从这里开始

- [getting-started.md](getting-started.md) — 构建与运行时要求、`make` 各阶段
  （fetch / build / install / wizard）、`install.sh` 安装路线、`koto install`
  的 preflight 预检以及 `koto setup` 向导的各个步骤。
- [architecture.md](architecture.md) — 各层级（主机 CLI、daemon、proxy、
  gVisor 网络网关、fcjail、Firecracker guest）、daemon 启动顺序、VM 规格
  预设和 vsock 端口映射。

## 参考

- [configuration.md](configuration.md) — 代码读取的每个 `/etc/koto/koto.env`
  键和 `KOTO_*` 环境变量及其默认值，外加每个 group 的配置键（model、
  provider、network、size、root、effort、ports、autostart 等）及其可接受
  取值。
- [ctl-reference.md](ctl-reference.md) — 主机上的每个 `koto ctl` 动词和标志，
  以及 guest 内 ctl 控制平面（`/workspace/.cs/ctl`）的动词。
- [acl-and-auth.md](acl-and-auth.md) — PKI、客户端令牌、角色、动词 × 目标
  ACL 以及仅限 admin 的动词。
- [protocol.md](protocol.md) — `koto.proto` 中的 gRPC 服务（所有 RPC 和请求
  字段）以及 guest vsock 各通道的帧格式。

## 使用 group

- [groups-goals-schedules.md](groups-goals-schedules.md) — 聊天会话与队列
  限制、`cs-job` 后台任务、goal 自动驾驶、cron 调度，以及 report/reply
  委派窗口。

## 运维

- [operations.md](operations.md) — daemon 日志与通知、resources 采样器与
  告警、用 `koto update` 升级、用 `koto uninstall` 卸载，以及按确切错误
  信息排障。

## 设计笔记（先前已有，未翻译）

- [firecracker-vsock.md](../firecracker-vsock.md) — Firecracker microVM
  group 与 vsock IPC 的设计笔记。
- [kernel-amzn-vs-vanilla.md](../kernel-amzn-vs-vanilla.md) — 为什么 guest
  内核从 Amazon Linux 源码构建而不是 kernel.org 原版。

## 历史（存档报告，不再维护，未翻译）

这些是时点分析；它们描述的是各自日期当时的代码，可能已经过时。

- [history/SECURITY_19_MAY.md](../history/SECURITY_19_MAY.md) — 后端安全
  分析，2026-05-19。
- [history/ANALYSIS_31_MAY.md](../history/ANALYSIS_31_MAY.md) — 问题分析，
  2026-05-31。
- [history/SECURITY_11_SEP.md](../history/SECURITY_11_SEP.md) — 安全评估
  分诊，2026-09-11。
- [history/FUZZ_09_OCT.md](../history/FUZZ_09_OCT.md) — fuzzing 行动，
  2026-10-09。
