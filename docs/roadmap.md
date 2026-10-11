# 开发路线图

更新：2026-10-11。Phase 9A、8D.1–8D.2 与 8E.1 已完成本地验收；8E.2a 网站提交预算已完成本地验收，8E.2b 共享 Provider 账号额度已完成本地验收，8E.2c 消息流与公平调度已完成本地验收，8E.2d 邮件有效期已完成本地验收，8E.3a 沙箱安全基础与 8E.3b 沙箱工具/网站示例已完成本地验收；这不等于生产就绪。后续按本文件排期，已完成阶段的实现与验收记录保留，不重编号或移动已有 Git 标签。

项目统一命名为 **RelayTale**，改名不改变投递语义或历史阶段标签。升级边界见 [改名说明](rename-relaytale.md)。

当前检查点：**[7B.7 人工退信核验与网站反馈](phase-7b7-manual-feedback.md)（已完成本地验收）**；上一完整检查点为 [8G 定时发送与排队控制](phase-8g-complete.md)。人工核验与 Provider 来源认证分开，完整 Phase 7 自动处理仍待验证可信来源。

Phase 7 的可信反馈自动化仍受来源认证证据约束；9B 已补齐当前受支持进程的单实例/迁移互斥，Provider 能力来源与后续生产验收仍是生产门槛，不因便利功能后置。历史验收记录见各阶段链接。

## Phase 0 · 基础服务（已完成）

- Go 项目、配置、数据库连接和内嵌迁移。
- 核心表与关键约束、健康检查、优雅退出。
- relaytale / postgres / caddy Compose。
- 验收：编译、单元测试、真实数据库迁移、Compose 启动与重启。

## Phase 1 · 可靠接收（已实现，含集成测试）

SMTP STARTTLS + AUTH、发件人限制、原始 EML 原子存档、UUIDv7、message / recipients / event 事务。默认拒绝匿名中继。以 Fake SMTP 客户端和故障注入验证“持久化完成才返回 250”。

## Phase 2 · Provider 投递与队列（已完成，含集成测试）

Generic SMTP（STARTTLS / implicit TLS）、Provider 凭证加密、priority 与并发容量选择、SKIP LOCKED 领取、一次投递、逐收件人结果、基础阶段事件与安全租约恢复。Fake SMTP 和 PostgreSQL 集成测试验证完整链路。该阶段默认关闭 worker，仅做本地测试；后续真实结果见 Phase 3.5。

为避免不安全的过渡版本，提前实现 DELIVERY_UNKNOWN 的保守判定与禁止自动重发。该阶段暂时失败停留 TEMP_FAILED；自动重试在 Phase 4A 实现，仍默认关闭。

## Phase 3 · Flight Recorder 与运维基础（已完成，含集成测试）

增加 DNS、SMTP greeting/EHLO/AUTH/QUIT 等阶段事件、实际网络耗时与错误分类，过程事件持续写入并幂等补齐。提供 Provider 列表和不发信的连接诊断。

按用户要求采用 AGPL-3.0-only，提前实现运行日志轮转、压缩导出、可预览的 EML/debug 保留期清理、可恢复删除、定时维护和追加式审计；默认关闭自动清理。元数据/事件期限删除、完整 debug transcript 和完善错误枚举仍待后续实现。见 [运维说明](operations.md) 和 [规格对照](phase-1-3-spec-review.md)。

## Phase 3.5 · 真实 Provider 验收（SMTP 层通过，三个收件服务原始邮件已核对）

本轮完成 SpaceMail / PurelyMail × 465 / 587，四封邮件、16 个收件人均取得最终 250；覆盖 Gmail、QQ、Google 托管学校邮箱和 iCloud，尚无 Outlook。Gmail / QQ / iCloud 共 12 份原始邮件已核对，Message-ID、主题及解码 MIME 内容保持；QQ 的 PurelyMail 587 被报告进入垃圾箱。QQ 认证差异与 SpaceMail DMARC 仍待定位，学校邮箱未独立核验。明确 SMTP 接受、实际收信、原文保持和 DKIM 验证的不同证据。见 [验收矩阵](real-world-validation.md)。本地 Fake SMTP 不能替代真实结果。

## Phase 4A · Delivery Decision Engine（已完成本地验收，自动重试默认关闭）

