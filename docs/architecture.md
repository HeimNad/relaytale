# 架构与边界

采用 Go 单体进程 + PostgreSQL + 本地文件存储，Caddy 负责 HTTP 入口。Phase 0 使用 Go 标准库路由，无需为两个健康接口增加框架。后续复杂路由可引入 chi。

## 模块演进

只为已实现模块创建代码目录，避免大量空包和假实现。后续依次添加：

| 模块 | 职责 |
| --- | --- |
| smtpserver | TLS、认证、信封收件人、提交原始 DATA |
| message | 接收用例、元数据、状态转换；协调存档与数据库事务 |
| storage | 原始 EML 的持久化、读取、校验；未来对象存储适配 |
| queue | PostgreSQL claim、lease、续租、重试调度、退出 |
| smtpclient | SMTP 阶段记录、原始响应、逐收件人结果 |
| provider | 优先级路由、加密凭证、限流与健康 |
| events | 状态变更与 append-only 事件在同一事务提交 |
| auth / encryption | 单向凭证哈希与 AES-256-GCM 的独立职责 |
| bounce | DSN 解析与收件人关联 |
| web | 管理页面与隔离的邮件预览 |

入口层调用业务用例，业务用例协调存储与事务。SMTP 网络调用不占用数据库事务；worker 先获取租约，再投递，最后事务提交结果。

## 数据模型

首个迁移包含规格要求的九类表，另加 attempt_recipients 保留每轮尝试的逐收件人结果。UUID 在后续业务层统一生成 UUIDv7，数据库不依赖额外 UUID 扩展。HTTP 幂等唯一键为 `(api_key_id, idempotency_key)`。

SMTP 信封无法准确区分 To / Cc / Bcc，新增 `envelope` 类型以免猜测。状态统一使用大写。RCPT 250 仅表示该收件人可进入 DATA，不能立即标记最终 SMTP_ACCEPTED；仍需 final 2xx。

## 可靠性约束

- 原始 EML 写临时文件、fsync、原子 rename，并同步目录，再提交 message / recipients / events / queue 事务，成功后才返回 SMTP 250。
- 文件系统与 PostgreSQL 不存在共同事务。数据库失败时不得确认；超时提交结果不明时不得立即删除 EML，后续孤儿清理须留宽限期。
- SMTP_ACCEPTED 表示 Provider 最终 2xx；只有 DSN / webhook 等证据才能标记 DELIVERED。
- DATA 完成后丢失最终响应，进入 DELIVERY_UNKNOWN，禁止自动 failover。
- 租约过期不能一律重新发送：若旧 worker 可能已经发送 DATA，恢复应进入不确定状态。领取后、明确尚未开始传输的任务才可安全重新排队。Phase 2/4 必须持久化阶段并测试。
- 重试只针对尚未成功的收件人，保持原始 MIME 与 Message-ID。
- 已签名邮件默认逐字节保留。缺少 Message-ID 时，先保存内部追踪 ID；是否添加头由后续显式策略决定，不能以生成 ID 为由破坏透明模式。
- 普通事件不允许 UPDATE/DELETE。未来 retention 采用专门维护身份和受控删除流程；当前不提供删除接口。

## Phase 1 的明确限制

已实现 STARTTLS SMTP listener、认证、EML 存档与事务入队；未实现 HTTP 发件 API、管理员登录、UI、worker 或下游投递。租约等字段只是未来实现的基础，不代表可靠投递已经完成。当前迁移自动执行，仅支持单实例开发启动；多实例部署前需增加迁移互斥和独立迁移发布步骤。

## 版本策略（2026-09-10）

新项目采用核实后的最新稳定 Go 1.27.1，并在 Docker 构建镜像与 go.mod 保持一致。直接依赖固定 pgx v5.11.0、Goose v3.28.0、go.yaml.in/yaml/v3 v3.0.5。YAML 使用维护中的官方继承项目。go.sum 纳入版本控制；不使用浮动 latest 构建，不自动追逐预发布或跨主版本升级。每次升级执行测试、静态检查、容器重建和数据库启动验证。


## Phase 1 实现细节

归档采用同目录临时文件 → fsync → 原子 hard-link 发布（禁止覆盖）→ 删除临时名 → 目录 fsync，具有原子发布语义且保留原始 MIME 字节。所有父目录也同步。收件人、message、三条事件与 PostgreSQL NOTIFY 同一事务提交，并显式启用 synchronous_commit。

SMTP 使用 go-smtp v0.25.0，认证使用 go-sasl 的 PLAIN 实现和兼容 LOGIN 状态机。Argon2id 参数固定，认证计算并发最多 4 个，每个连接最多 5 次 AUTH 尝试。没有匿名或明文认证旁路。完整的网络连接限额与跨连接速率限制仍属于生产加固。

`create-smtp-account` 是仅本地进程可执行的管理命令，生成随机密码并保存 Argon2id 哈希；`init-dev-tls` 显式生成 localhost 自签证书，不自动信任也不覆盖已有密钥。Compose 默认只向本机开放 1587。
