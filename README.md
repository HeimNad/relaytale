# RelayTale

RelayTale 是一个自托管的 SMTP 中转与投递观测系统。它接收应用邮件，持久化原始邮件和投递记录，再通过你已有的 SMTP Provider 发送，让队列、重试、通道切换和退信处理有据可查。

使用 Go 和 PostgreSQL，提供 SMTP、HTTP 发信接口与浏览器管理工作台。

**项目仍在开发中。** 已完成本地集成、故障注入和浏览器验收；生产部署与 Provider 认证边界见 [路线图](docs/roadmap.md) 和 [真实链路验收](docs/real-world-validation.md)。

## 功能

- **可靠接收**：SMTP STARTTLS、AUTH PLAIN / LOGIN、发件地址白名单；原始 EML 文件与数据库元数据分层存储，持久化完成后才确认接收。
- **投递控制**：逐收件人状态、Provider 路由、有限重试、安全切换、熔断和共享账号额度；结果不确定时保留 `DELIVERY_UNKNOWN`，禁止自动重发。
- **业务网站接入**：幂等 HTTP 发信、原始 MIME 或 JSON/附件、版本化模板、业务关联 ID、消息流、有效期、定时发送及受控取消、替换和改期。
- **结果反馈**：网站身份范围内的状态查询、签名 Webhook、持久化通知队列；支持收件箱退信收集和管理员人工关联、确认与撤回。
- **管理与观测**：角色权限、邮件与事件查询、模板管理、地址抑制、UNKNOWN 人工处置、Prometheus 指标、日志轮转和导出。
- **运维**：显式数据库迁移、受限运行账号、单实例保护、密钥轮换、备份恢复、可预览的存档清理和容量盘点。

```text
应用 ── SMTP / HTTP ──→ RelayTale ──→ SMTP Provider ──→ 收件服务
                          │
                    PostgreSQL + EML
                          │
                    工作台 / 状态查询 / Webhook
```

SMTP Provider 返回最终成功响应只表示 **SMTP 接受**，不代表最终送达或进入收件箱。人工确认的退信作为独立反馈展示，来源仍为 `UNVERIFIED`，不会改写 SMTP 接受事实或自动封禁收件人。Gateway 的回调签名也不等于 Provider 的退信来源认证。

## 安装与启动

需要 Docker Compose 和运行中的 Docker。默认 Compose 配置用于本地部署，提供 PostgreSQL、RelayTale 和 Caddy。

```sh
git clone https://github.com/HeimNad/relaytale.git
cd relaytale
cp .env.example .env
```

编辑 `.env`，设置独立的数据库密码和运行、迁移、恢复连接。启动数据库：

```sh
docker compose up -d postgres
```