集中、可解释的逐收件人决策，持久化同 Provider 重试、退避/jitter、24 小时与 7 次上限，以及有审计和并发保护的 UNKNOWN 人工处置。自动重试默认关闭，历史暂停邮件不自动启动。计划和验收边界见 [Phase 4 实施计划](phase-4-plan.md)。

## Phase 4B · 安全 Provider 切换（已完成本地验收，默认关闭）

在全部未完成收件人的最新决策许可且重试到期时选择备用，检查 Envelope/Header From 域、配置、并发容量，检查剩余配额。最多三个不同 Provider，不回切，保留逐人重试预算；切换事件与新尝试原子提交。混合 RCPT 错误保留原路由，UNKNOWN 不切换。4C 已进一步加入健康与配额控制。

## Phase 4C · Provider Health、熔断与配额（已完成本地验收，熔断默认关闭）

最近 10 分钟的 Provider 可用性样本、故障归属、60 秒熔断与单半开尝试；滚动 1h / 24h 收件人尝试配额、分批、原子预留和崩溃不退款。list-providers 输出健康/用量，熔断执行默认关闭。见 [4C 方案](phase-4c-plan.md)。后续 5B 已增加聚合指标，Phase 6 已增加管理界面。

## Phase 5A · 信誉保护基础（已完成本地验收）

已提供全网关地址抑制、添加/查询/解除 CLI、入队/领取/DATA 授权检查、审计和租约恢复。细节见 [5A 实现说明](phase-5a-suppression.md)；[可信反馈与身份责任](feedback-and-sender-identity.md) 明确当前真实证据及后续通道设计。没有实现自动 DSN 消费者。

- 明确抑制范围、地址规范化、来源、原因、有效期与解除规则。逐收件人执行，入队及出站前检查；定义与正在发送任务的并发边界，不能撤回已经发出的邮件。
- 提供受控的添加、查询、解除与审计，解除不自动重发历史邮件。最初使用本地 CLI；管理 API 在 5C 接入同一服务逻辑。
- 区分“本次永久失败”和“地址长期无效”。不把所有 5xx、认证失败、策略拒绝或配额问题写入抑制列表；自动抑制规则需要明确证据和测试，不能仅凭状态码大类启用。
- 设计可信退信通道：明确 Envelope From / 退信邮箱由谁控制，Provider 是否改写，以及如何关联 Gateway ID、attempt 与具体收件人。无法关联或来源不可信的反馈隔离待查，不直接改变投递状态。
- 明确认证责任：当前 Gateway 保持原文，不自行 DKIM 签名；Provider 的域声明不代表 SPF/DKIM/DMARC 已验收。为每个“发件域 × Provider”记录实际身份验证结果，双 Provider 互备需要分别验收。

**验收条件：** 被抑制地址的新投递与排队重试被拦截；多收件人邮件中其余合法收件人继续；解除、到期、并发发送边界和审计失败有测试；伪造/重复/无法关联的反馈不会误封地址。交付退信接入设计与认证责任文档，不宣称完整自动 DSN 已完成。

## Phase 5B · 大邮件与队列资源加固（已完成本地验收）

本阶段起点：整封原始邮件默认上限 25 MiB，EML 文件与 PostgreSQL 元数据已分层；出站整封读取及 dot-stuffing 缓冲存在并发内存放大。当前已改为流式快照，完成 16/25 MiB、故障注入、全套竞态及 120 秒持续负载验收；四 worker 处理 1237 封、停止输入后 2.506 秒排空。容量结论限于报告声明的本地范围，以下保留验收目标。

- 改为流式出站与点转义，明确哈希验证、文件修改防护和 DATA 授权顺序；不得为了流式处理牺牲原文一致性和已发送后的 UNKNOWN 语义。
- 增加进程级在途资源预算和背压，结合 worker 数量、单封大小限制与有界缓冲；多实例部署按实例预算规划总资源。
- 测试 >15 MiB、默认上限附近、超限拒绝、多封并发、慢 Provider、取消/断线、存储错误与最终响应丢失。记录峰值内存、吞吐和延迟，按部署目标给出明确预算后验收。
- 做 PostgreSQL 持续入队/出队与积压恢复测试，观察锁等待、表/索引膨胀、autovacuum、WAL 和连接占用；覆盖 events、attempts、quota ledger 的增长。
- 增加基础 Prometheus 指标（队列年龄、投递结果、UNKNOWN、配额阻塞、熔断、资源使用），避免用邮箱地址/消息 ID 作高基数标签。评估 LISTEN/NOTIFY 唤醒，保留轮询兜底，通知不作为队列事实。

