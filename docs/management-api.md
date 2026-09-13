# 管理 API v1

此 API 默认关闭，供受信任的非浏览器管理客户端使用。它不接收新邮件、不提供正文/附件，也不会自动发送诊断邮件。通过 HTTPS 或受控本机连接使用，不能把管理令牌暴露给公共前端代码。

## 启用与身份

环境变量 `ADMIN_API_KEYS` 是 JSON 数组，每项只有 `id`、`role`、`token`。最多 32 个身份；ID 为 1–128 个 ASCII 字母、数字或 `._-`；令牌为 32–512 个非空白可打印 ASCII 字符。每个 ID 和令牌必须唯一，不能与 metrics 令牌共用。用密码生成器生成随机令牌，不使用可猜测口令。

结构示例（占位符不是可用令牌）：

```json
[{"id":"alice","role":"admin","token":"<独立生成的随机管理令牌>"}]
```

将该值放进受保护的本地环境/秘密管理器，不提交到 Git。Compose 已传递该环境变量。启用时还要求有效的 `RELAYTALE_MASTER_KEY`，用于 Provider SMTP 凭证写入；不把管理 API key 与未来 HTTP 发信的 `api_keys` 表混用。空值不注册可用管理功能，返回 404；非法配置启动失败，不静默降级。

每次请求发送 `Authorization: Bearer <token>`。不接受 cookie、查询参数令牌或客户端自报 actor；审计 actor 固定为认证 ID。令牌配置在启动时加载，撤销/轮换需更新配置并重启；已经开始的请求不能被撤回。此阶段没有账户登录、MFA 或自动过期。

| 角色 | 查询 | suppression / UNKNOWN 写入 | Provider 写入 |
| --- | --- | --- | --- |
| viewer | 是 | 否 | 否 |
| operator | 是 | 是 | 否 |
| admin | 是 | 是 | 是 |

所有角色都能读取邮件元数据和收件人地址，因此 viewer 也必须可信。当前不开放 CORS，带 Origin 或跨站 Fetch 标记的请求被拒绝，包括浏览器同源 Origin；Phase 6 已提供独立的工作台会话/CSRF 入口，见 [Web UI](web-ui.md)；本节 Bearer API 的来源限制保持不变。

## 请求和响应约束

基路径 `/admin/v1`。写请求使用 `Content-Type: application/json`，最大 16 KiB，拒绝未知字段、重复字段、尾随 JSON 和超过 16 层的嵌套。每个已认证请求最多占用 5 秒操作上下文，最多 8 个并行请求，超额 503。令牌比较前后均不输出令牌或底层数据库错误；响应标记 no-store/nosniff。没有应用层无限等待队列。这些限制不是公网 HTTP 抗 DDoS 保证。

列表参数仅 `limit`（默认 50，1–100）、`after`（上页 `next_after`）。返回：

```json
{"items":[],"next_after":"","has_more":false}
```

`has_more=true` 时继续取下一页。普通列表按 UUID 升序；时间线按 `(event_time,id)` 升序，`after` 对应同一消息的事件。分页不是跨请求快照：并发插入较早排序位置的记录可能要求重新查询，不能把它当可靠事件订阅。不存在的消息的子列表为空，详情本身返回 404。

## 查询

| GET 路径 | 内容 |
| --- | --- |
| `/providers`、`/providers/{id}` | 配置、revision、健康状态/熔断状态、滚动小时与 24 小时预留单位 |
| `/messages`、`/messages/{id}` | Gateway ID、原 Message-ID、主题、发件人、状态、时间、大小、存档状态 |
| `/messages/{id}/recipients` | 收件人状态、SMTP 码、重试时间、latest_attempt_id |
| `/messages/{id}/attempts` | Provider、尝试序号、结果、关键时间、错误类别 |
| `/messages/{id}/events` | 事件类型、时间、来源、关联尝试/收件人、attempt_sequence |
| `/suppressions` | 活跃及历史抑制记录、原因和操作者 |

