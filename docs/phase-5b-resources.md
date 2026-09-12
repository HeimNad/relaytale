# Phase 5B · 大邮件与队列资源加固

状态：已完成本地验收，Git 里程碑 `phase-5b`。

日期：2026-09-12。范围是本地资源与故障验收，不代表生产容量承诺。没有新增数据库迁移，schema 仍为 7。worker、retry、failover、health 的默认开关保持不变。

## 实现与决策

出站从整封 EML 加点转义副本，改为匿名临时文件快照加有界缓冲。读取原始存档时同时计算 SHA-256，检查精确长度；复制不超过已声明大小，额外字节单独检测。快照创建权限 0600，随即 unlink，验证后一直使用同一描述符。原始路径被覆盖或替换不影响本次发送；关闭或进程退出释放临时块。此实现针对 Linux/macOS，Windows 的 open-unlink 行为不在支持范围。

快照随后完成 CRLF 预检并回卷，才进入 DNS/TLS/AUTH/信封阶段。DATA 授权仍必须先持久化。发送用 32 KiB 缓冲逐块执行点转义，只有源读取成功到 EOF 才发送结束标记；读写失败、取消或最终确认丢失继续进入保守 UNKNOWN，不自动重发。BytesSent 是已处理的原文字节计数，失败时不是精确 socket 字节数，更不是远端接受证明。

