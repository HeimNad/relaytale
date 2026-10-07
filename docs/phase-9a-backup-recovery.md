# Phase 9A · 停写备份与受控恢复

提供 `backup`、`restore` 和 `verify-restore`。这是专用 PostgreSQL 17 数据库、单实例、本地文件系统的停写备份流程，不是在线备份、PITR、跨版本迁移或自动灾备切换。

## 一致性边界

操作前必须停止全部接收、worker、IMAP 收集、管理写操作、清理及独立 CLI 写入进程；确认优雅退出已完成。`--offline` 是操作者对此的明确声明，不会代替停进程。数据库 SHARE 锁可以阻止表写入，但不能阻止已在网络上发送的 SMTP DATA，也不能阻止外部文件修改。备份目录与 EML 目录必须由操作者独占。

备份锁住 public 表、导出统一数据库快照，用同一快照生成 PostgreSQL custom dump。将整个 EML 树流式复制，包含尚未确认能清理的孤立文件；校验 AVAILABLE 原文大小和 SHA-256，PURGE_PENDING/PURGED 不要求已删除原文存在。数据库保存的 DSN 原件、IMAP 游标、审计、幂等 ledger 与原文退役记录随数据库一起备份。主密钥、环境配置、证书、数据库角色/权限与外部服务配置不在 bundle 内。

新建目录为 0700，文件为 0600；已有空目标目录需由操作者预先限制权限。不支持符号链接、特殊文件或指向存档外的消息路径。清单最后发布，无 manifest.json 的目录不算完成备份。每个文件都列出 SHA-256 与大小。清单最多 64 MiB、100,000 个文件（含 dump）；正文复制/校验流式进行，但清单内存随文件数增长。需要备份容量和一次额外读取的空间/时间预算。

哈希只检测意外损坏，不提供真实性认证或加密。PostgreSQL dump 包含可执行 SQL，只恢复来自可信操作者的备份。备份包含邮箱地址、原文、密码摘要和加密 Provider 凭证，必须按敏感数据加密保存并限制访问，不能提交 Git。`backups/` 已排除 Git 与 Docker 构建上下文。

## 备份操作

1. 记录部署镜像/提交、配置版本、备份开始时间和操作者。准备独立受保护的备份目录；不要位于 EML 目录内。
2. 停止所有写入者。Compose 的主服务可用 `docker compose stop relaytale caddy`；独立收集器、CLI 与其他部署实例必须另外停止。保持 PostgreSQL 运行。
3. 在相同镜像和工作目录下运行下列命令。DATABASE_URL 来自受保护环境，不能写在命令参数中。`--storage-dir` 必须对应实际 EML 根目录。

```sh
relaytale backup --offline --storage-dir /data/eml --bundle /backups/2026-09-27-001
```

Compose 可将宿主的私有备份目录挂到 `/backups`，例如：

```sh
docker compose --profile ops run --rm --no-deps \
  -v "$BACKUP_PARENT:/backups" \
  recovery backup --offline --storage-dir /data/eml --bundle /backups/2026-09-27-001
```

`BACKUP_PARENT` 必须已创建，只有运行 UID 10001 和负责备份的操作者可写；不要通过 0777 解决权限问题。已存在的 bundle 会被拒绝，失败目录保留供排查，不覆盖重试。

4. 将完成的 bundle 加密复制到独立故障域，并检查复制后完整性。**单独**保存对应主密钥、环境配置、SMTP TLS 私钥/证书及 IMAP/API/Webhook 等密钥；密钥必须与备份版本配对并分开授权。本工具不会自动收集这些文件。
5. 记录成功时间、规模、耗时和存放位置；确认不是失败的部分备份后，恢复原部署服务。

运行镜像自带 PostgreSQL 17 client；本机使用需自行提供 pg_dump/pg_restore。只支持带主机和用户的 postgres/postgresql URI，常规数据库名，不支持 DSN 字串、service 文件或自定义 search_path。支持 URI 的 sslmode、sslrootcert、sslcert、sslkey、sslcrl、connect_timeout、application_name、channel_binding；未知选项拒绝。密码通过环境传给工具，命令参数与错误日志不打印密码或驱动原始错误。

## 恢复操作

恢复使用**新建的专用空数据库和空数据卷**，原部署保持关闭。首先用备份时的同一版本恢复，升级是后续独立步骤。匹配迁移版本和嵌入迁移文件哈希，拒绝把恢复当作升级。

数据库中的原文路径和追加审计不重写，因此恢复必须保持备份时的绝对 EML 路径与进程工作目录；Compose 的新卷仍挂到 `/data/eml` 即可。不要对原生产目录做覆盖恢复，不要运行 `down -v` 清理原部署。DATABASE_URL 指向新目标库；操作者负责核对目标，工具同时拒绝非空目标。

```sh
relaytale restore --offline --storage-dir /data/eml --bundle /backups/2026-09-27-001
relaytale verify-restore --offline --storage-dir /data/eml
```

