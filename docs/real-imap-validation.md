# IMAP 真实链路验收 · 2026-09-18

本次用户明确选择 SpaceMail / Purelymail 的 INBOX，每个邮箱最多读取 5 封，保持只读。使用此前提供的两个测试账号；没有发送新邮件、创建文件夹、调整路由或修改账号配置。原件、数据库备份和详细结果仅在被 Git 忽略的 `temp/imap-validation/` 中保存，本报告不包含凭证、邮件正文或收件地址。

## 范围与方法

- 两家均使用官方 993 隐式 TLS 地址。Go TLS 验证证书成功，协商 TLS 1.3，IMAP 欢迎响应正常。预检时本机 Python 信任链验证失败，未跳过验证；实际验收使用项目 Go TLS 客户端。
- 在独立临时 PostgreSQL 中应用项目迁移，使用现有 `feedbackimap.Dial` 与 `feedback.Service.Collect`，不启动 SMTP worker。
- 为避免读取整箱历史，验收工具先只读选取 INBOX，取得 UIDNEXT，显式将**隔离数据库**的初始游标设为末尾最多 5 个 UID 槽位之前。这是本次验收夹具设置，不是产品新增的跳过历史功能；生产收集器仍从 UID 1 开始。
- 使用 EXAMINE、BODY.PEEK，未发送修改 flags、移动或删除命令。没有独立抓取前后 flags 进行对比，因此本次证据是只读命令路径成功，不额外宣称完成服务端 flags 差分验收。

## 结果

| 项目 | SpaceMail | Purelymail |
| --- | --- | --- |
| TLS / 登录 / INBOX 选取 | 成功 | 成功 |
| 本次保存 | 5 封 | 1 封（当前窗口仅有 1 封） |
| 原件总字节数 | 26,078 | 3,587 |
| 解析结果 | 5 个 PARSED | 1 个 UNSUPPORTED |
| 原始 MIME | multipart/report | multipart/mixed |
| 可提取 DSN 声明 | 5 个 failed / 5.0.0 | 无支持的 DSN 结构 |
| 待查状态 | 全部 UNVERIFIED / QUARANTINED | UNVERIFIED / QUARANTINED |
| 新增 suppression | 0 | 0 |

6 份数据库原件的 SHA-256 均与存储摘要一致；导出到本地的 6 份原件摘要均与数据库一致，字节数均匹配 IMAP 报告大小。无超限、消失、空信或收集告警。

使用相同的已观察邮箱快照再次调用收集器，均未触发新 FETCH、未重复导入。该项是持久化游标加固定快照的重放验证，不是第二次连接真实服务器的全链路测试。

## 结论与剩余边界

两家真实 IMAP 的 TLS、认证、只读取件和证据存档路径已经通过本次限定范围验证；SpaceMail 的现有标准 DSN 样本可解析。Purelymail 的这封邮件被保留为不支持的格式，不能据此判断其退信兼容性存在问题，也不能视为 Purelymail DSN 验收通过。

`failed / 5.0.0` 是报告中的通用永久失败声明，不证明收件地址不存在，更不证明报告来源可信。此次不关联历史 attempt，不变更投递状态，不自动抑制。

后续需要 Purelymail 的真实 DSN 样本，并验证可信来源、信封地址/关联信息及错误归属。是否新发受控测试邮件，应另行明确目标与数量。本次验证不涵盖所有 IMAP 服务、超大真实邮件、生产长时间轮询、真实 UIDVALIDITY 变化和邮箱保留策略。