不修复或重排 MIME。较旧实现的额外收紧是拒绝裸 CR（此前已拒绝裸 LF）；不合规存档在联网前暂停，不能静默更改已有签名内容。协议依据为 [RFC 5321 的 DATA 与透明传输规则](https://www.rfc-editor.org/rfc/rfc5321.html#section-4.5.2)。

进程共享 FIFO 工作准入：接收预留 1 MiB，出站预留 8 MiB 加最大邮件大小的快照空间。等待可取消，出站在等待时不领取租约或扣 Provider 配额。默认工作预算 64 MiB、临时快照预算 256 MiB；因此默认上限下最多八个出站操作同时持有工作预算，接收与出站共享它。预算不是实际分配，也不是 RSS 硬限额；认证、连接、Go 运行时和系统缓存需另算。永久存档及 PostgreSQL 数据不计入临时快照预算。配置与部署说明见 [运维文档](operations.md#在途资源与指标phase-5b)。

## 负载发现的收尾死锁

首次完整负载产生 1240 次 Provider 最终接受事件，其中 6 次收尾事务因 `40P01` 回滚，最终由过期租约恢复成 UNKNOWN。没有重复 SMTP 投递，但这会产生不必要的人工处置。

原因是 `decideRecipient` 更新 `last_provider_id` 时通过外键取得 Provider 的 KEY SHARE 锁；不同消息的 Finish 随后在健康更新中升级为 FOR UPDATE，形成锁升级死锁。修复在消息租约 fence 后、更新收件人之前取得 Provider FOR UPDATE，维持 message → provider 顺序。没有添加 SMTP 重试或从历史事件自动恢复接受状态。锁冲突依据见 [PostgreSQL 17 行锁与死锁说明](https://www.postgresql.org/docs/17/explicit-locking.html#LOCKING-ROWS)。

回归测试用第三个事务暂持 Provider 的 NO KEY UPDATE 锁，将两个 Finish 同时挡在升级点；修复前镜像稳定触发 deadlock。此缺陷说明单次并发领取测试不足以替代持续收尾压力测试。

## 基础指标

`GET /metrics` 使用 [Prometheus text 0.0.4 格式](https://prometheus.io/docs/instrumenting/exposition_formats/)，只输出固定状态标签及聚合值。包含消息状态（含 UNKNOWN）、最老排队年龄、24 小时尝试结果、配额耗尽、熔断、资源等待/预留、Go 堆与连接池。默认 Caddy 拦截指标路径，只在受控内部网络抓取；独立 HTTP 部署需要自己限制访问。

这些是 gauge，窗口结果会下降；不宣称单调累计计数器。数据库采集超时 2 秒、拒绝同进程并发采集，失败不输出 SQL 错误或邮件信息。查询仍依赖数据库聚合，超大历史表上的采集成本需在后续容量测试中评估。

## 测试范围

- 16 MiB 与精确 25 MiB 同时出站，慢 Fake SMTP 在解除点转义后核对 SHA-256 和长度；25 MiB + 1 字节拒收且不新增消息记录。
- 分块边界的行首点、单字节读取、裸 CR/LF、末尾行边界、源读取异常不得追加 DATA 终止符。
- 原始文件验证后被改写、快照目录不可用、资源等待取消、重复释放、内存/磁盘预算耗尽。
- 大邮件中途断线、最终确认丢失、发送期间取消维持 UNKNOWN；临时文件路径和描述符在正常收尾释放。
- 现有逐收件人、suppression、retry/failover、配额、租约恢复和最终提交失败测试继续运行。

## 负载方法与边界

使用隔离 PostgreSQL 17 和本地 TLS Fake SMTP，不接触真实 Provider。预先积压 40 封，每封 32 KiB；随后以每秒约 10 封持续入队 120 秒，四个 worker，32 MiB 工作预算、100 MiB 快照预算；生产停止后最多等待 90 秒排空。验收阈值为无遗漏/重复 Fake SMTP 接受、资源及快照描述符归零、采样 Go 堆不超过 128 MiB。

采样间隔 100 毫秒，包含同进程测试工具与 Fake SMTP，入队使用 Receiver 路径，不包含持续 SMTP AUTH 压力。数据库使用测试 tmpfs，与生产持久磁盘延迟不同。峰值为采样观测，不能排除短瞬间尖峰。记录六张表的物理大小、死元组估计及 autovacuum 次数；显式 VACUUM ANALYZE 前后对比不等于生产自动维护已充分验证，也不是精确 bloat 测量。

复现（应选择未用于其他测试的独立 Compose 项目）：

```sh
docker compose -p relaytale-phase5b-test --profile test run --build --rm test go test -race -count=1 -timeout=8m ./...
docker compose -p relaytale-phase5b-test --profile test run --rm \
  -e RELAYTALE_LOAD_SECONDS=120 test \
  go test -count=1 -timeout=6m -run '^TestResourceLoad$' -v ./tests/integration
```

## 验收记录

- `go vet ./...`、`git diff --check` 通过。
- 最终 `go test -race -count=1 -timeout=8m ./...` 全部通过，数据库集成测试 134.356 秒，包含确定性锁顺序回归。
- 运行镜像健康检查、内部指标抓取及 Caddy 对 `/metrics`、`/metrics/child` 返回 404 均通过；没有连接真实邮件 Provider。
- 非 race 大邮件测试通过：16 MiB 与 25 MiB，两个发送操作，含入队/发送/超限拒收的测量窗口 1.594 秒。Go 堆基线 516,384 字节，窗口采样峰值 4,259,984 字节（约 4.06 MiB）；RSS 基线 81,973,248 字节，窗口采样峰值 81,588,224 字节（约 77.81 MiB）。RSS 基线略高是独立的起始观测，Go GC 后页面归还可使后续值下降；不能用活动堆代替 RSS。
- 大邮件采样间隔 5 毫秒，包含测试工具/Fake SMTP；16 + 25 MiB 正文不再分别常驻整封内存副本。哈希/长度一致、超限拒收、快照清理及资源归还断言均通过。

### 修复后持续负载结果

原始聚合数据见 [负载 JSON](validation/phase-5b-load.json)。四个 worker、120 秒入队窗口，32 KiB/封，含初始积压 40 封：

| 观测项 | 结果 |
| --- | --- |
| 接受并完成收尾 | 1237 / 1237；无重复、无 UNKNOWN |
| 总处理窗口 / 停止入队后排空 | 122.517 秒 / 2.506 秒 |
| 观测处理速率 | 10.10 封/秒；受约 10 封/秒的输入速率限制，不是最大吞吐 |
| 入队至完成 p50 / p95 / p99 | 1.290 / 2.551 / 2.805 秒 |
| 活动 Go 堆基线 / 窗口采样峰值 | 0.565 / 3.184 MiB |
| RSS 基线 / 窗口采样峰值 | 81.258 / 24.301 MiB；GC 页面归还后低于基线 |
| 工作 / 临时快照预留峰值 | 32 / 100 MiB，未超过预算 |
| 排空后预留 / 等待 / 快照描述符 | 全部为 0 |
| Go goroutine 基线 / 收尾 | 4 / 4 |
| 应用数据库连接采样峰值 | 5 |
| 数据库锁等待者采样峰值 | 1；有短暂竞争，不等于死锁 |
| 独立数据库死锁累计数 | 0；收尾后查询核实 |
| WAL 增量 | 30,229,552 字节（约 28.83 MiB） |

数据库文件增长（KiB，普通 VACUUM 前；dead/live 为统计估计，不是精确行数）：

| 表 | 起始表/索引 | 结束表/索引 | 死元组估计 | autovacuum 次数 | 手动 VACUUM 后死元组 |
| --- | ---: | ---: | ---: | ---: | ---: |
| attempt_recipients | 8/16 | 464/160 | 164 | 1 | 0 |
| delivery_attempts | 8/40 | 1056/360 | 668 | 2 | 0 |
| events | 8/24 | 5744/4040 | 0 | 2 | 0 |
| messages | 8/72 | 720/544 | 883 | 2 | 0 |
| provider_quota | 0/16 | 168/120 | 0 | 0 | 0 |
| recipients | 8/32 | 584/168 | 177 | 2 | 0 |

手动 VACUUM 后消息与尝试统计收敛到 1237 条，事件 28451 条；之前 live 数量略高或偏低是统计延迟/估计。索引在 VACUUM 时也可能扩展维护页，不能把文件大小变化直接解释为业务数据增减。消息/尝试/事件历史会持续增长，清理和长期容量规划仍需单独完成。

空闲轮询导致秒级延迟在本次 p95/p99 中可见。此阶段接受这一延迟取舍，没有据此声称低延迟队列或高吞吐生产容量。


## 后续限制与唤醒决策

本阶段保留三秒轮询，空闲时带来约一个轮询周期的启动延迟；没有把 NOTIFY 当成可靠工作记录。LISTEN 需要独立长连接、断线重连与补扫，当前负载目标优先验证已有领取/恢复语义。需要更低空闲延迟时再增加唤醒优化并保留轮询兜底。

events、attempts、quota 等历史记录仍持续增长；普通 VACUUM 让空间可复用，并不承诺缩小文件或清除业务历史。元数据保留期、索引/分区策略、长时间压力与生产磁盘测试留在 Phase 9。完整连接准入、AUTH 压力下的总内存、多个实例竞争、真实 Provider 大附件与数日运行均未在此阶段验收。下一阶段是 5C 最小管理 API。
