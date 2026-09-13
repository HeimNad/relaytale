# 运维、导出与保留策略

适用版本：Phase 4A（包含 Phase 3 运维功能）。所有管理命令在本地容器中执行，需要数据库权限；尚无远程管理 API。默认关闭自动清理和外部投递。

## 三类数据，分别管理

| 数据 | 保存方式 | 当前策略 |
| --- | --- | --- |
| 运行日志 | relaytale / postgres / caddy 的 Docker 日志 | `local` 驱动，每个容器按 10 MB × 5 文件轮转，压缩旧文件；按大小，不保证保留天数 |
| 邮件元数据、投递尝试、事件及维护审计 | PostgreSQL | 长期保留；事件和维护审计禁止普通 UPDATE/DELETE；尚无期限删除配置 |
| 原始 EML | `/data/eml` 持久卷 | 安全终态默认保留 180 天；自动清理默认关闭 |
| SMTP debug transcript | attempt 的 `raw_debug_log` | 当前不采集完整 transcript；已有内容可按 30 天清理，自动清理默认关闭 |

运行日志轮转不影响数据库事件。导出不会删除源数据。邮件地址、主题及 SMTP 响应可能含私人信息，导出文件按敏感业务数据管理。

## 查看状态和连接诊断

```sh
docker compose exec -T relaytale relaytale doctor
docker compose exec -T relaytale relaytale list-providers
docker compose exec -T relaytale relaytale test-provider --id <Provider-UUID>
```

`doctor` 检查数据库并输出迁移版本、各邮件状态数量和最早接收时间；数据库连通不等于所有业务正常。`/health/ready` 另检查存储可写。

连接诊断实际连接指定 Provider，验证 DNS、TCP、TLS、EHLO、AUTH 后退出，不发送 MAIL/RCPT/DATA。需要已配置主密钥和 Provider 凭证，结果写入维护审计。不要将连接成功解释为邮件已送达。

## 导出邮件记录

```sh
docker compose exec -T relaytale relaytale export-records \
  --since 2026-09-01T00:00:00Z --until 2026-10-01T00:00:00Z \
  --output /data/exports/records-2026-09.jsonl.gz
mkdir -p exports
docker compose cp relaytale:/data/exports/records-2026-09.jsonl.gz ./exports/
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
  --output exports/runtime-2026-09-10.log.gz relaytale postgres caddy
```

只读取尚未被轮转淘汰的日志，`--tail` 是每个服务的上限（1–100000），默认只导出 relaytale。文件权限 0600，不覆盖已有文件，输出 SHA-256。宿主机脚本不写数据库审计。已有文件和 Docker 故障会返回非零退出码。

不要手动截断 Docker 管理的日志文件；轮转参数见 `docker-compose.yml`。修改后重建相应容器才生效。导出目录不会自动清理，应另设受控归档周期。

## 先预览，再清理

