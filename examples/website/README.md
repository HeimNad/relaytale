# 网站接入参考后端（默认沙箱）

这是可运行的 Go 后端示例，不包含浏览器 UI，也不是生产部署模板。它演示持久化提交、相同幂等键重试、状态查询、签名通知收件箱及状态对账。Gateway 契约见 [OpenAPI](../../docs/api/website.openapi.json)。

## 启动

在 RelayTale 中配置独立 `environment: sandbox` 发信身份、发件白名单，并为该身份配置 schema_version=2 Webhook。Webhook 默认关闭；必须显式启用。示例默认拒绝生产身份，先查询环境再提交。

为示例准备单独 PostgreSQL 数据库。设置以下环境变量，再在仓库根目录运行 `go run ./examples/website`：

| 变量 | 内容 |
| --- | --- |
| DEMO_DATABASE_URL | 示例自己的 PostgreSQL 连接地址 |
| DEMO_GATEWAY_URL | Gateway HTTPS 根地址；本地开发允许 localhost/loopback HTTP |
| DEMO_GATEWAY_TOKEN | 独立沙箱身份的 rts_ 凭证 |
| DEMO_TOKEN | 至少 32 字符的示例后端访问凭证 |
| DEMO_WEBHOOK_KEYS | JSON 数组，1–2 个 `{"id":"current","secret":"64位十六进制密钥"}`；与 Gateway 回调密钥一致 |
| DEMO_ENVIRONMENT | 默认 sandbox；生产接入须显式设置 production |
| DEMO_ALLOW_PRODUCTION | 生产接入还须设置为 true；会允许实际发送邮件 |

服务只监听 `127.0.0.1:8090`。如需 Gateway 真实回调，用自己的 HTTPS 反向代理仅暴露 `/relaytale/hooks`。不要为接入本地示例关闭 Gateway 的 SSRF 或 TLS 检查。自动集成测试使用独立测试发送器，不代表生产回调允许私网。

## 网站调用

`POST /demo/submissions`：Bearer DEMO_TOKEN、Content-Type application/json、Idempotency-Key，正文为 `{"from":"允许的发件地址","to":["收件地址"],"mime":"包含 CRLF 的完整原始 MIME"}`。MIME 上限 2 MiB，收件人 1–100 个。响应 202 返回网站记录 id 与 Gateway message_id。

网站必须在调用前保存自己的幂等键；遇到超时或 503，使用原键及完全相同的参数重试。相同键不同参数返回冲突。示例把请求先保存在自己的数据库，Gateway 已提交但 HTTP 确认丢失时，重启后仍能找回原 message_id。

`GET /demo/submissions/{id}`：同一 Bearer 凭证，读取持久化记录及最近查询快照。该端点不会返回存储的 MIME 或凭证。示例每 5 秒对有待刷新标记的记录查询 Gateway；失败保留待刷新标记继续重试。

`POST /relaytale/hooks`：使用 Gateway 的时间戳与 HMAC 签名，精确验证原始字节、五分钟时间窗口及轮换密钥。事务提交通知收件箱后才返回 204；按 event_id 去重。通知只触发重新查询，不把迟到的通知覆盖成当前投递状态。查询期间收到新通知会保留待刷新标记。

## 显式生产接入与人工退信

生产模式使用独立生产发信身份和独立示例数据库，同时设置 `DEMO_ENVIRONMENT=production`、`DEMO_ALLOW_PRODUCTION=true`。这是允许真实发信的开关；不要使用真实收件人做自动测试。身份环境必须与配置匹配，错配在提交前拒绝。

生产模式对账时分别查询邮件状态与 `/v1/messages/{message_id}/feedback`。示例记录的 `snapshot` 保留 SMTP 事实，`feedback` 保存独立的人工退信结论；二者不会互相覆盖。关联、确认、撤回和证据冲突的签名通知都只触发重新查询。来源仍为 `UNVERIFIED`，自动抑制关闭。Gateway 签名证明回调来自 Gateway，不能证明原始退信来自 Provider。

反馈查询新增 `snapshot_at`，示例用两份快照时间与持久化通知修订号阻止并发旧查询覆盖新结果。反馈超过单页容量时拒绝刷新，保留待刷新标记，不静默截断。当前 Gateway 每封邮件最多允许 100 个人工关联。

这项扩展已完成本地网站与浏览器闭环；完整验收结果及范围见 [验收记录](../../docs/phase-7b8-website-acceptance.md)。

## 沙箱练习

Gateway 的 `GET /v1/messages/{message_id}/inspection` 返回 MIME 部件、附件名称和编码/解码字节数，不返回正文、不渲染 HTML；仅支持不超过 2 MiB 的测试存档。

`POST /v1/messages/{message_id}/simulations` 使用独立 Idempotency-Key 和 JSON `{"recipient_id":"该邮件的收件人 UUID","outcome":"bounce"}`。outcome 可选 accepted、temporary_failure、permanent_failure、delivery_unknown、bounce。每封邮件最多 100 次已提交模拟；相同键可重放。模拟事件始终标记 sandbox/simulation，邮件及收件人仍为 SANDBOX_STORED，不发送 SMTP，也不修改真实 suppression。

## 使用边界

一个示例数据库只配一个稳定 Gateway 发信身份；轮换凭证应保持原身份。切换 Gateway 或身份时使用新数据库，不能把旧记录混入新范围。示例请求存储含完整 MIME，应自行限制数据库访问、保留期限及备份；这与 Gateway 文件存档分层不同。DEMO_TOKEN 仅用于自己的服务端调用，不能放进浏览器。

本阶段验证的是隔离 PostgreSQL 与 Fake SMTP 的闭环，不证明 Provider 最终送达或生产 HTTPS 回调可达。生产权限、迁移、密钥生命周期继续按 9B 验收。
