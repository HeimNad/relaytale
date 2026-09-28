# Phase 8D.2：签名 Webhook 与持久化通知

网站可以接收邮件事实变化通知，再通过 [8D.1 状态接口](phase-8d1-message-status.md) 读取当前逐收件人结果。通知失败只重试通知，不会重新发送邮件。默认关闭网络发送；本阶段不启用真实网站回调，也没有把未验证 DSN 升格为可信退信。

## 配置与启停

先配置现有 `SEND_API_KEYS`，再以环境变量设置通知目标：

```text
WEBHOOK_ENDPOINTS=[{"id":"<新 endpoint UUID>","api_key_id":"<现有 send identity UUID>","url":"https://your-site.example/relaytale/events","keys":[{"id":"key-2026-09","secret":"<32 随机字节的小写 hex，共 64 字符>"}]}]
WEBHOOK_ENABLED=false
```

占位符需要替换后才能使用。签名密钥独立生成、通过安全渠道交给网站，不复用 API token；真实配置不进入 Git。每个发信身份最多一个启用的 endpoint，总配置最多 32 个。配置由管理员控制，发信请求不能指定回调地址。

**配置 endpoint 后会捕获未来事件，即使 `WEBHOOK_ENABLED=false`。** 该开关只控制网络发送，设为 true 并重启后处理积压。无 endpoint 的默认部署不生成通知；新增目标不回补历史事件。删除配置并重启会暂停该 endpoint，旧记录保留。暂停期间不捕获新通知，网站需主动查询相关消息。

目标必须为 HTTPS 443，不允许 URL 用户名/密码、查询参数或片段。每次连接重新解析 DNS，所有地址均须通过公网检查；拒绝私网、回环、链路本地、元数据地址所在网段、映射/转换地址和特殊用途网段。直接连接通过检查的数字地址，TLS 使用原主机名并正常校验证书；不使用环境代理，不跟随重定向。DNS 答案最多 16 个，首个地址失败按通知退避重试。

同一 endpoint 的地址和归属不可更改。需要换地址时为同一身份配置**新的 endpoint ID**，旧 endpoint 自动暂停，历史通知不会转移到新网站。恢复旧目标须显式重新配置它。此限制避免积压数据被静默发送到新的接收方。

## 网站收到的内容

以 JSON POST 发送固定字节的事件提示，最大 4096 字节：

```json
{
  "schema_version": 1,
  "event_id": "<原始事件 UUID>",
  "type": "SMTP_ACCEPTED",
  "message_id": "<Gateway UUID>",
  "recipient_id": null,
  "attempt_id": "<尝试 UUID 或 null>",
  "occurred_at": "<数据库事实时间>",
  "sequence": 123,
  "feedback_availability": "NOT_EXPOSED"
}
```

只捕获 API 身份所属消息的公开事件：MESSAGE_QUEUED、MESSAGE_SUPPRESSED、SMTP_ACCEPTED、ATTEMPT_FINISHED、ATTEMPT_FAILED、DELIVERY_DECIDED、DELIVERY_UNKNOWN、UNKNOWN_RESOLVED、RETRY_EXHAUSTED、RETRY_QUEUED、RECIPIENT_SUPPRESSED、SUPPRESSION_GATE_BLOCKED、PROVIDER_FAILOVER、LEASE_RECOVERED。

不含邮件正文、主题、邮箱地址、SMTP 自由文本、event JSON、Provider 凭证或内部诊断。收件人/尝试引用必须属于同一消息，无法验证则为 null。消息状态不放进事件通知快照：事件可能在同一事务的状态更新之前生成；网站以 GET 查询最新状态，并处理部分成功、人工处置及 UNKNOWN 的原有边界。

`sequence` 是数据库捕获序号，允许空洞和乱序提交，不是提交水位、连续消息版本或网站应等待的下一个序号。事件时间也不保证交付顺序。不要用“丢弃小于最大序号的事件”代替去重；刷新最新状态时应合并同一消息的查询任务，并遵守查询接口的限流。

## 签名、去重和轮换

请求头：

```text
Content-Type: application/json
X-RelayTale-Timestamp: <Unix 秒>
X-RelayTale-Signature: key-2026-09:<64 字符 HMAC hex>
```

签名为 `HMAC-SHA256(hex_decode(secret), timestamp + "." + 原始请求体字节)`。不能先解析 JSON 再重新序列化验签。网站限制正文大小，检查时间戳为规范十进制整数且距本机时间不超过 5 分钟，使用预先配置的 key ID 和密钥进行常数时间比较。时间戳与签名每次尝试更新，正文和 event_id 保持不变。

轮换可同时配置两个不同 key ID，头中以逗号分隔两份签名，网站认可任意一个已信任密钥即可。先让网站信任新密钥，再配置新旧双签并重启，确认后移除旧密钥。旧进程的配置哈希不匹配时禁止领取或开始新的请求；已经开始的请求不能撤回。密钥只在环境配置中，数据库保存配置哈希而非明文密钥。