**验收条件：** 在文档声明的并发和大小范围内资源受限、无死锁/泄漏；流式后原文及逐收件人语义不变；负载报告包含持续增长与恢复结果，未解决瓶颈明确记录。容量结论必须来自测试，不用单次成功代替。

## Phase 5B.1 · 安全门禁

在管理 API 前补齐全仓库格式、静态检查、构建和强制 PostgreSQL 竞态测试的 CI；SMTP 总连接/来源连接限制、来源 AUTH 速率及并发隔离、全局 KDF 预算；可选 metrics Bearer 令牌、只读历史 EML 预检和容量说明。实现、验收与剩余边界见 [安全门禁报告](phase-5b1-safety-gates.md)。

迁移互斥、单实例运行约束和带版本的密钥轮换仍是生产发布前置条件，不能因为本阶段限流与 CI 完成而视为解决。

## Phase 5C · 最小管理 API（已完成本地验收）

实现与测试记录见 [阶段报告](phase-5c-management-api.md)，使用方式见 [API 手册](management-api.md)。

- 管理员认证、授权、凭证脱敏、分页与输入约束；按认证方式处理浏览器会话/CSRF 等边界。
- 提供 Provider 配置/启停/凭证轮换、配额与健康查询、消息与时间线查询、suppression 管理、受控 UNKNOWN 操作。
- 复用已验证的业务规则；写操作带操作者、理由、并发条件和追加审计。不得用直接修改 status 绕过决策、抑制或重试预算。

**验收条件：** 未授权访问、敏感字段泄漏、并发覆盖、重复人工操作和审计失败均有测试；API 操作与现有 CLI 语义一致。本阶段不开放 HTTP 发信，不做完整 Dashboard。

## Phase 6 · Web UI（已完成本地验收）

基于管理 API 实现 Provider、Messages、事件时间线、健康/用量、suppression 与 UNKNOWN 处置页面。明确区分 SMTP_ACCEPTED、用户确认收到、BOUNCED 和 UNKNOWN，不把 250 显示成入箱成功。

邮件 HTML、附件和外部资源按不可信内容隔离展示，默认不执行脚本或加载跟踪资源。危险人工操作展示具体对象、影响与并发版本。

**验收条件：** 覆盖从查询到人工处置的端到端流程、权限边界、邮件内容隔离，以及失败/空状态；界面不能扩大后台授权或绕过安全确认。

## Phase 7 · DSN / Bounce 自动化（分段推进）

**7A：DSN 解析与反馈待查。** 已实现限额内离线导入、原始证据、逐收件人声明解析、幂等与内容冲突展示、人工关闭和追加审计。始终 UNVERIFIED，不触发投递状态或 suppression。说明、资源限制和验收见 [7A 报告](phase-7a-feedback.md)。

**7B.1：关联与决策安全基础。** 已提供只读预检，验证候选 message/provider/attempt/recipient 关系、地址改写、多报告歧义与更新尝试；不授予可信身份、不改 UNKNOWN、不自动抑制。见 [阶段说明](phase-7b-feedback-safety.md)。

**7B.2：只读 IMAP 收集。** 已实现专用文件夹读取、UIDVALIDITY/UID 游标、原件与审计原子提交、大小/批量/时间/存储限制以及异常回执；默认不开启，不改变邮箱、不授予来源信任。见 [收集手册](phase-7b2-imap-collection.md)。已完成 [两家真实 INBOX 的限定范围验收](real-imap-validation.md)，已确认 Purelymail 当前样本是欢迎信，并增加真实结构对应的合成回归测试；随后已取得 [Purelymail 受控真实退信](purelymail-dsn-validation.md)，发现非标准收件人/状态字段及段落；兼容说明已由 7B.3 实现，来源信任和唯一关联仍独立验收。

**7B.3：非标准退信兼容说明。** 已提供只读 explain-feedback，分开保留原始字段、格式问题与辅助提示；旧 UNSUPPORTED 状态不变，说明不进入关联决策，不触发抑制。详见 [阶段说明](phase-7b3-feedback-explanation.md)。

**7B.4：退信关联候选查询。** 已实现按操作者提供的 Message-ID 线索，在统一只读快照中列出消息/尝试/收件人候选并复用预检规则；重复 Message-ID、超限和 UNKNOWN 均显式说明，不授予信任或自动处置权限。见 [阶段说明](phase-7b4-feedback-candidates.md)。

