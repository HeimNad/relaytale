# Phase 9C.1：协调保留规则与只读预检

新增 `preflight-retention`，用于评估拟定 EML 保留期，并解释逐消息保留原因。本阶段没有删除入口、自动调度或 schema 变更；既有 `cleanup` 的策略保持原样，尚未接入本报告的额外依赖。报告不是删除许可。

## 使用

使用现有最小权限运行账号的 DATABASE_URL：

```sh
relaytale preflight-retention --eml-days 180 --limit 100
relaytale preflight-retention --eml-days 180 --limit 100 \
  --after-id '<上一页 next_after>' --as-of '<上一页 as_of>'
```

EML 天数范围 0..36500，默认 0，表示不提出清理候选；每页 1..1000 条，按 Gateway UUID 顺序扫描所有消息。`as_of` 默认数据库事务时间，显式时间不能在未来。重复使用同一时间让保留期截止点稳定；保留时长从 completed_at 计算，恰好位于截止点仍保留。

每页使用 REPEATABLE READ READ ONLY 事务。页内的消息、依赖和反馈查询来自同一快照；不同页是不同快照，运行中的新消息或状态变化可能导致遍历结果变化，UUID 游标不是队列高水位。需要完整稳定盘点时先停写，再遍历。错误、取消或提交失败不发布部分报告或下一页游标；CLI 最长 30 秒，超时须缩小批次或调查查询负载。

报告只含 Gateway UUID、状态、保留原因和复核分类；不输出地址、主题、EML 路径、正文、反馈原件或 Webhook URL。报告不检查物理文件、哈希、软链接、路径安全或备份完整性。CLI 仍按现有协议登记/释放辅助占用，因此“只读”指业务数据与报告查询；占用登记不是只读数据库操作。

## 保留关系

| 数据 | 9C.1 规则 |
| --- | --- |
| EML | 默认保留；只有 AVAILABLE、终态、已记录完成时间且超过拟定期限，并无以下保护依赖，才标记 REVIEW_CANDIDATE |
| 未完成/UNKNOWN | 消息非终态、有效占用、进行中 attempt、未完成或 UNKNOWN 收件人均保留；EXPIRED 收件人仍可能已发送时保留 |
| 幂等记录 | 当前无退役期限，引用消息的 ledger 或旧 idempotency_key 均保护 EML；不是自动到期的缓存 |
| 网站通知 | PENDING、IN_FLIGHT、DEAD 均需要复核，保护消息原文；DEAD 仍可人工重放 |
| 未核对反馈 | 当前没有可信的 Gateway UUID 关联，因此任意未 DISMISS 的反馈导致全局 EML 复核保留，不用 RFC Message-ID 猜关联 |
| 元数据、收件人、events、attempts | 全部保留；不以 EML 达到期限推导可以删除数据库历史 |
| feedback、suppression、maintenance、退役与回收审计 | 全部保留；人工关闭反馈不删除原始证据 |
| 提交/Provider 配额账本、公平调度、Webhook 历史 | 全部保留；本阶段不重置或回收计费状态 |

`REVIEW_CANDIDATE` 只是待人工复核提示；`deletion_authorized` 永远 false，`metadata_disposition` 永远 RETAIN_ALL。不能把报告导出后当成日后直接删除的清单。

保留原因可叠加：RETENTION_DISABLED、NON_TERMINAL_MESSAGE、MESSAGE_LEASE、ATTEMPT_IN_PROGRESS、RECIPIENT_UNRESOLVED、COMPLETION_UNRECORDED、RETENTION_WINDOW、IDEMPOTENCY_REFERENCE、WEBHOOK_REQUIRES_REVIEW、UNSCOPED_FEEDBACK_REVIEW、ARCHIVE_NOT_AVAILABLE。PURGE_PENDING/PURGED 不被当成新候选；其既有可恢复清理协议仍单独处理。

全局反馈保留与永久幂等保留是本阶段有意采用的保守规则，可能导致长期没有候选。后续必须先实现可信或人工核对的明确关联、证据保留与幂等退役契约，才能缩小保留范围。不得为了释放容量自动关闭反馈或删除 ledger。

## 性能与权限

分页限制输出及消息扫描行数，不承诺所有依赖查询都是固定成本；使用已有 message_id 索引和 feedback_reviews 主键。历史数据库增大时应检查查询计划、容量与 VACUUM 压力，再评估专门索引或保留视图。业务函数没有 UPDATE、INSERT、DELETE；CLI 复用运行权限检查和持久辅助占用，拒绝高权限账号，阻止与离线迁移/恢复并发。

## 验收与下一段

集成测试覆盖原文候选、默认关闭、UNKNOWN/收件人不确定、lease、进行中尝试、缺失完成时间与期限、永久幂等引用、待处理/DEAD 通知、全局反馈保留与人工关闭、不可用存档，以及分页、无敏感字段、无业务数据/文件修改、非法参数与取消不发布游标。定向 race 测试通过（3.721 秒），完整 `make ci` 通过格式、静态检查、构建及全部 race 测试，集成测试用时 233.214 秒；暂存差异检查与离线秘密扫描通过。全部使用隔离 PostgreSQL 与 Fake SMTP，没有访问真实邮箱或删除真实存档。

9C.2 将先设计可执行的 EML 协调保护及事务竞争边界，复核/执行时重新检查依赖；元数据、append-only 历史与配额删除继续独立设计。项目统计与网站运营视图属于后续子阶段，9C 整体尚未完成。
