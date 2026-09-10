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

## 当前明确限制

已实现 SMTP 接收、存档、队列 worker 和 Generic SMTP 一次投递；未实现 HTTP 发件 API、管理员登录、UI、自动重试和故障切换。当前迁移自动执行，仅支持单实例开发启动；多实例部署前需增加迁移互斥和独立迁移发布步骤。

## 版本策略（2026-09-10）

新项目采用核实后的最新稳定 Go 1.27.1，并在 Docker 构建镜像与 go.mod 保持一致。直接依赖固定 pgx v5.11.0、Goose v3.28.0、go.yaml.in/yaml/v3 v3.0.5。YAML 使用维护中的官方继承项目。go.sum 纳入版本控制；不使用浮动 latest 构建，不自动追逐预发布或跨主版本升级。每次升级执行测试、静态检查、容器重建和数据库启动验证。


## Phase 1 实现细节

归档采用同目录临时文件 → fsync → 原子 hard-link 发布（禁止覆盖）→ 删除临时名 → 目录 fsync，具有原子发布语义且保留原始 MIME 字节。所有父目录也同步。收件人、message、三条事件与 PostgreSQL NOTIFY 同一事务提交，并显式启用 synchronous_commit。

SMTP 使用 go-smtp v0.25.0，认证使用 go-sasl 的 PLAIN 实现和兼容 LOGIN 状态机。Argon2id 参数固定，认证计算并发最多 4 个，每个连接最多 5 次 AUTH 尝试。没有匿名或明文认证旁路。完整的网络连接限额与跨连接速率限制仍属于生产加固。

`create-smtp-account` 是仅本地进程可执行的管理命令，生成随机密码并保存 Argon2id 哈希；`init-dev-tls` 显式生成 localhost 自签证书，不自动信任也不覆盖已有密钥。Compose 默认只向本机开放 1587。


## Phase 2 投递与租约

新增的 `00002_delivery_queue.sql` 为 attempt 增加领取标识和 `data_armed_at`，并限制一封邮件最多一个 IN_PROGRESS attempt；不修改已提交的首版迁移。

领取事务用 SKIP LOCKED 锁住邮件，再锁 Provider，检查连接容量，在同一事务创建 attempt、设置收件人 SENDING 并写入事件。租约为 Provider 操作总超时加 30 秒缓冲；网络操作受整体 context 和 socket deadline 限制。

在发送 DATA 命令之前，worker 必须用本次领取标识和未过期租约提交 DATA_ARMED。旧 worker 即使恢复执行，也无法跨过这个检查。最终结果事务锁住相同 message，并检查领取标识；不会覆盖其他 worker 或恢复流程的结果。

未进入 DATA_ARMED 的过期任务可以重新领取；已经进入该阶段的一律转为 DELIVERY_UNKNOWN。这可能包含实际尚未发送正文的邮件，是主动保守处理。最终结果提交失败也交给这一恢复路径，避免 Provider 实际接受后再次发送。

完整 SMTP 过程使用标准库 textproto 和 crypto/tls，按阶段发送命令。这样可以明确区分正文写入完成、最终响应、逐 RCPT 结果；对原文仅执行 SMTP 必需的 dot-stuffing，不经 MIME 编辑器。AUTH 阶段响应不保留内容，Provider 密码以 AES-256-GCM 加密并绑定 Provider ID。

存档以受限文件根目录读取，校验大小与 SHA-256 后形成内存快照，防止验证后再次打开得到不同内容。最大内存占用随 worker 数量和邮件大小增长；大邮件流式快照优化留待性能阶段。

本阶段使用三秒间隔的低频轮询，尚未接入 LISTEN 唤醒；吞吐量优先由 Provider 并发额度控制。收到停止信号后停止领取，等待已领取任务；宽限期后取消网络操作，再留最多 12 秒记录结果。默认 Compose 停止宽限为 50 秒。

尚未实现的 hourly/daily limits 如果手动配置在数据库中，该 Provider 不会被选择，避免忽略限制发送。完整限流与 Provider 健康统计留待后续阶段。
