# Phase 8F.1 · 有界 JSON 发信入口

## 使用

同一个 `POST /v1/messages` 增加 `Content-Type: application/json`，既有 `message/rfc822` 入口保持原文透明。身份、Idempotency-Key、X-RelayTale-From、重复 X-RelayTale-To、消息流、业务关联、有效期与查询/回调均复用现有契约，不从正文接受任意 project_id 或环境。

```json
{
  "date": "2026-10-07T12:00:00Z",
  "subject": "订单已确认",
  "from_name": "我的网站",
  "reply_to": "support@example.test",
  "text": "订单已确认，请登录网站查看。",
  "html": "<p>订单已确认，请登录网站查看。</p>"
}
```

提交时仍需信封与身份头，例如将上面的 JSON 保存为 message.json：

```sh
curl --request POST "$RELAYTALE_URL/v1/messages" \
  --header "Authorization: Bearer $SEND_API_TOKEN" \
  --header 'Content-Type: application/json' \
  --header 'Idempotency-Key: order-task-opaque-id' \
  --header 'X-RelayTale-From: sender@example.test' \
  --header 'X-RelayTale-To: customer@example.test' \
  --data-binary @message.json
```

地址使用实际已授权发件人和业务收件人；示例域不可作为真实投递目标。网络超时后重试同一幂等键、相同时间、信封与业务参数，不生成新任务。

Date 是网站创建此业务任务时捕获的时间，重试要继续使用同一时间，不能每次取当前时间。须 RFC3339、整秒、年份 1970–9999；服务端规范化为 UTC，时区等价的时间生成同一正文。这个字段是 MIME Date，不是定时发送或有效期。subject/from_name/reply_to 可省略；text/html 至少一项非空。

请求 JSON 最多 32 KiB（包含空白和转义），text 与 html 各最多 16 KiB UTF-8；subject 最多 256 字节，from_name 128 字节，reply_to 254 字节。Reply-To 接受单个邮箱地址，复用地址规范化。标题、发件人显示名和 Reply-To 禁止控制字符与 CR/LF。完整 EML 仍受现有 MaxBytes 限制。

只接受六个字段，且值必须是字符串。拒绝重复字段、未知字段、嵌套对象/数组、null、数字、尾随第二个 JSON 值及非 UTF-8 字节。未提供的可选字符串等同空字符串；JSON 字段顺序和空白不影响构建。身体文本不做 Unicode/NFC 或换行改写，base64 传输保留其解码后的字节。

## 固定构建与既有投递语义

第一版固定标记 `X-RelayTale-Builder: json-v1`，自动生成稳定 Message-ID；根据构建版本、规范化 JSON、身份、幂等键和规范化信封生成摘要，不在头中暴露幂等键或 API 凭证。正文变化生成不同 EML，同键重放会由现有请求指纹返回冲突。

发件人显示名使用标准邮件地址序列化；主题使用 UTF-8 encoded-word。所有信封收件人都会写入可见 To 头；此版本不提供 Bcc，私密多收件场景应逐消息提交或使用原始 EML。文本和 HTML 使用 base64、76 字符换行和 CRLF；两者都有时构建 multipart/alternative，纯文本在前，HTML 在后。边界固定且不超过 MIME 的 70 字符限制，不会与 base64 正文冲突。不注入追踪、不签 DKIM、不抓取 URL、不执行模板代码。HTML 由网站提供，服务端不把它渲染成网页，也不宣称净化了内容或阻止收件客户端加载外部资源。

渲染使用惰性 Reader：只有 APIReceiver 取得既有工作内存和临时存储预算后才读取 JSON 和构建正文。无需嵌套获取额外预算，避免小预算配置下相互等待。32 KiB JSON 和有界字段约束构建工作集；继续使用既有 4 个全局提交槽、每身份并发/速率限制和读取期限。请求体不落第二份 JSON 档案，也不写入日志。

生成后的完整 EML 交给现有 ReceiveExpiring，复用临时快照、固定指纹、跨进程同键锁、存档、提交预算、suppression、流调度、到期、沙箱和事务事件。202 只代表网关持久接收，不能当作入箱。生产 Worker 重试读取原存档，不重新构建 MIME。

原始 EML 与 JSON 共用同一身份幂等键空间；混用格式不创建第二套任务。字节/信封/策略不同则冲突；刻意提交完全相同的已生成 EML 可按原指纹重放。

## 响应与兼容

响应格式、查询与回调版本不变。无效 JSON/构建字段返回 400 invalid_json_message；请求 JSON/生成 EML 超限返回 413 message_too_large。未知 Content-Type 返回 415 use_message_rfc822_or_json（原 415 错误名称新增 JSON 支持）。凭证、发件白名单、业务字段、流、期限与预算仍沿用原有响应。不得依据泛化错误自行切换 Provider 或更换幂等键重发。

OpenAPI 为 application/json 增加 JsonMessage schema，原 message/rfc822 内容类型保留。

## 此阶段未实现

附件、任意头、自定义 Message-ID、模板、语言版本、预览界面、定时与取消均不在此入口接受。后续应以明确版本扩展；json-v1 生成规则应保持兼容，不能升级后使相同请求变成不同正文。

## 验收

验收通过：完整 `make ci` 执行格式检查、`go vet`、构建和全仓库 race 测试；隔离 PostgreSQL 的集成测试耗时 280.667 秒。测试使用 Fake SMTP，不访问真实 Provider 或收件箱。首次全量检查因 OrbStack 退出而中断，重启后重新完整运行通过，中断运行不计为验收。

新增测试覆盖中文主题与显示名、文本/HTML MIME、稳定 Message-ID 与固定 MIME 摘要、等价时区/字段顺序的幂等重放、正文变化冲突、真实存档与模拟投递、沙箱不出站，以及严格字段和大小限制。资源测试确认预算不足时不读取 JSON，取消等待返回失败；恢复预算后可正常提交，避免嵌套预算死锁。固定 MIME 摘要在 macOS 和 Linux 均通过。

OpenAPI JSON 与本地 schema 引用检查通过，`git diff --check` 和 staged diff 检查通过，暂存内容的 Gitleaks 扫描未发现秘密。
