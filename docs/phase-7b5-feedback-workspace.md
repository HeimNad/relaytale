# Phase 7B.5 · 反馈待查工作台

将 7A–7B.4 的只读能力接入管理 API 和浏览器工作台。操作者可以浏览反馈、查看保存的声明和格式问题，并通过人工提供的原始 Message-ID 查找本地投递候选。

## 使用与边界

启用现有管理工作台后，从「反馈待查」进入。viewer、operator、admin 均可查询，沿用会话、来源校验、no-store、并发和超时限制。没有新增写入端点，不导入邮件、不连接收件箱、不发信，也不执行反馈处置或地址抑制。

详情先显示保存的来源、状态、时间和证据摘要，再展示反馈声明与关联查询。原始 Message-ID 必须人工填写，收件人区块编号从 0 开始；它指严格解析结果 recipients 的索引，并非兼容说明中包含 Reporting-MTA 的 blocks 索引。工作台每次最多显示 25 个候选，歧义和截断会明确提示。候选中的「符合」只表示本地关联条件符合，不表示来源可信或退信已确认。

UNSUPPORTED、UNVERIFIED、QUARANTINED 等历史状态不因查看改变；DELIVERY_UNKNOWN 不会被查询覆盖。兼容说明不进入决策引擎。真实来源认证及自动处理仍未完成。

## 管理 API

全部为认证后的 GET 请求，基路径 `/admin/v1`；浏览器使用现有 `/console/api` 会话入口。

| 路径 | 参数与结果 |
| --- | --- |
| `/feedback` | limit 默认 50、1–100；after 游标；返回 items/next_after/has_more，不返回声明正文 |
| `/feedback/{id}` | 保存的元数据及解析声明，不返回原始 MIME/附件 |
| `/feedback/{id}/explanation` | 必填 expected_sha256；提供兼容格式说明 |
| `/feedback/{id}/candidates` | 必填 expected_sha256、rfc_message_id、report_index；limit 默认 25、1–50 |

反馈查询拒绝未知/重复参数、非法编码和超过 8192 字节的查询串。摘要必须为小写规范 SHA-256；不匹配返回 409。格式说明不适用或收件人区块不存在返回 422；不可用服务返回 503，不输出底层错误。候选服务再次核对实际证据摘要，并沿用单一只读快照。

反馈声明和格式说明可能包含收件人地址及 Provider 诊断文字，所有管理角色都必须受信任。它们是本阶段明确新增的可读范围，不是完整原始邮件访问。反向代理应避免记录这些查询参数，其中可能包含 Message-ID。浏览器只以文本节点呈现反馈内容，不渲染其 HTML、加载附件或外部资源。跳转只使用本地记录 ID。

## 验收

- 集成测试覆盖 viewer 查询、无效身份、分页限制、重复/未知参数、摘要冲突、区块越界和写入拒绝；核对 UNKNOWN、suppression 和审计记录不变。
- 浏览器测试覆盖真实合成数据的列表、严格/异常格式说明、候选查询、桌面/手机布局、不可信 HTML 文本隔离、导航后迟到结果隔离；同时回归原有工作台流程。
- 浏览器验收发现候选数组直接传入原生 replaceChildren 导致卡片被转换为文字；已改为展开节点并过滤空项，保留回归断言。
- 仅使用隔离 PostgreSQL 与 Fake SMTP，不使用私人邮件或外部邮箱。

2026-09-21 验收通过：

- `docker compose -p relaytale-7b5-test --profile test run --rm -e REQUIRE_INTEGRATION_TESTS=1 test make ci`：格式、vet、build、全部 race 测试通过，数据库集成测试 84.010 秒。
- 本机 Chrome 执行 `npm run test:browser`：3 项通过，4.8 秒；已查看 1440px 桌面及 390px 手机截图。
- `npm run check:ui`、`git diff --cached --check` 与暂存差异 Gitleaks 检查通过。

测试服务已停止。完整自动退信不在本阶段完成范围内。
