# RelayTale

RelayTale is a self-hosted SMTP control plane and flight recorder for the email providers you already use.

品牌名称统一为 **RelayTale**；模块、命令和容器服务使用 `relaytale`。已有部署升级前请阅读 [改名与兼容说明](docs/rename-relaytale.md)。

Change your SMTP host. Keep your email code.

```text
Your Apps → SMTP ingress → Durable queue + EML archive → Provider router
                                 ↓                         ↓
                            Event ledger         Your existing SMTP providers
```

## 当前进度：Phase 4C

已实现 Go 服务入口、配置优先级、PostgreSQL 连接、内嵌 Goose 迁移、核心数据库表、存储可写检查、健康接口、JSON 日志、优雅退出和 Docker Compose。

已支持 SMTP STARTTLS、AUTH PLAIN / LOGIN、逐账号发件地址限制、原始 EML 持久化、收件人记录和事务入队。只在文件同步与数据库提交完成后回复 SMTP 250。

已实现 Generic SMTP Provider 投递、加密凭证、PostgreSQL 队列 worker、逐收件人投递结果和租约恢复。支持 STARTTLS 与隐式 TLS，严格验证 Provider 证书。

**默认 `WORKER_COUNT=0`，只接收不投递。** 配置主密钥、Provider 并显式启用 worker 后才开始发送。当前没有管理 UI；自动重试和安全跨 Provider 切换需分别显式启用，默认失败与不确定邮件暂停。真实 Provider 首轮 SMTP 接受层已通过，Gmail / QQ / iCloud 共 12 份收到的原始邮件已核对，Message-ID 与 MIME 内容保持；认证差异见验收报告。

## 启动

需要 Docker Compose 和运行中的 Docker / OrbStack。

```sh
cp .env.example .env
# 修改 .env 中的 POSTGRES_PASSWORD，建议用 openssl rand -hex 24 生成。
# 首次启动：显式生成仅用于 localhost 开发的自签证书（已有证书时不要重复执行）。
docker compose run --build --rm relaytale init-dev-tls
docker compose up --build -d
curl http://localhost:8080/health/live
curl http://localhost:8080/health/ready
```

首次启动自动执行迁移。成功响应分别为 `{"status":"alive"}` 和 `{"status":"ready"}`。依赖不可用时 ready 返回 HTTP 503。根路径暂时返回 404。

默认仅绑定本机，Caddy HTTP 入口为 8080。PostgreSQL 不向主机公开端口。当前 Compose 是本地开发配置；SMTP STARTTLS 入口为本机 1587，容器内部监听 587。生产需要替换为有效域名证书，并配置正式端口和 HTTPS。

```sh
docker compose logs -f relaytale
docker compose down
```

`down` 保留数据卷；不要使用 `down -v`，除非确实要删除数据库和存档。

## 本地开发

Go 1.27.1+。需可访问的 PostgreSQL，并设置 `DATABASE_URL`。

```sh
export GO111MODULE=on
export SMTP_TLS_CERT='/path/to/smtp.crt'
export SMTP_TLS_KEY='/path/to/smtp.key'
export DATABASE_URL='postgres://user:password@localhost:5432/relaytale?sslmode=disable'
make test
make vet
make build
go run ./cmd/relaytale --config config.example.yaml
```

配置顺序：CLI > 环境变量 > YAML > 默认值。`.env` 由 Compose 读取，Go 程序不会自动读取它。SMTP 默认监听 `:587` 且必须有 TLS 证书；仅测试 HTTP 时可以显式设置 `SMTP_LISTEN_ADDR=` 关闭 SMTP。

| CLI | 环境变量 | 默认值 |
| --- | --- | --- |
| `--http-addr` | `HTTP_LISTEN_ADDR` | `:8080` |
| `--database-url` | `DATABASE_URL` | 必填 |
| `--storage-dir` | `EML_STORAGE_DIR` | `data/eml` |
| `--smtp-addr` | `SMTP_LISTEN_ADDR` | `:587` |
| `--smtp-domain` | `SMTP_DOMAIN` | `localhost` |
| `--smtp-cert` | `SMTP_TLS_CERT` | 启用 SMTP 时必填 |
| `--smtp-key` | `SMTP_TLS_KEY` | 启用 SMTP 时必填 |
| `--max-message-bytes` | `MAX_MESSAGE_BYTES` | `26214400` |
| `--workers` | `WORKER_COUNT` | `0`（关闭投递） |
| `--shutdown-timeout` | `SHUTDOWN_TIMEOUT` | `30s` |

