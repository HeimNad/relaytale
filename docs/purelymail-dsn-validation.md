# Purelymail 单封退信实验

发送时间：2026-09-19 01:00:56 UTC（纽约时间 2026-09-18）；记录整理于 2026-09-20。

## 授权与执行范围

用户明确批准从已有 Purelymail 测试账号，向其 atland.icu 域下指定测试地址发送 **1 封无附件纯文本邮件**，随后只读检查新退信。实际仅发送一次，无重试、无切换 Provider。发送前记录 INBOX 的 UIDVALIDITY/UIDNEXT，并持久化防重发标记与原始发信文件。

本次夹具直接调用项目 SMTP 客户端，经 `smtp.purelymail.com:465` 验证 TLS 后发送；不是重新验收完整 SMTP ingress/队列路径。随后使用实际 IMAP 客户端与反馈收集服务，将新退信存入独立 PostgreSQL。私有证据位于被 Git 忽略的 `temp/dsn-validation/`，包含原始发信、退信、SMTP 结果、关联核对和数据库备份；凭证未写入这些证据或 Git。

## 观测结果

- SMTP 返回 **250 / SMTP_ACCEPTED**，收到最终确认，没有不确定后自动重发。
- Purelymail INBOX 新增并只读收集 1 封退信，原件 3,271 字节。此后未继续轮询。
- 外层 In-Reply-To、返回原始邮件头的 Message-ID 均与本次发信一致，返回的 To 与指定目标一致。它们是本次受控实验的关联证据，不自动建立生产 attempt/recipient 的可信绑定。
- 报告声称目标邮件服务器拒绝收件人，诊断文本包含 `554 5.1.1`；不据此创建 suppression。
- 原件数据库摘要与实际字节一致，本地导出摘要也与数据库一致。保留为 **UNSUPPORTED / UNVERIFIED / QUARANTINED**；新增 suppression 为 0。

SMTP 接受与之后收到失败报告并不矛盾。前者是 Provider 接受邮件的事实，后者是异步反馈；不能用后者抹去前者。

## 发现的格式差异

本次样本的 `message/delivery-status` 部分存在三个独立差异：

1. `Final-Recipient` 只有地址，缺少 `rfc822;` 等地址类型前缀。
2. `Status` 为 `554`，不是三段式状态码；诊断文本中的 `5.1.1` 不等于该字段已经合规。
3. `Remote-MTA` 与 `Last-Attempt-Date` 被空行分到独立段落，该段没有收件人、Action 或 Status。

标准要求 Final-Recipient 携带地址类型，Status 使用点分三段状态码，并将远端信息置于对应收件人的字段组内。依据 [RFC 3464 §2.3](https://www.rfc-editor.org/rfc/rfc3464.html#section-2.3)。本次仅确认这一个样本的差异，不推断 Purelymail 所有退信都如此。

严格解析器拒绝该样本符合当前设计；原件已经保留，没有丢信。已添加完全合成的回归测试，分别覆盖三个差异及组合情形，确保不会从诊断文本猜测字段、擅自合并段落后当作标准报告。反馈包 race 测试、go vet、全仓库格式检查通过；本次未修改生产解析逻辑。

## 后续安排

真实样本缺口已补齐，下一项是设计**带格式问题说明的兼容解析**：原始字段与辅助解释分开保存，标出缺失类型、非标准状态和段落歧义；不得通过“兼容”提升来源信任、自动封禁或重写原始证据。可信来源和唯一 attempt/recipient 关联仍需独立验收。当前没有兼容解析或自动处置实现。
