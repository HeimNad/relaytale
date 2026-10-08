# Phase 8F.2 · 有界 JSON 附件

同一 `POST /v1/messages` JSON 入口增加可选 attachments，复用 8F.1 的凭证、信封头、日期、幂等键、预算、沙箱、状态查询和 Webhook。原始 EML 入口保持透明。无迁移、无新增网络抓取、无配置开关。

## 请求示例

```json
{
  "date": "2026-10-07T12:00:00Z",
  "subject": "订单说明",
  "text": "请查看附件。",
  "attachments": [
    {
      "filename": "订单.txt",
      "content_type": "text/plain",
      "content_base64": "aGVsbG8="
    }
  ]
}
```

网站读取本地文件后使用带填充的标准 base64 编码，不提交 URL。重试保持日期、身份、幂等键、信封、附件顺序、文件名和内容相同；同键内容变更返回 409。附件 MIME 类型大小写会规范化，JSON 字段顺序不影响正文。

## 限制与边界

完整 JSON 请求仍最多 32 KiB，包含所有空白、字段、转义与 base64；最多四个附件，解码后合计最多 16 KiB。附件占用请求空间，正文和附件不能分别耗尽各自上限后仍保证提交成功。大附件应使用既有原始 EML 入口，并遵守部署的消息大小与 Provider 限制；本阶段没有扩大资源预算。

每个附件必须且只能含 filename、content_type、content_base64 三个字符串。文件名为非空 basename，最多 120 UTF-8 字节，禁止控制字符、路径分隔符、`.` 与 `..`。类型为不带参数的 MIME 类型，最多 100 字节，拒绝通配符、multipart/* 和 message/*。base64 禁止空白、非标准字母、非规范填充及非零尾部填充位；零字节附件允许。未知字段、重复字段、null、嵌套对象、超过四项或无效元数据返回 400 invalid_json_message。完整请求或生成 EML 超限返回 413 message_too_large。

附件既不执行、不解压、不读取路径，也不访问 URL；声明的 MIME 类型不是文件格式鉴定。接收成功不表示经过病毒扫描。既有正文至少一项非空的要求、所有收件人可见的 To 头规则继续适用；没有内联附件、Content-ID 或 Bcc。

## MIME 与兼容

省略 attachments 或空数组继续使用 json-v1，既有字节摘要保持不变。有附件使用 json-v2：外层 multipart/mixed，第一部分为原文本/HTML（两者都有时嵌套 multipart/alternative），其后按请求顺序写附件。文件名使用标准 MIME 参数编码，内容保持二进制字节、base64 76 列与 CRLF。边界固定且不超过 70 字符，正文 base64 不会产生边界分隔行。

附件元数据和内容纳入固定构建摘要及 Message-ID。JSON 只在接收资源预算到位后读取；既有临时快照、文件存档、幂等锁与事务入队保持。worker 重试使用存档，不重新读取网站文件或重建附件。

## 验收

完整隔离镜像 `make ci` 通过：格式检查、go vet、构建与全仓库 race 测试；PostgreSQL 集成测试耗时 247.596 秒。新增集成覆盖附件持久化、规范化类型的幂等重放、内容变化冲突、路径文件名拒绝、模拟 SMTP 接受与存档不变。仅使用隔离 PostgreSQL 与 Fake SMTP，不向真实 Provider 发送邮件。

补充边界与固定 json-v2 字节摘要测试在 Linux 下 race 检查通过（1.019 秒）；覆盖文本/HTML 单正文及嵌套 alternative、中文文件名、二进制字节、四个空附件、16 KiB 临界值、总量超限、错误 base64、重复/未知字段和危险元数据。既有 json-v1 固定摘要继续通过。

OpenAPI JSON 及引用、暂存 diff 检查与 Gitleaks 秘密扫描通过。工作区整体格式检查另发现已忽略的本地 `temp/explain-validation/main.go` 未格式化；该文件不在提交与测试镜像中，未改动。镜像格式检查和本阶段所有 Go 文件格式均通过。