敏感配置优先使用环境变量，避免在命令行历史中保存密码。

## 目录

```text
cmd/relaytale/       进程装配、启动和退出
internal/config/   配置加载与校验
internal/database/ PostgreSQL 连接与迁移
internal/api/      HTTP 路由与健康检查
internal/storage/  EML 原子发布、文件与目录同步、SHA-256
internal/smtpserver/ STARTTLS、认证、SMTP 会话和错误映射
internal/auth/     Argon2id 凭证与发件地址授权
internal/message/  接收用例和原子数据库入队
internal/devtls/   显式生成 localhost 开发证书
internal/provider/ SMTP Provider 配置与加密凭证创建
internal/encryption/ AES-256-GCM，绑定 Provider ID
internal/smtpclient/ SMTP 阶段、原文传输与结果分类
internal/queue/    并发领取、租约、投递和事务完成
internal/testsmtp/ 本地 Fake Provider，用于失败注入
tests/integration/ 真实 SMTP 与 PostgreSQL 故障注入测试
migrations/        内嵌、版本化数据库迁移
docker/            Caddy 配置
docs/              架构决策与分阶段计划
```

详细边界见 [架构说明](docs/architecture.md)，开发顺序见 [路线图](docs/roadmap.md)。原始规格书保留为产品需求来源。

## 数据与备份

PostgreSQL 和 `/data/eml` 必须成对备份。邮件接收后，原始 EML 存储在 `/data/eml/YYYY/MM/DD/<uuid>.eml`，数据库保存路径、大小与 SHA-256。事件表禁止普通 UPDATE/DELETE；后续保留期清理必须通过专门维护流程实现。生产环境还需分离迁移账号和运行账号，当前开发环境使用同一账号。

采用 **AGPL-3.0-only**，完整文本见 [LICENSE](LICENSE)。项目原创代码按此许可证发布；第三方依赖保留各自许可证。可通过 `relaytale license` 或 `GET /license` 查看正文。发布或部署修改版时，请按许可证提供对应源代码；仅展示许可证正文不能代替源代码提供安排。

## 创建 SMTP 账号

服务启动并完成迁移后执行：

```sh
docker compose exec relaytale relaytale create-smtp-account \
  --username local-app \
  --allowed-from noreply@example.com
```

输出密码仅此一次，数据库只保存 Argon2id 哈希。不预置默认账号。多个允许地址用逗号分隔，必须是完整地址；空列表不授权。SMTP 信封 From、邮件头 From，以及可选 Sender 均需匹配。当前不接受空信封发件人或 SMTPUTF8 地址。

导出开发证书供客户端信任：

```sh
docker compose cp relaytale:/data/tls/smtp.crt /tmp/relaytale-smtp.crt
```

客户端配置：`localhost:1587`、STARTTLS、上面创建的账号密码，并信任该证书。可使用 swaks（交互输入密码，不将密码写入 shell 历史）：

```sh
swaks --server localhost --port 1587 --tls --tls-verify \
  --tls-ca-file /tmp/relaytale-smtp.crt \
  --auth PLAIN --auth-user local-app --auth-password \
  --from noreply@example.com --to recipient@example.test
```

可以查看 QUEUED 状态：

```sh
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$@"' sh \
  -c 'SELECT id, subject, status, eml_size FROM messages ORDER BY created_at DESC LIMIT 10;'
```

## 验证可靠接收

```sh
make test
make vet
# 完整测试：独立的 PostgreSQL 容器，不操作开发数据库。
docker compose --profile test run --build --rm test
docker compose --profile test stop postgres-test
```

集成测试覆盖 TLS 与两种 AUTH、匿名/错误密码/禁用账号、发件人越权、原文与哈希保留、多收件人、超限、中断上传、存储失败、真实数据库事务回滚，以及提交前不确认和优雅退出。未设置 `TEST_DATABASE_URL` 时，本地 `make test` 跳过数据库集成部分；Compose 测试会实际运行它们。

