# 开发路线图

## Phase 0 · 基础服务（已完成）

- Go 项目、配置、数据库连接和内嵌迁移。
- 核心表与关键约束、健康检查、优雅退出。
- gateway / postgres / caddy Compose。
- 验收：编译、单元测试、真实数据库迁移、Compose 启动与重启。

## Phase 1 · 可靠接收（已实现，含集成测试）

SMTP STARTTLS + AUTH、发件人限制、原始 EML 原子存档、UUIDv7、message / recipients / event 事务。默认拒绝匿名中继。以 Fake SMTP 客户端和故障注入验证“持久化完成才返回 250”。

## Phase 2 · Provider 投递与队列（已完成，含集成测试）

Generic SMTP（STARTTLS / implicit TLS）、Provider 凭证加密、priority 与并发容量选择、SKIP LOCKED 领取、一次投递、逐收件人结果、基础阶段事件与安全租约恢复。Fake SMTP 和 PostgreSQL 集成测试验证完整链路。默认关闭 worker，没有真实外部发件测试。

为避免不安全的过渡版本，提前实现 DELIVERY_UNKNOWN 的保守判定与禁止自动重发。暂时失败停留 TEMP_FAILED，自动重试尚未开启。

## Phase 3 · Flight Recorder 与运维基础（已完成，含集成测试）

增加 DNS、SMTP greeting/EHLO/AUTH/QUIT 等阶段事件、实际网络耗时与错误分类，过程事件持续写入并幂等补齐。提供 Provider 列表和不发信的连接诊断。

按用户要求采用 AGPL-3.0-only，提前实现运行日志轮转、压缩导出、可预览的 EML/debug 保留期清理、可恢复删除、定时维护和追加式审计；默认关闭自动清理。元数据/事件期限删除、完整 debug transcript 和完善错误枚举仍待后续实现。见 [运维说明](operations.md) 和 [规格对照](phase-1-3-spec-review.md)。

## Phase 3.5 · 真实 Provider 验收（SMTP 层通过，三个收件服务原始邮件已核对）

本轮完成 SpaceMail / PurelyMail × 465 / 587，四封邮件、16 个收件人均取得最终 250；覆盖 Gmail、QQ、Google 托管学校邮箱和 iCloud，尚无 Outlook。Gmail / QQ / iCloud 共 12 份原始邮件已核对，Message-ID、主题及解码 MIME 内容保持；QQ 的 PurelyMail 587 被报告进入垃圾箱。QQ 认证差异与 SpaceMail DMARC 仍待定位，学校邮箱未独立核验。明确 SMTP 接受、实际收信、原文保持和 DKIM 验证的不同证据。见 [验收矩阵](real-world-validation.md)。本地 Fake SMTP 不能替代真实结果。

## Phase 4A · Delivery Decision Engine（已完成本地验收，自动重试默认关闭）

集中、可解释的逐收件人决策，持久化同 Provider 重试、退避/jitter、24 小时与 7 次上限，以及有审计和并发保护的 UNKNOWN 人工处置。自动重试默认关闭，历史暂停邮件不自动启动。计划和验收边界见 [Phase 4 实施计划](phase-4-plan.md)。

## Phase 4B · 安全 Provider 切换

在可证明安全的协议阶段判断候选 Provider，检查发件域、配置、配额与健康；切换单独记录事件。增加跨 Provider 故障注入与重复投递测试。4A 的 FailoverAllowed 仅表达协议许可，不执行切换。

## Phase 4C · Provider Health、熔断与配额

滚动健康、故障归属、熔断/半开、hourly/daily limit 记账；连接诊断已实现。取代原 Phase 7 的排期，优先于管理界面。

## 后续 · 管理界面与 HTTP API

在状态模型稳定后实现管理员认证、Provider/凭证管理、Dashboard、Messages 与时间线，以及受控的 UNKNOWN 操作；随后增加隔离 HTML、EML/headers/MIME 预览。HTTP 发件包含 API key 和并发幂等性测试。对应原 Phase 5–6/9。

## 后续 · DSN / Bounce / Suppression

真实反馈关联、受认证 ingestion、suppression 执行。对应原 Phase 8，安排在投递决策与管理入口稳定之后。

## Phase 10 · 生产加固

HTTPS / SMTP TLS 部署指引、指标、备份恢复演练、元数据/事件期限删除、完整删除流程、独立数据库权限、限流与安全测试。EML/debug 清理与备份流程文档已提前实现，不等于生产加固完成。

真实 Provider 首轮已按用户提供的账号与收件人执行。日常自动测试仍只使用 Fake SMTP；后续外部测试需有明确范围。
