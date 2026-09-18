# Phase 7B.2 · 只读 IMAP 反馈收集

本阶段把指定邮箱文件夹接入现有反馈待查队列。收集到的内容仍为 **UNVERIFIED**：认证成功只能证明读取了这个邮箱，不能证明收到的 DSN 真实。收集器不关联投递、不修改 UNKNOWN、不创建 suppression，也不修改已记录的 SMTP 接受事实。

## 使用

先启动升级后的 RelayTale，使迁移 `00010_feedback_imap.sql` 完成，再运行独立收集命令。常规服务不会自动启动收集器。凭证从环境读取，不支持密码命令行参数；建议使用专用邮箱与应用密码，避免将个人收件箱全部存入数据库。

配置环境变量 `DATABASE_URL`、`FEEDBACK_IMAP_HOST`、`FEEDBACK_IMAP_USERNAME`、`FEEDBACK_IMAP_PASSWORD`，通过本地秘密配置或进程管理器注入。SpaceMail / Purelymail 的公开地址见 [调查记录](provider-feedback-research.md)，本阶段未使用真实账号验证 IMAP。

```sh
# 文件夹必须明确指定；Bounces 只是示例，需要先由操作者创建/配置路由。
relaytale collect-feedback --source purelymail-bounces --folder Bounces --batch 20

# 显式启用轮询，每批结束后等待至少 30 秒；发生任何错误即退出。
relaytale collect-feedback --source purelymail-bounces --folder Bounces --batch 20 --interval 1m

# 当前游标、用量、最近 20 条告警和逐 UID 收集结果。
relaytale feedback-collection-status --source purelymail-bounces --limit 20
relaytale feedback-collection-status --source purelymail-bounces --after-uid 20 --limit 20
```

端口默认 993，可指定 `--port`，始终为隐式 TLS、验证证书且至少 TLS 1.2。只使用 `EXAMINE` 和 `BODY.PEEK`，不发送 STORE、MOVE、DELETE、EXPUNGE 或 CLOSE。断开时直接关闭连接，不改变已读状态或删除邮件；客户端不会启用原始协议日志。使用固定版本 `github.com/emersion/go-imap/v2 v2.0.0-beta.8`，属于 beta 依赖，升级必须重新运行协议测试。

## 游标与持久化

- 来源名绑定主机、端口、用户名和文件夹的 SHA-256；密码不参与身份，因此换密码不重置游标。同一来源名不能静默换邮箱；应先人工确认再使用新的来源名。
- 外部 ID 包含上述身份摘要、UIDVALIDITY 和 UID，绝不使用 IMAP 序号。每封原始证据、解析结果、收集回执、用量、审计与游标在同一个同步提交事务里保存。事务回滚不会推进该 UID；若提交确认丢失，下次以数据库中已持久化的游标为准。
- 每个来源使用 PostgreSQL 会话级 advisory lock，第二个收集器在连接邮箱之前返回 busy；不跨网络持有数据库事务。需要直连 PostgreSQL 或 session pooling，不能使用 transaction pooling 来承载此会话锁。取消/失败释放锁；解锁失败丢弃连接，避免将带锁会话放回池中。
- 每批查询一个有界数字 UID 区间。完整 FETCH 成功且已观察到的邮件均处理后，才能跳过不存在的 UID。一个批次是最多 N 个 **UID 槽位**，不是保证 N 封邮件；稀疏历史可能需要多批扫描。
- 超过 1 MiB 的邮件仅记录 OVERSIZE，不拉取正文；空信记录 EMPTY；元数据读取后消失的邮件记录 VANISHED。它们均可在状态命令中查询。已发现正文不完整或大小不一致时停止，不把截断数据当原件。

UIDVALIDITY 变化会停止，并追加去重告警，不自动重置。操作者检查新邮箱状态、确认需要重扫后，可执行一次：

```sh
# 43 仅为例子，必须使用服务器当前实际报告的值；禁止与 --interval 同用。
relaytale collect-feedback --source purelymail-bounces --folder Bounces --accept-uidvalidity 43
# 查询旧 epoch 的回执，不覆盖历史。
relaytale feedback-collection-status --source purelymail-bounces --uidvalidity 42
```

重新确认 epoch 保留既有证据和累计用量，从 UID 1 开始扫描。UIDNEXT 倒退同样停止并告警。超大/消失回执不会自动重新抓取，应根据回执人工检查原邮箱；原邮箱内邮件不会被本功能删除。

## 资源与失败边界

| 限制 | 当前值与行为 |
| --- | --- |
| 原件 | 1 MiB；沿用 7A 解析上限 |
| 每批 UID 槽位 | 默认 20，允许 1–100 |
| 每批总耗时 | 最长 2 分钟，包括锁、网络与数据库 |
| 每连接入站协议流量 | 16 MiB，包括正文和服务器其他响应，耗尽即关闭 |
| 每来源累计原件 | 100 MiB；达到后停止，已提交证据保留 |
| 每来源累计记录 | 10,000 条回执/新增告警，跨 epoch 累计 |

批量和总流量共同约束吞吐。若积压邮件接近 1 MiB，建议 `--batch 8`；大批次可能达到连接流量预算并退出，重新运行从已提交游标继续。轮询无自动故障重试，监督进程若重启应设置退避；邮箱变化和容量错误必须人工处理。

100 MiB 只统计原件，不是数据库实际体积；解析字段、索引、WAL、审计和其他来源还会占空间。管理员新增来源会增加总容量。当前没有自动反馈清理或释放用量的接口，不能通过换来源名绕过容量保护；长期无人值守使用仍需后续协调保留策略。状态查询为运维视图，收集进行中各字段可能来自相邻时刻。

## 验收

协议测试覆盖 TLS 证书拒绝、认证错误脱敏、EXAMINE、BODY.PEEK、超大 literal、中途超时和整体响应预算。PostgreSQL 测试覆盖中断后的逐封恢复、重复执行、超大/空/消失邮件、UID 间隙、身份冲突、epoch 变化及显式确认、容量停止、并发来源互斥、取消释放锁，以及审计失败/最终 COMMIT 失败时证据与游标共同回滚。

本地验收通过：在独立 Compose 项目 `relaytale-7b2-test` 中运行 `make ci`，格式检查、go vet、构建、全仓库 race 测试均通过，最终 PostgreSQL 集成测试耗时约 80.5 秒。暂存差异检查及 Gitleaks 扫描通过。未完成真实 SpaceMail/Purelymail IMAP、账号权限及退信路由验收，不能把本阶段称为可信 DSN 自动化完成。

下一步是在限定文件夹与范围内验收真实 IMAP 和 DSN 样本，再决定可信来源与逐收件人关联方案。没有可信证据时保持人工处理。
