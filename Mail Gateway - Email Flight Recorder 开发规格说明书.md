# Mail Gateway / Email Flight Recorder
## 产品与工程开发规格说明书

版本：v0.1  
目标阶段：MVP → 可实际生产使用的 v1  
推荐技术栈：Go + PostgreSQL + Docker Compose  
产品类型：Self-hosted Outbound Email Gateway / SMTP Control Plane / Email Flight Recorder

---

# 1. 产品定位

本项目不是邮件服务器、不是邮箱服务、不是营销邮件平台，也不是 SendGrid / Resend / Mailchimp 的直接替代品。

产品核心定位：

> 一个 SMTP 原生、自托管的发件网关，为用户现有的 SMTP Provider 提供统一队列、日志、路由、重试、故障切换、邮件留档和可观测能力。

用户已有：

- SpaceMail
- PurelyMail
- Amazon SES SMTP
- ZeptoMail
- SMTP2GO
- Mailgun SMTP
- Postmark SMTP
- Brevo SMTP
- SendGrid SMTP
- 阿里云 SMTP
- 任意标准 SMTP Provider

本系统位于应用程序和 SMTP Provider 之间。

基本架构：

```text
Applications
     │
     ├── SMTP 587
     └── HTTP API
          │
          ▼
┌───────────────────────┐
│     Mail Gateway      │
│                       │
│ SMTP Ingress          │
│ HTTP API              │
│ Durable Queue         │
│ Event Ledger          │
│ EML Archive           │
│ Retry Engine          │
│ Provider Router       │
│ Health Monitor        │
│ Bounce Processor      │
│ Admin UI              │
└───────────┬───────────┘
            │
       Provider Router
      /       |        \
     ▼        ▼         ▼
SpaceMail PurelyMail Amazon SES
```

核心卖点：

1. 用户原有应用无需改发件代码，仅修改 SMTP Host。
2. 每封邮件都有完整生命周期记录。
3. 保留原始 `.eml`。
4. 明确区分：
   - Gateway 已接收
   - SMTP Provider 已接受
   - 最终 Delivery
   - Bounce
   - Delivery Unknown
5. 支持多 SMTP Provider。
6. 支持安全 Failover。
7. 支持没有 Webhook 的普通 SMTP。
8. 强调可观测性，而不是营销功能。

---

# 2. 明确不做的功能

MVP 和 v1 不实现以下功能：

- Newsletter
- Marketing Campaign
- Contact CRM
- Drag-and-drop Email Editor
- Marketing Automation
- Subscriber Management
- Inbox
- IMAP 邮箱托管
- POP3 邮箱托管
- 自己直接连接 Gmail / Outlook MX 进行公网 MTA 投递
- IP warming
- Dedicated IP management
- Mailing list
- Email AI writer
- CRM
- 大规模营销邮件统计

产品必须保持基础设施定位。

---

# 3. 推荐技术栈

## Backend

Go 1.24+。

建议：

```text
HTTP Router
github.com/go-chi/chi

PostgreSQL Driver
github.com/jackc/pgx/v5

SMTP Server
github.com/emersion/go-smtp

SMTP Client
github.com/wneessen/go-mail

Migration
github.com/pressly/goose

Logging
log/slog

Config
环境变量 + YAML

UUID
UUIDv7 或 ULID
```

推荐内部 ID：

```text
UUIDv7
```

理由：

- 全局唯一
- 大致按时间排序
- PostgreSQL 索引友好

---

# 4. 第一阶段总体架构

第一阶段尽量减少外部依赖。

仅运行：

```text
gateway
postgres
caddy
```

不要第一版加入：

- Redis
- RabbitMQ
- Kafka
- Elasticsearch
- Kubernetes

PostgreSQL 同时承担：

- 数据库
- Durable Queue
- Event Ledger
- Provider 状态

邮件正文 / EML：

第一版：

```text
local filesystem
```

未来支持：

```text
S3
Cloudflare R2
Backblaze B2
MinIO
```

---

# 5. 项目仓库结构

建议：

```text
mailgateway/
│
├── cmd/
│   └── gateway/
│       └── main.go
│
├── internal/
│   ├── api/
│   │   ├── server.go
│   │   ├── middleware.go
│   │   ├── emails.go
│   │   ├── providers.go
│   │   └── health.go
│   │
│   ├── smtpserver/
│   │   ├── server.go
│   │   ├── backend.go
│   │   ├── session.go
│   │   └── auth.go
│   │
│   ├── smtpclient/
│   │   ├── client.go
│   │   ├── connection.go
│   │   └── result.go
│   │
│   ├── provider/
│   │   ├── provider.go
│   │   ├── router.go
│   │   ├── health.go
│   │   ├── rate_limit.go
│   │   └── credentials.go
│   │
│   ├── queue/
│   │   ├── worker.go
│   │   ├── scheduler.go
│   │   ├── retry.go
│   │   └── lease.go
│   │
│   ├── message/
│   │   ├── message.go
│   │   ├── parser.go
│   │   ├── archive.go
│   │   ├── hash.go
│   │   └── headers.go
│   │
│   ├── events/
│   │   ├── event.go
│   │   ├── ledger.go
│   │   └── types.go
│   │
│   ├── bounce/
│   │   ├── parser.go
│   │   ├── dsn.go
│   │   └── matcher.go
│   │
│   ├── database/
│   │   ├── db.go
│   │   └── tx.go
│   │
│   ├── config/
│   │   └── config.go
│   │
│   ├── auth/
│   │
│   ├── encryption/
│   │
│   ├── metrics/
│   │
│   └── web/
│
├── migrations/
│
├── web/
│   ├── templates/
│   └── static/
│
├── docker/
│
├── Dockerfile
├── docker-compose.yml
├── .env.example
├── README.md
└── LICENSE
```