```sh
# 只报告候选项，不更改文件或数据库。
docker compose exec -T relaytale relaytale cleanup --eml-days 180 --debug-days 30 --batch 100
# 使用相同参数执行实际清理。
docker compose exec -T relaytale relaytale cleanup --eml-days 180 --debug-days 30 --batch 100 --apply
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

1. 停止 relaytale，等待容器正常退出，阻止新接收、投递和自动清理；PostgreSQL 保持运行。
2. 对数据库执行 `pg_dump -Fc`，以受限权限保存；同时备份 `mail_data`。邮件文件与数据库应在业务停止期间取得，不能随意拼接不同时间点的副本。
3. 在仓库外加密保存 `.env`、主密钥及必要证书，记录 Git 标签、迁移版本、备份时间与文件校验值。
4. 确认备份完成后恢复 relaytale。备份需有异地副本与独立保留策略。

恢复演练应在隔离实例中进行：先恢复数据库、原路径存储和配置，以对应 Git 标签构建；保持 `WORKER_COUNT=0`、`MAINTENANCE_INTERVAL=0s`。检查迁移版本、健康接口、文件数量及抽样 SHA-256，核对所有 SENDING / DELIVERY_UNKNOWN，再决定是否恢复投递。不要让恢复实例和原实例同时向同一批收件人发送。

这是一份操作流程，尚未提供一键备份脚本，也未完成灾难恢复演练、WAL/PITR、磁盘水位告警或生产容量认证。应用回滚不能代替数据库/文件恢复；清理后的 EML 只能从独立备份找回。


## 重试与 UNKNOWN 人工处置（Phase 4A）

服务配置 `RETRY_ENABLED=false` 默认禁用自动重试；也支持 YAML `retry_enabled` 与 CLI `--retry-enabled`。此开关独立于 WORKER_COUNT；没有 worker 就不会投递或推进重试。真实 Provider 验收尚未完成，目前应保持禁用。

启用时，**新完成的尝试**中符合决策条件的收件人才得到 `retry_at`。旧阶段 TEMP_FAILED 不会被迁移或开关批量重新发送。未启用 failover 时，重试使用同一个 Provider、同一份原始 EML，仅收件人信封子集改变；已成功或永久失败的收件人排除。Provider 禁用、发件域不匹配或容量不可用时等待，不偷偷换到其他 Provider。

重试预算固定为收件人首次领取后 24 小时、最多 7 次领取。间隔基数为 1/4/16/64/256/720 分钟，加 ±20% 的确定性 jitter（按 attempt 与 recipient ID 派生），最终时刻落库。进程重启不重算计划；窗口耗尽或超过次数会保留 TEMP_FAILED，并记录 MANUAL_INTERVENTION，而非编造 SMTP 永久拒绝。首轮开始前的排队等待不计入 24 小时。

`retry_at` 到期后，数据库事务和 message 行锁将收件人排队；Claim 再检查开关、次数与时间，防止已排队后长时间停机绕过窗口。关闭自动重试会暂停已有自动队列项，但不会撤销已经开始的网络操作。原有未许可 DATA 的安全租约恢复仍可在预算内重新排队，它不是对 SMTP 失败的自动重试；已许可 DATA 的过期租约仍归 UNKNOWN。

手动 UNKNOWN 重试是单次显式操作，不受自动开关阻止；它不会重置历史次数或时间窗口，后续自动重试仍受原预算约束。

SMTP AUTH 明确 5xx 拒绝、密钥/本地存储问题进入人工检查；AUTH 4xx 或可识别的认证网络断开可同 Provider 重试。RCPT 4xx/5xx 分别按收件人处理。DATA 后缺少确定结果保持 UNKNOWN；一个 message 还有 UNKNOWN 收件人时，其他重试也暂缓，直至人工完成处置。

决策的 `FailoverAllowed` 是协议层许可，4B 在显式启用后还会检查候选配置、容量和全部未完成收件人的证据，见下节。`delivery_attempts.result` 是当次事实；`recipients.status`/`messages.status` 是当前投递状态投影，例如 AUTH 535 事实可对应当前人工暂停 TEMP_FAILED，两者不应混淆。

### 查看证据

使用 `export-records --message-id <Gateway-UUID>` 导出记录，查找 UNKNOWN recipient 的 ID 及其最新 attempt ID；每轮 `DELIVERY_DECIDED` 事件含证据、预算、决策和实际重试时间。`attempt_recipients.decision` 保留该轮决策，`recipients.decision` 表示当前决策。

本阶段没有 UI。操作者只能通过本地受信任 CLI 和数据库权限操作；`--actor` 是审计标签，不是额外身份认证。

### 人工确认已送达 / 失败

```sh
docker compose exec -T relaytale relaytale resolve-unknown \
  --recipient-id <Recipient-UUID> --expected-attempt <Latest-Attempt-UUID> \
  --action mark-delivered --actor <Operator> --reason '已核查收件端原始邮件'