恢复先校验清单、路径和每个文件，再建立 `.restore-pending` 标记，复制 EML，不带 CASCADE 地移除目标的空 public schema，再以单事务、遇错停止方式恢复数据库（dump 自带 CREATE SCHEMA）。任何失败都保留标记；半恢复目录不能直接重跑，保留供排查后另建空目标重试。禁止用删除标记的方式修复恢复失败。

校验包括 AVAILABLE 原文大小/摘要、Provider 凭证解密、幂等关联、退役 ID/路径冲突，并报告 SENDING/UNKNOWN 数量。Provider 存在时必须提供正确的主密钥。校验不执行迁移、不改变状态、不发送邮件，也不删除启动阻断标记。

主服务在迁移、监听及 worker 启动前检查标记。此保护要求正确挂载恢复卷；不是跨实例锁，也不能约束独立 CLI 写操作。恢复期间继续停用全部写入工具。

## 人工对账后才恢复运行

备份之后原系统可能已经发送邮件，而旧备份仍将其记为 QUEUED。**校验成功不等于可以安全重发。** 必须核对从快照到故障/停机这段时间的所有投递，不仅核对标为 UNKNOWN 的记录。对账需要保留的外部 Provider 证据、应用提交记录或其他独立审计；没有证据就继续暂停，不能猜测。

操作者确认数据、密钥、幂等历史、退役保护和外部副作用后，记录解除理由与时间，再手动移除 `/data/eml/.restore-pending`。启动前显式设 `WORKER_COUNT=0`、`RETRY_ENABLED=false`、`FAILOVER_ENABLED=false`、`HEALTH_ENABLED=false`、`MAINTENANCE_INTERVAL=0s`，并暂不启动独立收集器/回收命令。原备份包含旧配置时不能照搬已启用 worker 的环境。先只读检查，后续恢复发送另作运维决定。

## RPO / RTO 与责任

- 一致性点是数据库导出快照（清单 created_at 记录导出时数据库时间），不是备份复制结束时刻。RPO 为故障时刻到最近已验证快照的差值；没有 WAL/PITR，不能承诺零数据丢失。备份开始前停止接收期间，客户端必须按协议重试。
- RTO 包含准备新数据库/卷、取回并解密备份、校验、导入、密钥验证和人工对账。小样本测试耗时不能作为生产 RTO 承诺，尤其不能排除对账时间。
- 运维负责人定义频率、保留期、加密/异地存放、恢复权限、演练周期、磁盘告警及 RPO/RTO 目标。Phase 9A 不自动调度备份，也不清理旧备份。
- 尚未覆盖跨大版本、对象存储、在线写入备份、大规模容量、整机掉电和真实故障恢复；数据库最小权限、迁移中断及通用生命周期仍属于后续 Phase 9。

## 验收记录

2026-09-27，仅使用隔离 PostgreSQL、合成原文与测试密钥，不连接真实 Provider。

- 隔离 Compose 执行 `make ci`：格式、go vet、build、全量 race 测试通过；集成包 132.806 秒。
- 最终空库检查补充了函数/类型/额外 schema，相关恢复专项 race 测试通过；实际非 root 运行镜像中的 backup → restore → verify-restore → 启动拒绝也已通过。
- 合成往返样本包含 2 条消息（其中 1 条 UNKNOWN、1 条原文已 PURGED）、1 个 AVAILABLE 原文、1 个加密 Provider、1 条 API 幂等记录和 1 条退役记录。一次专项演练备份约 0.081 秒，恢复及校验约 0.070 秒；不含准备与人工对账，不作为生产 SLA。
- 覆盖损坏/缺失原文、错误/缺失主密钥、非空数据库/目录、路径穿越、符号链接、重复条目、清单版本错误、备份工具不可用、恢复工具不可用及数据库导入失败。失败保持启动阻断，恢复后同键请求仍返回原消息，退役 ID 仍不能入队，投递尝试保持为零。
- 演练发现并修正两处真实问题：libpq 不会将 PGDATABASE 环境值中的 URI 按连接串展开，改为拆分环境参数；空目标自带 public schema 与 dump 的 CREATE SCHEMA 冲突，改为只删除空 schema、严禁 CASCADE，再事务导入。没有静默忽略导入错误。

实际 SIGKILL/整机掉电、生产规模耗时、跨版本迁移及真实灾难现场仍未验收；工具失败与 SQL 导入失败测试不能替代这些演练。

## 8D.2 通知恢复补充

备份与恢复现包含 Webhook endpoint、outbox 和发送日志，校验报告增加 `webhook_pending_or_in_flight`。恢复后保持 `WEBHOOK_ENABLED=false`，核对外部网站已处理的事件后再恢复通知；旧备份可能重放已收到事件，网站须持久化 event_id 去重。签名环境配置不在数据库备份内，需独立保管。详见 [8D.2](phase-8d2-webhooks.md)。

9B.3 起 Compose 的备份/恢复使用 ops profile 的 recovery 服务与独立凭证；直接二进制命令仍是 relaytale backup / restore / verify-restore。恢复后重新应用 runtime-grants.sql，账号配置见 [数据库权限部署](phase-9b3-database-privileges.md)。
