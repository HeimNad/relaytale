# 运维、导出与保留策略

适用版本：Phase 4A（包含 Phase 3 运维功能）。所有管理命令在本地容器中执行，需要数据库权限；尚无远程管理 API。默认关闭自动清理和外部投递。

## 三类数据，分别管理

| 数据 | 保存方式 | 当前策略 |
| --- | --- | --- |
| 运行日志 | gateway / postgres / caddy 的 Docker 日志 | `local` 驱动，每个容器按 10 MB × 5 文件轮转，压缩旧文件；按大小，不保证保留天数 |
| 邮件元数据、投递尝试、事件及维护审计 | PostgreSQL | 长期保留；事件和维护审计禁止普通 UPDATE/DELETE；尚无期限删除配置 |
| 原始 EML | `/data/eml` 持久卷 | 安全终态默认保留 180 天；自动清理默认关闭 |
| SMTP debug transcript | attempt 的 `raw_debug_log` | 当前不采集完整 transcript；已有内容可按 30 天清理，自动清理默认关闭 |

运行日志轮转不影响数据库事件。导出不会删除源数据。邮件地址、主题及 SMTP 响应可能含私人信息，导出文件按敏感业务数据管理。

## 查看状态和连接诊断

```sh
docker compose exec -T gateway gateway doctor
docker compose exec -T gateway gateway list-providers
docker compose exec -T gateway gateway test-provider --id <Provider-UUID>
```

`doctor` 检查数据库并输出迁移版本、各邮件状态数量和最早接收时间；数据库连通不等于所有业务正常。`/health/ready` 另检查存储可写。

连接诊断实际连接指定 Provider，验证 DNS、TCP、TLS、EHLO、AUTH 后退出，不发送 MAIL/RCPT/DATA。需要已配置主密钥和 Provider 凭证，结果写入维护审计。不要将连接成功解释为邮件已送达。

## 导出邮件记录

```sh
docker compose exec -T gateway gateway export-records \
  --since 2026-09-01T00:00:00Z --until 2026-10-01T00:00:00Z \
  --output /data/exports/records-2026-09.jsonl.gz
mkdir -p exports
docker compose cp gateway:/data/exports/records-2026-09.jsonl.gz ./exports/
```

可加 `--message-id <Gateway-UUID>`，这里是内部 UUID，不是邮件头的 Message-ID。筛选按邮件接收时间，开始包含、结束不包含；匹配邮件的所有尝试与事件随之导出。省略日期时从 Unix epoch 到当前时间。

输出是 gzip 压缩 JSONL：manifest → message / recipient / attempt / attempt_recipient / event → summary。只读可重复读事务保证各表来自同一快照。排除 EML 正文、存储路径、Provider 凭证、领取 token 和 raw debug transcript。普通事件中的 SMTP 响应仍保留，不能视为匿名数据。

命令输出记录数量、压缩文件大小和 SHA-256。新文件权限为 0600，通过临时文件、同步和禁止覆盖的原子发布生成；失败不会发布半份导出。数据库记录导出开始、完成或失败。复制后校验：

```sh
shasum -a 256 exports/records-2026-09.jsonl.gz
gzip -t exports/records-2026-09.jsonl.gz
```

该导出用于排障与分析，**不是完整备份**：不包含 EML、配置、账户、Provider 凭证或维护审计表。维护审计随数据库备份保存。目前没有 JSONL 导入命令。

## 导出运行日志

在仓库根目录使用 Python 3：

```sh
python3 scripts/export-logs.py --since 24h --tail 10000 \
  --output exports/runtime-2026-09-10.log.gz gateway postgres caddy
```

只读取尚未被轮转淘汰的日志，`--tail` 是每个服务的上限（1–100000），默认只导出 gateway。文件权限 0600，不覆盖已有文件，输出 SHA-256。宿主机脚本不写数据库审计。已有文件和 Docker 故障会返回非零退出码。

不要手动截断 Docker 管理的日志文件；轮转参数见 `docker-compose.yml`。修改后重建相应容器才生效。导出目录不会自动清理，应另设受控归档周期。

## 先预览，再清理