```

`mark-failed` 使用同一入口。它们更新当前收件人状态，追加 `UNKNOWN_RESOLVED` 事件及维护审计；原始 UNKNOWN attempt 和 RCPT/SMTP 响应保持不变。这里的 DELIVERED 是人工判断，不能在未来 UI 中显示为自动回执已验证。

### 明确承担风险后手动重试

```sh
docker compose exec -T relaytale relaytale resolve-unknown \
  --recipient-id <Recipient-UUID> --expected-attempt <Latest-Attempt-UUID> \
  --action retry --actor <Operator> --reason '业务负责人要求重发' \
  --acknowledge-duplicate-risk
```

每次只操作一个收件人。缺少风险确认、旧 attempt、已处置收件人、活跃投递或原文不可用时拒绝；并发操作只允许一次生效。它只排队，不在 CLI 中直接发送。发送时仍校验原文大小与 SHA-256，原 Provider 不可用时等待。剩余 UNKNOWN 收件人未处置前不会发出这次重试。

当前命令仅处理 UNKNOWN；配置问题、重试预算耗尽的通用人工恢复流程仍待补充，不应通过直接改数据库状态绕过保护。尚未提供 undo、修改 Provider 或重置重试预算的操作。


## 安全 Provider 切换（Phase 4B）

`FAILOVER_ENABLED=false` 默认关闭，也支持 YAML `failover_enabled` 和 CLI `--failover-enabled`。开启必须同时开启 RETRY_ENABLED，否则启动拒绝配置。关闭开关不会取消已经领取或开始的网络操作；它阻止下一次领取时再次更换路由，已选中的 Provider 会保留为当前路由。

切换等待已持久化的重试时间，不立即重发。所有未完成收件人都必须已到期排队、预算有效，且最新尝试证明正文前失败、允许切换。DNS / TCP / TLS 失败与正文前 DATA 4xx 可触发；AUTH 错误、RCPT 4xx、正文后明确 4xx 不触发切换，UNKNOWN 必须人工处置。混合收件人策略不同则继续原路由；不会为了切换重发已接受收件人。

备用 Provider 必须同时授权 Envelope From 与 Header From 域，启用 TLS，满足并发容量，剩余配额足够，并通过已启用的熔断检查。`from_domains` 是操作者对该服务发件能力的声明，系统不自动验证服务商是否允许域内每个地址；应先完成相应身份验收。不会改写发件地址或 MIME 来迁就备用账号。不同发件域的两套账号通常不能直接互备。

一次消息最多使用三个不同 Provider，且不会返回已离开的 Provider；逐人 7 次 / 24 小时预算跨 Provider 累计。没有合格备用时仍可在预算内重试当前 Provider；当前 Provider 也不可用则等待。4C 提供可选熔断；关闭时仅记录样本，不能把备用可连接当作稳定性保证。

导出时间线中的 `PROVIDER_FAILOVER` 事件包含每个收件人的前后 Provider、前一次 attempt 和决策。事件与路由/新尝试一起提交；审计写入失败不启动投递。切换后的 DATA 仍需持久化授权，已发出正文却无法提交最终结果时保持 UNKNOWN，不尝试第三个 Provider。


## 健康与收件人配额（Phase 4C）

熔断执行由 `HEALTH_ENABLED` 控制（默认 false），并受 Provider 的 `health_check_enabled` 限制。该开关不控制配额；只要 Provider 配置了 hourly/daily limit，就会强制执行。可在创建时指定 `--hourly-limit 100 --daily-limit 1000`，单位是收件人投递尝试，0 表示无限制。现有 Provider 的管理修改入口仍待后续管理 API；不要把单位当作逻辑邮件封数。

额度按领取时刻计算滚动窗口，预留与 attempt/审计一同提交，不依赖进程内计数。配额满时等待窗口释放；少于收件人数时自动分批。连接失败、UNKNOWN、进程崩溃和重试均计费，不退款。服务商外部发送及多个配置共享账号的限额不能自动合并，需要操作者配置保守的独立额度池。

健康窗口 10 分钟，至少 5 个有效尝试、失败比例 >=50% 打开熔断，60 秒后领取一个半开尝试。半开成功关闭并开启新统计周期；失败或无法证明成功再次冷却。进程崩溃由租约恢复释放半开名额，保持 DATA 后 UNKNOWN。连接诊断不会消耗额度或强行关闭熔断；熔断开启也不扩大 failover 的安全许可。

`list-providers` 可查看 `hourly_reserved`、`daily_reserved`、`health_successes`、`health_failures`、`circuit_state`、`open_until` 和 `probe_attempt_id`。这些健康计数排除 IGNORED 样本，熔断恢复后从新周期起点统计；历史完整证据仍在 attempts。事件 `QUOTA_RESERVED` 与 `PROVIDER_CIRCUIT_OPEN/HALF_OPEN/CLOSED` 可随消息记录导出。

升级迁移 6 会为最近 24 小时历史尝试补记配额，不恢复暂停邮件。回退代码前应关闭投递并评估数据库版本兼容，不能只切换 Git 标签后直接连接新数据库。配额 ledger 的长期清理策略待后续生命周期阶段。


## 抑制名单（Phase 5A）

管理命令、全局地址范围、DATA 授权边界、不可自动重放的历史与审计规则见 [Phase 5A 操作说明](phase-5a-suppression.md#cli)。自动退信处理尚未开放，不要把收到的 DSN 内容直接导入名单。


## 在途资源与指标（Phase 5B）

`MEMORY_BUDGET_BYTES` 默认 64 MiB，是工作准入预算，不是进程 RSS 硬上限。每个接收操作预留 1 MiB，每个 worker 操作预留 8 MiB；共享 FIFO 等待可取消。`SPOOL_BUDGET_BYTES` 默认 256 MiB，每个出站操作按 `MAX_MESSAGE_BYTES` 预留临时磁盘容量，等待期间不领取租约或预留 Provider 配额。

Compose 将快照放在 `/data/snapshots`，要求可写；快照打开后立即移除路径，关闭或进程退出后释放空间。路径为空时使用系统临时目录；若目录位于 tmpfs，其页也占用内存。预算不包含原始 EML 存档、数据库、文件系统缓存、认证及空闲连接开销。多实例预算逐进程生效，部署总量必须相加；目前不能将此视为完整的生产连接限流或内存保障。

指标从内部 `http://relaytale:8080/metrics` 抓取，默认 Caddy 对 `/metrics` 及子路径返回 404。独立部署需自行限制 HTTP 监听与网络访问。可设置 `METRICS_BEARER_TOKEN` 启用独立 Bearer 验证；空值保持无认证，不能直接暴露公网。建议从 30 秒抓取周期开始；单次数据库采集超时 2 秒，重叠采集返回 503。