---

# 6. 核心数据模型

必须从一开始正确设计。

不要把所有数据塞进一张 emails 表。

至少分为：

```text
messages
recipients
delivery_attempts
events
providers
provider_health
smtp_accounts
suppression_entries
api_keys
```

---

# 7. messages 表

表示“一封逻辑邮件”。

建议字段：

```sql
CREATE TABLE messages (
    id UUID PRIMARY KEY,

    message_id TEXT,
    source_type TEXT NOT NULL,

    envelope_from TEXT,
    header_from TEXT,

    subject TEXT,

    created_at TIMESTAMPTZ NOT NULL,
    queued_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,

    status TEXT NOT NULL,

    eml_path TEXT NOT NULL,
    eml_size BIGINT NOT NULL,
    eml_sha256 TEXT NOT NULL,

    idempotency_key TEXT,

    priority SMALLINT DEFAULT 0,

    metadata JSONB,

    last_error TEXT,

    next_attempt_at TIMESTAMPTZ,

    attempt_count INTEGER DEFAULT 0
);
```

source_type：

```text
smtp
api
internal
```

---

# 8. recipients 表

一封邮件可能有多个 recipient。

不要把：

```text
to_addresses
```

直接塞成 JSON 然后当唯一投递单位。

SMTP 是 recipient-level delivery。

因此：

```sql
recipients
```

建议：

```sql
CREATE TABLE recipients (
    id UUID PRIMARY KEY,

    message_id UUID NOT NULL REFERENCES messages(id),

    address TEXT NOT NULL,

    recipient_type TEXT NOT NULL,

    status TEXT NOT NULL,

    provider_id UUID,

    smtp_response TEXT,

    smtp_code INTEGER,

    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);
```

recipient_type：

```text
to
cc
bcc
```

状态可能独立：

```text
accepted
rejected
bounced
suppressed
unknown
```

因为 SMTP 可能出现：

```text
RCPT TO user1@gmail.com
250 OK

RCPT TO user2@gmail.com
550 User unknown
```

所以不能只记录整封邮件成功或失败。

---

# 9. delivery_attempts 表

代表每一次 Provider 尝试。

```sql
CREATE TABLE delivery_attempts (
    id UUID PRIMARY KEY,

    message_id UUID NOT NULL REFERENCES messages(id),

    provider_id UUID NOT NULL,

    attempt_number INTEGER NOT NULL,

    started_at TIMESTAMPTZ NOT NULL,
    connected_at TIMESTAMPTZ,
    tls_at TIMESTAMPTZ,
    authenticated_at TIMESTAMPTZ,

    mail_from_at TIMESTAMPTZ,
    rcpt_at TIMESTAMPTZ,
    data_started_at TIMESTAMPTZ,
    data_completed_at TIMESTAMPTZ,
    final_response_at TIMESTAMPTZ,

    finished_at TIMESTAMPTZ,

    result TEXT NOT NULL,

    smtp_code INTEGER,
    smtp_enhanced_code TEXT,
    smtp_response TEXT,

    error_class TEXT,
    error_message TEXT,

    bytes_sent BIGINT,

    connection_duration_ms INTEGER,
    tls_duration_ms INTEGER,
    auth_duration_ms INTEGER,
    data_duration_ms INTEGER,
    total_duration_ms INTEGER,

    provider_message_id TEXT,

    remote_host TEXT,
    remote_ip INET,

    raw_debug_log TEXT
);
```

---

# 10. Event Ledger

系统必须使用 append-only event ledger。

不要只修改：

```text
status
```

每次状态变化同时生成 event。

例如：

```sql
events

id
message_id
recipient_id nullable
attempt_id nullable

event_type
event_time

source
data JSONB
```

示例：

```text
MESSAGE_RECEIVED
MESSAGE_ARCHIVED
MESSAGE_QUEUED

ATTEMPT_STARTED
TCP_CONNECTED
TLS_ESTABLISHED
AUTH_SUCCEEDED

MAIL_FROM_ACCEPTED
RCPT_ACCEPTED
RCPT_REJECTED

DATA_STARTED
DATA_COMPLETED
SMTP_ACCEPTED

ATTEMPT_FAILED
DELIVERY_UNKNOWN

RETRY_SCHEDULED

PROVIDER_CHANGED

BOUNCE_RECEIVED
SOFT_BOUNCE
HARD_BOUNCE

DELIVERED

SUPPRESSED
```

