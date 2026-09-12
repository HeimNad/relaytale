# Phase 5A · 信誉保护基础

本阶段提供人工管理的 suppression、接收入队和 DATA 前的强制检查；完整自动 DSN 接收与判定仍属于 Phase 7。自动重试、切换、熔断和 worker 的默认开关不变。配置了有效抑制记录后不能用关闭 retry/health 绕过检查。

## 地址规则与历史

范围是整个 RelayTale 数据库，不区分 SMTP 账号、Provider 或发件域。使用 PostgreSQL `lower()` 对完整地址匹配，索引和查询使用同一种规则；这是网关的保守抑制策略，并非声明所有邮箱服务的 local-part 都不区分大小写。不会去掉 `+tag`、折叠点号、解析别名或自行执行 Unicode/IDNA 等价转换。操作命令要求单个裸邮箱地址，不接受显示名、地址列表或控制字符。

条目包含 UUID、邮箱、分类、操作来源、创建者、说明、创建时间、到期和解除时间。`hard_bounce / complaint / invalid / unsubscribe` 是操作者填写的分类，不是系统已自动核实的结论。修改必须带 actor 和 reason；本地 actor 是审计标签，实际权限边界仍是进程与数据库凭证，不能当成已实现管理员身份认证。

同一地址最多有一个未解除的条目。有效期为未解除且 `expires_at` 为空或大于数据库当前时间。到期条目再次添加时保留旧行并腾出唯一位置；新条目使用新 UUID。解除按 UUID 操作，重复解除或解除已替换的旧 ID 返回冲突，不会碰到新条目。添加/解除与 `maintenance_audit` 在同步提交事务内完成，审计失败则整体回滚。

查询包含已解除和过期历史，返回 `active`、创建者与说明。按 UUID 游标翻页，每页 1–200 条；`next_after` 为空表示本页已到末尾，满页时下一页可能为空。并发新增不提供查询快照保证。

## 投递检查点

| 检查点 | 行为 |
| --- | --- |
| 入队事务 | 先可靠保存原始 EML，再逐收件人检查；命中者 SUPPRESSED，其余 QUEUED，和事件一起提交 |
| 全部命中 | message 为 SUPPRESSED，记录 MESSAGE_SUPPRESSED，不创建 attempt，不预留 Provider 配额 |
| 领取前 | 每次 Claim 独立处理最多 20 封未锁定邮件；即使 Provider 不可用或自动重试关闭，也能处理已有 QUEUED/TEMP_FAILED 收件人 |
| 领取事务 | 活跃抑制地址不可被选入 attempt；命中数量超出扫描批次也不会漏发 |
| DATA 授权事务 | 在租约和领取标识校验后重新检查；抑制操作先提交则拒绝授权 |
| 授权之后添加 | 已持久化 DATA_ARMED 的尝试允许继续，不能撤回；其成功、失败或 UNKNOWN 仍按 SMTP 证据记录 |
| UNKNOWN 人工重试 | 活跃抑制时拒绝 retry，保留 UNKNOWN；人工标记送达/失败仍是单独审计的操作 |

SMTP ingress 的最终 250 表示原文、处理结果与审计已持久化，不表示所有收件人都已发送。命中地址仍保留原文与收件人历史，没有修改 MIME 的 To/Cc/Message-ID。包含 Bcc 或多个信封收件人的邮件，其他收件人仍按原 MIME 发送。

扫描是 worker 执行的后台进度，关闭 worker 时已有排队行不立即全部改成 SUPPRESSED；入队检查仍生效，未来 Claim 与 DATA 检查仍会阻止活跃名单中的地址。解除或到期不会恢复任何已经 SUPPRESSED 的行，也不会触发历史邮件重放。若一条规则在排队邮件被检查前就到期或解除，该尚未被抑制的排队邮件可继续；短时规则不是“永久取消已有队列”的命令。

多收件人聚合顺序继续优先 UNKNOWN、SENDING 和 QUEUED；全部抑制时为 SUPPRESSED，部分接受加部分抑制为 PARTIAL_ACCEPTED。逐收件人的明确状态是完整依据，不把部分成功写成全员接受。

## 会话中途抑制与崩溃

如果 RCPT 已发出但 DATA 尚未获准时新增抑制，SMTP 无法撤销其中一个已接受的 RCPT，因此中止整次会话。授权事务保存命中者 SUPPRESSED、RECIPIENT_SUPPRESSED、SUPPRESSION_GATE_BLOCKED 与 `suppression_blocked_at`；数据库约束禁止同一个 attempt 同时具有这个标记和 DATA_ARMED。

