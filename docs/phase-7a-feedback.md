# Phase 7A：DSN 解析与反馈待查队列

日期：2026-09-15。Phase 7 拆为 7A（本阶段）和 7B（可信通道与自动处理）；7A 不代表 DSN 自动化已完成。

## 为什么先做待查队列

已有 SMTP 接受事实不能被一封可伪造的退信覆盖。此次先交付离线导入、原始证据留存、有界解析、幂等和冲突展示、人工关闭，以及审计回滚保护。没有网络消费者、webhook、信封改写、自动关联、自动重发或自动 suppression。

2026-09-15 查阅 [Purelymail 文档](https://smtp.purelymail.com/docs/)、其 [2026 Roadmap](https://news.purelymail.com/posts/updates/2025-12-18-roadmap-2026.html) 以及 [Spacemail 入门文档](https://www.spaceship.com/en-GB/knowledgebase/getting-started-with-spacemail/)。Purelymail 路线图提到入站邮件 webhook 需求，但不能据此认定已有可验证的投递反馈接口。本项目仍未取得并验收签名规范、账号归属及 attempt/recipient 关联证据；这不是断言 Provider 没有 API。读取邮箱的认证也不等于来信真实。

格式依据 [RFC 3464](https://www.rfc-editor.org/rfc/rfc3464.html)：按消息与逐收件人字段提取状态，保留类型前缀；Action 与增强状态码需一致。所有解析结果仍是未经验证的声明。Reporting-MTA 和 Original-Envelope-Id 都不授予信任。

## 实现与边界

- `feedback_reports` 保存原始 bytes、SHA-256、解析结果、来源标签、外部事件 ID、导入操作者和理由。同一事务落盘并写维护审计，强制 synchronous_commit。
- 数据库强制 `trust=UNVERIFIED`；证据和 review 表追加式，拒绝 UPDATE/DELETE。Down migration 明确拒绝隐式销毁证据。
- 唯一键是 `(source, external_id, sha256)`。相同三元组重试返回原 ID，不重复审计。未提供 external ID 时用内容摘要，因此不同内容会成为独立事件，不能据此检测事件 ID 冲突。
- 同一来源和事件 ID、不同摘要的变体全部留存。列表和详情以查询时事实显示 `CONFLICT`，优先于 `DISMISSED`，因此旧记录关闭后仍能暴露新冲突。来源名称区分大小写，运行方应固定使用 Provider/账号命名空间。
- `DISMISS` 只追加人工审阅记录，要求原反馈 ID、摘要、操作者、理由；重复关闭拒绝，审计失败整体回滚。冲突不会因关闭单个变体而被隐藏。
- 列表最多 200 条、UUID 游标；不读取原始 MIME 或完整解析 JSON。详情输出 JSON 字符串，不渲染 HTML，不输出原始 MIME。CLI 的操作者是本地审计标签，不是远程认证身份。
- 没有任何调用投递状态更新或 suppression 写入的路径；`failed/5.1.1`、delayed、delivered、SMTP_ACCEPTED 和 UNKNOWN 之间不会发生隐式状态转换。

## 资源与兼容范围

离线导入只接受普通文件，单份原始反馈上限 **1 MiB**。在开事务之前读取并校验大小；机器状态部分最多 64 KiB，100 个收件人，字段物理行最多 4096 bytes。顶层只解析 `multipart/report; report-type=delivery-status`，第二部分必须是未编码的 `message/delivery-status`（无编码标记、7bit 或 8bit）。第一部分支持 text/plain，第三部分可选 message/rfc822 或 text/rfc822-headers；这两个部分不提供状态字段。接受 CRLF 和 LF 导出，机器状态部分拒绝裸 CR、NUL、无效 UTF-8、重复字段及 Action/Status 矛盾。

格式不支持或解析失败的限额内文件仍原样留存，标记 `UNSUPPORTED`，不是静默丢弃。嵌套/转发 DSN、国际化 message/global-delivery-status、编码机器状态、其他 MIME 变体需要人工检查；没有递归扫描附件或从自然语言猜测原因。超限文件直接拒绝，不入库。带完整大附件的退信可能超过本阶段限额，应保持私有原件待后续流式接入，不能把原件截断后称作完整证据。

这里使用 PostgreSQL bytea 原子保存限额内的小型人工反馈；普通投递 EML 仍在文件存储。没有向公网开放导入，没有无限后台抓取。每个导入进程仍有自身内存开销，CLI 并行度由运行方控制。本阶段没有总保留容量或自动清理；低量人工验收适用，启用连续消费者前必须补流式证据存储、容量限制和保留策略。数据库备份包含私密原件，既有 EML 清理和 export-records 不处理这些反馈。

## 使用

升级服务以执行迁移 00009；CLI 不自行执行迁移。使用具备所需表访问权限的 DATABASE_URL。示例地址和标签均为测试用途：

```sh
relaytale import-feedback --file /private/path/bounce.eml \
  --source provider-account-a --external-id mailbox-uidvalidity-123-uid-456 \
  --actor operator --reason '人工从受控邮箱导出；来信真实性尚未验证'
relaytale list-feedback --limit 50
relaytale show-feedback --id FEEDBACK_UUID
relaytale dismiss-feedback --id FEEDBACK_UUID --expected-sha256 SHA256_FROM_DETAIL \
  --actor operator --reason '已人工检查，此反馈不适用于本次投递'
```

`--source` 和 `--external-id` 是运行方提供的关联线索，绝不是可信认证。不要把密码、访问令牌或私钥写入标签与理由。导入重试应沿用相同来源、外部 ID 和原件；发生数据库提交后响应丢失时也遵循此规则。

## 验收

- 解析单元测试：混合多收件人、CRLF/LF、重复字段、错 Action/Status、转发、截断 MIME、编码机器部分、长字段、数量/大小上限、伪造返回邮件字段隔离。
- 10 秒 Go 模糊测试：1,890,475 次执行，无崩溃或无界结果；不代表所有 MIME 兼容性已验收。
- PostgreSQL 集成测试：8 路并发幂等导入、内容冲突及关闭后冲突、原始 bytes 保持、摘要条件、重复关闭、追加式约束、未知 MIME 留存、审计失败导入/关闭回滚、SMTP_ACCEPTED 与 UNKNOWN 保持及无自动 suppression。
- 全仓库格式、vet、构建、竞态和隔离 PostgreSQL 回归通过（`make ci`，集成测试 76.113 秒）。本阶段未进行真实外部发信。

## 下一步：7B 的入口条件

先确定可验证的反馈来源和账号，再实现 token key ID/轮换、唯一 attempt/recipient 关联、独立反馈状态、可信硬退信的事务内抑制，以及迟到/重复/错账号/伪造/审计失败测试。若只有邮箱原件，继续人工待查，不能增加 `--trusted` 开关绕过来源验证。7A 不将 Message-ID 当唯一关联键，也不改写透明模式的 MAIL FROM 或 MIME。
