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

## Phase 4 · 重试与安全切换

实现指数退避与 jitter、24 小时窗口、逐收件人重试、明确可安全切换阶段的 failover，以及不确定结果的人工处理流程。保留 Message-ID 与原始 EML，增加重复投递风险测试。

## Phase 5–6 · 管理界面

单管理员认证、SMTP 凭证和 Provider 管理、Dashboard、Messages、邮件详情与时间线。随后实现 raw EML / headers / MIME 和 sandbox HTML 预览。使用真实数据，不以演示数据冒充发送结果。

## Phase 7 · Provider 运维

连接测试已提前实现；后续增加基础健康统计、限流；区分 Provider 故障和收件人错误。成熟后接入 circuit breaker。

## Phase 8–9 · Bounce 与 HTTP API

DSN、suppression、受认证的 bounce ingestion；随后 HTTP 发件、API key 和并发幂等性测试。

## Phase 10 · 生产加固

HTTPS / SMTP TLS 部署指引、指标、备份恢复演练、元数据/事件期限删除、完整删除流程、独立数据库权限、限流与安全测试。EML/debug 清理与备份流程文档已提前实现，不等于生产加固完成。

真实 Provider 验证需要用户提供测试账号与明确收件人；默认开发测试不向外部发送邮件。