**7B.5：反馈待查工作台。** 将列表、详情、兼容说明与候选查询接入现有管理权限体系；仅作人工核对，不提升来源信任或改变投递/抑制状态。见 [阶段说明](phase-7b5-feedback-workspace.md)。

**7B.6：反馈证据总容量保护。** 统一约束手工导入与 IMAP 的证据记录数和逻辑字节，事务计费、重复不计费、满额不推进失败邮件的游标；保留历史证据并提供只读用量查询。见 [阶段说明](phase-7b6-feedback-capacity.md)。

**7B.7：人工退信核验与网站反馈（已完成本地验收）。** 按用户选择增加显式关联、管理员确认/撤回、网站独立查询与签名通知；冲突证据使结论失效，不改 SMTP 接受事实或来源信任，不自动抑制。见 [阶段说明](phase-7b7-manual-feedback.md)。

**7B：可信通道与自动处理（其余部分待实现）。** 必须先验收来源认证、账号归属与 attempt/recipient 关联，不通过人工传入 trusted 标记启用自动封禁。完整 Phase 7 的目标仍是：

在 5A 的可信来源与关联设计基础上，实现已选定通道的接收、解析、去重、逐收件人反馈状态及保守自动抑制。可根据可用 Provider 通道在 UI 开发期间推进，但完整 UI 不是其安全依赖。

**验收条件：** 可信硬退信可关联并阻止后续新投递；临时、重复、伪造、转发和不匹配反馈不会误抑制；异步退信不会抹去此前 SMTP 接受事实。不从“没有退信”推断实际送达。

## Phase 8 · HTTP 发信 API（分段推进）

**8A：持久化幂等入队基础。** 已实现 API 身份范围内的请求指纹、并发唯一入队、冲突拒绝和事务回滚；SMTP/API 共用 suppression、收件人和事件逻辑。无公开 HTTP 入口。见 [阶段说明](phase-8a-idempotent-enqueue.md)。

**8B：认证与有界 HTTP 接入。** 已加入独立环境凭证与轮换/撤销、发件白名单、流式原文、速率/并发/资源限额，以及每键锁内的重放检查；默认关闭。见 [接口手册](phase-8b-http-submission.md)。

**8C：端到端协议与故障验收。** 已完成 HTTP/数据库确认丢失、取消与限额验收，以及单日/跨日期有界续扫、候选记录持久化和显式安全回收。回收使用数据库退役保护拦截延迟入队，默认只预览，引用、UNKNOWN、变化文件和不确定提交均保守处理。完整流程、迁移与验收矩阵见 [8C 收尾报告](phase-8c-complete.md)。自动回收调度与一般生命周期管理继续属于 Phase 9。

**8D：网站投递结果查询与 Webhook（分段实现）。** [8D.1 状态查询](phase-8d1-message-status.md) 已完成本地验收：稳定 API 身份隔离、逐收件人结果、最近已完成尝试、有界事件分页和独立读取限流。[8D.2 签名通知](phase-8d2-webhooks.md) 已实现事务 outbox、双密钥 HMAC、HTTPS/SSRF 边界、有界通知重试和人工重放，默认关闭网络发送。同步 SMTP 结果和异步退信分开表达；8D.1 尚不暴露收件箱反馈，返回 NOT_EXPOSED；未来 DSN 未通过可信来源及唯一关联验证时不能冒充确认失败。见 [功能边界与分段验收](website-delivery-results.md)。

**8E：业务网站接入保护（分段实现）。** [8E.1 业务关联与检索](phase-8e1-business-reference.md) 已完成本地验收：有界 ID/标签、稳定身份范围检索、指纹兼容与冲突、显式 v2 状态/通知，以及备份恢复。8E.2a 已完成网站提交预算本地验收；8E.2b 共享 Provider 账号额度已完成本地验收；8E.2c 消息流/公平调度已完成本地验收；8E.2d expires_at 已完成本地验收；8E.3a 已完成本地验收：独立不可变测试身份、零 SMTP 投递与明确标记的签名通知；[8E.3b](phase-8e3b-sandbox-tools.md) 已完成网站示例、契约、MIME 检查与受控模拟。复用现有身份、配额和 DATA 授权，过期不能抹去可能已发送事实。

