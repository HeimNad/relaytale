# Phase 3.5 / 4 实施计划

日期：2026-09-10。基线：`phase-3`。保持 Go + PostgreSQL + 原始 EML + recipient-level state，不重写 SMTP 核心。

本文保留 Phase 4 的历史实施与验收记录。Phase 4C 后的最新排期以 [开发路线图](roadmap.md) 为准：信誉保护基础 → 资源加固 → 最小管理 API → Web UI；完整 DSN 与 HTTP 发信分别验收。

## 顺序与交付边界

1. **Phase 3.5 真实链路验收**：准备测试矩阵与证据模板；需要操作者提供 SpaceMail/PurelyMail 测试凭证及明确允许接收测试邮件的地址。未取得这些信息不发送外部邮件，结果不得用 Fake SMTP 替代。
2. **Phase 4A（本轮实现）**：纯函数投递决策、逐收件人持久化重试、同 Provider 固定路由、24 小时/7 次上限、退避与 jitter、UNKNOWN 人工处置及追加审计。自动重试默认关闭，真实验收通过前不启用。保持原文与原始 Message-ID 不变。
3. **Phase 4B**：安全跨 Provider 切换，候选域授权/配额/健康检查，明确事件，逐阶段故障注入。决策允许切换不等于执行切换；4A 不执行切换。
4. **Phase 4C**：滚动 Provider 健康、熔断与半开、配额记账；以决策的错误归属区分 Provider 与收件人错误。
5. 后续顺序已于 2026-09-11 调整：先做 suppression 与可信反馈基础、资源加固，再做管理 API / UI；完整 DSN 自动化、HTTP 发信和生产运维见最新路线图。

## 4A 模型与不变量

决策输入含具体协议阶段、收件人结果、明确最终响应、DATA 是否可能发送、错误类别、收件人尝试次数和截止时间。输出包含 action、reason、failure scope、may-have-delivered、failover-allowed、terminal、retry-after；存储版本号以支持以后解释历史。

- SMTP 事实保存在 attempt/attempt_recipients，重试/人工决策另记事件；不把人工结论冒充 SMTP 250。
- 已成功或永久失败的收件人不重新发送；部分成功的 message 状态根据全部收件人重算。
- AUTH 明确拒绝、密钥、存储等配置/本地问题暂停人工处理，不反复自动连接。
- DATA 后缺失确定结果始终 UNKNOWN；无效/矛盾证据按保守人工处理，不推断安全。
- 重试时间和原 Provider 落库；调度器通过数据库行锁与状态检查恢复，不使用内存 sleep 保存重试计划。
- 按收件人首次领取起算 24 小时、最多 7 次，指数退避加有界 jitter；超过边界暂停人工处理，不能把窗口耗尽说成收件人永久拒绝。
- UNKNOWN 人工操作要求 recipient ID、对应最新 attempt ID、操作者、原因；重试还需明确承认重复风险。使用事务和行锁拒绝并发/过时操作。
- 人工标记使用独立的人工处置记录；原 attempt 不改写。原文已清理时拒绝重试，发送前再次校验哈希。
- 旧阶段暂停邮件不在迁移时自动启动重试；默认关闭自动重试。后续启用也不扫描历史 TEMP_FAILED 盲目重发。

## 验收

决策表驱动单测；混合 RCPT 结果后只重发暂时失败收件人；同 Provider 固定与备用不连接；未来重试时间不能提前领取；重启后从数据库恢复；次数/窗口耗尽；AUTH 与 UNKNOWN 不自动重试；并发调度与人工处置单次生效；原始 attempt、EML、Message-ID 保持不变；执行 race/数据库集成测试、vet、迁移和容器检查后提交 `phase-4a`。

## 协议与 Message-ID 后续

入站使用 go-smtp，出站为 textproto/tls。按方向补测 EHLO、PLAIN/LOGIN、TLS、RSET/NOOP/QUIT、多个 RCPT、参数/SIZE、SMTPUTF8/8BITMIME 支持和拒绝、dot-stuffing、长头、CRLF 与中途断开。本轮先扩展重试相关 Fake Provider 以便精确观察每轮收件人，不把这当作兼容矩阵全部完成。

保持 `preserve` 默认：Gateway UUID 独立于可为空的 Original Message-ID。未来引入 `require`/`inject_if_missing` 时保存原始与实际投递两个版本及各自哈希、Effective Message-ID；投递版本固定后重试只复用它。注入属于独立功能，不在 4A 偷改 MIME。


## 本轮实现与验证记录

