# Phase 1–3 开发规格对照与设计复盘

日期：2026-09-10。依据：[产品与工程开发规格说明书 v0.1](../Mail%20Gateway%20-%20Email%20Flight%20Recorder%20开发规格说明书.md)。范围为 Phase 0 基础骨架以及 Phase 1–3；不将未来路线图或只有表结构的功能算作已实现。

结论：当前实现遵循 SMTP 原生网关、先持久化再确认、逐收件人状态、原文留档和追加式事件这些核心要求。前三个功能阶段已具备可测试的后端链路；有一些比原阶段顺序更早落地的可靠性措施，也存在明确的简化和未完成项。**当前尚不满足规格 §90 的完整 MVP，更不等于生产 v1。**

## 1. 阶段与验收映射

| 阶段 | 原规格 | 当前实现与证据 | 判断 |
| --- | --- | --- | --- |
| Phase 0（§79） | Go、配置、DB、迁移、Docker、健康 | `cmd/gateway`、`internal/config`、`internal/database`、Compose；数据库及存储就绪检查 | 已实现，包含于 phase-1 基线 |
| Phase 1（§80） | SMTP AUTH、信封、DATA、EML、messages/recipients | `smtpserver`、`message`、`storage`；TLS 接收、账号发件人限制、原文同步后事务入队 | 已实现；协议客户端和 PostgreSQL 集成测试替代手工 swaks 作为主要回归证据 |
| Phase 2（§81） | Generic SMTP、队列读取、SMTP_ACCEPTED | `provider`、`encryption`、`queue`、`smtpclient`；STARTTLS / implicit TLS、priority、并发额度、逐人结果 | 本地 Fake Provider 全链路已验收；真实 SpaceMail/PurelyMail 尚未验收 |
| Phase 3（§82） | attempts、events、耗时、错误分类 | 连续事件、序号去重、DNS/连接/TLS/AUTH/RCPT/DATA/最终响应耗时、阶段错误分类 | 核心已实现；完整错误枚举、全量 transcript 和 UI 聚合不在当前完成范围 |
| 用户追加运维需求 | AGPLv3、日志清理/导出 | LICENSE、日志轮转、JSONL.gz、运行日志导出、安全 EML/debug 清理、维护审计、运维文档 | 已实现基础；未把所有生产运维能力一并宣称完成 |

## 2. 关键要求遵循情况

| 规格章节 | 要求 | 实现与边界 |
| --- | --- | --- |
| §1–4、§95 | 自托管 SMTP 控制面；不要第一版堆消息中间件 | Go 单体 + PostgreSQL + 本地文件 + Caddy；没有 Redis/Kafka/Kubernetes，也没有营销功能或直接公网 MX 投递 |
| §3、§59 | Go 1.24+，YAML/env/CLI，结构化日志 | 固定 Go 1.27.1 与依赖版本；服务配置按 CLI > env > YAML > 默认。管理子命令使用自身 flags/env，未统一复用服务 YAML 配置；本次同时修正 README 中 Compose exec 缺少 gateway 可执行文件的命令示例 |
| §6–10、§67 | 分离消息、收件人、尝试和事件 | 首版迁移包含九类基础表，额外 `attempt_recipients` 保存每轮收件人结果；表存在不代表 API key、suppression 或 health 功能已经可用 |
| §11–12 | 不能把 SMTP_ACCEPTED 叫 DELIVERED；不确定结果禁止盲目切换 | final 2xx 才认定 SMTP_ACCEPTED；DATA 后失去响应或已许可 DATA 的租约过期转 DELIVERY_UNKNOWN，不自动重发 |
| §13–14、§46、§49 | STARTTLS、AUTH PLAIN/LOGIN、发件人限制、密码哈希 | 入站强制 TLS/AUTH、Argon2id、信封和 Header From 都检查；Compose 默认仅本机开放；没有匿名 trusted-network 旁路 |
| §14、§17、§98 | 文件持久化 + DB commit 后才返回 250 | 原文临时文件同步、禁止覆盖的原子发布、目录同步；message/recipients/events/queue 同事务且显式同步提交；失败不返回成功 |
| §18–20、§35 | Generic SMTP、Provider 加密凭证、优先级 | AES-256-GCM 并绑定 Provider ID，主密钥不入库；按发件域、priority、并发容量选择；暂不支持明文 Provider |
| §23–24、§64 | SKIP LOCKED、租约、并发隔离与 worker 故障恢复 | message/Provider 行锁、领取 token、每封邮件最多一个 IN_PROGRESS attempt；投递函数捕获 panic，保留租约交给恢复；并发测试验证同一邮件不重复领取 |
| §25–26、§53、§68–69 | 协议记录、耗时、脱敏、enhanced code | 保存阶段事件与结果、SMTP/enhanced code；DNS/连接/TLS/命令/正文/最终响应独立计时；AUTH 响应脱敏，已知密码与 AUTH PLAIN 编码过滤；不是任意敏感文本的通用脱敏器 |
| §37–38、§66 | 保留 MIME、Message-ID、DKIM 内容 | 不重新生成 MIME，不添加跟踪头；发送前校验大小与 SHA-256，以同一内存快照发送，仅做 SMTP dot-stuffing；没有验证或重新签署 DKIM |
| §45 | Test Connection 不发送邮件 | `test-provider` 执行 DNS/TCP/TLS/EHLO/AUTH 后 QUIT；Fake Server 测试断言没有邮件正文 |
| §55–57、§96–97 | 健康检查、优雅退出、UTC、超时 | live/ready、停止领取与限时等待、网络 context/deadline、TIMESTAMPTZ 与 UTC；取消后留短暂窗口提交最终结果；无法完成时保留租约，不无条件释放重发 |
| §60、§89、§100 | 保留期、审计、清理与备份说明 | EML/debug 保留期可配、默认手动预览、可选定时维护；运行日志按大小轮转；备份明确包含 DB、原文、配置与主密钥 |

