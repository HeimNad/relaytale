# 运维、导出与保留策略

适用版本：Phase 3。所有管理命令在本地容器中执行，需要数据库权限；尚无远程管理 API。默认关闭自动清理和外部投递。

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