消息状态数量、最老 QUEUED 年龄、24 小时尝试结果、Provider 配额耗尽与熔断、资源预算/等待数、Go 堆和数据库连接均为聚合指标，无邮箱/消息 ID 标签。24 小时结果是 gauge，不能对它使用 counter 的 `rate()` 推导精确吞吐；无记录的状态标签不输出。数据库指标在多个实例上是重复的全局视图，不能按实例简单相加。`SMTP_ACCEPTED` 仍不表示收件箱投递成功。

复现负载测试、观测数据及剩余限制见 [5B 验收报告](phase-5b-resources.md)。


## SMTP 准入与部署预算（Phase 5B.1）

默认总连接 128、单来源 8；来源按 TCP 对端 IPv4 地址或 IPv6 /64 聚合，不信任邮件头。每来源连接建立桶为突发 32、每秒恢复 1；来源状态最多 4096 条，空闲至少五分钟后周期回收，满表拒绝新来源。连接超额在问候前关闭。每来源最多一个 AUTH 正在执行，默认突发 20、每分钟恢复 60 次，超额返回临时 454。会话绝对时长默认 300 秒，不能靠持续发送命令延长。

对应设置为 `SMTP_MAX_CONNECTIONS`、`SMTP_MAX_CONNECTIONS_PER_IP`（IPv6 实际按 /64）、`SMTP_AUTH_PER_MINUTE`、`SMTP_AUTH_BURST`、`SMTP_MAX_SESSION_SECONDS`。NAT 后的客户端或反向代理会共享来源额度，部署前按真实拓扑调整。这些保护按进程生效，不能保证抵御分布式攻击或来源表耗尽。