## 3. 相比原阶段计划，更早或更具体的改进

### 3.1 提前阻断重复投递风险

原计划将 DELIVERY_UNKNOWN 放在 Phase 4。Phase 2 已持久化 DATA_ARMED，并用领取 token 和有效租约检查后才允许发送 DATA；最终结果提交失败也不能直接重新投递。这样早期 Provider 阶段本身就避免了“对方已接收但本地没记上”的盲目重发。

证据：[队列实现](../internal/queue/repository.go)、[投递集成测试](../tests/integration/delivery_test.go)中的租约恢复、旧 token、提交失败及禁止自动切换测试。

代价：部分实际尚未发送的邮件也会保守进入 UNKNOWN，必须等后续人工处置流程解决。这是可靠性取舍，不是 exactly-once 保证。

### 3.2 过程记录不等发送完成才保存

每个协议事件独立持久化；完成事务再按 `(attempt_id, attempt_sequence)` 幂等补齐。worker 在中途退出时，已经提交的阶段仍可追踪。DATA 前记录失败即停止发送，DATA 后尽量拿到确定的最终响应，避免为了记日志而放弃已发生的传输结果。

证据：[事件持久化实现](../internal/queue/repository.go)、[操作集成测试](../tests/integration/operations_test.go)的发送过程中查库断言。

代价：同步记录增加数据库写入和总时长；数据库不可用期间仍可能缺记录。网络耗时与总时长分开计量，后续需压测吞吐，不应宣称零成本。

### 3.3 清理可预览、可追踪、可恢复

相比只按年龄删文件，增加归档 AVAILABLE/PURGE_PENDING/PURGED 状态、删除意图先提交、受限路径删除、完成事件与维护审计。待处理、部分成功和不确定邮件不参与归档清理。

证据：[清理实现](../internal/operations/cleanup.go)、预览/根目录逃逸拒绝/中断恢复/重复执行测试。维护审计不能由普通 UPDATE/DELETE 更改。

代价：需要处理长期暂停邮件、失败的 pending 项和持续增长的事件表；它并不提供整封消息删除，也不是数据库管理员无法篡改的外部审计系统。

### 3.4 导出有快照、权限和完整性检查

跨表读取采用一致快照，按 JSONL 流式输出；压缩文件权限 0600、禁止覆盖、完成后才原子发布，并给出 SHA-256。记录导出排除凭证、领取 token 和 debug transcript，适合日后排障工具消费。

证据：[导出实现](../internal/operations/export.go)、[原子发布测试](../internal/operations/files_test.go)。相比简单重定向，失败产物和误覆盖更容易控制。

限制：地址/主题/响应仍可能敏感；校验和不是签名或加密；长时间快照会占用数据库资源；导出不具备完整恢复所需数据。

### 3.5 补充真实 SMTP 数据建模与原文完整性

增加 `envelope` 收件人类型，避免从 SMTP 信封猜测 To/Cc/Bcc；额外 attempt_recipients 保存每次逐人结果。入库后再次发信前校验原文，防止文件损坏或读错路径后发送。

这些措施细化了原规格的 recipient-level 和 transparent-mode 原则，而非改变产品方向。内存快照会随 worker 数量和最大邮件大小增长，需在性能阶段评估。

## 4. 有意的偏离与尚未完成项