Web UI Timeline 必须从 event ledger 构建。

---

# 11. 邮件状态机

不要只设计：

```text
pending
sent
failed
```

建议：

```text
RECEIVED

ARCHIVED

QUEUED

SENDING

SMTP_ACCEPTED

PARTIAL_ACCEPTED

TEMP_FAILED

PERM_FAILED

DELIVERY_UNKNOWN

BOUNCED

DELIVERED

SUPPRESSED

CANCELLED
```

重点：

## SMTP_ACCEPTED

定义：

```text
SMTP Provider 返回成功的 final 2xx response。
```

绝对不能叫：

```text
DELIVERED
```

---

# 12. DELIVERY_UNKNOWN

这是产品核心能力之一。

以下场景：

```text
Gateway
   │
   │ DATA
   ▼
Provider

message transferred
      ↓
connection lost
      ↓
final SMTP response not received
```

此时不能判断：

```text
发送成功
```

也不能判断：

```text
失败
```

必须：

```text
DELIVERY_UNKNOWN
```

系统默认不得立即 Failover。

原因：

可能 Provider 已经接受邮件。

如果立即通过另一个 Provider 再发：

```text
duplicate email
```

UI 应显示：

```text
Delivery uncertain

The complete SMTP DATA payload was transmitted,
but the final server response was not received.
```

用户可以：

```text
Retry manually
Wait
Mark as delivered
Mark as failed
```

未来也可以通过 DSN / webhook 自动 resolve。

---

# 13. SMTP Ingress

系统需要提供：

```text
SMTP submission server
```

默认：

```text
587 STARTTLS
```

未来：

```text
465 implicit TLS
```

监听配置：

```env
SMTP_LISTEN_ADDR=:587
```

必须要求认证。

不允许默认 open relay。

认证方式 MVP：

```text
AUTH PLAIN
AUTH LOGIN
```

用户可以创建内部 SMTP Credentials：

```text
username
password
allowed_from
enabled
```

例如：

```text
wordpress-prod
password

allowed sender:
noreply@example.com
```

---

# 14. SMTP Ingress 流程

收到连接：

```text
CONNECT
↓
EHLO
↓
STARTTLS
↓
AUTH
↓
MAIL FROM
↓
RCPT TO
↓
DATA
```

DATA 完成后：

第一步：

```text
将完整 raw MIME 写入临时文件
```

第二步：

```text
fsync / atomic rename
```

第三步：

```text
生成 SHA256
```

第四步：

```text
数据库 transaction
```

创建：

```text
message
recipients
events
queue state
```

第五步：

确保邮件已经：

```text
durably stored
```

然后才能返回：

```text
250 2.0.0 Queued
```

绝对不能先：

```text
250 OK
```

然后才写文件或数据库。

否则 Gateway 崩溃会丢邮件。

---

# 15. HTTP API

同时提供 HTTP API。

MVP：

```http
POST /api/v1/messages
```

示例：

```json
{
  "from": "noreply@example.com",
  "to": [
    "test@gmail.com"
  ],
  "cc": [],
  "bcc": [],
  "subject": "Hello",
  "text": "Hello",
  "html": "<p>Hello</p>",
  "headers": {
    "X-App-ID": "billing"
  }
}
```

响应：

```json
{
  "id": "0199...",
  "status": "queued"
}
```

支持 header：

```text
Idempotency-Key
```

如果相同 API Key + Idempotency Key 已创建邮件：

返回同一 message。

不能重复创建。

---

# 16. Raw EML API

未来支持：

```http
POST /api/v1/messages/raw
Content-Type: message/rfc822
```

直接上传 `.eml`。

该方式必须尽量不修改 MIME。

---

# 17. EML Archive

所有 SMTP ingress 邮件必须保存 raw `.eml`。

目录：

```text
/data/eml/YYYY/MM/DD/<uuid>.eml
```

例如：

```text
/data/eml/2026/09/10/0199.....eml
```

每个文件计算：

```text
SHA-256
```

数据库保存：

```text
path
size
sha256
```

后续支持：

```text
storage_driver
```

接口：

```go
type MessageStore interface {
    Save(...)
    Open(...)
    Delete(...)
    Exists(...)
}
```

实现：

```text
LocalStore
S3Store
```

---

# 18. Provider Model

所有普通 SMTP Server 使用统一 Provider。

provider 配置：

```text
name
enabled

host
port

security:
implicit_tls
starttls
plain

username
encrypted_password

priority

weight

hourly_limit
daily_limit

max_connections

timeout

from_domains

health_check_enabled
```

---

# 19. Provider Secret

SMTP 密码不得 plaintext 保存。

使用 master key：

```env
MAILGATEWAY_MASTER_KEY=
```