```sh
# 只报告候选项，不更改文件或数据库。
docker compose exec -T gateway gateway cleanup --eml-days 180 --debug-days 30 --batch 100
# 使用相同参数执行实际清理。
docker compose exec -T gateway gateway cleanup --eml-days 180 --debug-days 30 --batch 100 --apply
```

CLI 保留天数默认 180 / 30、批量默认 100；手动命令应显式传参，它不继承自动任务的保留期配置。`--storage-dir` 默认读取 `EML_STORAGE_DIR`。天数 0 禁用对应类别，范围 0–36500；批量 1–1000，每类独立计数。

EML 按 `completed_at` 判断年龄，只处理 SMTP_ACCEPTED / PERM_FAILED / DELIVERED / BOUNCED / SUPPRESSED / CANCELLED，且无活跃领取、进行中 attempt 或待处理收件人。**QUEUED、SENDING、TEMP_FAILED、PARTIAL_ACCEPTED、DELIVERY_UNKNOWN 均保留。** 保留 SMTP_ACCEPTED 只说明 Provider 已接受；删除其 EML 后不能再依靠本地原文重发。

每次实际执行追加维护审计。归档按 AVAILABLE → PURGE_PENDING → PURGED 处理：先提交删除意图，再在受限存储根内删除文件并同步目录，最后记录完成与 ARCHIVE_PURGED 事件。中断后可重跑，已删除文件可以补记完成；失败 ID 返回给操作者。修复存储权限/挂载后再重试，不能通过随意修改状态跳过核对。

PostgreSQL advisory lock 防止多个清理任务同时运行。清理不删除元数据、收件人、attempt 或历史事件。不处理孤儿 EML、空日期目录、账户、备份、导出文件，也尚未实现整封邮件的软删除与彻底删除。

## 可选自动清理

确认保留策略和备份后，在 `.env` 设置：

```dotenv
MAINTENANCE_INTERVAL=24h
EML_RETENTION_DAYS=180
DEBUG_RETENTION_DAYS=30
CLEANUP_BATCH=100
```

然后 `docker compose up -d`。默认间隔 `0s` 禁用；启用时至少 1 分钟，首次执行在一个间隔后。每轮最多运行一分钟，批量处理，使用同一审计与恢复流程。大量积压需要缩短间隔或多轮执行，不会一次无限清空。

## 备份与恢复流程

Git 只保存源代码和迁移。恢复运行至少需要同一时间点的 PostgreSQL、`mail_data`（EML 与 TLS）、配置和独立保存的主密钥。主密钥丢失无法解密 Provider 密码；JSONL 导出不能替代这些内容。

当前推荐维护窗口备份：

1. 停止 gateway，等待容器正常退出，阻止新接收、投递和自动清理；PostgreSQL 保持运行。
2. 对数据库执行 `pg_dump -Fc`，以受限权限保存；同时备份 `mail_data`。邮件文件与数据库应在业务停止期间取得，不能随意拼接不同时间点的副本。
3. 在仓库外加密保存 `.env`、主密钥及必要证书，记录 Git 标签、迁移版本、备份时间与文件校验值。
4. 确认备份完成后恢复 gateway。备份需有异地副本与独立保留策略。

恢复演练应在隔离实例中进行：先恢复数据库、原路径存储和配置，以对应 Git 标签构建；保持 `WORKER_COUNT=0`、`MAINTENANCE_INTERVAL=0s`。检查迁移版本、健康接口、文件数量及抽样 SHA-256，核对所有 SENDING / DELIVERY_UNKNOWN，再决定是否恢复投递。不要让恢复实例和原实例同时向同一批收件人发送。

这是一份操作流程，尚未提供一键备份脚本，也未完成灾难恢复演练、WAL/PITR、磁盘水位告警或生产容量认证。应用回滚不能代替数据库/文件恢复；清理后的 EML 只能从独立备份找回。


## 重试与 UNKNOWN 人工处置（Phase 4A）

服务配置 `RETRY_ENABLED=false` 默认禁用自动重试；也支持 YAML `retry_enabled` 与 CLI `--retry-enabled`。此开关独立于 WORKER_COUNT；没有 worker 就不会投递或推进重试。真实 Provider 验收尚未完成，目前应保持禁用。