**8F：可选 JSON 构建与版本化模板（分段实现）。** 8F.1 [简单 JSON 发信](phase-8f1-json-submission.md) 增加有界纯文本/HTML、显示名/Reply-To 与确定性 MIME 构建，复用原提交入口、幂等和队列；8F.2 [JSON 附件](phase-8f2-json-attachments.md) 增加有界本地内容、严格元数据与 multipart/mixed，保留无附件 json-v1 字节兼容；8F.3 [固定版本模板](phase-8f3-versioned-templates.md) 增加逐网站配置目录、内容寻址版本、严格变量与上下文转义，复用稳定 EML；8F.4 [本地预览与语言版本](phase-8f4-template-preview.md) 增加无投递的 JSON 源码预览、明确语言选择和兼容构建；8F.5a [持久模板目录](phase-8f5a-persistent-templates.md) 提供不可改写版本、事务审计发布、逐网站显式启用与启动快照，以及备份恢复校验；8F.5b.1 [版本退役](phase-8f5b1-template-retirement.md) 增加不可逆退役、活跃/历史分离限额与恢复校验，需等待业务请求重试窗口并重启生效；8F.5b.2 [草稿与精确修订发布](phase-8f5b2-template-drafts.md) 增加有界不可改写修订、并发冲突保护、指定修订导出与原子发布；8F.5b.3 [模板管理工作台](phase-8f5b3-template-management.md) 已增加管理 API、完整页面编辑、精确修订源码审阅、显式发布与退役；8F.5b.4 [模板渲染预览](phase-8f5b4-template-preview.md) 已增加精确修订样例渲染、隔离 HTML 结构展示及并发保护；保真邮件客户端预览和历史归档仍不在已完成范围。原始 EML 入口保持透明；生成 MIME 固化后复用现有队列，模板变更不能改变重试正文。

**8G：定时、受控取消、替换与改期（类别已实现）。** [8G.1 定时准入](phase-8g1-scheduled-submission.md) 提供不可改写 UTC 排期、100 封/128 MiB 未来容量与提前领取/授权保护；[8G 收尾](phase-8g-complete.md) 补齐同网站待发任务取消、精确旧 Gateway ID 原子替换、以新任务改期、查询/签名通知与备份恢复。取消与领取/授权共用消息锁，活动尝试即拒绝；历史 DATA、SMTP 接受和 UNKNOWN 不承诺撤回。批量发送与活动连接中断仍是独立后续范围。8E–8G 的场景与验收详见 [业务网站能力规划](business-website-capabilities.md)。

与管理 API 分离凭证和权限，实现 API key、请求限额、并发幂等性和持久化确认。复用 SMTP 路径的原文/存档、suppression、配额与投递决策，不另建一套发送语义。

**验收条件：** 并发重复请求不会重复入队；幂等键冲突、持久化失败、权限与资源限制都有测试；接收成功只代表网关可靠接收。

## Phase 9 · 生产运维与生命周期

**9A：停写备份与受控恢复。** 已实现配套数据库快照与 EML 清单、空目标恢复、启动阻断，以及原文/凭证/幂等/退役一致性校验；验收结果和 RPO/RTO 边界见 [运维手册](phase-9a-backup-recovery.md)。校验不自动解除启动阻断，备份之后的外部发送必须人工对账。完成后优先回到 8D 网站结果查询与回调；9B 的运行安全分段已完成，Provider 能力与后续生命周期工作继续推进。

**9B：运行安全与 Provider 可见性（分段实现）。** 9B.1 [显式迁移与启动 schema 门槛](phase-9b1-migrations.md) 已完成；9B.2a [持久单实例占用](phase-9b2a-runtime-ownership.md) 已完成核心范围；9B.2b [辅助进程持久占用](phase-9b2b-auxiliary-ownership.md) 补齐受支持 CLI；9B.3 [数据库最小权限](phase-9b3-database-privileges.md) 分离账号并实施启动检查；9B.4 [密钥轮换](phase-9b4-key-rotation.md) 增加向后读取兼容与离线原子更新；9B.5a [运维告警](phase-9b5a-operations-alerts.md) 补齐共享额度与人工处置指标；9B.5b.1 [能力证据台账](phase-9b5b1-provider-evidence.md) 记录来源、配置版本和复核提示；9B.5b.2 [协议与 DNS 诊断](phase-9b5b2-diagnostics.md) 提供 TLS 后观察与精确域名 TXT 候选报告；组织域策略发现和跨 Provider 身份仍需验收。 [9B.6 升级故障演练](phase-9b6-upgrade-drills.md) 已通过完整本地验收，覆盖版本写入失败、取消与离线连接丢失后的阻断及显式恢复；主机级故障与 Provider 身份的真实链路验收仍待补齐；共享额度与队列年龄告警规则已提供，通知接收端部署仍需运维接通。与 8E 同优先层推进，不等待模板和定时功能。DNS 正常不是入箱证明，网关用量不冒充 Provider 实际余额。

