# Phase 3.5 真实 Provider 验收

## 第一轮：SMTP 接受层通过，收件端待核对

测试时间：2026-09-11T03:05:52.888000+00:00。基线：`phase-4a`（`06a5791`）；运行编号：`ef8ad30b66`。

使用操作者提供的两套 SMTP 账号和四个明确收件地址，执行 Gateway → SpaceMail/PurelyMail → 收件服务的真实投递。凭证通过标准输入导入并在数据库加密保存，没有写入 Git、测试报告或命令输出。

## 范围与结果

| Provider | 端口 / TLS | 连接与认证 | 最终响应 | 收件人接受数 | 本次尝试耗时 |
| --- | --- | --- | --- | --- | --- |
| spacemail | 465 / implicit_tls | 通过 | 250 | 4/4 | 1587 ms |
| spacemail | 587 / starttls | 通过 | 250 | 4/4 | 1902 ms |
| purelymail | 465 / implicit_tls | 通过 | 250 | 4/4 | 526 ms |
| purelymail | 587 / starttls | 通过 | 250 | 4/4 | 589 ms |

**共四封逻辑邮件、四次投递尝试、16 个 SMTP_ACCEPTED 收件人结果。没有重试或跨 Provider 切换。** 每个收件地址最多四封本轮邮件；没有据此标记 DELIVERED。耗时是本地单次样本，不能作为性能或稳定性排名。

SpaceMail 最终响应为 `250 2.0.0 Ok: queued as …`；PurelyMail 为 `250 2.6.0 Message received`。完整响应和 Provider 队列标识保存在本地 attempt/event 记录。

收件目标覆盖 Gmail、QQ、学校邮箱和 iCloud。学校域的实时 MX 查询返回 `ASPMX.L.GOOGLE.COM` 及 GoogleMail 备用服务器，因此推断学校邮箱由 Google 托管。本轮**没有 Outlook 地址**，不能把学校邮箱当作 Outlook 验收。

| 收件服务 | 收件箱 / 垃圾箱 | 中文、HTML 与附件 | 原始头 / DKIM |
| --- | --- | --- | --- |
| Gmail | 待操作者确认 | 待确认 | 待核对 |
| QQ | 待操作者确认 | 待确认 | 待核对 |
| 学校邮箱（Google MX） | 待操作者确认 | 待确认 | 待核对 |
| iCloud | 待操作者确认 | 待确认 | 待核对 |

## 已验证的行为

- 两个真实 Provider 均完成 465 implicit TLS 和 587 STARTTLS 的证书验证、EHLO 与认证。
- 每封邮件先经本地 SMTP ingress 的 STARTTLS/AUTH 接收，完成 EML 与数据库持久化，再由应用实际 worker 投递；未绕过队列直接用 Python 向 Provider 发信。
- 四个收件人使用独立 RCPT；To 头不公开地址列表，正文没有跟踪像素、脚本或外部资源。
- 邮件包括 UTF-8 中文主题/显示名、multipart text/plain + text/html、256 KiB 附件，以及行首点测试文本；SMTP 传输内容使用 7-bit-safe 编码。
- 生成原文 SHA-256 与入库 EML SHA-256 一致；每轮 bytes_sent 等于存档字节数；全部收件人的决策为 ACCEPT，attempt_count 均为 1。
- 固定每封测试邮件的 route_provider_id，发送前核对整个活动队列只有这四封；临时 worker 设 240 秒上限并关闭自动重试。测试完成后立即停止并移除。
- 临时 SMTP 账号与四个测试 Provider 均已禁用，避免以后开启 worker 时误用；加密配置和本地审计证据保留。主服务仍是 WORKER_COUNT=0、RETRY_ENABLED=false。

## 证据索引

| 场景 | Gateway UUID | 存档字节数 | 原文 SHA-256 |
| --- | --- | --- | --- |
| spacemail-465 | `01a08e6d-c1f8-766a-a436-74b21e71ad1c` | 360693 | `caaedbb0d727061eb697dede76646b215d61837f107ba89d7819a8d88872b35a` |
| spacemail-587 | `01a08e6d-c355-7408-83dd-618e150d8712` | 360693 | `403009ee595603b06983c7428b27d1345d6dfcd52373232cfb1c185ea57c1296` |
| purelymail-465 | `01a08e6d-c4b8-7e43-9847-62f5597a5aba` | 360697 | `015aada8b01966d03ef463934b10b2d835ac137fc231427eac9e284f44841e96` |
| purelymail-587 | `01a08e6d-c5fc-7c1e-911b-66c800bd7316` | 360697 | `cdfe6f7772908195b79470b7e28bba6e3435d58e739333c1934c4df5e40fbf8a` |

邮件主题形如 `[MailGateway ef8ad30b66] <Provider>-<port> · 中文主题/附件测试`。可用 Gateway UUID 执行记录导出查询具体 attempt、事件、原始 Message-ID 和收件人状态。原文、地址与完整导出留在仓库外。

## 尚未完成，不能算通过

- 收件箱实际到达、垃圾箱分类、延迟和显示/附件完整性：需要操作者核对。
- 收件端原始 Message-ID 是否保持、Provider 的头/正文改写、Authentication-Results 与 DKIM：需要收件端原始邮件头或 EML。网关本地哈希一致不等于远端 DKIM 通过。
- 本轮原文没有预先签署的 DKIM，因此没有实测“已签名入站邮件经过网关后仍可验证”的场景。
- 256 KiB 附件只是保守的 MIME 冒烟测试，不代表接近 Provider 大小上限的大邮件已通过；大消息边界另行约定后测试。
- 没有覆盖 Outlook、单收件人独立事务、SMTPUTF8 地址、8BITMIME 协商以及完整旧客户端兼容矩阵。UTF-8 显示名/主题使用编码头，不是 SMTPUTF8 地址测试。

在收件侧证据补齐前，不将 Phase 3.5 标记为完整通过，也不据此启用自动重试或 failover。

## 配置依据

- [SpaceMail 官方客户端配置](https://www.spaceship.com/knowledgebase/connect-spacemail-to-email-client/) 列出 `mail.spacemail.com:465`。587 本次实际探测和投递成功，但这不代表官方长期支持承诺。
- [PurelyMail 官方配置](https://support.purelymail.com/support/solutions/articles/159000430778-server-settings-imap-smtp-and-pop3) 列出 `smtp.purelymail.com` 的 465 SSL/TLS 与 587 STARTTLS。
