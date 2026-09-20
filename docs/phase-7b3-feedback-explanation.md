# Phase 7B.3 · 非标准 DSN 的只读兼容说明

本阶段增加 `explain-feedback`。它读取已存档原件，保留字段与段落，单独提供格式问题和有限辅助解释。严格 `Parse`、历史 parsed JSON、parse_state、信任和投递决策均不因说明而改变；无需数据库迁移或重新导入旧邮件。

## 使用

```sh
relaytale show-feedback --id FEEDBACK_UUID
relaytale explain-feedback --id FEEDBACK_UUID --expected-sha256 ORIGINAL_SHA256
```

说明前同时核对操作者提供的 SHA-256、数据库摘要和原件实际摘要。错误摘要拒绝读取结果。输出是 JSON，可能包含收件地址与诊断文本，应按原件证据管理，避免放入公共日志。

输出版本 `dsn-explanation-v1`：

- `raw_status`：message/delivery-status 的原始文本，包括折行、重复字段和原始行结束符；完整 MIME 原件仍在原数据库记录中。
- `blocks`：独立编号的字段组，`fields` 展开折行并规范化字段名，重复字段仍以数组保留，不覆盖。编号 0 是报告级字段组，其余仅是字段组，不承诺都是合法收件人组。
- `issues`：字段级格式问题；编号 -1 表示整份报告的严格解析失败。排序稳定，不根据 Provider 名称、平台标记或 From 自动放宽规则。
- `hints`：仅在本组没有重复字段时，给缺少类型前缀的精确裸邮箱提供 mailbox 提示；把 400–599 的三位 Status 提示为 smtp_code。不会推断增强状态码，不从 Diagnostic-Code、返回原始邮件头或人类可读正文中提取决策字段。
- `strictly_parsed`：当前严格解析器对原件机器部分的判断，不是修改历史 parse_state 的指令。
- `trust` 固定 UNVERIFIED，`automatic_action_allowed` 固定 false；说明类型与 Report 类型分离，关联预检和投递决策均不消费这些提示。

## Purelymail 样本结果

使用已保存的受控真实退信做本地验证，无新网络访问或发信。说明保留 3 个字段组，明确指出：

| 问题 | 说明 |
| --- | --- |
| MISSING_ADDRESS_TYPE | Final-Recipient 缺少地址类型前缀 |
| INVALID_ENHANCED_STATUS | Status 不是三段式状态码 |
| SMTP_CODE_IN_STATUS | 554 仅提示为 SMTP 回复码，不改为 5.1.1 |
| UNASSIGNED_BLOCK | 远端信息被分成无收件人组，保持独立，不自动合并 |
| STRICT_PARSE_REJECTED | 标准解析仍拒绝原件 |

普通欢迎信不因存在平台头或认证结果头变成 DSN。重复字段会明确标出，并禁用该组提示；标准字段也不会授权自动处理。

## 限制与验收

说明与严格解析共用同一个 MIME 边界检查：原件最多 1 MiB，机器部分最多 64 KiB，最多 3 个顶层部分，必须是顶层 multipart/report 与第二部分 message/delivery-status。说明额外允许最多 201 个字段组以展示拆段情形；每行最多 4,096 字节。非法 UTF-8、裸 CR、NUL、损坏/截断 MIME、转发包装或编码机器部分仍拒绝，不尝试任意 MIME 修复。

已覆盖真实结构的合成样本、各项偏差、重复字段、诊断文本不被采信、说明稳定性、危险封装和 fuzz 入口。数据库测试验证旧 UNSUPPORTED 记录可解释、摘要不匹配被拒绝、原件/parsed/state/trust 保持不变且没有新增审计写入。本地真实样本得到上述 5 条说明。

本阶段仍只提供本地 CLI，没有新增 HTTP API/Web UI。下一步可在待查界面展示这些问题，并分别推进可信来源和唯一投递关联；兼容解释本身不是自动抑制的前提已满足。

本地验收（2026-09-20）：独立 Compose 项目 `relaytale-7b3-test` 的 `make ci` 通过，包含格式检查、go vet、全仓库构建和 race 测试；PostgreSQL 集成测试约 81.9 秒。新增 FuzzExplain 短时测试执行 400,717 次，通过；暂存差异检查与凭证扫描通过。本阶段没有新的真实发信或邮箱读取。
