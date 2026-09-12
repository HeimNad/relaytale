# 开发路线图

更新：2026-09-11。当前已完成 Phase 4C 的本地验收；这不等于生产就绪。后续按本文件排期，已完成阶段的实现与验收记录保留，不重编号或移动已有 Git 标签。

项目统一命名为 **RelayTale**，改名不改变投递语义或历史阶段标签。升级边界见 [改名说明](rename-relaytale.md)。

下一阶段：**Phase 5A 信誉保护基础**，随后资源加固、最小管理 API，再进入完整 Web UI。

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

最近 10 分钟的 Provider 可用性样本、故障归属、60 秒熔断与单半开尝试；滚动 1h / 24h 收件人尝试配额、分批、原子预留和崩溃不退款。list-providers 输出健康/用量，熔断执行默认关闭。见 [4C 方案](phase-4c-plan.md)。管理界面与指标面板仍未实现。

## Phase 5A · 信誉保护基础（下一阶段）

先提供可执行的 suppression 和最小 CLI 运维能力，不等待 Web UI，也不把退信解析器等同于可信反馈系统。

- 明确抑制范围、地址规范化、来源、原因、有效期与解除规则。逐收件人执行，入队及出站前检查；定义与正在发送任务的并发边界，不能撤回已经发出的邮件。
- 提供受控的添加、查询、解除与审计，解除不自动重发历史邮件。最初使用本地 CLI；管理 API 在 5C 接入同一服务逻辑。
- 区分“本次永久失败”和“地址长期无效”。不把所有 5xx、认证失败、策略拒绝或配额问题写入抑制列表；自动抑制规则需要明确证据和测试，不能仅凭状态码大类启用。
- 设计可信退信通道：明确 Envelope From / 退信邮箱由谁控制，Provider 是否改写，以及如何关联 Gateway ID、attempt 与具体收件人。无法关联或来源不可信的反馈隔离待查，不直接改变投递状态。
- 明确认证责任：当前 Gateway 保持原文，不自行 DKIM 签名；Provider 的域声明不代表 SPF/DKIM/DMARC 已验收。为每个“发件域 × Provider”记录实际身份验证结果，双 Provider 互备需要分别验收。

**验收条件：** 被抑制地址的新投递与排队重试被拦截；多收件人邮件中其余合法收件人继续；解除、到期、并发发送边界和审计失败有测试；伪造/重复/无法关联的反馈不会误封地址。交付退信接入设计与认证责任文档，不宣称完整自动 DSN 已完成。

## Phase 5B · 大邮件与队列资源加固

现状：整封原始邮件默认上限 25 MiB，EML 文件与 PostgreSQL 元数据已分层；出站仍会读入整封邮件并生成 dot-stuffing 缓冲，存在并发内存放大。

- 改为流式出站与点转义，明确哈希验证、文件修改防护和 DATA 授权顺序；不得为了流式处理牺牲原文一致性和已发送后的 UNKNOWN 语义。
- 增加进程级在途资源预算和背压，结合 worker 数量、单封大小限制与有界缓冲；多实例部署按实例预算规划总资源。
- 测试 >15 MiB、默认上限附近、超限拒绝、多封并发、慢 Provider、取消/断线、存储错误与最终响应丢失。记录峰值内存、吞吐和延迟，按部署目标给出明确预算后验收。
- 做 PostgreSQL 持续入队/出队与积压恢复测试，观察锁等待、表/索引膨胀、autovacuum、WAL 和连接占用；覆盖 events、attempts、quota ledger 的增长。
- 增加基础 Prometheus 指标（队列年龄、投递结果、UNKNOWN、配额阻塞、熔断、资源使用），避免用邮箱地址/消息 ID 作高基数标签。评估 LISTEN/NOTIFY 唤醒，保留轮询兜底，通知不作为队列事实。

**验收条件：** 在文档声明的并发和大小范围内资源受限、无死锁/泄漏；流式后原文及逐收件人语义不变；负载报告包含持续增长与恢复结果，未解决瓶颈明确记录。容量结论必须来自测试，不用单次成功代替。

## Phase 5C · 最小管理 API

- 管理员认证、授权、凭证脱敏、分页与输入约束；按认证方式处理浏览器会话/CSRF 等边界。
- 提供 Provider 配置/启停/凭证轮换、配额与健康查询、消息与时间线查询、suppression 管理、受控 UNKNOWN 操作。
- 复用已验证的业务规则；写操作带操作者、理由、并发条件和追加审计。不得用直接修改 status 绕过决策、抑制或重试预算。