AES-256-GCM 加密 credentials。

数据库：

```text
password_ciphertext
nonce
```

master key 不进入数据库。

---

# 20. Provider Router

必须支持以下策略。

MVP：

```text
priority
```

例如：

```text
SpaceMail priority 10
PurelyMail priority 20
SES priority 30
```

正常：

```text
SpaceMail
```

不可用：

```text
PurelyMail
```

以后增加：

```text
weighted
round_robin
domain_based
sender_based
recipient_domain_based
```

例如：

```text
@gmail.com → SES
@company.com → SpaceMail
```

---

# 21. Safe Failover

这是非常重要的模块。

只有在明确确认：

```text
Provider 没有接收到 DATA
```

的情况下才允许自动切换。

可以自动切换：

```text
DNS lookup failed

connection refused

TCP timeout before SMTP connection

TLS negotiation failed before DATA

authentication connection failure

SMTP 421 before DATA

provider unavailable
```

谨慎：

```text
4xx after RCPT
```

通常先 retry。

禁止自动切换：

```text
DATA payload 已发送完
但没有收到 final response
```

状态：

```text
DELIVERY_UNKNOWN
```

---

# 22. Retry Engine

采用 exponential backoff + jitter。

建议默认：

```text
Attempt 1
立即

Attempt 2
1 minute

Attempt 3
5 minutes

Attempt 4
15 minutes

Attempt 5
1 hour

Attempt 6
4 hours

Attempt 7
12 hours
```

最大 retry window：

```text
24 hours
```

可配置。

明确 `5xx`：

通常：

```text
PERM_FAILED
```

不要重试。

`4xx`：

```text
TEMP_FAILED
```

重试。

---

# 23. PostgreSQL Durable Queue

不要用：

```text
SELECT pending LIMIT ...
```

直接并发取。

必须：

```sql
SELECT id
FROM messages
WHERE status = 'QUEUED'
AND next_attempt_at <= now()
ORDER BY priority DESC, created_at ASC
FOR UPDATE SKIP LOCKED
LIMIT 20;
```

Worker 获得 lease。

建议增加：

```text
locked_at
locked_by
lease_expires_at
```

防止 worker crash 后邮件永久卡住。

---

# 24. Worker 模型

配置：

```env
WORKER_COUNT=4
```

每个 worker：

```text
claim jobs
↓
load EML
↓
select provider
↓
send
↓
record attempt
↓
record events
↓
update message
```

必须捕获 panic。

Worker crash 不能影响其他 worker。

---

# 25. SMTP Protocol Logging

项目核心功能之一。

尽可能记录以下阶段：

```text
DNS lookup

TCP connect

TLS handshake

SMTP greeting

EHLO

AUTH

MAIL FROM

RCPT TO

DATA

final response

QUIT
```

不要默认保存 SMTP AUTH 密码。

Debug log 必须 redact：

```text
AUTH LOGIN
AUTH PLAIN
Authorization
password
token
```

---

# 26. Timing Metrics

每个 attempt 记录：

```text
DNS duration

TCP duration

TLS duration

AUTH duration

RCPT duration

DATA transfer duration

final response duration

total duration
```

Dashboard 可以计算：

```text
P50
P95
P99
```

---

# 27. Provider Health

每个 Provider 保留滚动健康状态。

状态：

```text
HEALTHY
DEGRADED
UNHEALTHY
DISABLED
```

依据：

```text
最近 N 次连接成功率
最近 N 次 temporary failure
connection latency
TLS error
authentication error
5xx rate
```

不要因为：

```text
550 recipient unknown
```

判定 Provider 不健康。

必须区分：

```text
provider failure
recipient failure
message failure
```

---

# 28. Circuit Breaker

未来 v1 加入。

例如：

过去：

```text
10 次
```

出现：

```text
8 次 connection timeout
```

Provider：

```text
UNHEALTHY
```

进入：

```text
circuit open
```

例如 5 分钟。

期间正常流量跳过该 Provider。

之后进入：

```text
half-open
```

发测试流量。

---

# 29. Rate Limit

需要知道 Provider 限制。

例如：

```text
SpaceMail
500/hour
```

配置：

```text
hourly_limit
daily_limit
max_connections
```

Router 不应超过限制。

实现建议：

PostgreSQL 统计 + 内存 token bucket。

MVP 可先：

```text
rolling DB counters
```

---

# 30. Suppression

必须支持 suppression list。

原因：

hard bounce 地址不应继续发送。

表：

```text
suppression_entries

email
reason
source
created_at
expires_at nullable
```

reason：

```text
hard_bounce
complaint
manual
invalid
unsubscribe
```

虽然本项目不是 marketing 系统，但 hard bounce suppression 属于基础设施能力。

---

# 31. Bounce / DSN

普通 SMTP Provider 没有 webhook 时，需要尽可能通过 DSN 获取信息。

目标支持 RFC 3464 Delivery Status Notification。

解析：

```text
Final-Recipient

Original-Recipient

Action

Status

Diagnostic-Code

Reporting-MTA

Original-Message-ID
```

