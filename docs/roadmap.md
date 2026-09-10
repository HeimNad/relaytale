# 开发路线图

## Phase 0 · 基础服务（已完成）

- Go 项目、配置、数据库连接和内嵌迁移。
- 核心表与关键约束、健康检查、优雅退出。
- gateway / postgres / caddy Compose。
- 验收：编译、单元测试、真实数据库迁移、Compose 启动与重启。

## Phase 1 · 可靠接收（已实现，含集成测试）

SMTP STARTTLS + AUTH、发件人限制、原始 EML 原子存档、UUIDv7、message / recipients / event 事务。默认拒绝匿名中继。以 Fake SMTP 客户端和故障注入验证“持久化完成才返回 250”。

## Phase 2–4 · 可靠投递与 Flight Recorder

先建设 Fake SMTP Server。实现单 Provider，再增加 PostgreSQL SKIP LOCKED、lease、逐收件人 attempt、阶段时间与错误分类。完整记录状态和事件后，再接 retry、priority failover 和 DELIVERY_UNKNOWN。必须覆盖并发领取、崩溃恢复、final response 丢失、多收件人部分失败。

## Phase 5–6 · 管理界面

单管理员认证、SMTP 凭证和 Provider 管理、Dashboard、Messages、邮件详情与时间线。随后实现 raw EML / headers / MIME 和 sandbox HTML 预览。使用真实数据，不以演示数据冒充发送结果。

## Phase 7 · Provider 运维

连接测试、基础健康统计、限流；区分 Provider 故障和收件人错误。成熟后接入 circuit breaker。

## Phase 8–9 · Bounce 与 HTTP API

DSN、suppression、受认证的 bounce ingestion；随后 HTTP 发件、API key 和并发幂等性测试。

## Phase 10 · 生产加固

HTTPS / SMTP TLS 部署指引、指标、备份恢复演练、保留期清理、独立数据库权限、限流与安全测试。

真实 Provider 验证需要用户提供测试账号与明确收件人；默认开发测试不向外部发送邮件。