时间窗口不能代替幂等。推荐网站处理顺序：

1. 检查大小、时间窗口和 HMAC，再解析 schema_version 与事件内容。
2. 在业务数据库事务中以 event_id 唯一键保存通知和后续查询任务；重复通知不重复执行业务动作。
3. 提交事务成功后回复 2xx，后台查询 Gateway 状态并更新业务记录。

接收端崩溃或事务失败时不要提前回复成功。事件历史和去重记录要覆盖人工重放及灾难恢复周期，不能只保留 5 分钟。不要从 SMTP_ACCEPTED 推断入箱，也不要在 UNKNOWN 时自动重发邮件。

## 持久化与失败语义

迁移 00014 增加 endpoint、outbox 和追加式 delivery log。events 的数据库触发器在原邮件事实事务内创建不可变通知正文；通知插入失败会使原事务失败，遵循既有邮件事务失败/UNKNOWN 恢复策略，不静默丢通知。确认和领取事务要求 synchronous_commit=on。

通知状态为 PENDING → IN_FLIGHT → DELIVERED（网站确认），或回到 PENDING / 进入 DEAD。这里 DELIVERED 只表示 **Webhook 被网站确认**，不是邮件送达。每次领取先持久化次数和 STARTED 日志，使用 60 秒租约及随机 token；过期租约可回收，旧 token 不得确认新领取。

2xx 且响应读取成功才确认。非 2xx、超时、TLS/DNS 错误、重定向或超限响应均使用有界重试；不保存网站响应正文或自由文本网络错误。响应头上限 8 KiB、响应体上限 4 KiB、HTTP 总超时 10 秒；单次调度工作限制 15 秒，收尾数据库操作另有 5 秒限额。

每轮最多 8 次领取，24 小时过期，退避为 1/2/4/8/16/32/64 分钟加 0–15 秒 jitter。崩溃后已领取次数不退还。最后一次崩溃或过期记录也会进入 DEAD；退役扫描每批最多 100 条。暂停/未配置 endpoint 的过期记录在重新启用时处理。

采用有界的“至少一次尝试”交付，允许重复、乱序和最终死信，不承诺网站一定收到。典型重复：网站完成处理并回复成功后，网关确认事务失败；租约回收后再次发送同一 event_id 和精确正文。故障注入覆盖这一边界。

当前一个串行 dispatcher 轮询所有配置目标，每轮每目标最多一条、轮询间隔 1 秒；慢目标会延长一轮时间。DB 连接池仍与主服务共享，不宣称高吞吐或多实例生产就绪。通知历史保留在数据库中，本阶段不自动删除；容量告警、保留期治理继续受生产运维阶段约束。

## 运维

```sh
relaytale list-webhooks --after-sequence 0 --limit 100
relaytale pause-webhook --endpoint-id '<UUID>' --actor '<operator>' --reason '<reason>'
relaytale replay-webhook --sequence 123 --expected-generation 0 --actor '<operator>' --reason '<reason>'
```

列表返回通知状态、尝试次数、重放 generation、调度时间和 endpoint 是否启用，不输出密钥或 URL。翻页使用最后一条 sequence。暂停不会撤回在途请求；持久暂停还应删除环境配置，重启同步会重新启用仍配置的目标。

只有 DEAD 可重放，必须匹配当前 generation，重复执行旧命令会失败。重放增加 generation、重置本轮预算，保留 event_id、正文及历史日志；目标仍暂停时不会发送。DELIVERED 不提供再次发送命令。delivery log 区分 STARTED、RETRY、ACKNOWLEDGED、EXHAUSTED、REPLAY_REQUESTED；孤立 STARTED 表示最终确认未成功持久化，不证明网站没收到。

9A 备份包含通知表；恢复校验核对消息归属和正文引用，并报告待发送/在途通知数。签名配置需独立安全保管。恢复后保留启动阻断，并保持 WEBHOOK_ENABLED=false，先核对网站可能已经收到的通知。恢复旧备份可能再次交付历史事件，网站持久化的 event_id 去重是必要边界。

## 验收范围

单元/本机 TLS 测试覆盖签名原文字节、篡改、过期/未来时间、双密钥轮换、非法配置、私网/特殊网段、混合 DNS 和重绑定、数字地址绑定、重定向、响应大小、取消及不可信 TLS。

隔离 PostgreSQL + Fake SMTP 覆盖事务捕获/回滚、跨身份与 SMTP 隔离、失败退避、正文/日志不可变、租约回收、最终确认事务失败、旧租约确认拒绝、并发领取、预算耗尽、generation 重放、暂停/撤销/轮换、换地址保留旧目标和通知备份恢复。全量检查包括格式、vet、构建和 race 测试。

没有执行真实网站回调或真实外部发信。Provider DSN 的可信来源和唯一关联仍待 7B 衔接；`NOT_EXPOSED` 明确表示此阶段没有输出已验证异步退信。