关联 message。

---

# 32. Return Path / VERP

未来建议支持：

```text
bounce+<message_id>@bounce.example.com
```

或者：

```text
bounce+<recipient_id>@bounce.example.com
```

类似 VERP。

这样 bounce 可以直接映射：

```text
recipient
```

---

# 33. Bounce Ingestion

第一版可提供两种。

## API/Webhook

```http
POST /api/v1/bounces
```

## IMAP Polling

配置：

```text
IMAP host
port
username
password
folder
```

系统定期读取 bounce mailbox。

注意：

IMAP 功能只是：

```text
bounce ingestion
```

不是提供邮箱服务。

---

# 34. Provider Webhooks

未来针对 Provider Adapter 加原生 webhook。

例如：

```text
SES
Postmark
Mailgun
Resend
SendGrid
```

统一转换成：

```text
DELIVERED
BOUNCED
DEFERRED
COMPLAINT
OPEN
CLICK
```

Event Ledger。

---

# 35. SMTP Provider Adapter

第一阶段所有 Provider：

```text
Generic SMTP
```

一个 adapter 即支持：

```text
SpaceMail
PurelyMail
ZeptoMail
SES SMTP
Brevo
SMTP2GO
Mailgun SMTP
SendGrid SMTP
Postmark SMTP
```

后续再添加：

```text
Native SES
Native Postmark
Native Resend
```

---

# 36. Message-ID

如果原邮件已经带：

```text
Message-ID
```

默认保留。

如果没有：

Gateway 生成。

例如：

```text
<0199...@gateway.example.com>
```

同一邮件无论经过几次 Provider retry：

必须保持同一个：

```text
Message-ID
```

---

# 37. Header Preservation

默认 Transparent Mode。

不要修改：

```text
Subject
From
To
Message-ID
MIME boundaries
HTML
text
attachments
```

必要时允许增加：

```text
Received
X-Gateway-ID
```

但最好提供设置：

```text
preserve_message=true
```

如果目标是真正 byte-level transparent，则不能重新序列化 MIME。

直接保存并发送原始 DATA。

---

# 38. DKIM

必须特别处理。

如果收到的 EML 已经包含有效 DKIM：

任何 header/body 修改都有可能破坏 DKIM。

所以默认：

```text
DO NOT MODIFY MESSAGE
```

未来 Gateway 可以支持自己 DKIM 签名。

但 MVP 不要求。

---

# 39. Web UI 页面

MVP 至少：

```text
Dashboard
Messages
Message Detail
Providers
Provider Detail
Settings
SMTP Credentials
API Keys
```

---

# 40. Dashboard

显示：

```text
Last 24h

Messages received

SMTP accepted

Temporary failed

Permanent failed

Unknown

Bounced

Queue depth
```

Provider Health：

```text
SpaceMail
Healthy
99.8%

P95
720ms

Last error
—
```

---

# 41. Message List

字段：

```text
Status

Timestamp

From

Recipient

Subject

Provider

Attempts

Latency
```

支持搜索：

```text
recipient
sender
subject
message ID
gateway ID
provider
```

筛选：

```text
status
provider
date
sender domain
recipient domain
```

---

# 42. Message Detail

这是整个产品最重要页面。

Header：

```text
Subject

From

To

Current status

Gateway ID

Message-ID
```

Timeline：

```text
Received

Archived

Queued

Attempt #1

TCP Connected

TLS

AUTH

MAIL FROM

RCPT TO

DATA

SMTP 250
```

如果失败：

显示：

```text
raw SMTP error
```

---

# 43. Message Inspector

Tabs：

```text
Overview

Timeline

HTML

Text

Headers

MIME

Raw EML

Attempts

Events
```

---

# 44. Provider Page

展示：

```text
Provider name

Health

Host

Port

TLS

Priority

Rate limit

Success rate

Temporary failure rate

Connection latency

Last successful send

Last failure
```

提供：

```text
Test Connection
```

---

# 45. Test Connection

测试：

```text
DNS

TCP

TLS

EHLO

AUTH
```

不要真的发邮件。

返回：

```text
DNS OK
TCP 38ms
TLS 91ms
AUTH OK
```

---

# 46. SMTP Credential 管理

用户创建：

```text
username
password
```

password：

只在创建时显示一次。

数据库保存 hash：

```text
Argon2id
```

不要可逆。

---

# 47. API Key

格式：

```text
mg_live_xxxxxxxxx
```

数据库只保存：

```text
hash
prefix
last_used_at
created_at
```

只在创建时显示完整 key。

---

# 48. Admin Authentication

MVP：

```text
single admin account
```

密码：

```text
Argon2id
```

Session cookie：

```text
HttpOnly
Secure
SameSite
```

未来：

```text
RBAC
multi-user
OIDC
```

---

# 49. Security

必须注意：

SMTP Gateway 如果配置错误会成为 spam relay。

所以：

默认：

```text
require SMTP auth
```

