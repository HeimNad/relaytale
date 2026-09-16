# Phase 7B.1：反馈关联与决策安全基础

日期：2026-09-15。用户确认暂时没有 Provider 的可验证反馈接口资料，本阶段先交付只读关联预检。**7B 自动处理尚未完成，自动 suppression 仍关闭。**

## 已交付

新增 `inspect-feedback` CLI，基于 7A 保存的反馈和现有投递记录，输出可解释的候选关联与阻断原因。没有新迁移，没有网络监听、后台任务或可信标记开关，没有修改 SMTP 信封、原始 MIME、Message-ID、UNKNOWN 或 suppression。

```sh
relaytale inspect-feedback --id FEEDBACK_UUID --expected-sha256 SHA256_FROM_SHOW \
  --message-id GATEWAY_UUID --provider-id PROVIDER_UUID \
  --attempt-id ATTEMPT_UUID --recipient-id RECIPIENT_UUID --report-index 0
```

先用 `show-feedback` 取得摘要和逐收件人报告，再从既有消息/尝试查询中选择具体对象。`report-index` 从 0 开始，不能用 Message-ID 或邮箱字符串搜索结果直接代替唯一对象。Provider ID 只是候选对象条件，不证明反馈来自该 Provider 或其账号。

`candidate_matches` 只说明指定对象与所选地址在结构上对应；**不是可信关联、最新结果或自动操作许可**。例如，历史尝试可以结构上匹配，但仍有 `NEWER_RECIPIENT_ATTEMPT`。输出的 `action` 始终为 `QUARANTINE`，`automatic_suppression` 始终为 false，包含 `SOURCE_UNVERIFIED`。

## 预检边界

| 检查 | 不满足时 |
| --- | --- |
| 原始反馈摘要与操作者提供的一致 | EVIDENCE_DIGEST_MISMATCH |
| 来源事件内容无冲突、反馈未关闭 | SOURCE_EVENT_CONFLICT / FEEDBACK_DISMISSED |
| 支持的机器报告 | UNSUPPORTED_REPORT |
| attempt 属于 message 与 provider，recipient 同属 message 且在该 attempt 的成员表中 | RELATION_MISMATCH |
| Final-Recipient 为支持的 rfc822 单邮箱并匹配候选地址 | UNSUPPORTED_RECIPIENT_ADDRESS / RECIPIENT_ADDRESS_MISMATCH |
| Original-Recipient 如存在必须与 Final-Recipient 相同 | RECIPIENT_REWRITTEN |
| 同一地址只出现一个报告块，即使状态一致也不自动消歧 | AMBIGUOUS_RECIPIENT_BLOCKS |
| 本地反馈导入时间不早于候选尝试开始 | REPORT_PREDATES_ATTEMPT |
| 同一 recipient 没有更新的尝试 | NEWER_RECIPIENT_ATTEMPT |
| 当前或尝试状态不是 UNKNOWN | DELIVERY_UNKNOWN_PRESERVED |
| 当前、尝试逐收件人结果均接受，且有已结束的最终 2xx | NO_STABLE_SMTP_ACCEPTANCE |

邮箱匹配只忽略域名大小写，保留 local-part 大小写，不折叠点号、加号或别名。国际化邮箱/其他地址类型在此预检中保守拒绝；不会改动已有 suppression 的数据库规范化规则。Original-Envelope-Id、Reporting-MTA、自然语言 Diagnostic-Code 不用作真实性或唯一关联证明。

`REPORT_PREDATES_ATTEMPT` 使用本地保存时间，不是验证远端事件时间；旧退信今日导入仍可能通过这一条件，绝不能据此认定新鲜。

## 声明分类与事实分离

纯函数 `Assess` 将报告声明分类，并同时给出独立阻断原因：

- failed + 5.1.1 → ADDRESS_INVALID_REPORTED，只是地址无效声明。
- 其他 failed + 5.x.x → PERMANENT_FAILURE_REPORTED，不等于地址长期无效。
- delayed + 4.x.x → TEMPORARY_FAILURE_REPORTED，不能封禁地址。
- delivered / relayed / expanded + 2.x.x → 对应报告声明，不证明入箱。
- 不一致或未知字段 → UNCLASSIFIED。

即使文本含“unknown user”，策略拒绝、配额或信箱满不会被猜测成地址无效。来源认证未实现前，所有分类都只用于人工排查。

## 并发与权限

反馈、冲突/关闭状态、投递成员关系和最新尝试检查在同一个 PostgreSQL **READ ONLY + REPEATABLE READ** 事务快照中完成。不申请投递行锁，不写审计或业务状态，不阻塞 worker 等待人工处置。输出带 `snapshot_at`；查询完成后新反馈或新尝试可能出现，所以输出不能作为之后写入操作的授权票据。

当前只有本地 CLI，没有 HTTP/UI 入口。调用者需要数据库读取权限，输出包含收件地址等私有信息，不应作为公开日志。输入限定对象 ID 和 0..99 的报告索引；不扫描全库自动猜匹配关系。

## 验收

- 单元测试：来源未验证不可转为自动抑制，错摘要/对象、冲突/关闭、转发地址、重复块、新尝试、UNKNOWN、缺失最终响应、过早反馈、地址类型/大小写/别名；硬退信、临时、策略/信箱满、投递/转发声明分别分类。
- PostgreSQL/Fake SMTP：真实本地投递记录匹配；逐个替换 message/provider/attempt/recipient 后拒绝匹配；摘要和报告索引边界；历史尝试的新尝试提示；多块/改写/不支持内容；UNKNOWN 与 SMTP_ACCEPTED 不被改写，维护审计不被只读查询增加，suppression 不被写入。
- 全套格式、vet、build、race 与隔离 PostgreSQL 回归通过（`make ci`，集成测试 179.140 秒）；暂存补丁 Gitleaks 扫描通过。没有使用真实账号或发送外部测试邮件。

## 7B 剩余工作

必须取得并验收实际 Provider 反馈通道。签名验证/重放窗口、Provider 账号归属、独立反馈密钥的 key ID 与轮换、绑定 attempt/recipient 的不可猜测令牌及运输方式均尚未实现；SMTP 凭证不等于这些能力。Purelymail 的 [2026 路线图](https://news.purelymail.com/posts/updates/2025-12-18-roadmap-2026.html) 中出现 webhook 需求不能替代已验收接口。

后续执行层须独立接收已验证证据，在一个写事务内重新检查关联、冲突、当前状态以及全网关同地址的新成功/投递证据，再原子写独立反馈状态、事件与抑制。还需处理多来源、不同消息的迟到冲突、改写信封、重放与崩溃恢复，以及证据总容量/生命周期。当前预检只检查同一 recipient 的更新尝试，不能替代上述全局规则。

不要通过为 `Assess` 增加 CLI 可控的 `trusted=true` 来解锁写入，也不要将“候选匹配”直接连接到 suppression.Add。缺少可信通道期间保持人工待查；7B.1 完成不代表生产级自动退信完成。
