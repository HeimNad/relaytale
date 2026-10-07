# Phase 9C.5 · 只读容量盘点

## 新命令

```sh
relaytale capacity-report --storage-dir /data/eml --limit 100
relaytale capacity-report --storage-dir /data/eml --limit 100 --after-id <上页 next_after>
```

使用既有 DATABASE_URL、运行账号最小权限、辅助进程占用与 schema 门槛。可与网关共存，不能与离线迁移/恢复互相绕过；异常丢失占用连接时，继续使用既有人工释放流程。CLI 注册/释放辅助占用会写占用记录，盘点本身不修改业务记录、原文或审计。

每页 1–1000 封，默认 100，UUID 顺序；默认 storage-dir 为 EML_STORAGE_DIR 或 data/eml。单次设置 30 秒上下文期限；数据库调用与批次检查响应取消，底层文件元数据系统调用不能被该期限强制打断。参数、读取、数据库或取消失败，不输出部分容量或游标。has_more=true 表示还需继续，不提供全实例合计。

响应 schema_version=1，snapshot_at/finished_at、consistency=LIVE_OBSERVATIONS_NOT_ATOMIC、deletion_authorized=false、has_more/next_after、items、relations。items 包含 message_id、archive_state、recorded_bytes、可观察时的 observed_bytes 和 observation。报告不含邮件内容、收发件地址或原文路径。

## 文件观察口径

- REGULAR_SIZE_MATCH：普通文件长度与记录一致。没有读取正文或校验 SHA-256，不能证明内容正确。
- SIZE_MISMATCH_REVIEW：当前长度不符。保留 observed_bytes，不自动修复。
- MISSING / UNREADABLE：路径不存在或不能观察。必须结合 archive_state；PURGED 缺失是可能的正常结果，不是丢信结论。PURGED 有文件应人工检查，不能恢复成 AVAILABLE 或自动删除。
- OUTSIDE_ROOT、SYMLINK_REVIEW、NON_DIRECTORY_REVIEW、NON_REGULAR_REVIEW：路径越界、符号链接或类型异常。不给出跟随链接后的大小。
- CHANGED_DURING_OBSERVATION：两次元数据观察检测到身份、大小或修改时间变化，或第二次观察失败。不给出确定字节。

os.Root 约束路径访问，每级组件显式拒绝符号链接。两次元数据比较只检测观察点间可见的变化，不能检测所有竞争、同大小重写或扫描后变化。操作不打开正文，也不删除、搬移、重试或处置 UNKNOWN。

observed_bytes 是文件长度，不是文件系统分配块、独占物理占用或卷剩余空间。硬链接可能重复计数，稀疏文件、压缩、快照与共享块不能据此计费。报告是基于数据库引用的分页检查，不覆盖孤儿和临时文件。

已有 inventory-eml / scan-archives 继续用于按日期发现未引用、临时或异常目录条目。此次为 inventory 条目增加 observed_bytes，仅在普通文件两次观察稳定时提供，并增加 CHANGED_DURING_OBSERVATION；未引用文件仍只有复核意义，不授权删除。仍使用原日期游标和有界分页机制；每次枚举目录以确定页名，内存有界，不保证目录枚举耗时恒定。

## 数据库占用与增长

固定观察八张表：messages、recipients、events、delivery_attempts、api_submission_ledger、webhook_outbox、feedback_reports、maintenance_audit。table_bytes 使用 pg_table_size（含 TOAST 等表附属空间），index_bytes 使用 pg_indexes_size，total_bytes 使用 pg_total_relation_size。

固定清单避免无界扫描整个数据库目录。该清单不是全库容量，未含全部额度/反馈/运行表、WAL、临时文件、日志、备份、数据库空闲卷空间。也不将数据库文件大小称为有效数据大小或精确表膨胀量。无需 count(*)、VACUUM、ANALYZE 或新数据库权限。

运维可保存不同时间的报告，比较各表与索引的增长。此次不持久化采样、不计算增长率，也没有后台定时盘点或工作台物理容量图表。未来加入增长趋势前需定义采样间隔、数据库身份、保留期和失败缺口。

## 一致性与安全边界

消息元数据使用 Repeatable Read 只读事务；数据库关系文件大小、文件系统与不同分页不是原子快照。新增较小 UUID、修改或删除可能跨页遗漏/变化。需要完整一致盘点时，应在受控停写窗口中配合既有备份清单校验。报告明确标注 live observations，不用多页累加得出未经说明的全实例事实。

盘点结果不能作为清理授权，不能绕过 9C.2 保留依赖与数据库栅栏。状态查询、自动反馈与外部邮件发送行为均不改变。本阶段无迁移、无新 HTTP 入口。

## 验收

- 完整 make ci 通过：gofmt、go vet、构建及全部竞争检测测试；集成测试 317.941 秒。
- 最终容量与最小权限专项竞争检测通过，5.589 秒。覆盖缺失/大小不符/正常文件、UUID 分页、取消后清空部分报告、异常游标、孤儿条目长度和链接不跟随。
- 路径单元测试覆盖根目录外路径、符号链接、目录和缺失文件；不存在读取外部内容的操作。
- 确定性并发用例在 messages 表锁上暂停引用查询，改写文件后释放锁；两次观察准确判为 CHANGED_DURING_OBSERVATION，observed_bytes 不返回确定值。
- 运行账号最小权限实测可读取八张表的大小，不新增授权。盘点调用前后维护审计与投递尝试未增加。
- 暂存差异检查与 Gitleaks 凭证检查通过。

测试使用隔离 PostgreSQL 和合成存档，不访问真实服务商或收件箱。未做大型生产卷/NFS 性能验收，未测量实际文件系统分配块或自动增长趋势；无相关完成声明。