`AUTH_CONCURRENCY` 默认 2，允许 2–8；每次 Argon2 使用 64 MiB，`AUTH_MEMORY_BUDGET_BYTES` 默认 128 MiB，必须覆盖并发数乘以 64 MiB，最大 512 MiB。这是活跃密码计算预算，不是认证全部 RSS：GC、TLS、连接、工作缓冲及运行时仍需余量。计算开始后不能立即取消，但请求取消不会继续认证成功。

Compose 默认进程容器内存硬限制 512 MiB、CPU 2，Go 软内存目标 384 MiB。可通过 `RELAYTALE_MEMORY_LIMIT`、`RELAYTALE_CPU_LIMIT`、`RELAYTALE_GO_MEMORY_LIMIT` 调整。提高认证并发时必须同时评估这些限制；Go 软目标不会替代容器硬上限，硬上限不足仍可能触发 OOM。数据库容器需另行规划资源。

默认工作内存 64 MiB、每个出站预留 8 MiB，因此最多八个出站操作同时持有预算。把 worker 设成 32 不会提高这个上限，多余 worker 等待准入；入站共享预算会进一步降低可用出站容量。临时磁盘预算除以单封大小上限也限制并发。提高 worker 数前应同时评估两种预算与实际负载。

## 指标令牌

`METRICS_BEARER_TOKEN` 仅从环境读取，非空时需 32–512 字节且不能包含空白；使用随机令牌。抓取请求携带 `Authorization: Bearer <token>`，缺失或错误返回 401，验证发生在数据库采集之前。该令牌不是管理 API 账户。Caddy 的默认 404 规则仍保留，令牌也不能替代 TLS 和网络隔离；不要把真实令牌放进 Git、URL 或共享日志。

## 升级前只读 EML 预检

旧存档若存在裸 CR，新的发送检查会暂停它。升级前暂停投递和入站变更，备份后运行目标版本的预检命令，连接现有数据库并挂载同一存档路径：

```sh
relaytale preflight-eml --storage-dir /data/eml --limit 1000 --max-message-bytes 26214400 > preflight.json
```

数据库来自 `DATABASE_URL`，根目录也可用 `EML_STORAGE_DIR`。大小参数默认 25 MiB，需显式匹配目标部署大小上限。本命令不执行迁移，不修改原文、不写事件、不重试邮件；扫描 AVAILABLE 且处于 QUEUED、TEMP_FAILED、SENDING、DELIVERY_UNKNOWN 的存档。检查路径约束、文件大小、CRLF 和 SHA-256；报告只包含 Gateway UUID 与原因码。

JSON 中 `complete=true` 表示本批扫描完成，不代表所有分页完成。`has_more=true` 时，把 `next_after` 传给下一批 `--after-id`，直到没有更多。发现问题或执行失败均退出非零；已产生的 JSON 保留在 stdout，错误在 stderr。启动或数据库连接失败可能尚无 JSON。`complete=false` 不能作为通过证据。预检不自动修复历史邮件，不解除 UNKNOWN；在线并发写入时仅反映读取时状态，不能替代停写后的完整检查。


## 最小管理入口（Phase 5C）

管理 API 默认关闭，独立令牌和角色通过 `ADMIN_API_KEYS` 配置；启用、权限、字段、版本冲突和操作结果不确定时的处理见 [管理 API 手册](management-api.md)。它与 metrics Bearer 令牌、SMTP 账户、未来 HTTP 发信凭证相互独立。迁移 00008 增加 Provider revision 及自动递增触发器，回退需要显式计划；仍需按单实例升级约束部署。


## 浏览器工作台（Phase 6）

通过独立 WEB_UI_ORIGIN/WEB_UI_USERS 配置启用 `/console/`，无需开放长期 API 令牌给浏览器。会话、HTTPS、额外 64 MiB 登录工作量、反向代理共享登录限额及完整操作边界见 [Web UI 手册](web-ui.md)。本阶段不新增数据库迁移。