启用时，**新完成的尝试**中符合决策条件的收件人才得到 `retry_at`。旧阶段 TEMP_FAILED 不会被迁移或开关批量重新发送。重试使用同一个 Provider、同一份原始 EML，仅收件人信封子集改变；已成功或永久失败的收件人排除。Provider 禁用、发件域不匹配或容量不可用时等待，不偷偷换到其他 Provider。

重试预算固定为收件人首次领取后 24 小时、最多 7 次领取。间隔基数为 1/4/16/64/256/720 分钟，加 ±20% 的确定性 jitter（按 attempt 与 recipient ID 派生），最终时刻落库。进程重启不重算计划；窗口耗尽或超过次数会保留 TEMP_FAILED，并记录 MANUAL_INTERVENTION，而非编造 SMTP 永久拒绝。首轮开始前的排队等待不计入 24 小时。

`retry_at` 到期后，数据库事务和 message 行锁将收件人排队；Claim 再检查开关、次数与时间，防止已排队后长时间停机绕过窗口。关闭自动重试会暂停已有自动队列项，但不会撤销已经开始的网络操作。原有未许可 DATA 的安全租约恢复仍可在预算内重新排队，它不是对 SMTP 失败的自动重试；已许可 DATA 的过期租约仍归 UNKNOWN。

手动 UNKNOWN 重试是单次显式操作，不受自动开关阻止；它不会重置历史次数或时间窗口，后续自动重试仍受原预算约束。

SMTP AUTH 明确 5xx 拒绝、密钥/本地存储问题进入人工检查；AUTH 4xx 或可识别的认证网络断开可同 Provider 重试。RCPT 4xx/5xx 分别按收件人处理。DATA 后缺少确定结果保持 UNKNOWN；一个 message 还有 UNKNOWN 收件人时，其他重试也暂缓，直至人工完成处置。

决策的 `FailoverAllowed` 是协议层许可，4A **没有实现跨 Provider failover**。最终是否切换还需 4B 的候选配置与容量等策略。`delivery_attempts.result` 是当次事实；`recipients.status`/`messages.status` 是当前投递状态投影，例如 AUTH 535 事实可对应当前人工暂停 TEMP_FAILED，两者不应混淆。

### 查看证据

使用 `export-records --message-id <Gateway-UUID>` 导出记录，查找 UNKNOWN recipient 的 ID 及其最新 attempt ID；每轮 `DELIVERY_DECIDED` 事件含证据、预算、决策和实际重试时间。`attempt_recipients.decision` 保留该轮决策，`recipients.decision` 表示当前决策。

本阶段没有 UI。操作者只能通过本地受信任 CLI 和数据库权限操作；`--actor` 是审计标签，不是额外身份认证。

### 人工确认已送达 / 失败

```sh
docker compose exec -T gateway gateway resolve-unknown \
  --recipient-id <Recipient-UUID> --expected-attempt <Latest-Attempt-UUID> \
  --action mark-delivered --actor <Operator> --reason '已核查收件端原始邮件'
```

`mark-failed` 使用同一入口。它们更新当前收件人状态，追加 `UNKNOWN_RESOLVED` 事件及维护审计；原始 UNKNOWN attempt 和 RCPT/SMTP 响应保持不变。这里的 DELIVERED 是人工判断，不能在未来 UI 中显示为自动回执已验证。

### 明确承担风险后手动重试

```sh
docker compose exec -T gateway gateway resolve-unknown \
  --recipient-id <Recipient-UUID> --expected-attempt <Latest-Attempt-UUID> \
  --action retry --actor <Operator> --reason '业务负责人要求重发' \
  --acknowledge-duplicate-risk
```

每次只操作一个收件人。缺少风险确认、旧 attempt、已处置收件人、活跃投递或原文不可用时拒绝；并发操作只允许一次生效。它只排队，不在 CLI 中直接发送。发送时仍校验原文大小与 SHA-256，原 Provider 不可用时等待。剩余 UNKNOWN 收件人未处置前不会发出这次重试。

当前命令仅处理 UNKNOWN；配置问题、重试预算耗尽的通用人工恢复流程仍待补充，不应通过直接改数据库状态绕过保护。尚未提供 undo、修改 Provider 或重置重试预算的操作。
