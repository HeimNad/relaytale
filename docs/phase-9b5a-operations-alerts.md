# Phase 9B.5a：共享额度指标与运维告警

本阶段提供只读指标、可选 Prometheus 告警规则和处置流程。不改变投递决策、重试开关、熔断执行开关或 UNKNOWN 人工处置边界；不自动连接 Provider，不开启通知发送。Provider 能力声明、限制来源及发件域诊断继续列入 9B.5b。

## 指标边界

`relaytale_providers_quota_exhausted` 现在统计受单独额度或共享组额度阻挡的已启用 Provider，每个 Provider 只计一次。共享组用量包含所有成员的历史预留，包括已停用配置和未成功发送的尝试；不是成功邮件数、Provider 剩余套餐或实际余额。按现有滚动 1 小时/24 小时窗口计算，0 表示共享组未配置该窗口限制。

新增指标：

| 指标 | 含义 |
| --- | --- |
| `relaytale_providers_enabled` | 已启用配置数，不保证域名、消息大小、身份、健康或配额允许发送 |
| `relaytale_quota_groups_active` | 至少有一个已启用成员的共享组数 |
| `relaytale_quota_groups_exhausted` | 上述组中至少一个滚动窗口耗尽的组数 |
| `relaytale_recipients_delivery_unknown` | 必须人工确认的收件人数，不自动重发 |

指标保持聚合形式，不包含 Provider 名称/ID、收件地址、邮件内容、密钥或账号信息。整个抓取超时/失败返回 503 和 `relaytale_metrics_scrape_success 0`，不输出部分成功快照。指标的不同查询不是一致性数据库快照，瞬时边界可能有小幅差异；告警需持续满足条件。

## 可选部署

规则在 `docker/prometheus/alerts.yml`，抓取示例在 `docker/prometheus/prometheus.example.yml`。在现有 Prometheus 配置中使用 `job_name: relaytale`，加载规则并部署独立 Alertmanager 的接收与路由。项目默认 Compose 不部署这两个服务，不代表通知已经接通。依据 [Prometheus 告警规则文档](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/)，通知投递与去重由 Alertmanager 负责。

示例通过私有文件提供 bearer token，文件内容应与 Relaytale 的 `METRICS_TOKEN` 相同，不能提交到仓库。抓取端要能访问容器网络内的 8080；不要为抓取直接公开端口，也不要通过默认阻挡 `/metrics` 的 Caddy 地址抓取。跨主机访问使用受控网络和 TLS。为实例增加部署环境标签便于值班区分，避免将邮件地址、域名或账号标成告警标签。

阈值是可调整的起点，不是业务 SLA：

| 告警 | 条件及持续时间 | 首次处置 |
| --- | --- | --- |
| MetricsUnavailable | 抓取失败、显式失败或 `up=1` 时缺少成功指标，2 分钟 | 检查服务、token、抓取路径、数据库与慢查询；监控失败不能当作队列健康 |
| QueueStalled | 最老 QUEUED 邮件超过 300 秒，持续 5 分钟 | 检查 worker 是否启用、预定发送/重试时间、域名路由、suppression、共享额度和健康策略；不直接重置状态 |
| DeliveryUnknown | UNKNOWN 收件人数非零，2 分钟 | 查看 attempt 与反馈证据，人工决定；严禁通过重试/切换绕过 UNKNOWN |
| QuotaExhausted | 至少一个已启用 Provider 的个人/共享额度耗尽，5 分钟 | 查看管理 API 共享额度用量，确认 Provider 套餐与其它发送系统用量，等待窗口或审计调整；不删 ledger |
| NoEnabledProvider | 已启用配置数为零，2 分钟 | 确认是停机/收件沙箱还是误停用；不能自动启用 Provider |
| CircuitOpen | 有已启用 Provider 记录 OPEN，5 分钟 | 检查近期结果与认证/配置故障，确认健康执行开关；记录 OPEN 不等于执行已阻断 |
| ResourceWaiting | 有操作等待资源预算，10 分钟 | 检查工作预算、并发、磁盘与进程资源；不要只加 worker 数 |

所有规则匹配 `job="relaytale"`，每个实例独立评估。目标仍在抓取配置中时，抓取不可达/指标缺失会被检测；目标被从 Prometheus 配置移除时，需部署清单或独立监控发现，不能由此规则保证。正常计划维护使用监控平台的时间受限静默，不能清除 UNKNOWN 或历史额度。

队列年龄按 `created_at` 计算，包含预定、退避或受阻的 QUEUED 邮件，不能直接证明 worker 卡死；根据业务定时需求调整阈值或静默。没有已启用 Provider 在 ingress-only/沙箱部署中可能是预期行为。警告表示需要检查，不是 SMTP 接受或入箱证明。

## 验收

规则测试通过官方 `promtool`，固定 Prometheus 3.5.0 镜像摘要；测试格式依据 [官方规则测试说明](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/)。运行 `make test-alerts` 检查语法、延迟触发、恢复、无法抓取、成功指标缺失和其它 job 不误报；CI 新增独立任务执行它。

隔离数据库测试覆盖共享组耗尽、停用成员的历史用量仍计入、窗口到期恢复、无启用成员、指标无私密标签。完整 `make ci` 通过（格式、静态分析、构建与竞态测试；数据库集成耗时 223.739 秒）。最后补入的最终响应丢失场景定向竞态测试通过（2.404 秒），确认两个 UNKNOWN 收件人的聚合计数。`make test-alerts` 对七条规则及抓取配置校验通过，秘密扫描无泄漏。未向任何真实通知接收方发送告警。