| 事项 | 与规格的差异 | 原因、风险与后续动作 |
| --- | --- | --- |
| 缺失 Message-ID（§36） | 不自动插入头，只保留内部 UUID | 优先遵守 §37–38 的原文不变；不是完全满足 §36。后续提供显式补头策略及 DKIM/重试测试 |
| 推荐库与目录（§3、§5） | HTTP 标准库路由；SMTP 使用 textproto/tls；未建立大量空包 | 少量路由不需要框架；逐阶段 SMTP 便于确定 DATA 边界。自有 SMTP 实现增加维护责任，需继续扩充协议兼容测试；目录示例不是必须逐文件照抄 |
| 原始 SMTP 响应（§25、§96） | AUTH 响应脱敏、其他响应限长；部分成功阶段只记代码 | 为避免认证泄露与无限增长。尚非逐行完整 transcript，不能把当前记录称为所有线缆字节留存 |
| 错误枚举（§69） | 已有阶段分类；未一一映射所有示例常量 | RCPT 4xx/5xx 通过逐人状态与响应表达；后续补齐 RCPT_TEMP/PERM、DATA_TEMP/PERM、Provider rate-limit 等稳定分类 |
| 保留期与删除（§60–61） | 元数据/事件固定长期保留，无整封删除 | EML/debug 可配置不等于四类保留期全部完成；后续需要受控维护身份、软删除、审计与删除/重试竞争测试 |
| 孤儿文件（§99） | DB 提交不明时保留 EML，暂无扫描清理 | 避免误删已提交数据；后续增加宽限期、引用复查及磁盘告警 |
| 重试与 failover（§21–22、§83） | 暂时失败暂停；不自动跨 Provider 重试 | Phase 4 待实现逐人退避、jitter、24h 窗口、安全切换和 UNKNOWN 人工处理；安全暂停不能替代重试功能 |
| Provider health / rate limit（§27–29、§86） | 只有模型、连接诊断和并发容量 | 无滚动健康、熔断、小时/日配额；配置了未支持配额的 Provider 被跳过，不能当作配额算法已实现 |
| 队列唤醒（§63） | 接收提交 NOTIFY，worker 当前三秒轮询 | 减少忙轮询但没接入 LISTEN；后续做通知唤醒与轮询兜底 |
| Web / API / Bounce（§30–34、§39–52、§84–88） | 未实现业务入口及管理 UI | 尚无管理员 session、CSRF、HTML sandbox、HTTP idempotency、DSN 或 suppression 执行；后续阶段逐项验收 |
| 日志/监控（§53–54） | JSON 日志，字段依组件而异，无 `/metrics` | slog 默认 time/level/msg 字段不完全同示例；需要统一 component、Prometheus、告警与磁盘水位监控 |
| 生产安全（§89、§96） | 当前是本地单实例 Compose | 未完成独立 DB 最小权限、迁移发布隔离、跨连接限流、多实例和灾难恢复演练；连接诊断会实际访问配置的主机 |
| 真实 Provider（§94） | 只使用本地 Fake SMTP | 未验证 SpaceMail/PurelyMail 与 Gmail/Outlook/iCloud 实际收信；需要明确测试账号及收件人后执行，不可根据 Generic SMTP 推断已通过 |
| 发布与 AGPL | 用户追加 AGPL-3.0-only；提供文本与容器许可证标记 | 尚无公开源码地址、发布产物和对应源码分发流程。许可证显示入口不能替代修改版发布/网络服务所需的源代码安排 |

## 5. 验证证据与实际覆盖范围

本次阶段验收执行：

- `go vet ./...` 静态检查。
- `docker compose --profile test run --build --rm test`：隔离 PostgreSQL + 本地 Fake SMTP，执行 `go test -race -count=1 ./...`。
- 本地 Compose 重建后 schema_version=3、gateway/postgres 健康；`/health/ready` 返回 ready，`/license` 与 LICENSE 字节一致；三个运行容器均确认 local 日志轮转配置。
- 本地 CLI 清理预览无删除、空库记录导出 gzip/校验和通过；运行日志导出可解压，重复路径被拒绝且原文件哈希未改变。实际业务库未执行删除，也未向外部发送邮件。
- 全链路 ingress → EML → PostgreSQL queue → TLS Provider → 逐人结果；正常、部分成功、4xx/5xx、最终响应丢失/超时。
- 认证拒绝、发件人限制、存档/数据库故障、原文完整性、错误主密钥、并发领取和租约恢复、最终提交失败。
- 新增清理预览、状态保护、路径约束、已删文件恢复、重复执行、审计不可修改、快照导出过滤、原子文件不覆盖、发送完成前事件落库。

测试提供受控协议/数据库故障注入证据；租约恢复通过模拟到期验证，不等于完成真实断电、宿主机 kill 与磁盘故障矩阵。Fake TLS/DATA 测试不替代真实邮件投递、互联网 TLS 配置和生产负载测试。

## 6. Git 整合方式

`phase-1`（`7299fe7`）包含基础骨架和可靠接收；`phase-2`（`3833852`）在其上增加队列与 Provider；`phase-3` 在同一祖先链上增加 Recorder 和运维功能。本次把第三阶段分支合入 `main`，前三阶段源码、迁移和文档随历史一起进入主线，保留原分支与里程碑标签。

不压缩旧提交、不移动 phase-1/phase-2 标签，不把密码、数据库、证书或 EML 加入 Git。具体完成提交可用 `git show phase-3` 与 `git log --graph --decorate --oneline` 查看。这里只涉及本地整合，没有推送远程。

下一阶段优先完成规格 Phase 4 的安全重试与人工处置；并把元数据/事件删除、孤儿回收和生产监控作为明确的后续运维项目，避免将当前“有清理命令”误认为“成熟系统已全部完成”。
