# Phase 3.5 真实 Provider 验收

状态：待账号和收件人，尚未执行。Fake SMTP 测试不代表以下项目通过。

操作者需在仓库外准备 Provider 凭证、允许的发件域、测试收件人及允许的最大邮件大小。不要将密码粘贴到日志或提交到 Git。每轮发信须有明确的测试范围，保持自动重试关闭，避免失败时扩大外部发送量。

| Provider | TLS 模式 | Gmail | Outlook | iCloud |
| --- | --- | --- | --- | --- |
| SpaceMail | 465 implicit TLS | 待测 | 待测 | 待测 |
| SpaceMail | 587 STARTTLS | 待测 | 待测 | 待测 |
| PurelyMail | 465 implicit TLS | 待测 | 待测 | 待测 |
| PurelyMail | 587 STARTTLS | 待测 | 待测 | 待测 |

每个组合先连接诊断，再提交已批准的测试邮件：单/多收件人、multipart HTML/text、附件、UTF-8 主题/显示名、接近约定上限的大邮件。记录 Gateway UUID、Original Message-ID、Provider/attempt ID、协商能力、最终代码/响应、各阶段耗时和收件端原始邮件证据。

分别判断：网关提交给 Provider 的字节保持不变；Provider 是否改写头/正文；收件端 DKIM 验证结果。Provider 250 仅表示接受，不等于收件箱出现；垃圾箱/延迟也应记录。敏感原文与截图保存在仓库外，只提交脱敏结论和证据编号。

完成标准：上述矩阵有真实结果、异常有解释或修复回归，原文/关联 ID 行为可核查；失败项目不得标记通过。完成前不在真实环境启用自动重试或 failover。