绝对禁止：

```text
anonymous public relay
```

除非 explicit trusted IP。

---

# 50. Trusted Networks

可选：

```env
SMTP_TRUSTED_CIDRS=
```

例如 Docker 内网：

```text
172.18.0.0/16
```

允许免 AUTH。

但默认空。

---

# 51. HTTP Security

必须：

```text
CSRF protection
rate limiting
secure cookies
content security policy
```

Raw HTML Preview 要 sandbox。

不要直接：

```text
innerHTML
```

到 Admin 页面上下文。

使用：

```text
iframe sandbox
```

避免邮件 HTML XSS。

---

# 52. Attachment Security

不要自动执行附件。

下载：

```text
Content-Disposition: attachment
```

Raw preview 做 MIME type handling。

---

# 53. Logging

应用日志 JSON structured。

字段：

```text
timestamp
level
component
message_id
attempt_id
provider_id
error
```

不要日志：

```text
SMTP password
API key
AUTH payload
```

---

# 54. Metrics

Prometheus endpoint：

```text
/metrics
```

至少：

```text
mailgateway_messages_received_total

mailgateway_messages_queued

mailgateway_delivery_attempt_total

mailgateway_delivery_success_total

mailgateway_delivery_failure_total

mailgateway_delivery_unknown_total

mailgateway_provider_latency_seconds

mailgateway_queue_depth
```

---

# 55. Health Endpoints

```http
GET /health/live
GET /health/ready
```

live：

应用进程活着。

ready：

```text
PostgreSQL available
storage writable
```

---

# 56. Graceful Shutdown

收到：

```text
SIGTERM
```

行为：

```text
停止接收新请求

停止 claim 新 job

等待当前 SMTP operation

释放 lease

关闭 DB
```

Docker stop 不应造成邮件状态损坏。

---

# 57. Docker Compose

默认 compose：

```yaml
services:
  gateway:
  postgres:
  caddy:
```

Volumes：

```text
postgres_data

mail_data
```

---

# 58. 推荐生产服务器

最低：

```text
1 vCPU
1GB RAM
20GB SSD
```

推荐：

```text
2 vCPU
2GB RAM
40GB SSD
```

适用于：

```text
几十万封 / 月以下
```

具体 throughput 更多由下游 SMTP Provider 限制。

---

# 59. 配置系统

配置优先级：

```text
CLI args
↓
Environment variables
↓
config.yaml
↓
defaults
```

敏感信息：

推荐 environment variables / secret file。

---

# 60. Retention

用户必须能配置：

```text
metadata retention

event retention

EML retention

debug log retention
```

例如：

```text
Metadata:
forever

EML:
180 days

SMTP debug:
30 days
```

默认不要自动永久保存 debug transcript。

---

# 61. 数据删除

删除 Message 时：

```text
delete metadata
delete recipients
delete attempts
delete events
delete EML
```

最好：

```text
soft delete
```

然后后台 cleanup。

---

# 62. Database Index

至少：

```text
messages(created_at)

messages(status)

messages(next_attempt_at)

messages(message_id)

recipients(address)

delivery_attempts(message_id)

events(message_id, event_time)

providers(enabled)

suppression_entries(email)
```

---

# 63. Queue 性能

避免 worker：

```text
每100ms疯狂 poll
```

建议 PostgreSQL：

```text
LISTEN / NOTIFY
```

发送时：

```text
NOTIFY queue_new_message
```

Worker：

```text
wait notification

fallback poll every few seconds
```

---

# 64. 并发控制

同一 message 不能被两个 worker 同时发送。

使用：

```text
FOR UPDATE SKIP LOCKED
```

以及 lease。

必须写测试验证。

---

# 65. Idempotency

HTTP API：

必须支持：

```text
Idempotency-Key
```

SMTP：

自然没有这个 header。

可以允许：

```text
X-MailGateway-Idempotency-Key
```

但不是 MVP 必需。

---

# 66. Duplicate Prevention

同一 message 在 retry 时：

始终保持：

```text
message id
EML
```

provider attempts 独立。

不要每 retry 重新生成邮件。

---

# 67. Multi Recipient

必须支持 SMTP：

```text
MAIL FROM

RCPT TO A

RCPT TO B

DATA
```

如果：

```text
A 250
B 550
```

要正确记录：

```text
A accepted
B rejected
```

不要把整个 message 简化成 failed。

---

# 68. SMTP Enhanced Status Code

解析：

```text
550 5.1.1 User unknown
```

保存：

```text
smtp_code = 550

enhanced_code = 5.1.1
```

---

# 69. Error Taxonomy

不要只保存 string。

分类：

```text
DNS_ERROR

CONNECT_TIMEOUT

CONNECT_REFUSED

TLS_ERROR

AUTH_ERROR

MAIL_FROM_REJECTED

RCPT_TEMP_REJECTED

RCPT_PERM_REJECTED

DATA_TEMP_REJECTED

DATA_PERM_REJECTED

FINAL_RESPONSE_TIMEOUT

CONNECTION_RESET

PROVIDER_RATE_LIMIT

LOCAL_STORAGE_ERROR

DATABASE_ERROR

UNKNOWN
```