尚未完成 kill -9 与主机断电恢复演练、孤儿文件清理和生产限流。数据库提交结果不明确时保留 EML，避免误删可能已经提交的邮件。不要手动清空存档目录。

## Git 阶段检查点

每个阶段验证完成后提交代码并创建里程碑标签。阶段基线以 `phase-1`、`phase-2` 等标签保存；具体约定见 [Git 工作流程](docs/git-workflow.md)。Git 不包含数据库、EML、密码或证书，这些数据需要单独备份。


## 启用 Provider 投递（Phase 2）

1. 生成主密钥：`openssl rand -hex 32`。将结果保存到 `.env` 的 `RELAYTALE_MASTER_KEY`。该值用于加密 Provider 密码，需要独立备份；更换或丢失会导致已有凭证无法解密。
2. 保持 `WORKER_COUNT=0`，运行 `docker compose up -d --build`，让容器载入主密钥并应用迁移。
3. 使用仅本地可读的密码文件创建 Provider，例如：

```sh
docker compose exec -T relaytale relaytale create-provider \
  --name primary \
  --host smtp.your-provider.example --port 587 --security starttls \
  --username your-smtp-username --from-domains example.com \
  --priority 10 --max-connections 1 --timeout 30s \
  --password-stdin < /path/to/provider-password.txt
```

隐式 TLS 使用 `--security implicit_tls --port 465`。上面的 hostname 是占位符，须替换为真实 Provider。密码文件不要放入仓库。创建命令不连接 Provider、不发送测试邮件。

4. 在 `.env` 设置 `WORKER_COUNT=4`，再运行 `docker compose up -d`。符合 Provider 发件域规则的已有 QUEUED 邮件也会开始投递。

Provider 优先选择较小的 priority；并发容量或滚动收件人配额耗尽时跳过该 Provider。未匹配到 Provider 的邮件保持 QUEUED。低优先级 Provider 可承接领取时的可用容量，默认尝试失败后不换 Provider；显式开启 4B 后按安全证据切换。

### 结果语义

| 状态 | 含义与本阶段行为 |
| --- | --- |
| SMTP_ACCEPTED | Provider 返回最终 2xx；不代表最终送达 |
| PARTIAL_ACCEPTED | 部分收件人被接受，其他收件人失败；逐人保留结果 |
| TEMP_FAILED | 默认暂停；Phase 4A 显式启用后按逐收件人决策重试 |
| PERM_FAILED | 明确永久失败，本阶段暂停 |
| DELIVERY_UNKNOWN | DATA 后缺少确定结果，或持久化的 DATA 许可后 worker 租约过期；不自动重发 |

查看投递记录：

```sh
docker compose exec postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$@"' sh \
  -c 'SELECT message_id, attempt_number, result, smtp_code, error_class FROM delivery_attempts ORDER BY started_at DESC LIMIT 20;'
```

消息结果、收件人结果、attempt 和事件在同一事务完成。发送前校验存档大小和 SHA-256；不重新生成 MIME，不改 Message-ID。当前仅传输 CRLF 格式且以 CRLF 结束的 EML，非规范原文会保留并暂停，避免静默改写。

Phase 2 集成测试另外覆盖：SMTP ingress → 存档 → PostgreSQL 队列 → Fake Provider 全链路、并发领取与容量限制、过期领取标识拒绝、崩溃恢复、错误主密钥、存档损坏、最终结果提交失败以及 worker 退出。测试只使用本地 Fake Provider，尚未连接真实外部邮箱服务。


## Flight Recorder 与运维（Phase 3）

新增 DNS / SMTP 阶段持续记录、阶段耗时、错误分类、Provider 列表与不发信的连接诊断。运行日志按大小轮转；支持压缩 JSONL 记录导出、运行日志导出、EML 与已有 debug 内容保留期清理，以及追加式维护审计。

**自动清理默认关闭。** 手动 `cleanup` 默认只预览；邮件元数据和事件长期保留，待处理和结果不确定的邮件不会被 EML 保留策略删除。完整命令、保留边界与备份恢复流程见 [运维说明](docs/operations.md)。

