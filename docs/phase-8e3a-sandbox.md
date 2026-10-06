# Phase 8E.3a：不可转成生产的测试沙箱

更新：2026-10-06。安全基础已完成本地验收；8E.3 整体尚未完成，网站示例、OpenAPI 契约、受控模拟结果与 MIME 结构检查在 8E.3b 继续。

## 使用方式

在 `SEND_API_KEYS` 的每个独立 HTTP 身份配置中增加 `"environment":"sandbox"`。省略字段兼容旧部署，仍为 `production`；仅允许这两个小写值。测试和生产必须使用不同 UUID、不同 token。已有身份不能改变环境，换 token 也不能改变；切换上线时创建新的生产身份并重新提交业务需要发送的邮件，不能放行历史测试邮件。

示意配置（占位符不能直接启动）：

```json
[
  {
    "id": "<独立测试身份 UUID>",
    "token": "rts_<64 位随机小写十六进制>",
    "allowed_from": ["sender@example.test"],
    "environment": "sandbox"
  }
]
```

原有 `POST /v1/messages`、原始 `message/rfc822`、发件人/收件人头、业务关联、流、有效期和幂等键均保留。环境由服务器身份配置确定；请求头和 MIME 里的环境字段不能改变它。有效期和身份/流提交预算继续生效，测试身份的用量独立，绝不消耗 Provider 投递额度。

新建或重放返回 HTTP 202：

```json
{
  "message_id": "<网关 ID>",
  "replayed": false,
  "status": "SANDBOX_STORED",
  "environment": "sandbox",
  "simulation": true
}
```

202 表示已持久化测试存档，未表示 Provider 接收。单封状态和列表包含 `environment=sandbox`、`simulation=true`，响应头也包含 `X-RelayTale-Environment: sandbox`。单封和收件人状态都是 `SANDBOX_STORED`，尝试数为零；事件时间线保留接收、存档和沙箱存储。身份隔离、撤销、限流和分页继续适用。生产返回 JSON 保持原有形状，环境响应头为 `production`。

## 隔离边界

- 新增迁移 `00020_sandbox.sql`，历史身份/邮件全部默认为生产，不能将已有 UUID 从生产改成测试，或从测试改成生产。
- API 入队事务从数据库读取身份环境；测试邮件不进入 QUEUED，不设置下一次尝试、路由或租约，也不产生 MESSAGE_QUEUED。原文存档流程保持逐字节不变。
- 数据库 CHECK 将测试环境与 SANDBOX_STORED 绑定，触发器保护测试邮件身份归属，拒绝创建/修改 SMTP attempt、将测试收件人标记为真实接受，以及写入真实 SMTP 接受/不确定投递事件。
- 正常 worker 不会领取测试邮件，也不会连接 SMTP Provider；最终 DATA 授权的 lease fence 还要求生产环境。绕过普通 worker 调用授权仍会被拒绝。
- 测试邮件不进入生产 suppression 状态机。它表达存档事实，没有模拟反垃圾策略判定或投递成功。生产 suppression/重试/有效期检查保持原逻辑。
- 恢复校验检查身份环境、状态、尝试、路由、租约及收件人一致性；恢复数据库后的相同约束继续生效。
- SANDBOX_STORED 是可执行既有保留期清理的终态。原文可按已有明确预览/应用流程清理，事件和幂等记录保留；重复请求不会重新发布已清理原文。

这组约束针对正常应用和直接 SQL 的误操作；拥有修改数据库 schema 权限的管理员仍可删除约束。没有声称抵御管理员主动拆除保护。

## 测试通知

独立测试身份可以配置自己的 Webhook endpoint，沿用现有 HTTPS/SSRF 边界、HMAC 验签、事务 outbox、重试和人工重放。新事件为 `SANDBOX_STORED`，payload 明确附加 `environment=sandbox`、`simulation=true`；schema 1/2 都携带标记，schema 2 继续包含业务关联。生产 payload 不增加字段，历史 outbox 内容不重写。

网站必须将测试身份的 endpoint、签名密钥和业务数据与生产分开。这种通知用于测试接入，不是 SMTP 或 DSN 的成功/失败证据。网络回调仍默认关闭，需要显式开启已有 Webhook 配置才能向网站发出 HTTPS 请求；测试通知也是真实外部 HTTP 请求，不意味着网络完全离线。

## 验收

隔离 PostgreSQL 17 + Fake SMTP，禁止连接真实 Provider。专项覆盖：独立身份环境配置、重放不增加原文/通知、逐收件人测试状态、零 SMTP 连接、最终授权拒绝、数据库层绕过拒绝、生产身份读不到测试邮件、请求头/MIME 不能改变环境、生产正常投递，以及停写备份/恢复后的沙箱保护。

首轮完整回归发现两处测试需要更新：恢复样本新增测试邮件后，幂等记录总数由 1 变为 2；期限授权锁等待测试依赖 SQL 文本前缀，新环境谓词令它无法识别正在等待的事务。后者改为检查 `pg_blocking_pids`，验证实际数据库阻塞关系，保留期限后拒绝 DATA 的断言。沙箱 SQL 绕过检查也明确要求约束/触发器错误，不能用参数或语法错误冒充保护生效。

最终完整 `make ci` 通过：格式检查、`go vet ./...`、`go build ./...`、`go test -race -count=1 -timeout=8m ./...`，隔离数据库集成测试耗时 178.787 秒。管理脚本语法检查、暂存差异检查和无网络 Gitleaks 扫描通过。没有连接真实 Provider，没有发送真实邮件。

## 后续 8E.3b

提供可运行网站示例与 HTTP/OpenAPI 契约，展示持久化 message_id、相同幂等键重试、状态查询、原始字节验签、通知去重与乱序处理。增加有界 MIME/附件结构检查和明确模拟事件；不向生产记录写入假的 SMTP_ACCEPTED/DELIVERED，不在浏览器执行邮件 HTML。当前没有测试 SMTP 账户模式，也没有模拟退信改变 suppression 的接口；可信 Provider 异步反馈仍是单独能力。