这是后续健康检查的基础。

---

# 70. Testing Strategy

测试必须至少分：

```text
unit
integration
SMTP protocol
database
failure injection
```

---

# 71. SMTP Fake Server

测试目录需要提供一个 Fake SMTP Server。

它可以模拟：

```text
正常 250

AUTH 失败

RCPT 550

DATA 451

final response timeout

connection reset

slow response

TLS failure
```

这是这个项目最重要的测试组件之一。

---

# 72. 必须测试 DELIVERY_UNKNOWN

测试：

```text
Gateway sends full DATA
Fake server receives message
Fake server closes socket
without final 250
```

预期：

```text
DELIVERY_UNKNOWN
```

并且：

```text
不能自动 failover
```

---

# 73. Failover 测试

场景：

```text
Provider A TCP refused

Provider B normal
```

应该：

```text
A attempt failed

provider changed

B accepted
```

---

# 74. Retry 测试

Provider：

```text
451 temporary failure
```

应该：

```text
TEMP_FAILED

retry scheduled
```

然后下一次成功。

---

# 75. Crash Recovery

测试：

Worker claim message 后：

```text
kill process
```

等待 lease expire。

另一个 Worker 必须可以重新 claim。

---

# 76. SMTP Durability Test

客户端完成 DATA 后：

Gateway 必须确保：

```text
file persisted

DB transaction committed
```

才返回：

```text
250
```

测试模拟程序 crash。

---

# 77. Security Test

必须验证：

```text
无认证 SMTP 不能 relay

错误用户不能认证

用户不能 spoof restricted sender
```

---

# 78. Web UI Test

重点测试：

邮件 HTML 不能：

```text
execute JS
steal admin cookie
redirect parent
```

---

# 79. 开发阶段

建议 Codex 分阶段完成。

## Phase 0

项目骨架。

实现：

```text
Go project

config

database connection

migrations

Docker

health endpoint
```

完成标准：

```text
docker compose up
```

能运行。

---

# 80. Phase 1

实现 SMTP ingress。

包括：

```text
587 listener

AUTH

MAIL FROM

RCPT TO

DATA

EML archive

messages table

recipients table
```

完成标准：

可以用：

```text
swaks
```

发一封 SMTP 邮件到 Gateway。

后台：

```text
收到 EML
写 DB
返回 250
```

---

# 81. Phase 2

实现 Provider SMTP Sender。

Generic SMTP Provider。

例如：

```text
SpaceMail

PurelyMail
```

从数据库 Queue 读取。

发送成功记录：

```text
SMTP_ACCEPTED
```

---

# 82. Phase 3

实现：

```text
delivery_attempts

events

protocol timings

error taxonomy
```

此时开始有真正 Flight Recorder。

---

# 83. Phase 4

实现：

```text
Retry

safe failover

DELIVERY_UNKNOWN
```

这是 MVP 最核心可靠性阶段。

---

# 84. Phase 5

实现 Web UI。

至少：

```text
Dashboard

Messages

Message Detail

Providers
```

---

# 85. Phase 6

实现：

```text
HTML preview

Raw EML

Headers

MIME tree
```

---

# 86. Phase 7

实现 Provider Health。

```text
health calculation

provider latency

circuit breaker

test connection
```

---

# 87. Phase 8

Bounce。

实现：

```text
DSN parser

IMAP bounce ingestion

manual bounce webhook
```

---

# 88. Phase 9

HTTP API。

```text
send endpoint

API Keys

Idempotency
```

---

# 89. Phase 10

Production Hardening。

包括：

```text
metrics

backup docs

retention

cleanup worker

security

rate limits

audit logs
```

---

# 90. MVP 定义

MVP 不是 UI Demo。

MVP 必须可以真实作为：

```text
App → Gateway → SpaceMail
```

运行。

至少具备：

- SMTP ingress
- SMTP AUTH
- Raw EML 保存
- PostgreSQL durable queue
- Generic SMTP provider
- SpaceMail
- PurelyMail
- 多 provider priority
- retry
- safe failover
- DELIVERY_UNKNOWN
- delivery attempts
- event timeline
- basic Web UI
- provider health basic
- Docker Compose
- HTTPS admin
- TLS SMTP
- SMTP credential management

---

# 91. v1 定义

v1 增加：

- HTTP API
- API keys
- Idempotency
- DSN bounce parser
- IMAP bounce ingestion
- suppression
- advanced search
- retention
- metrics
- circuit breaker
- rate limit
- Webhook
- Object Storage
- audit log
- provider-specific adapters

---

# 92. README 首屏建议

不要把产品描述成：

```text
Open-source email sending platform
```

应该：

> MailGateway is a self-hosted SMTP control plane and flight recorder for the email providers you already use.

架构图：