已实现 4A 的纯函数决策、逐人预算与数据库调度、固定 Provider、UNKNOWN 人工处置、追加审计和默认关闭开关。版本化决策保存证据、预算、动作与实际重试时间。额外修正了 AUTH 配置拒绝/网络断开分类，新增仅 LOGIN Provider 协议测试。

验证：Go vet、决策/配置 race 单测，以及隔离 PostgreSQL + Fake SMTP 全套 race 集成测试。覆盖只重发临时拒绝收件人、原文一致、备用 Provider 不连接、未来计划不提前领取、进程内状态无关的调度、并发单次推进、次数/时间窗口、已排队后过期、关闭开关、UNKNOWN 人工操作竞争、旧操作拒绝和调度事务故障回滚。

真实 Provider 矩阵仍待账号/收件人；没有启用自动重试或发送外部测试邮件。本轮未实现 4B/4C、完整协议兼容矩阵、Message-ID 注入、通用人工恢复或 UI。前三阶段 main 基线保留，4A 在独立分支提交。

容器验收：迁移版本 4，ready 正常，RETRY_ENABLED=false、WORKER_COUNT=0；人工处置非法 UUID 被拒绝且未更改数据。


## Phase 4B 实施方案（2026-09-11）

基线为 4A 与真实收件侧核验检查点 `0db4acb`。本轮实现默认关闭的安全切换，不更换 SMTP 核心，不修改 MIME，也不在 worker 的网络错误分支即时重发。

- 配置 `FAILOVER_ENABLED` / `failover_enabled` / `--failover-enabled`，默认 false，要求同时启用 RETRY_ENABLED。保留现有退避、逐人 7 次 / 24 小时预算。
- 到期调度后，在领取事务中检查所有未完成收件人均已排队、自动重试获准、预算有效。每人的最新不可变 attempt 决策必须是 version 2 且允许切换且不存在可能已投递；拒绝使用租约恢复遗留的旧决策。
- 目前一封邮件仍只有一条路由；混合 RCPT 暂时拒绝、未来到期或人工暂停会阻止整封路由切换。允许已经完成部分收件人的邮件只为剩余安全子集切换。此保守边界避免引入第二套并行路由状态。
- 只为 DNS / TCP / TLS 失败、正文前 DATA 临时拒绝执行切换；AUTH、RCPT、正文后的最终拒绝保持原有处理，UNKNOWN 绝不自动切换。
- 候选必须启用、TLS 配置有效，并同时授权 Envelope From 与 Header From 的域，满足并发限制。hourly/daily limit 非空仍排除（尚未实现记账）。滚动健康、熔断和半开放行留在 4C，不能把连接诊断模型当作健康算法。
- 优先选择未尝试过的合格备用，最多涉及三个不同 Provider；不切回已尝试的旧 Provider。没有合格备用时按原预算重试当前 Provider；当前也不可用时等待，不放宽限制。
- route、attempt、逐人 PROVIDER_FAILOVER 事件在同一事务提交，记录前后 Provider、来源 attempt 与决策；失败全部回滚。后续 DATA fence、租约恢复、UNKNOWN 和最终提交失败语义保持。
- 决策 version 2 收紧矛盾最终接受证据与正文前 DATA 许可；version 1 历史决策只允许原路由重试，不直接切换。迁移 5 只添加收件人历史尝试查询索引，不改旧状态，不自动激活历史暂停邮件。

验收涵盖真实 Fake SMTP 网络失败与跨 Provider 恢复、原文保持、已成功收件人不重发、混合拒绝不切换、候选授权/容量/配额/开关/预算、并发领取、切换审计回滚、切换后崩溃与最终提交失败。真实邮箱本轮不发送；两套已有账号的发件域不同，不能直接作为互备验收。

### 4B 验收结果

- `go vet ./...` 通过。
- 隔离 PostgreSQL + Fake SMTP 容器内 `go test -race -count=1 -timeout=5m ./...` 全部通过，最终集成测试耗时 100.404 秒。
- 迁移版本 5；隔离容器 `/health/ready` 返回 ready，启动记录确认 workers=0、automatic_retry=false、automatic_failover=false。
- 已检查暂存差异；原始邮件 `temp/` 同时排除出 Git 和 Docker 构建。本轮没有外部邮件发送，未启用开发环境投递。

DNS 许可通过决策单测验证；TCP 拒绝、TLS 验证失败和 DATA 451 通过真实本地 socket + PostgreSQL 验证跨 Provider 恢复。没有将这些故障测试描述为真实服务商互备验收。