Phase 3 测试覆盖导出原子发布/不覆盖、过滤与敏感字段排除、清理预览、安全状态筛选、受限路径、删除中断恢复、审计不可修改，以及发送完成前事件已落库。规格遵循情况、设计取舍和剩余工作见 [前三阶段规格对照](docs/phase-1-3-spec-review.md)。


## Delivery Decision Engine（Phase 4A）

将 SMTP 事实与后续决策分离：每个收件人保存版本化决策及事件，临时失败可持久化安排同 Provider 重试，成功/永久失败的收件人不重发。退避含 jitter，默认预算为首次领取后 24 小时、最多 7 次。达到上限转人工处理，绝不把预算耗尽冒充远端永久拒绝。

`RETRY_ENABLED=false` 默认关闭自动重试；只有真实 Provider 验收后才应考虑启用。历史暂停邮件不在迁移时自动恢复。配置错误/本地故障需要人工检查，UNKNOWN 不能自动重发。`resolve-unknown` 提供带预期 attempt、操作者、理由与重复风险确认的人工处理，旧尝试证据不改写。

见 [实施计划](docs/phase-4-plan.md)、[真实链路矩阵](docs/real-world-validation.md) 和 [重试与人工处置说明](docs/operations.md#重试与-unknown-人工处置phase-4a)。4B 的安全跨 Provider 切换已实现；4C 的滚动健康、熔断与收件人尝试配额已实现，熔断执行默认关闭。


真实链路首轮：SpaceMail/PurelyMail 的 465 与 587 各一次投递，四封邮件、16 个收件人均获最终 250。临时 worker 已停止，测试配置已禁用。Gmail / QQ / iCloud 的 12 份原始邮件已确认 Message-ID、主题与解码 MIME 内容保持（QQ 的 PurelyMail 587 在垃圾箱）；QQ 认证差异、SpaceMail DMARC、大消息边界和 Outlook 仍待核验，详见 [真实验收报告](docs/real-world-validation.md)。


## 安全跨 Provider 切换（Phase 4B）

`FAILOVER_ENABLED=false` 默认关闭，启用要求同时开启 RETRY_ENABLED。到期领取时，只在全部未完成收件人的最新决策明确允许切换时选择备用；混合 RCPT 错误保留原路由，已接受的收件人不重发。每条消息最多使用三个不同 Provider，不返回离开的 Provider，且仍受逐人 7 次 / 24 小时预算限制。

候选同时匹配 Envelope From / Header From 发件域，检查启用状态、TLS、容量、剩余配额与已启用的熔断。切换审计和新尝试一起提交。DATA 后结果不明或最终数据库提交失败仍进入 UNKNOWN。见 [运维说明](docs/operations.md#安全-provider-切换phase-4b) 与 [实施计划](docs/phase-4-plan.md)。


## Provider 健康与配额（Phase 4C）

`HEALTH_ENABLED=false` 默认仅记录健康样本，不执行熔断。启用后按最近 10 分钟有效尝试统计，至少 5 个样本、失败率达到 50% 时冷却 60 秒，再放行一个半开尝试。收件人拒绝和本地/配置问题不计入 Provider 可用性失败。邮件 UNKNOWN 不因熔断恢复而重发。

创建 Provider 可设置 `--hourly-limit` / `--daily-limit`（0 不限），按滚动 1h / 24h 的收件人尝试预留计数；额度不够会分批投递。已提交的预留在失败或崩溃时不返还，防止不确定结果重复使用额度。配额独立于熔断开关，明确配置后始终执行。`list-providers` 展示剩余用量依据与熔断状态。详见 [4C 方案与验收](docs/phase-4c-plan.md)。


## 后续开发顺序

Phase 4C 之后先做 **5A 信誉保护基础**（suppression、人工解除、可信退信设计与发件身份验收），再做 **5B 大邮件/队列资源加固**、**5C 最小管理 API** 和 **6 Web UI**。完整 DSN 自动化、HTTP 发信 API 与生产运维分别验收。已完成本地故障测试不代表生产就绪；当前功能、阶段验收条件与剩余缺口以 [开发路线图](docs/roadmap.md) 为准。