```text
Your Apps
   │
   │ SMTP
   ▼
MailGateway
   │
   ├── SpaceMail
   ├── PurelyMail
   ├── Amazon SES
   └── Any SMTP
```

下一句：

> Change your SMTP host. Keep your email code.

---

# 93. README 核心展示

展示一封邮件 Timeline：

```text
11:03:42.121  Received
11:03:42.126  Archived
11:03:42.131  Queued

11:03:42.225  SpaceMail connection
11:03:42.267  TCP connected
11:03:42.369  TLS established
11:03:42.451  Authenticated
11:03:42.499  RCPT accepted
11:03:42.901  DATA completed
11:03:43.114  SMTP 250 accepted
```

让开发者第一眼明白产品价值。

---

# 94. 第一批实际测试 Provider

开发过程中至少真实测试：

## SpaceMail

```text
mail.spacemail.com
465 TLS
587 STARTTLS
```

## PurelyMail

```text
smtp.purelymail.com
465 TLS
587 STARTTLS
```

然后测试：

```text
Gmail recipient

Outlook recipient

iCloud recipient
```

---

# 95. 不要做的错误架构

Codex 不应：

### 错误 1

业务请求直接等待 Provider SMTP。

正确：

```text
durably queue first
```

### 错误 2

用：

```text
sent = true
```

正确：

```text
SMTP_ACCEPTED
```

### 错误 3

SMTP error 就直接 fallback。

正确：

根据：

```text
失败阶段
```

判断。

### 错误 4

重新生成 MIME。

正确：

尽可能保存 raw MIME。

### 错误 5

所有 recipient 一个状态。

正确：

recipient-level state。

### 错误 6

queue 全放内存。

正确：

PostgreSQL durable queue。

### 错误 7

Provider 密码明文数据库。

正确：

encrypted credentials。

### 错误 8

公开 SMTP relay。

正确：

默认认证。

---

# 96. Codex 编码要求

代码要求：

- Go idiomatic
- 小接口
- 避免过度 abstraction
- 避免 premature microservice
- 所有 DB 操作 context-aware
- 所有 network call timeout
- 所有 goroutine 可 shutdown
- 使用 structured logging
- 错误使用 wrap
- SMTP error 不丢 raw response
- 所有 timestamp 使用 UTC
- UI 显示按浏览器 timezone 转换
- database 使用 TIMESTAMPTZ

---

# 97. 时间规则

数据库统一：

```text
UTC
```

例如：

```text
2026-09-10T15:03:42.121Z
```

Web UI：

根据浏览器 timezone：

```text
America/New_York
```

显示。

Raw event 保留 microseconds / milliseconds。

---

# 98. 数据一致性

关键操作：

```text
archive EML
+
database message creation
+
queue creation
```

必须考虑 partial failure。

建议：

1. 写 temporary EML
2. fsync
3. rename
4. DB transaction
5. commit
6. SMTP return 250

如果 DB commit 失败：

不要返回 SMTP 250。

---

# 99. Orphan File Cleanup

可能出现：

```text
EML 写成功
DB commit 失败
```

定期 cleanup：

扫描：

```text
/data/eml
```

找没有 DB reference 的文件。

例如：

```text
24h 后删除
```

---

# 100. 备份

必须文档说明：

备份两个部分：

```text
PostgreSQL

/data/eml
```

仅备份 PostgreSQL 不完整。

仅备份 EML 也不完整。

---

# 101. 最终产品目标

最终用户应该能做到：

原来：

```env
SMTP_HOST=mail.spacemail.com
SMTP_PORT=465
```

现在：

```env
SMTP_HOST=gateway.example.com
SMTP_PORT=587
```

然后所有邮件自动拥有：

```text
Durable queue

完整发送历史

原始邮件存档

Provider response

SMTP latency

Retry

Safe Failover

Provider Health

Bounce

Search

Timeline
```

而用户原有应用几乎无需修改。

---

# 102. 产品成功标准

对于任何一封邮件，管理员应该可以回答：

```text
谁提交了邮件？

什么时候提交？

提交的原始内容是什么？

Gateway 是否可靠保存？

进入 Queue 的时间？

尝试了哪个 Provider？

TCP 是否连接成功？

TLS 是否成功？

SMTP AUTH 是否成功？

MAIL FROM 是否成功？

哪些 RCPT 被接受？

DATA 是否完整发送？

Provider 有没有返回 250？

SMTP 原始响应是什么？

耗时多久？

是否发生 retry？

是否发生 failover？

是否存在 duplicate risk？

是否收到 DSN？

是否 Bounce？

最终状态是什么？
```

如果系统无法回答其中大多数问题，就没有实现本产品的核心价值。

---

# 103. 第一版开发优先级

必须优先保证：

```text
Correctness
↓
Durability
↓
Observability
↓
Security
↓
Performance
↓
UI polish
```

绝对不要为了漂亮 Dashboard 牺牲 SMTP 状态正确性。

这个产品最大的价值不是：

```text
漂亮
```

而是：

```text
可信。
```