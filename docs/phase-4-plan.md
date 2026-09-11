# Phase 3.5 / 4 实施计划

日期：2026-09-10。基线：`phase-3`。保持 Go + PostgreSQL + 原始 EML + recipient-level state，不重写 SMTP 核心。

## 顺序与交付边界

1. **Phase 3.5 真实链路验收**：准备测试矩阵与证据模板；需要操作者提供 SpaceMail/PurelyMail 测试凭证及明确允许接收测试邮件的地址。未取得这些信息不发送外部邮件，结果不得用 Fake SMTP 替代。
2. **Phase 4A（本轮实现）**：纯函数投递决策、逐收件人持久化重试、同 Provider 固定路由、24 小时/7 次上限、退避与 jitter、UNKNOWN 人工处置及追加审计。自动重试默认关闭，真实验收通过前不启用。保持原文与原始 Message-ID 不变。
3. **Phase 4B**：安全跨 Provider 切换，候选域授权/配额/健康检查，明确事件，逐阶段故障注入。决策允许切换不等于执行切换；4A 不执行切换。
4. **Phase 4C**：滚动 Provider 健康、熔断与半开、配额记账；以决策的错误归属区分 Provider 与收件人错误。
5. 随后管理 UI / HTTP API，再做 DSN / bounce / suppression；配套生产指标、LISTEN 唤醒和数据生命周期工作保留在路线图。

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