**9C：协调保留与网站运营视图（分段实现）。** 9C.1 [保留规则与只读预检](phase-9c1-retention-preflight.md) 提供逐消息依赖说明、保守原文候选和有界分页，不授权删除；9C.2 [协调原文清理](phase-9c2-coordinated-cleanup.md) 将保留依赖接入既有清理和维护调度，以数据库栅栏与锁后复查保护并发写入；9C.3 [网站运营统计](phase-9c3-website-statistics.md) 提供身份隔离、有界窗口查询及逻辑原文字节，9C.4 [工作台统计与逻辑容量](phase-9c4-console-statistics.md) 增加管理范围的跨网站/来源/环境分组视图；9C.5 [只读容量盘点](phase-9c5-capacity-inventory.md) 增加引用原文长度与异常分页观察、孤儿条目长度及主要表/索引占用采样；卷分配空间、完整全库合计和自动增长趋势仍待实施。 在现有审计、指标、导出与 EML 清理上补按项目统计和协调生命周期；验证码等敏感原文保留策略不能破坏 UNKNOWN、有效幂等记录和反馈证据。

完成备份/恢复演练、最小数据库权限、迁移与回退方案、密钥轮换、告警与值班手册。定义 EML、元数据、events、attempts 和 quota ledger 的协调保留策略；保留 UNKNOWN/待处理数据与审计约束，清理不得重置有效配额窗口或破坏反馈关联。

**验收条件：** 恢复演练与升级中断测试通过；明确 RPO/RTO、容量范围、保留期及操作责任；长期负载与真实链路缺口有结论后才评估生产发布。已有 EML/debug 清理功能不能代替完整生命周期管理。

## 后续候选 · 7C 业务偏好 / 10A 入站回复

7C 在既有全局 suppression 之上设计网站/通知类型偏好，不允许偏好覆盖强制阻断，不把所有 5xx 当作地址无效。10A 在有具体业务使用者后，独立提供用户回复到工单/订单的受控入站回调，不能直接复用 DSN 的信任判断。均未实现；打开/点击追踪、营销平台和拖拽编辑器不进入近期排期。

## 持续验收与不可退让的边界

- Worker、自动重试、切换、熔断执行和定时清理继续保持显式启用；配置了配额就必须执行，不能被其他开关绕过。
- UNKNOWN 不自动重发；Provider 恢复、suppression 解除或 UI 操作都不能隐式重置这个边界。此设计降低重复风险，不承诺端到端 exactly-once。
- 当前 Flight Recorder 是阶段事件与耗时，不是完整 SMTP transcript。若补充会话记录，先落实凭证/内容脱敏、访问权限、采样与保留期。
- Phase 3.5 继续补齐：QQ 认证差异、SpaceMail DMARC、同一发件域跨 Provider 身份验收、预先 DKIM 签名透传、大消息边界和 Outlook。学校邮箱保留未独立核验，不凭 Google MX 视为与个人 Gmail 相同。
- SMTP 客户端兼容矩阵继续覆盖 EHLO、AUTH、TLS、RSET/NOOP/QUIT、多个 RCPT、SIZE、SMTPUTF8/8BITMIME、长头、CRLF 和中途断开。可选 Message-ID 注入及自行 DKIM 签名均为独立提案，不隐式改变默认透明模式。
- 日常自动测试使用隔离 PostgreSQL + Fake SMTP；真实外部发信需要明确账号、目标、数量和测试范围。每阶段更新文档、执行相关检查、保存 Git 检查点；只给通过验收的里程碑打标签。

## 与原排期的对应关系

原来的管理 UI / HTTP API 被拆为 5C 管理 API、6 Web UI 和 8 发信 API；原 DSN / Bounce / Suppression 拆为前置的 5A 保护基础与 7 自动反馈。原 Phase 10 生产加固拆为前置的 5B 资源验证与 9 运维发布验收。这里只调整未完成工作，Phase 0–4C 的名称、代码和标签不变。