完成事务保留实际 RCPT 450/550 等证据；命中者维持 SUPPRESSED。其他已获 RCPT 接受但未发 DATA 的收件人记录 `RESUME_SAME_PROVIDER`，沿当前路由重新领取。这个动作来自持久化的本地抑制中止，不适用于一般数据库错误，也不允许据此 failover。原来的自动重试许可条件、逐收件人次数和 24 小时预算保留；达到 7 次或时间窗口上限则暂停人工处理。尚未发送的初次投递可以继续，不依赖启用远端失败的自动重试。

已预留的收件人尝试配额不退款，继续投递会消耗下一次配额。抑制中止不计入 Provider 失败样本；半开探测若被本地策略中止，不凭此宣告 Provider 已恢复。

即使随后解除条目、完成事务失败或进程崩溃，已提交的抑制结果也不会丢失。未授权 DATA 的旧尝试通过租约恢复，保留被抑制者，只恢复其余安全收件人；授权过 DATA 的崩溃继续进入 UNKNOWN。

并发由 PostgreSQL 事务级共享/独占 advisory lock 协调。修改名单使用独占锁且不锁消息；检查使用共享锁，领取者之间可并行。锁只覆盖数据库事务，不跨 SMTP、TLS 或 EML 文件读取。当前是全局策略锁，批量扫描及高频名单修改的吞吐、延迟与索引成本需在 5B 的负载测试中测量；本阶段不宣称高负载容量结论。

## 迁移与运行版本

schema 7 增加名单历史字段和 attempt 的抑制标记，不改写已有 SMTP 事实。旧条目创建者标为 legacy_unknown。回退迁移默认拒绝执行，避免删除名单历史或发送边界证据；需另做明确的数据与版本回退方案。

升级时停止旧 ingress/worker 后再迁移并运行新版本，不能混跑尚不认识 suppression 的旧 worker。旧二进制即使能读取新表，也不保证执行本阶段规则。本次只在隔离测试数据库验证迁移，未升级真实投递服务。

## CLI

命令在 schema 7 迁移完成后使用；通过本地操作进程连接数据库，不对外开放管理接口。既有部署先参考 [改名说明](rename-relaytale.md) 完成服务名升级。

```sh
docker compose exec -T relaytale relaytale add-suppression \
  --email blocked@example.test --category manual \
  --actor operator --reason '已人工核实，工单 SUP-001'

docker compose exec -T relaytale relaytale list-suppressions \
  --email blocked@example.test --limit 100

docker compose exec -T relaytale relaytale release-suppression \
  --id <Suppression-UUID> --actor operator --reason '已复核，允许未来投递'
```

可选 `--expires-at` 使用未来的 RFC3339 时间；不填写即永久有效，直到显式解除。分页通过 `--after <next_after>`。分类默认 manual，所有分类均需提供 actor/reason。保留规则和审计尚无自动删除路径，不复用 EML 清理来删除 suppression。

## 验收范围

测试覆盖入队全命中与部分命中、大小写匹配与不合并别名、原始 MIME 保持、无 Provider 时的暂停重试拦截、到期与重新添加、旧 ID/重复解除、并发添加、查询分页、UNKNOWN 与人工重试、DATA 授权前后边界、策略锁等待、混合 RCPT 450/550、预算耗尽、崩溃恢复、完成提交失败以及管理/接收/授权审计失败回滚。

普通 SMTP 5xx 不会自动生成名单；重复、伪造、无法关联的 DSN 样式邮件作为普通邮件内容处理，不改变原投递状态或名单。该测试只证明当前没有“内容直接驱动抑制”的入口，不代表已经实现可信 DSN 消费者。

可信通道设计与发件身份验收见 [反馈与认证责任](feedback-and-sender-identity.md)。

## 本地验收记录（2026-09-11）

- `go vet ./...` 与 `git diff --check` 通过。
- `go test -race -count=1 -timeout=8m ./...` 全套通过，隔离 PostgreSQL + Fake SMTP 集成测试耗时 94.764 秒。
- 生产镜像构建与非 root 运行通过；SMTP/worker 关闭时 `/health/ready` 返回 ready，`doctor` 报告 schema_version 7。
- 实际二进制的 add/list/release 命令通过：添加后 active=true，解除后 active=false，输出明确 historical_messages_requeued=false。
- 文档本地链接检查通过。没有启动真实投递服务或发送外部邮件。

首轮集成测试发现条件通知语句中 UUID 参数被 `pg_notify` 推断为 text；增加显式 UUID 转换后，上述全套测试通过。高负载容量、完整自动反馈与真实域身份的剩余缺口仍按路线图继续验收。
