# Phase 8D.1：网站身份范围内的状态查询

网站提交邮件取得 Gateway `message_id` 后，可以查询网关状态、逐收件人结果和公开事件。查询不会发送邮件、触发重试或修改审计记录。仅适用于 HTTP API 提交的消息；SMTP 账号查询授权另行设计。

## 接入契约

服务端通过 HTTPS 使用现有 `SEND_API_KEYS` 发信令牌：

```http
GET /v1/messages/{message_id}?limit=50
Authorization: Bearer <现有 rts_ 令牌>
```

不配置发信身份时接口关闭。禁止浏览器跨站调用；令牌不应放到网页脚本。GET 不接受请求体、内容编码或未知查询参数。ID 必须为非零、规范小写 UUID。`limit` 默认 50，范围 1–100；`before` 为上一页返回的 `next_before`，参数不可重复。

响应含 `schema_version: 1`、`message_id`、`status`、固定说明 `description`、`created_at`、`snapshot_at`、`feedback_availability: "NOT_EXPOSED"`、`recipients`、`events` 和 `has_more_events`，有下一页时附 `next_before`。使用枚举判断状态，不解析面向人的说明文本。

每个收件人包含 `recipient_id`、`address`、`status`、说明、尝试次数，及可用时的 `decision_action`、`retry_at`、`latest_completed_attempt`。后者包含尝试 ID/序号、该次收件人状态、协议阶段、合法 SMTP 数字码/增强码和结束时间；它不代表当前正在进行的尝试。无尝试时省略。响应最多 100 个收件人，与提交上限一致；超出时拒绝返回截断摘要。

每个事件只有 ID、类型、时间、来源分类和同一消息内的收件人/尝试引用。仅公开接收、存档、排队、抑制、尝试开始/完成/失败、SMTP 接受、投递决策、UNKNOWN/人工处置、重试、切换与租约恢复等白名单事件。未知来源归为 `internal`。不返回 event JSON、Provider 标识、自由文本 SMTP 回复、内部错误、邮件主题、MIME、凭证或调试记录。未知尝试状态/阶段显示 `UNCLASSIFIED`。

## 状态边界

| 状态/字段 | 网站应如何理解 |
| --- | --- |
| QUEUED / SENDING | 等待或正在尝试发送 |
| SMTP_ACCEPTED | Provider 接受；不能认定收件箱已收到 |
| PARTIAL_ACCEPTED | 查看每个收件人的结果 |
| TEMP_FAILED | 暂时失败或暂停；不一定启用了自动重试 |
| PERM_FAILED | 本次永久拒绝或人工标记失败；不等于地址永久无效 |
| DELIVERY_UNKNOWN | 远端可能已接受，禁止自动重新提交 |
| DELIVERED | 网关记录（可能是人工确认），不是独立的入箱证据 |
| SUPPRESSED / CANCELLED | 策略阻断或取消，不撤回已经发送的邮件 |
| retry_at | 调度候选时间，不是送达时间或正在运行的保证 |
| NOT_EXPOSED | 此接口尚未提供可信关联的异步反馈，不代表没有退信 |

人工处置改变当前摘要，但不会重写历史尝试。例如 UNKNOWN 被人工标记 DELIVERED 后，最近已完成尝试仍保留 DELIVERY_UNKNOWN。

## 隔离、分页与资源边界

授权依据稳定 API 身份 ID；轮换令牌保留原消息访问范围。数据库撤销或变更哈希会拒绝旧令牌，包括尚未重启的旧 handler。每次读取使用只读 Repeatable Read 事务，授权与结果共享快照；快照之后发生的撤销影响后续请求，在途响应仍可完成。

事件按 `(event_time, event_id)` 倒序分页，相同时间以 ID 区分。游标必须是本消息的公开事件。**这是向旧历史翻页，不是增量订阅或持久水位。** 延迟提交或回填时间的事件可能出现在已翻过的位置；刷新第一页获取最新视图。每页是新的数据库快照，不能把跨页摘要当成同一时刻。未来 Webhook 另行提供通知交付机制。

读取使用独立于发信的进程内预算：全局最多 4 个、每身份最多 2 个并发；每身份每秒补充 1 个令牌、突发 10 次，超出返回 429 与 Retry-After。请求数据库工作最长 10 秒。读写仍共享底层数据库连接池，不宣称完全资源隔离；重启会重置内存限流。

| HTTP | 含义 |
| --- | --- |
| 200 | 查询成功，附 no-store / nosniff |
| 400 | 非法查询或游标 |
| 401 | 缺少、错误、撤销或已轮换的凭证 |
| 403 / 405 | 浏览器来源限制 / 方法不支持 |
| 404 | 不存在、其他身份所属或 SMTP 提交，响应一致 |
| 429 | 查询频率或并发超限，请退避 |
| 503 | 数据库或读取失败，不应解释成空状态 |

## 验收与后续

隔离 PostgreSQL、Fake SMTP 与本机 TLS HTTP 集成覆盖：身份隔离、SMTP 消息隐藏、默认关闭、撤销/轮换、查询无写入副作用、250、混合 RCPT 成败、451/550、认证拒绝、最终响应丢失、UNKNOWN 人工处置、相同时间分页、外部/私有游标、自由文本隔离、读取限流不耗尽发信额度与数据库取消。单元测试覆盖查询大小、重复参数、非法 UUID、请求体及编码边界。

阶段检查：格式、go vet、构建、全套 race 单元及数据库集成测试。只使用合成邮件与本机服务；未发送真实邮件。

后续 [8D.2 签名 Webhook](phase-8d2-webhooks.md) 已实现事务通知队列与通知重试，网络发送默认关闭。可信退信关联仍受 7B 来源证据约束；8D.1 不宣称 SpaceMail/PurelyMail 的异步 DSN 已自动反馈给网站。
