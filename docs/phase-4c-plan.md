# Phase 4C：Provider 健康、熔断与配额

基线 `phase-4b`（`5b09317`）。保持投递决策、原始 EML 和 UNKNOWN 边界。熔断执行默认关闭，配额只在 Provider 明确配置限制时生效。本轮不发送真实邮件。

## 健康与熔断

- 每次完成尝试记录 SUCCESS / FAILURE / IGNORED 和数据库观察时间。统计最近 10 分钟的有效样本；至少 5 个样本且失败比例 >=50% 时熔断。
- DNS/TCP/TLS、Provider 协议建立失败、认证临时故障、DATA 临时拒绝和最终响应丢失归于可用性失败。收到最终接受视为成功，含部分收件人被拒绝的正常投递。
- 收件人拒绝、永久消息拒绝、AUTH 配置拒绝、凭证解密、本地存储和记录器故障不计入可用性分母。认证配置错误仍需操作者修复/禁用 Provider，不伪装为网络抖动。
- CLOSED → OPEN，冷却 60 秒；冷却后实际领取一个 HALF_OPEN 尝试。Provider 行锁与持久化 probe attempt 保证跨 worker/进程只有一个探测。它使用本来可安全投递的队列邮件，不额外发送探测邮件。
- 半开收到最终接受后 CLOSED，重置统计起点；其余结果重新 OPEN。先前在途尝试不能关闭新的熔断周期，也不将旧尝试带入新周期统计。UNKNOWN 作为邮件状态继续保持，不因 Provider 恢复而重发。
- 半开 worker 崩溃时，租约恢复释放 probe 并重新冷却；不会用进程崩溃捏造 Provider 失败样本。DATA 授权后的邮件仍进入 UNKNOWN。
- `HEALTH_ENABLED` / YAML `health_enabled` / `--health-enabled` 默认 false；Provider 的 `health_check_enabled` 也控制是否执行熔断。样本仍记录。停用执行不取消已在途工作；重新启用会继续读取持久化熔断状态。

## 配额语义

- `hourly_limit` / `daily_limit` 为滚动 1 小时 / 24 小时的**收件人尝试预留数量**。创建 Provider 支持 `--hourly-limit` / `--daily-limit`，0 表示不限制，数据库存 NULL。
- 在领取事务里按当前剩余额度选择收件人子集，记录一条 attempt 唯一的 reservation 和 QUOTA_RESERVED 事件。Provider 锁内再次检查额度、并发与熔断；没有额度不领取。
- 配额在领取时计费，重试、切换、连接失败、本地准备失败、UNKNOWN 和 worker 崩溃都不退回。事务整体回滚则没有消耗。这比只计最终接受保守，但避免重复使用不确定额度。
- 配额时间基于 admission/reservation，不等同于服务商实际发送时间或计费口径。系统外发送、不同 Provider 配置共用同一账号的额度不会自动合并；应使用一个配置代表一个配额池，并按服务商口径设置保守值。
- 额度不足时分批发送；已完成收件人不会重发。跨 Provider 后余下批次也会保留逐人切换审计。额度到期后由正常队列轮询推进，无需内存计时器。
- 迁移 6 为最近 24 小时历史尝试保守补记收件人预留，添加健康字段和查询索引，不改历史投递状态。未来配额不会因进程重启而清零。

## 可观测性与验收

`list-providers` 输出配置限额、1h/24h 已预留数量、10 分钟健康样本、熔断状态、冷却时间、probe attempt 和最近成功/失败。`test-provider` 仍只是连接诊断，不扣投递额度，不替代实际半开尝试，也不关闭熔断。

阶段测试覆盖并发配额不超售、1h/24h 窗口、分批与跨 Provider 审计、事务失败回滚、UNKNOWN/崩溃不退款、健康故障归属、过期样本、执行开关、单半开尝试、成功恢复/失败重开、崩溃与过时完成拒绝。配额 ledger 的长期清理、按服务商自定义计费单位、指标面板与管理员修改配置入口留到后续。

## 验收结果

- `go vet ./...` 通过；健康归属与配置单元 race 测试通过。
- 最终隔离容器 `go test -race -count=1 -timeout=8m ./...` 全部通过，PostgreSQL + Fake SMTP 集成部分耗时 110.625 秒。
- `list-providers` 对隔离测试配置验证了限额、用量、健康样本与熔断字段。
- 隔离容器 readiness 正常，迁移版本 6；未提供控制开关时启动记录确认 workers=0、automatic_retry=false、automatic_failover=false、provider_health=false。
- 已核对暂存内容，不含真实邮件、凭证或数据库数据。真实服务商的配额口径及熔断互备仍未做外部验收；现有真实账号未启用投递。
