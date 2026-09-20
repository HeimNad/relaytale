# Phase 7B.4 · 退信关联候选查询

新增本地只读命令 `find-feedback-candidates`。操作者提供明确的原始 Message-ID 查询线索，系统列出可能对应的 Gateway 消息、Provider、attempt 和 recipient，并复用 `inspect-feedback` 的规则说明匹配与阻断情况。无需数据库迁移，没有后台扫描、发信或自动处置。

## 使用

```sh
relaytale find-feedback-candidates \
  --id FEEDBACK_UUID \
  --expected-sha256 ORIGINAL_SHA256 \
  --rfc-message-id '<original-message-id@example.test>' \
  --report-index 0 \
  --limit 20
```

`--rfc-message-id` 是邮件头中的原始 Message-ID，不是 Gateway UUID。它必须由操作者提供，按保存值精确查询，不做大小写归并、去括号或模糊搜索。不会从退信标题、诊断文本、返回头或 7B.3 提示中自动提取可信标识。原邮件没有 Message-ID 时，本命令不能猜测替代值，仍需人工使用已有 `inspect-feedback` 指定候选 ID。

返回 `hint_source=OPERATOR_SUPPLIED_UNVERIFIED`、`automatic_action_allowed=false`、统一 `snapshot_at`、全局原因及候选 `Inspection` 数组。每项包含可用于进一步显式 inspect 的完整 ID。`candidate_matches` 仅表示现有局部关系检查匹配，**不是唯一关联、可信来源或处置许可**；SOURCE_UNVERIFIED 和 OPERATOR_MESSAGE_ID_HINT 始终保留。

## 查询边界

- 先校验反馈 ID、操作者摘要、数据库摘要与原件实际 SHA-256，摘要不符直接失败。
- 在同一只读 repeatable-read 事务中查找并解释所有返回项，避免候选发现与状态解释来自不同数据库时刻。
- 通过已存在的 Message-ID 索引查询消息，再检查 attempt_recipient 成员关系与 recipient 的消息归属；包含匹配消息的不同收件人/尝试，不在 SQL 中先随意挑最新一项。
- 默认最多 20 项，可指定 1–50；多取一行用于判断截断。总查询 10 秒、每条 SQL 5 秒上限。
- 两封或更多消息共用 Message-ID，即使其中一封还没有 attempt，也标记 AMBIGUOUS_MESSAGE_ID，并将返回项的 candidate_matches 置 false。
- 超限标记 CANDIDATES_TRUNCATED，返回项也不能据此声称匹配成立；本阶段不提供跨快照分页来推断唯一性。
- 无消息与消息尚无尝试分别显示 NO_MESSAGE_MATCH、NO_ATTEMPT_CANDIDATES。
- UNSUPPORTED 反馈仍可按人工线索列出候选，但不使用兼容提示填充标准收件人字段，不取得 candidate_matches。

旧尝试、重复收件人报告、地址改写、报告早于尝试、冲突/已关闭反馈、UNKNOWN 与不稳定 SMTP 接受事实，继续由原有关联预检给出原因。任何结果都不写 feedback、messages、recipients、suppression 或审计表，也不是随后写操作的授权凭证。

## 验收

集成测试覆盖正常候选、UNKNOWN、非标准反馈、错误摘要、无匹配、无尝试、同 Message-ID 多消息、候选截断以及历史尝试；核对所有候选使用同一快照，查询不改变投递与 suppression、不新增反馈审计。原有 inspect 测试同时运行，验证共享规则抽取没有放宽边界。

该功能仍是人工关联工具。尚未证明 Provider 反馈来源可信，也未提供服务端验证后的唯一关联写入。下一阶段应先明确可验证的来源证据，或将候选与兼容说明接入待查工作流；不能把搜索成功当作自动抑制开关。

本地验收（2026-09-20）：隔离 Compose 项目 `relaytale-7b4-test` 的 `make ci` 全部通过，包括格式、go vet、构建、全仓库 race 与 PostgreSQL 集成测试（约 80.6 秒）。使用实际编译后的 CLI 查询隔离测试记录，返回 1 个候选且 automatic_action_allowed=false；暂存差异检查与凭证扫描通过。没有新的外部发信或邮箱读取。
