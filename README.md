# MailGateway

MailGateway is a self-hosted SMTP control plane and flight recorder for the email providers you already use.

Change your SMTP host. Keep your email code.

```text
Your Apps → SMTP ingress → Durable queue + EML archive → Provider router
                                 ↓                         ↓
                            Event ledger         Your existing SMTP providers
```

## 当前进度：Phase 2

已实现 Go 服务入口、配置优先级、PostgreSQL 连接、内嵌 Goose 迁移、核心数据库表、存储可写检查、健康接口、JSON 日志、优雅退出和 Docker Compose。

已支持 SMTP STARTTLS、AUTH PLAIN / LOGIN、逐账号发件地址限制、原始 EML 持久化、收件人记录和事务入队。只在文件同步与数据库提交完成后回复 SMTP 250。

已实现 Generic SMTP Provider 投递、加密凭证、PostgreSQL 队列 worker、逐收件人投递结果和租约恢复。支持 STARTTLS 与隐式 TLS，严格验证 Provider 证书。

**默认 `WORKER_COUNT=0`，只接收不投递。** 配置主密钥、Provider 并显式启用 worker 后才开始发送。当前没有管理 UI、自动重试或故障切换；失败与不确定邮件会暂停。

## 启动

需要 Docker Compose 和运行中的 Docker / OrbStack。

```sh
cp .env.example .env
# 修改 .env 中的 POSTGRES_PASSWORD，建议用 openssl rand -hex 24 生成。
# 首次启动：显式生成仅用于 localhost 开发的自签证书（已有证书时不要重复执行）。
docker compose run --build --rm gateway init-dev-tls
docker compose up --build -d
curl http://localhost:8080/health/live
curl http://localhost:8080/health/ready
```

首次启动自动执行迁移。成功响应分别为 `{"status":"alive"}` 和 `{"status":"ready"}`。依赖不可用时 ready 返回 HTTP 503。根路径暂时返回 404。

默认仅绑定本机，Caddy HTTP 入口为 8080。PostgreSQL 不向主机公开端口。当前 Compose 是本地开发配置；SMTP STARTTLS 入口为本机 1587，容器内部监听 587。生产需要替换为有效域名证书，并配置正式端口和 HTTPS。

```sh
docker compose logs -f gateway
docker compose down
```

`down` 保留数据卷；不要使用 `down -v`，除非确实要删除数据库和存档。

## 本地开发

Go 1.27.1+。需可访问的 PostgreSQL，并设置 `DATABASE_URL`。

```sh
export GO111MODULE=on
export SMTP_TLS_CERT='/path/to/smtp.crt'
export SMTP_TLS_KEY='/path/to/smtp.key'
export DATABASE_URL='postgres://user:password@localhost:5432/mailgateway?sslmode=disable'
make test
make vet
make build
go run ./cmd/gateway --config config.example.yaml
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
cmd/gateway/       进程装配、启动和退出
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

许可证尚未选择。

## 创建 SMTP 账号

服务启动并完成迁移后执行：

```sh
docker compose exec gateway create-smtp-account \
  --username local-app \
  --allowed-from noreply@example.com
```

输出密码仅此一次，数据库只保存 Argon2id 哈希。不预置默认账号。多个允许地址用逗号分隔，必须是完整地址；空列表不授权。SMTP 信封 From、邮件头 From，以及可选 Sender 均需匹配。当前不接受空信封发件人或 SMTPUTF8 地址。

导出开发证书供客户端信任：

```sh
docker compose cp gateway:/data/tls/smtp.crt /tmp/mailgateway-smtp.crt
```

客户端配置：`localhost:1587`、STARTTLS、上面创建的账号密码，并信任该证书。可使用 swaks（交互输入密码，不将密码写入 shell 历史）：

```sh
swaks --server localhost --port 1587 --tls --tls-verify \
  --tls-ca-file /tmp/mailgateway-smtp.crt \
  --auth PLAIN --auth-user local-app --auth-password \
  --from noreply@example.com --to recipient@example.test
```

可以查看 QUEUED 状态：

```sh
docker compose exec postgres psql -U mailgateway -d mailgateway \
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

1. 生成主密钥：`openssl rand -hex 32`。将结果保存到 `.env` 的 `MAILGATEWAY_MASTER_KEY`。该值用于加密 Provider 密码，需要独立备份；更换或丢失会导致已有凭证无法解密。
2. 保持 `WORKER_COUNT=0`，运行 `docker compose up -d --build`，让容器载入主密钥并应用迁移。
3. 使用仅本地可读的密码文件创建 Provider，例如：

```sh
docker compose exec -T gateway create-provider \
  --name primary \
  --host smtp.your-provider.example --port 587 --security starttls \
  --username your-smtp-username --from-domains example.com \
  --priority 10 --max-connections 1 --timeout 30s \
  --password-stdin < /path/to/provider-password.txt
```

隐式 TLS 使用 `--security implicit_tls --port 465`。上面的 hostname 是占位符，须替换为真实 Provider。密码文件不要放入仓库。创建命令不连接 Provider、不发送测试邮件。

4. 在 `.env` 设置 `WORKER_COUNT=4`，再运行 `docker compose up -d`。符合 Provider 发件域规则的已有 QUEUED 邮件也会开始投递。

Provider 优先选择较小的 priority；连接额度耗尽或配置了本阶段尚不支持的 hourly/daily limit 时跳过该 Provider。未匹配到 Provider 的邮件保持 QUEUED。低优先级 Provider 可承接领取时的可用容量，但单次尝试失败后不会自动换 Provider。

### 结果语义

| 状态 | 含义与本阶段行为 |
| --- | --- |
| SMTP_ACCEPTED | Provider 返回最终 2xx；不代表最终送达 |
| PARTIAL_ACCEPTED | 部分收件人被接受，其他收件人失败；逐人保留结果 |
| TEMP_FAILED | 暂时失败，本阶段暂停，不自动重试 |
| PERM_FAILED | 明确永久失败，本阶段暂停 |
| DELIVERY_UNKNOWN | DATA 后缺少确定结果，或持久化的 DATA 许可后 worker 租约过期；不自动重发 |

查看投递记录：

```sh
docker compose exec postgres psql -U mailgateway -d mailgateway \
  -c 'SELECT message_id, attempt_number, result, smtp_code, error_class FROM delivery_attempts ORDER BY started_at DESC LIMIT 20;'
```

消息结果、收件人结果、attempt 和事件在同一事务完成。发送前校验存档大小和 SHA-256；不重新生成 MIME，不改 Message-ID。当前仅传输 CRLF 格式且以 CRLF 结束的 EML，非规范原文会保留并暂停，避免静默改写。

Phase 2 集成测试另外覆盖：SMTP ingress → 存档 → PostgreSQL 队列 → Fake Provider 全链路、并发领取与容量限制、过期领取标识拒绝、崩溃恢复、错误主密钥、存档损坏、最终结果提交失败以及 worker 退出。测试只使用本地 Fake Provider，尚未连接真实外部邮箱服务。
