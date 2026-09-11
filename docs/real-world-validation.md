# Phase 3.5 真实 Provider 验收

## 第一轮：SMTP 接受层通过，Gmail / QQ / iCloud 原始邮件已核对

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

| 收件服务 | 到达 / 分类 | Message-ID、主题、解码 MIME 内容 | 收件端认证结果 |
| --- | --- | --- | --- |
| Gmail | 四种组合 EML 齐全；文件夹分类未单独确认 | 全部与发送原文一致 | SPF / DKIM 均 pass；PurelyMail DMARC pass，SpaceMail 未列 DMARC 结果 |
| QQ | 四种组合 EML 齐全；用户报告 PurelyMail 587 在垃圾箱 | 全部与发送原文一致 | SPF pass；DKIM 与 DMARC 差异见下文 |
| 学校邮箱（Google MX） | 未提供 EML，不作独立完整验收 | 未核对 | 未核对 |
| iCloud | 四种组合 EML 齐全；文件夹分类未单独确认 | 全部与发送原文一致 | SPF / DKIM 均 pass；PurelyMail DMARC pass，SpaceMail DMARC none |

## 收件侧原始邮件核验

操作者提供了 13 张截图，随后将 Gmail、QQ、iCloud 各四份 EML 放入本地 `temp/`。逐一用运行编号与场景匹配发送原文，再比较 Message-ID、解码主题、原始 To 头，以及每个非 multipart 部件的 MIME 类型、文件名、解码字节数和 SHA-256。12 份均匹配，解析器未报告 MIME 缺陷。`temp/` 已加入忽略规则；原始邮件、个人邮箱地址和截图不纳入 Git。

- 12 份的 Message-ID、主题和 To 均保持；解码后的 text/plain、text/html 以及附件 SHA-256 全部与各自发送原文相同。每份附件恰为 262144 字节（256 KiB），截图显示的 262.74 KB 不代表附件损坏。此结果不是整封 EML 字节相同：接收链路增加了邮件头。
- 所有收到的原始 To 仍为 `undisclosed-recipients:;`，没有截图中的 `invalid` 或异常引号。因此现有证据指向客户端对该头的显示/解析问题，没有发现 Gateway 或 Provider 改坏 To 的证据；不修改透明转发策略。
- 学校截图中 PurelyMail 465 / 587 的主题出现 `[THIS EMAIL IS NOT FROM NCC]` 前缀，疑似学校链路添加的外部发件人标记。用户选择不导出学校邮件，故不定位其添加环节，也不把 Google MX 等同于与个人 Gmail 完全相同的策略。
- 用户确认和收到的 EML 作为验收证据；数据库仍保留本轮 SMTP_ACCEPTED 事实，不覆盖历史投递记录。

### Authentication-Results 的差异

以下是收到的邮件头所报告的结果，并非本地重新执行 DNS 与 DKIM 密码学验证。

- Gmail / iCloud：四种组合 SPF、DKIM 均报告 pass。PurelyMail 的发件域签名与 Provider 域签名均通过，DMARC 也报告 pass。
- SpaceMail：Gmail 的 Authentication-Results 未列 DMARC 项；iCloud 明确报告 `dmarc=none`。不将缺失或 none 写成 pass，也不据此断言当前 DNS 配置原因。
- QQ / SpaceMail：DKIM 报告 pass，DMARC 报告 `none(permerror)`。
- QQ / PurelyMail：顶层 DKIM 为 pass，但括号说明发件域签名验证失败、其他域签名通过；DMARC 为 `none(permerror)`。这与 Gmail / iCloud 的结果不同，不能简化为“DKIM 对齐验证全部通过”。QQ 导出的认证头还在部分单词内部出现折叠空白，以上按原始文本记录语义，不把它当作本地验证结论。
- QQ 的 PurelyMail 465 / 587 认证描述相同，而用户仅明确报告 587 在垃圾箱。现有样本无法把垃圾箱分类归因于端口或单一认证项；后续若排障，应核对发件域 DNS 与接收方诊断。本轮不触发重试或切换 Provider。

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

- 学校邮箱未独立核验，Outlook 未覆盖；其余文件夹分类、精确到达延迟未逐项确认。Gmail / QQ / iCloud 的 12 份原始邮件及附件完整性已核对。
- 收件端 Message-ID 和 MIME 部件保持已核对；认证结果按上述接收方报告记录。QQ 的认证差异与 SpaceMail 的 DMARC 缺失/none 尚未定位，不宣称所有认证项通过。
- 本轮原文没有预先签署的 DKIM，因此没有实测“已签名入站邮件经过网关后仍可验证”的场景。
- 256 KiB 附件只是保守的 MIME 冒烟测试，不代表接近 Provider 大小上限的大邮件已通过；大消息边界另行约定后测试。
- 没有覆盖 Outlook、单收件人独立事务、SMTPUTF8 地址、8BITMIME 协商以及完整旧客户端兼容矩阵。UTF-8 显示名/主题使用编码头，不是 SMTPUTF8 地址测试。

本轮真实链路冒烟与三个收件服务的内容保持验证通过；上述认证差异和未覆盖场景保留为后续验收项。Phase 3.5 不标记为完整通过，也不据此启用自动重试或 failover。

## 配置依据

- [SpaceMail 官方客户端配置](https://www.spaceship.com/knowledgebase/connect-spacemail-to-email-client/) 列出 `mail.spacemail.com:465`。587 本次实际探测和投递成功，但这不代表官方长期支持承诺。
- [PurelyMail 官方配置](https://support.purelymail.com/support/solutions/articles/159000430778-server-settings-imap-smtp-and-pop3) 列出 `smtp.purelymail.com` 的 465 SSL/TLS 与 587 STARTTLS。