按 [数据库权限部署指南](docs/phase-9b3-database-privileges.md#首次部署) 创建角色、转移所有权、执行迁移并应用授权。该步骤是首次启动的必要条件：日常服务使用受限账号，启动时不会自动迁移数据库。

完成数据库准备后，首次本地部署生成开发证书并启动：

```sh
docker compose run --build --rm relaytale init-dev-tls
docker compose run --build --rm relaytale check-db-permissions
docker compose up --build -d

curl http://localhost:8080/health/live
curl http://localhost:8080/health/ready
```

已有证书不要重复生成。开发证书仅用于 localhost；公网部署需有效证书和 HTTPS。默认 HTTP 入口为 `localhost:8080`，SMTP STARTTLS 入口为 `localhost:1587`，PostgreSQL 不公开主机端口。

### 默认配置

| 功能                                 | 默认行为                             |
| ------------------------------------ | ------------------------------------ |
| 出站投递                             | `WORKER_COUNT=0`，接收后排队，不发送 |
| 自动重试、跨 Provider 切换、健康熔断 | 分别显式启用，默认关闭               |
| HTTP 发信、管理 API、浏览器工作台    | 配置各自独立身份后启用               |
| Webhook 网络发送                     | `WEBHOOK_ENABLED=false`              |
| 定时清理                             | `MAINTENANCE_INTERVAL=0s`，默认关闭  |

配置优先级为 CLI > 环境变量 > YAML > 默认值。`.env` 由 Compose 读取，Go 程序不会自动加载它。完整配置示例见 [.env.example](.env.example) 和 [config.example.yaml](config.example.yaml)。

## 接入应用

### SMTP

创建账号并限定允许的发件地址：

```sh
docker compose exec relaytale relaytale create-smtp-account \
  --username my-app \
  --allowed-from noreply@example.com
```

生成的密码仅输出一次。配置应用使用 `localhost:1587`、STARTTLS 和该账号；本地测试客户端还需信任开发证书。SMTP 信封和邮件头发件人都需满足白名单。接入细节见 [架构说明](docs/architecture.md)。

### 配置出站 Provider

先保持 `WORKER_COUNT=0`，生成并保存主密钥：

```sh
openssl rand -hex 32
```

将结果写入私有 `.env` 的 `RELAYTALE_MASTER_KEY`，运行 `docker compose up -d --build relaytale` 加载配置。主密钥须独立备份；后续变更使用 [密钥轮换流程](docs/phase-9b4-key-rotation.md)。

用仓库外的私有密码文件创建 Provider：

```sh
docker compose exec -T relaytale relaytale create-provider \
  --name primary \
  --host smtp.provider.example --port 587 --security starttls \
  --username your-smtp-username --from-domains example.com \
  --priority 10 --max-connections 1 --timeout 30s \
  --password-stdin < /path/to/provider-password.txt
```

替换示例主机、账号和域名；465 端口使用 `--security implicit_tls`。创建命令不会发送测试邮件。确认配置后，在 `.env` 设置 `WORKER_COUNT=4`，运行 `docker compose up -d` 启用投递；符合路由条件的已有排队邮件也会开始发送。

### HTTP 与网站结果反馈

通过 `SEND_API_KEYS` 配置独立网站身份，可提交原始 MIME、JSON 邮件或模板请求。同一幂等键与相同参数可以安全重试提交，`202` 表示 Gateway 已接受请求。

网站使用返回的 `message_id` 查询状态，并在收到签名 Webhook 后重新查询最新结果。示例后端展示持久化提交、通知验签与去重、状态对账和人工退信反馈，默认运行在不发送 SMTP 的沙箱中。

- [HTTP 发信配置](docs/phase-8b-http-submission.md)
- [JSON 邮件与附件](docs/phase-8f2-json-attachments.md)
- [状态查询与 Webhook](docs/website-delivery-results.md)
- [OpenAPI 契约](docs/api/website.openapi.json)
- [可运行的网站示例](examples/website/README.md)

### 管理工作台

按 [工作台配置指南](docs/web-ui.md) 设置 `WEB_UI_ORIGIN` 和 `WEB_UI_USERS`，重启后访问 `/console/`。工作台支持 viewer、operator、admin 角色，使用独立密码与会话；生产运行不需要 Node.js 或外部 CDN。

## 运维与文档

| 主题                     | 文档                                                                                 |
| ------------------------ | ------------------------------------------------------------------------------------ |
| 系统架构与投递边界       | [架构说明](docs/architecture.md)                                                     |
| 当前进度、计划与验收记录 | [开发路线图](docs/roadmap.md)                                                        |
| 日志、导出与存档维护     | [运维说明](docs/operations.md)                                                       |
| 数据库升级与权限         | [迁移](docs/phase-9b1-migrations.md) · [权限](docs/phase-9b3-database-privileges.md) |
| 备份与恢复               | [恢复手册](docs/phase-9a-backup-recovery.md)                                         |
| 异常停机后的实例占用     | [人工恢复流程](docs/phase-9b2a-runtime-ownership.md)                                 |
| Provider 与发件域认证    | [身份与反馈责任](docs/feedback-and-sender-identity.md)                               |
| 人工退信处理             | [核验手册](docs/phase-7b7-manual-feedback.md)                                        |
| 既有部署改名兼容         | [升级说明](docs/rename-relaytale.md)                                                 |

数据库、EML 存档和凭证密钥需要按恢复手册成套备份，Git 不能替代数据备份。`docker compose down` 保留卷；`down -v` 会删除卷。升级前停止相关进程、备份并显式迁移，不自动释放异常停机遗留的实例占用。

当前 Gateway 透传原始 MIME，不自行签署 DKIM。收件箱中的退信不会仅凭 From、主题或 Message-ID 获得可信来源身份；自动可信 DSN 处理仍待完成。当前不支持 SMTPUTF8 信封地址。其他范围与限制以对应文档为准。

## 开发与测试

本地构建需要 Go 1.27.1 或更高版本；数据库集成测试使用 PostgreSQL。浏览器测试另需 Node.js 和 Playwright。

```sh
go build -o bin/relaytale ./cmd/relaytale
```

完整检查可在独立测试容器中运行，不使用应用数据库：

```sh
POSTGRES_PASSWORD=isolated-test-only docker compose --env-file /dev/null \
  -p relaytale-test --profile test run --build --rm test make ci

POSTGRES_PASSWORD=isolated-test-only docker compose --env-file /dev/null \
  -p relaytale-test --profile test down
```

`make ci` 执行 Go 格式、静态检查、构建和竞态测试，并要求隔离的 `TEST_DATABASE_URL`；GitHub Actions 使用同样的检查。单独运行 `make test` 时，未配置测试数据库会跳过数据库集成测试。

浏览器验收：

```sh
npm ci
npx playwright install chromium
# TEST_DATABASE_URL 必须指向独立测试数据库。
TEST_DATABASE_URL='postgres://user:password@localhost/test_db?sslmode=disable' \
  sh scripts/test-browser.sh
```

测试覆盖 SMTP 协议、持久化故障、并发领取、租约恢复、未知投递结果、重试与安全切换、API 身份隔离、签名通知及工作台操作。最近完整的网站闭环验收见 [验收记录](docs/phase-7b8-website-acceptance.md)。

## 参与贡献

欢迎通过 [Issues](https://github.com/HeimNad/relaytale/issues) 报告问题或讨论改进，通过 Pull Request 提交变更。问题报告请附复现步骤、版本和脱敏日志；不要上传密码、密钥或私人邮件。

提交前运行相关检查，涉及投递行为的修改应覆盖失败场景并保持保守投递语义。开发约定见 [Git 工作流程](docs/git-workflow.md)，规划见 [路线图](docs/roadmap.md)。

## 许可证

[AGPL-3.0-only](LICENSE)。第三方依赖保留各自许可证。