不返回密码、密文、nonce、EML 路径、原文、任意 metadata/data、原始 SMTP 回复/调试日志或 claim token。时间线当前是结构化摘要；完整 Flight Recorder 导出仍用受信任 CLI。主题截至 2048 字符、原 Message-ID 至 998 字符、发件地址至 320 字符，避免通过巨型邮件头放大响应。Provider username 属配置字段，会返回，不应将密码放入该字段。

`SMTP_ACCEPTED` 只代表 SMTP 接受；`DELIVERED` 可以来自人工声明，不能用它覆盖历史尝试证据。配额是保守预留的收件人尝试单位，不是入箱数；健康记录为空表示尚无观测，不能当作健康。

## Provider 写入

创建：`POST /providers`，请求含客户端生成的 UUID `id`、`settings`、`password`、`reason`。重复 UUID/名称返回 409，避免网络结果不确定后重复创建。新 ID 必须由客户端保存，失败后先查询再决定下一步。

完整配置替换：`PUT /providers/{id}`，含 `expected_revision`、完整 `settings`、`reason`。settings 字段：

```json
{
  "name":"primary",
  "host":"smtp.example.test",
  "port":587,
  "security":"starttls",
  "username":"smtp-user",
  "priority":10,
  "max_connections":1,
  "timeout_seconds":30,
  "hourly_limit":0,
  "daily_limit":0,
  "from_domains":["example.test"],
  "enabled":false
}
```

`enabled` 默认 false；启停也通过完整配置替换。`security` 只允许 starttls/implicit_tls，域名与超时等复用 Provider 验证规则。0 配额表示不限；from_domains 是路由资格声明，不是 SPF/DKIM/DMARC 验收证明。不要把详情响应直接作为 settings 回传：详情还包含只读字段。

SMTP 密码轮换：`POST /providers/{id}/credentials`，含 `expected_revision`、`password`、`reason`。成功响应含 id 与新 revision；不返回密码。配置替换不修改密码。该操作不更换网关主加密密钥，主密钥版本与轮换仍未实现。

数据库触发器对所有 Provider UPDATE 增加 revision，包括本地 SQL，避免旧 API 版本覆盖新配置；审计与修改同事务提交。版本冲突返回 409，客户端重新读取后人工决定。停用仅影响后续领取，不能撤回已领取的 SMTP 尝试；配置/凭证变更不替换在途快照。更新不清空配额、健康状态或重试预算。

现有 `create-provider` CLI 保持历史的启用行为；API 创建默认停用供检查。两者共用配置验证规则，API 额外要求身份、理由和版本，不改变 CLI 的既有调用格式。

## suppression 与 UNKNOWN

`POST /suppressions`：`email`、`category`、`reason`、可选 RFC3339 `expires_at`。category 为 manual/hard_bounce/complaint/invalid/unsubscribe。添加要求当前没有活跃条目，否则 409；复用全局策略互斥和追加审计。

`POST /suppressions/{id}/release`：`reason`。指定不可复用的条目 UUID 即为并发条件；已解除或不存在返回 409。解除不重发历史邮件。

`POST /recipients/{id}/resolve-unknown`：

```json
{
  "expected_attempt":"<最新尝试 UUID>",
  "action":"mark-failed",
  "reason":"人工核对后的处理依据",
  "acknowledge_duplicate_risk":false
}
```

action 允许 retry/mark-delivered/mark-failed。retry 必须显式 acknowledge_duplicate_risk=true，且继续接受存档、suppression、租约和最新 UNKNOWN 检查。重复操作或旧尝试返回 409，不允许直接修改消息 status。成功返回 audit_id，原 SMTP 尝试结果保持不变。

## 错误与重试

400 输入不合法；401 未认证；403 权限或浏览器来源不允许；404 对象/路由不存在；409 并发或业务状态冲突；415 非 JSON；503 容量、超时或持久化失败。业务拒绝的少数底层通用错误目前也归为 503，以免泄漏内部信息。

收到 503 或网络断开不代表事务一定未提交。Provider 按 ID/revision、UNKNOWN 按最新尝试/当前状态、suppression 按条目/活跃状态重新查询后再决定，不能盲目重放危险操作。所有写操作要求理由；审计插入失败会回滚业务修改。
