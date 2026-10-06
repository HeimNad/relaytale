# Phase 9B.2b：辅助进程持久占用

本阶段补齐独立退信采集、维护、诊断、查询和数据库配置 CLI 的占用登记。与 9B.2a 合并构成当前版本受支持进程的互斥协议；不提供跨区域 HA、自动接管或数据库超级用户隔离。最小权限与密钥轮换仍为 9B.3 / 9B.4。

## 共存与互斥

Gateway 与辅助 CLI 可以同时运行。每个辅助进程在 public.auxiliary_owner 同步提交 UUID、命令槽位、主机/进程、专用 backend 和联系时间，并全程持有迁移准入共享会话锁。取得共享准入后再检查 schema，避免检查与登记之间发生迁移。离线迁移、备份、恢复、恢复验证持有独占准入；辅助进程存在时拒绝离线工作。控制表检查补上会话丢失后的互斥，不能仅依赖 advisory lock。

同一命令槽位只能有一份占用；采集槽位为 collect-feedback:<source>，不同 source 可并行，原有 mailbox source 锁和 UID checkpoint 语义保持。其他命令按命令名分槽，不提供同名 CLI 的并行执行。运行中的 Gateway 内部 maintenance / Webhook 已由其 runtime 占用涵盖。

覆盖 runOperation 分发的全部命令，包括 cleanup、export-records、归档盘点/回收、suppression、UNKNOWN 处置、Provider 诊断、配额、Webhook 和反馈管理；也覆盖 collect-feedback、feedback-collection-status、create-provider、create-smtp-account。只生成本地开发证书、哈希或输出许可证的命令不连接数据库，不登记。list-runtime-owner / list-auxiliary-owners 和人工释放命令属于恢复控制面，不能因遗留占用而无法使用。

## 断连与人工恢复

专用连接每秒检查一次，单次检查两秒超时；失去连接或准确记录后取消命令上下文。命令同步工作结束后才释放；即使采集器把普通 SIGTERM 当作成功，失去占用的释放仍会报错并保留记录。SIGKILL、断连和不确定提交均不自动清除，没有 TTL。文件或远端操作可能需要时间响应取消，遗留记录在此期间继续阻止离线工作。

先用 `relaytale list-auxiliary-owners` 查看 UUID 与槽位，再确认该主机上的原进程、连接及文件操作均已停止，核对采集 checkpoint 或维护结果。随后执行：

```sh
relaytale release-auxiliary-owner --owner-id '<准确 UUID>' \
  --confirm-stopped --actor '<操作者>' --reason '<停止和核对证据>'
```

活跃专用会话持有每份占用的独立锁，拒绝人工释放；错误 UUID 不生效。删除记录与 AUXILIARY_OWNER_RELEASED 审计同事务同步提交。清除辅助占用不清除 runtime 占用、不自动重试 UNKNOWN，也不修改采集 checkpoint。旧 backend 消失与时间陈旧都不是进程停止的证据。

辅助遗留记录阻止离线操作和同槽位重启；不阻止 Gateway 及其他槽位正常运行。运行实例若也有遗留占用，须分别核对并释放。不要写定时清除任务或直接 DELETE 控制表。

## 备份、升级与限制

备份保留两张控制表 schema，但排除占用数据；全程 SHARE 表锁跳过两张控制表，避免阻塞 heartbeat。恢复验证拒绝带遗留占用的数据。旧版本、直接 SQL、跳过 CLI 协议的嵌入调用以及共享 EML 根目录的不同数据库不受保护。升级前必须停止旧 Gateway 和全部旧采集/维护进程，再显式 migrate --offline；不可用新锁替代停止旧进程。

占用只是运行准入，不等于业务操作之间全局串行：正常在线管理仍依赖既有收件人、Provider、Webhook 和归档事务锁及校验。查询命令在正常 Gateway 运行时可用，但在离线恢复时被拒绝；占用查询是例外。

## 验收

针对性 race 集成测试通过（4.657 秒），覆盖 Gateway/不同辅助槽位共存、同 source 重复拒绝、活跃人工释放拒绝、辅助进程阻止迁移与恢复、离线操作阻止辅助启动、占用断连取消、遗留记录禁止自动接管、错误 UUID 与审计、人工释放后重启。既有运行占用与恢复往返测试同时通过。

真实二进制验收通过：Gateway 与 doctor / 采集器共存、重复 source 拒绝、活跃占用拒绝人工释放、采集器阻止迁移、切断专用 backend 后取消假 IMAP TLS 操作并非零退出、遗留记录继续阻止迁移、准确 UUID 人工释放与审计、恢复后查询及迁移成功。完整 make ci 通过：格式、go vet、构建及全部 race 测试通过，隔离 PostgreSQL 集成测试耗时 206.425 秒。暂存差异检查与离线敏感信息扫描通过。全部网络故障测试使用本地假服务，没有连接真实 Provider 或真实收件箱。
