# 7B.7 · 人工退信核验与网站反馈

本阶段按用户选择完成人工闭环。收件箱登录只证明读取权限，不能证明邮件由 Provider 发出。证据始终 `UNVERIFIED`；人工核验不提升来源信任，不自动抑制地址，也不更新 messages、recipients 或 attempt 的 SMTP 状态。

## 使用流程

1. 现有 IMAP 收集或离线导入保留原始证据。
2. 管理员在“反馈详情 → 通知业务网站”选择候选，或输入明确的 Gateway/Provider/attempt/recipient ID，并填写理由、确认来源未验证。
3. 创建关联后，网站看到 `PENDING_REVIEW`，收到 `FEEDBACK_LINKED` 通知。仅 production API 邮件、已完成并收到最终 2xx 的 SMTP_ACCEPTED 尝试可关联；UNKNOWN 不可通过此路径处置。
4. 管理员确认失败或撤回。确认失败显示 `FAILURE_CONFIRMED_BY_OPERATOR`，不会覆盖先前 SMTP 接受事实。严格解析的临时/送达声明不能被确认失败。
5. 后来导入同一 source/external_id 的不同内容会显示 `EVIDENCE_CONFLICT` 并发送 `FEEDBACK_INVALIDATED`；全局关闭证据则显示 `WITHDRAWN` 并通知。冲突优先显示，即使人工已经撤回。

标准 DSN 要求收件人地址匹配、无改写或重复区块，证据不能早于尝试。非标准格式需显式 `manual_association=true` 且区块为 0：这是人承担关联判断，解析提示不自动变成权威关联。现有 Purelymail 非标准样本可以走此路径，但本阶段未读取真实邮箱或发送新真实邮件。

## API

管理员（operator/viewer 无写权限）：

- `POST /admin/v1/feedback/{id}/links`：InspectRequest 的全部字段，加 `reason`、`acknowledge_unverified:true`、可选 `manual_association`。
- `GET /admin/v1/feedback/{id}/links`：已有公开关联与有效状态。
- `POST /admin/v1/feedback-links/{id}/reviews`：`expected_revision`、`action:CONFIRM_FAILURE|WITHDRAW`、`reason`、`acknowledge_unverified:true`。

浏览器沿用现有登录、CSRF 与权限校验。关联创建返回 201；重复关联或过期版本返回 409。提交后连接中断时应读取现状核对，不自动重新创建另一份关联。

网站：`GET /v1/messages/{gateway_id}/feedback?after={uuid}&limit=50`。限额 1..100，稳定身份归属验证，生产 API 消息范围，独立读取限流、只读快照、不缓存。返回 `schema_version:1`、`source_trust:UNVERIFIED`、`automatic_suppression:false`、items 与分页字段；items 仅包含公开关联 ID、recipient/attempt ID、状态、版本与时间，不包含原始退信 ID、邮箱来源、Provider ID、诊断、MIME 或审核人/理由。

现有 `/v1/messages/{id}` v1/v2 的 shape 和 `feedback_availability:NOT_EXPOSED` 保持兼容：它仍只展示 SMTP 状态。异步人工反馈使用独立 endpoint，网站应把两者并列展示，不能把人工失败当作机器证明的地址无效。

Webhook 复用已配置的签名、事务 outbox、重试、重放和 HTTPS/SSRF 约束。新事件 `FEEDBACK_LINKED`、`FEEDBACK_REVIEWED`、`FEEDBACK_INVALIDATED` 携带 `feedback_availability:MANUAL_REVIEW` 和 `feedback_id`（公开关联 ID），不携带理由、原文或 Provider 诊断。收到通知后查询最新反馈；旧 outbox 中历史事件不会被改写，不能把旧事件当作当前结论。签名证明通知来自 RelayTale，不证明原始退信来自 Provider。没有 endpoint 配置时不创建通知；网络发送仍由既有开关控制。

## 持久化与边界

迁移 00030 增加 append-only feedback_links 和 feedback_link_reviews。每份证据区块至多一个关联、每消息最多 100 个关联。关联初始版本 0；版本 1 可确认或撤回，版本 2 只能从确认转为撤回。撤回后不能重新确认；错误关联需保留历史并人工重新评估，不能偷偷改绑到另一封邮件。

导入、证据关闭和核验共享数据库事务栅栏。并发核验只有一个版本能提交；关联/核验、审计、事件与 outbox 一起提交，审计失败全部回滚。数据库守卫检查 message/attempt/recipient 的生产接受关系、关联容量与核验顺序，禁止覆盖历史。

核验关联保守保护该消息原始 EML，不随撤回自动删除；生命周期预检显示 `MANUAL_FEEDBACK_HISTORY`，清理与数据库守卫一致。后续细化保留策略需要独立验收。备份包含新表，恢复检查关联归属与核验顺序。升级仍需离线迁移并重新应用 runtime grants，不能让运行进程自迁移。

## 验收

- 首轮隔离 PostgreSQL/Fake SMTP 针对性竞态集成通过（3.489s），覆盖关联、确认、撤回、来源冲突失效与通知记录。
- 最终浏览器两项通过（4.9s）：查看者只读；管理员人工关联、确认和撤回。桌面与 390px 手机截图已检查，并修正勾选框排列。写入响应使用可控模拟，真实权限、事务和 outbox 由数据库集成测试验证；未宣称真实网站网络回调已验收。
- UI 格式检查、已跟踪 Go 格式及 diff whitespace 检查通过。最终隔离 Docker 完整 `make ci` 通过：格式、vet、build、强制数据库集成的全量 race tests（integration 266.835s），包含新增 UNKNOWN 拒绝和审计失败回滚验证。
- 回归发现测试样例复用固定来源编号会触发正确的重复关联拒绝，且全局抑制表并非空表。已改为每次独立来源编号和操作前后数量比较，最终全量在独立新数据库重跑。

全自动可信 DSN、地址自动抑制和 Provider 来源认证仍属 7B 未完成范围，不能把本阶段标成全自动退信已完成。