**验收条件：** 未授权访问、敏感字段泄漏、并发覆盖、重复人工操作和审计失败均有测试；API 操作与现有 CLI 语义一致。本阶段不开放 HTTP 发信，不做完整 Dashboard。

## Phase 6 · Web UI

基于管理 API 实现 Provider、Messages、事件时间线、健康/用量、suppression 与 UNKNOWN 处置页面。明确区分 SMTP_ACCEPTED、用户确认收到、BOUNCED 和 UNKNOWN，不把 250 显示成入箱成功。

邮件 HTML、附件和外部资源按不可信内容隔离展示，默认不执行脚本或加载跟踪资源。危险人工操作展示具体对象、影响与并发版本。

**验收条件：** 覆盖从查询到人工处置的端到端流程、权限边界、邮件内容隔离，以及失败/空状态；界面不能扩大后台授权或绕过安全确认。

## Phase 7 · DSN / Bounce 自动化

在 5A 的可信来源与关联设计基础上，实现已选定通道的接收、解析、去重、逐收件人反馈状态及保守自动抑制。可根据可用 Provider 通道在 UI 开发期间推进，但完整 UI 不是其安全依赖。

**验收条件：** 可信硬退信可关联并阻止后续新投递；临时、重复、伪造、转发和不匹配反馈不会误抑制；异步退信不会抹去此前 SMTP 接受事实。不从“没有退信”推断实际送达。

## Phase 8 · HTTP 发信 API

与管理 API 分离凭证和权限，实现 API key、请求限额、并发幂等性和持久化确认。复用 SMTP 路径的原文/存档、suppression、配额与投递决策，不另建一套发送语义。

**验收条件：** 并发重复请求不会重复入队；幂等键冲突、持久化失败、权限与资源限制都有测试；接收成功只代表网关可靠接收。

## Phase 9 · 生产运维与生命周期

完成备份/恢复演练、最小数据库权限、迁移与回退方案、密钥轮换、告警与值班手册。定义 EML、元数据、events、attempts 和 quota ledger 的协调保留策略；保留 UNKNOWN/待处理数据与审计约束，清理不得重置有效配额窗口或破坏反馈关联。

**验收条件：** 恢复演练与升级中断测试通过；明确 RPO/RTO、容量范围、保留期及操作责任；长期负载与真实链路缺口有结论后才评估生产发布。已有 EML/debug 清理功能不能代替完整生命周期管理。

## 持续验收与不可退让的边界

- Worker、自动重试、切换、熔断执行和定时清理继续保持显式启用；配置了配额就必须执行，不能被其他开关绕过。
- UNKNOWN 不自动重发；Provider 恢复、suppression 解除或 UI 操作都不能隐式重置这个边界。此设计降低重复风险，不承诺端到端 exactly-once。
- 当前 Flight Recorder 是阶段事件与耗时，不是完整 SMTP transcript。若补充会话记录，先落实凭证/内容脱敏、访问权限、采样与保留期。
- Phase 3.5 继续补齐：QQ 认证差异、SpaceMail DMARC、同一发件域跨 Provider 身份验收、预先 DKIM 签名透传、大消息边界和 Outlook。学校邮箱保留未独立核验，不凭 Google MX 视为与个人 Gmail 相同。
- SMTP 客户端兼容矩阵继续覆盖 EHLO、AUTH、TLS、RSET/NOOP/QUIT、多个 RCPT、SIZE、SMTPUTF8/8BITMIME、长头、CRLF 和中途断开。可选 Message-ID 注入及自行 DKIM 签名均为独立提案，不隐式改变默认透明模式。
- 日常自动测试使用隔离 PostgreSQL + Fake SMTP；真实外部发信需要明确账号、目标、数量和测试范围。每阶段更新文档、执行相关检查、保存 Git 检查点；只给通过验收的里程碑打标签。

## 与原排期的对应关系

原来的管理 UI / HTTP API 被拆为 5C 管理 API、6 Web UI 和 8 发信 API；原 DSN / Bounce / Suppression 拆为前置的 5A 保护基础与 7 自动反馈。原 Phase 10 生产加固拆为前置的 5B 资源验证与 9 运维发布验收。这里只调整未完成工作，Phase 0–4C 的名称、代码和标签不变。
